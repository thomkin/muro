// Package worktree implements the host-side git plumbing behind DESIGN.md
// §15 (multi-repo git worktree isolation): creating and reusing a fresh
// `git worktree` per sandbox, detecting whether it has unmerged work, and
// squash-merging it back into the real repo's base branch. Everything here
// runs against real host paths, unsandboxed, as the real user — the same
// posture internal/gitproxy already takes for the sandbox-mediated git
// tool-proxy (this package reuses gitproxy.RunGit rather than duplicating
// its capped-output subprocess exec). Nothing in this package ever runs
// inside a sandbox; it's exclusively called from murod (internal/sandbox's
// Manager), which is what makes "the agent can't merge itself, only ask"
// (DESIGN.md §15) an actual guarantee rather than a convention.
package worktree

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/thomkin/muro/internal/gitproxy"
)

// baseBranchSidecarPath returns the plain-text file muro uses to remember
// which branch a worktree was created from — a SIBLING of the worktree
// directory itself (never inside it), so it's never visible to `git
// status`/committed by the agent working inside the worktree. Git itself
// has no first-class "this branch's base branch was X" relationship to
// query later, so this is muro-owned bookkeeping, read back by Create on a
// restart that reuses an existing worktree.
func baseBranchSidecarPath(worktreeHost string) string {
	return worktreeHost + ".base-branch"
}

// Create ensures a git worktree exists at worktreeHost, checked out on a
// fresh branch off repoHost's current HEAD, and returns the base branch
// name. Idempotent: if worktreeHost already exists on disk (a restart
// reusing the same sandbox ID's own worktree), this does nothing but read
// back the base branch it recorded the first time — it never re-runs `git
// worktree add` against a path that's already a live worktree, which would
// either fail outright or (worse) silently do nothing while masking that
// the caller's assumption was wrong. This is what keeps
// `restart --from-profile` from destroying commits the agent already made.
func Create(ctx context.Context, repoHost, worktreeHost, branch string) (baseBranch string, err error) {
	if _, statErr := os.Stat(worktreeHost); statErr == nil {
		data, readErr := os.ReadFile(baseBranchSidecarPath(worktreeHost))
		if readErr != nil {
			return "", fmt.Errorf("worktree %q already exists but its recorded base branch is missing: %w", worktreeHost, readErr)
		}
		return strings.TrimSpace(string(data)), nil
	}

	baseBranch, err = currentBranch(ctx, repoHost)
	if err != nil {
		return "", fmt.Errorf("determine base branch for %q: %w", repoHost, err)
	}

	_, stderr, exitCode, err := gitproxy.RunGit(ctx, repoHost, []string{"worktree", "add", worktreeHost, "-b", branch, baseBranch})
	if err != nil {
		return "", fmt.Errorf("create worktree: %w", err)
	}
	if exitCode != 0 {
		return "", fmt.Errorf("git worktree add failed: %s", strings.TrimSpace(stderr))
	}

	if err := os.WriteFile(baseBranchSidecarPath(worktreeHost), []byte(baseBranch), 0o644); err != nil {
		return "", fmt.Errorf("record base branch for worktree %q: %w", worktreeHost, err)
	}
	return baseBranch, nil
}

// HasUnmergedCommits reports whether worktreeHost's branch has any commits
// not reachable from baseBranch's current tip. A worktree that no longer
// exists on disk (already cleaned up some other way) reports false, not an
// error — there's nothing left to protect.
func HasUnmergedCommits(ctx context.Context, worktreeHost, baseBranch string) (bool, error) {
	if _, err := os.Stat(worktreeHost); err != nil {
		return false, nil
	}
	stdout, stderr, exitCode, err := gitproxy.RunGit(ctx, worktreeHost, []string{"rev-list", "--count", baseBranch + "..HEAD"})
	if err != nil {
		return false, err
	}
	if exitCode != 0 {
		return false, fmt.Errorf("git rev-list failed: %s", strings.TrimSpace(stderr))
	}
	n, convErr := strconv.Atoi(strings.TrimSpace(stdout))
	if convErr != nil {
		return false, fmt.Errorf("parse rev-list count %q: %w", stdout, convErr)
	}
	return n > 0, nil
}

