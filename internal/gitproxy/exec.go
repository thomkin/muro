package gitproxy

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// maxCapturedOutput bounds how much of a subprocess's stdout/stderr gitproxy
// will hold in memory — generous for any realistic git command's output
// (status/diff/log/commit/push confirmations), while still capping
// worst-case growth from something unusual like a runaway `git log` on a
// huge history, the same reasoning internal/sandbox/agentsocket.go's
// maxAgentRequestSize already documents for a different boundary.
const maxCapturedOutput = 1 << 20 // 1MiB

// RunGit execs the real git binary on the host against hostRepo (via -C,
// the only path-translation git itself needs — see the package doc
// comment), with argv appended after that. err is only set for a genuine
// launch failure (git not found, context cancelled, etc.) — a nonzero git
// exit code is a normal, successful RunGit call whose result the caller
// relays to the sandbox exactly as if git had run there directly.
func RunGit(ctx context.Context, hostRepo string, argv []string) (stdout, stderr string, exitCode int, err error) {
	fullArgs := append([]string{"-C", hostRepo}, argv...)
	cmd := exec.CommandContext(ctx, "git", fullArgs...)

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &capWriter{buf: &outBuf, max: maxCapturedOutput}
	cmd.Stderr = &capWriter{buf: &errBuf, max: maxCapturedOutput}

	runErr := cmd.Run()
	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			return outBuf.String(), errBuf.String(), exitErr.ExitCode(), nil
		}
		return "", "", 0, fmt.Errorf("run git: %w", runErr)
	}
	return outBuf.String(), errBuf.String(), 0, nil
}

// capWriter caps how many bytes get appended to buf, appending a
// truncation note once the limit is first crossed rather than silently
// dropping the tail or growing without bound.
type capWriter struct {
	buf *bytes.Buffer
	max int
}

func (w *capWriter) Write(p []byte) (int, error) {
	if w.buf.Len() >= w.max {
		return len(p), nil // already truncated; discard further writes but report success so git isn't disrupted
	}
	remaining := w.max - w.buf.Len()
	if len(p) > remaining {
		w.buf.Write(p[:remaining])
		w.buf.WriteString("\n...[truncated]")
		return len(p), nil
	}
	w.buf.Write(p)
	return len(p), nil
}

// CheckCurrentBranch runs `git rev-parse --abbrev-ref HEAD` in hostRepo and
// verifies the result matches at least one of allowedBranches — the
// pre-flight gate for `commit`, run BEFORE the real commit executes.
func CheckCurrentBranch(ctx context.Context, hostRepo string, allowedBranches []string) error {
	stdout, stderr, exitCode, err := RunGit(ctx, hostRepo, []string{"rev-parse", "--abbrev-ref", "HEAD"})
	if err != nil {
		return fmt.Errorf("determine current branch: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("determine current branch: git exited %d: %s", exitCode, strings.TrimSpace(stderr))
	}
	branch := strings.TrimSpace(stdout)
	if branch == "" || branch == "HEAD" {
		return fmt.Errorf("repo is in a detached HEAD state, which is never an allowed branch")
	}
	if !matchesAny(allowedBranches, branch) {
		return fmt.Errorf("current branch %q does not match any allowed branch pattern for this repo", branch)
	}
	return nil
}

// RefUpdate is one line of `git push --dry-run --porcelain` output.
type RefUpdate struct {
	Flag    string // one of " ", "+", "-", "*", "!", "="
	From    string
	To      string
	Summary string
}

// ParsePushPorcelain parses `git push --dry-run --porcelain` output. Each
// update line has the form "<flag>\t<from>:<to>\t<summary>"; the first
// line ("To <url>") and the trailing "Done" line are informational and
// skipped if present — porcelain output doesn't guarantee either appears
// (e.g. a push that updates nothing produces neither), so both are treated
// as optional rather than required.
func ParsePushPorcelain(output string) ([]RefUpdate, error) {
	var updates []RefUpdate
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "To ") || line == "Done" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 2 {
			return nil, fmt.Errorf("malformed push porcelain line: %q", line)
		}
		flag := parts[0]
		fromTo := parts[1]
		summary := ""
		if len(parts) == 3 {
			summary = parts[2]
		}
		ft := strings.SplitN(fromTo, ":", 2)
		if len(ft) != 2 {
			return nil, fmt.Errorf("malformed push porcelain ref field: %q", fromTo)
		}
		updates = append(updates, RefUpdate{Flag: flag, From: ft[0], To: ft[1], Summary: summary})
	}
	return updates, nil
}

// CheckPushPlan verifies every update in a parsed dry-run push plan is
// both non-rejected (flag != "!") and targets an allowed branch. If ANY
// update fails either check, the whole push is rejected — a multi-ref push
// is all-or-nothing, never partially executed.
func CheckPushPlan(updates []RefUpdate, allowedBranches []string) error {
	if len(updates) == 0 {
		return fmt.Errorf("push dry-run reported no ref updates — refusing to push nothing")
	}
	for _, u := range updates {
		if u.Flag == "!" {
			return fmt.Errorf("push of %q was rejected by git itself: %s", u.To, u.Summary)
		}
		ref := strings.TrimPrefix(u.To, "refs/heads/")
		if !matchesAny(allowedBranches, ref) {
			return fmt.Errorf("push would update %q, which does not match any allowed branch pattern for this repo", ref)
		}
	}
	return nil
}

// pushArgsWithDryRun returns argv with --dry-run and --porcelain inserted
// right after "push" — used by orchestrate.go to run the safe pre-flight
// check before the real push.
func pushArgsWithDryRun(argv []string) []string {
	out := make([]string, 0, len(argv)+2)
	out = append(out, argv[0], "--dry-run", "--porcelain")
	out = append(out, argv[1:]...)
	return out
}