// LastCommitMessage returns the full message of worktreeHost's HEAD commit
// — used as the draft merge commit message: the agent's own last commit is
// its closest thing to a structured "here's my summary" (DESIGN.md §15's
// accompanying AGENT.md convention asks it to leave exactly that as its
// final commit before saying it's done).
func LastCommitMessage(ctx context.Context, worktreeHost string) (string, error) {
	stdout, stderr, exitCode, err := gitproxy.RunGit(ctx, worktreeHost, []string{"log", "-1", "--format=%B"})
	if err != nil {
		return "", err
	}
	if exitCode != 0 {
		return "", fmt.Errorf("git log failed: %s", strings.TrimSpace(stderr))
	}
	return stdout, nil
}

// Diff returns worktreeHost's changes relative to baseBranch's merge-base
// (three-dot diff, matching what will actually be squash-merged) — the
// merge preview shown to the operator before confirming.
func Diff(ctx context.Context, worktreeHost, baseBranch string) (string, error) {
	stdout, stderr, exitCode, err := gitproxy.RunGit(ctx, worktreeHost, []string{"diff", baseBranch + "...HEAD"})
	if err != nil {
		return "", err
	}
	if exitCode != 0 {
		return "", fmt.Errorf("git diff failed: %s", strings.TrimSpace(stderr))
	}
	return stdout, nil
}

// SquashMerge merges worktreeHost's branch into repoHost's baseBranch as
// one commit with message, and returns the resulting commit hash.
//
// Preconditions, checked before anything is touched: repoHost must
// currently be checked out on baseBranch (a worktree can never share a
// checked-out branch with another worktree, including repoHost's own
// primary checkout — that's a hard git constraint, so the merge has to
// happen in whichever single working tree already has baseBranch checked
// out, normally the user's own), and repoHost's working tree must be fully
// clean. Neither precondition is worked around automatically (no
// auto-checkout, no auto-stash) — this function never touches the user's
// own uncommitted work.
//
// On any failure during the merge/commit itself (most commonly a
// conflict), repoHost is reset back to exactly its pre-attempt state
// (`git reset --hard HEAD`, safe because the clean precondition just
// verified there was nothing else to lose) and a plain error is returned —
// nothing is committed, nothing is pruned, the worktree and its branch are
// untouched, matching DESIGN.md §15's "no auto-resolution attempted."
func SquashMerge(ctx context.Context, repoHost, worktreeHost, branch, baseBranch, message string) (commit string, err error) {
	cur, err := currentBranch(ctx, repoHost)
	if err != nil {
		return "", fmt.Errorf("determine current branch of %q: %w", repoHost, err)
	}
	if cur != baseBranch {
		return "", fmt.Errorf("repo %q is currently on branch %q, not %q — check out %q there before merging",
			repoHost, cur, baseBranch, baseBranch)
	}
	clean, err := isClean(ctx, repoHost)
	if err != nil {
		return "", fmt.Errorf("check working tree status of %q: %w", repoHost, err)
	}
	if !clean {
		return "", fmt.Errorf("repo %q has uncommitted changes — commit or stash them there before merging", repoHost)
	}

	if message == "" {
		return "", fmt.Errorf("merge commit message must not be empty")
	}

	_, mergeStderr, mergeExit, err := gitproxy.RunGit(ctx, repoHost, []string{"merge", "--squash", branch})
	if err != nil {
		return "", fmt.Errorf("run git merge --squash: %w", err)
	}
	if mergeExit != 0 {
		resetErr := resetHard(ctx, repoHost)
		if resetErr != nil {
			return "", fmt.Errorf("merge conflict squashing %q into %q AND failed to roll back — repo %q may be left mid-conflict, resolve manually: merge error: %s; reset error: %v",
				branch, baseBranch, repoHost, strings.TrimSpace(mergeStderr), resetErr)
		}
		return "", fmt.Errorf("merge conflict squashing %q into %q — resolve manually in %q, then retry: %s",
			branch, baseBranch, repoHost, strings.TrimSpace(mergeStderr))
	}

	_, commitStderr, commitExit, err := gitproxy.RunGit(ctx, repoHost, []string{"commit", "-m", message})
	if err != nil {
		return "", fmt.Errorf("run git commit: %w", err)
	}
	if commitExit != 0 {
		resetErr := resetHard(ctx, repoHost)
		if resetErr != nil {
			return "", fmt.Errorf("commit failed after squash merge AND failed to roll back — repo %q may be left mid-merge, resolve manually: commit error: %s; reset error: %v",
				repoHost, strings.TrimSpace(commitStderr), resetErr)
		}
		return "", fmt.Errorf("commit failed after squash merge, rolled back: %s", strings.TrimSpace(commitStderr))
	}

	stdout, stderr, exitCode, err := gitproxy.RunGit(ctx, repoHost, []string{"rev-parse", "HEAD"})
	if err != nil {
		return "", err
	}
	if exitCode != 0 {
		return "", fmt.Errorf("resolve new commit hash: %s", strings.TrimSpace(stderr))
	}
	return strings.TrimSpace(stdout), nil
}

// Prune removes a worktree and its branch after a successful merge — the
// force delete on the branch (`-D`, not `-d`) is correct and expected
// here, not a footgun: a squash merge produces a brand-new commit hash on
// baseBranch, so the branch's own original commits are never "fully
// merged" by git's own reachability check, even though their content is.
func Prune(ctx context.Context, repoHost, worktreeHost, branch string) error {
	return remove(ctx, repoHost, worktreeHost, branch, false)
}

// Discard force-removes a worktree and its branch WITHOUT any unmerged-
// commits check — the caller (Manager.Delete's --discard-worktree path) is
// responsible for having already confirmed this is the intended, explicit
// discard of real work.
func Discard(ctx context.Context, repoHost, worktreeHost, branch string) error {
	return remove(ctx, repoHost, worktreeHost, branch, true)
}

func remove(ctx context.Context, repoHost, worktreeHost, branch string, force bool) error {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, worktreeHost)
	_, stderr, exitCode, err := gitproxy.RunGit(ctx, repoHost, args)
	if err != nil {
		return fmt.Errorf("remove worktree: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("git worktree remove failed: %s", strings.TrimSpace(stderr))
	}

	_, stderr, exitCode, err = gitproxy.RunGit(ctx, repoHost, []string{"branch", "-D", branch})
	if err != nil {
		return fmt.Errorf("delete branch: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("git branch -D failed: %s", strings.TrimSpace(stderr))
	}

	_ = os.Remove(baseBranchSidecarPath(worktreeHost))
	return nil
}

func currentBranch(ctx context.Context, repoHost string) (string, error) {
	stdout, stderr, exitCode, err := gitproxy.RunGit(ctx, repoHost, []string{"rev-parse", "--abbrev-ref", "HEAD"})
	if err != nil {
		return "", err
	}
	if exitCode != 0 {
		return "", fmt.Errorf("git rev-parse failed: %s", strings.TrimSpace(stderr))
	}
	branch := strings.TrimSpace(stdout)
	if branch == "" || branch == "HEAD" {
		return "", fmt.Errorf("repo is in a detached HEAD state")
	}
	return branch, nil
}

func isClean(ctx context.Context, repoHost string) (bool, error) {
	stdout, stderr, exitCode, err := gitproxy.RunGit(ctx, repoHost, []string{"status", "--porcelain"})
	if err != nil {
		return false, err
	}
	if exitCode != 0 {
		return false, fmt.Errorf("git status failed: %s", strings.TrimSpace(stderr))
	}
	return strings.TrimSpace(stdout) == "", nil
}

func resetHard(ctx context.Context, repoHost string) error {
	_, stderr, exitCode, err := gitproxy.RunGit(ctx, repoHost, []string{"reset", "--hard", "HEAD"})
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return fmt.Errorf("git reset --hard failed: %s", strings.TrimSpace(stderr))
	}
	return nil
}
