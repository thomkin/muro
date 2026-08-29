package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runOK, initRepo mirror internal/gitproxy/exec_test.go's own helpers —
// this package's tests exercise real git subprocesses against real temp
// repos, the same established preference for real integration over mocks.
func runOK(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

func initRepo(t *testing.T, dir, branch string) {
	t.Helper()
	runOK(t, dir, "init", "-q", "-b", branch)
	runOK(t, dir, "config", "user.email", "test@example.com")
	runOK(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runOK(t, dir, "add", "README.md")
	runOK(t, dir, "commit", "-q", "-m", "initial")
}

func TestCreate_NewWorktreeOnFreshBranch(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	initRepo(t, repo, "main")

	worktreeHost := filepath.Join(t.TempDir(), "wt")
	base, err := Create(ctx, repo, worktreeHost, "agent/default/a1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if base != "main" {
		t.Errorf("base branch = %q, want main", base)
	}
	if _, err := os.Stat(filepath.Join(worktreeHost, "README.md")); err != nil {
		t.Errorf("worktree missing checked-out content: %v", err)
	}
	branch := strings.TrimSpace(runOK(t, worktreeHost, "rev-parse", "--abbrev-ref", "HEAD"))
	if branch != "agent/default/a1" {
		t.Errorf("worktree branch = %q, want agent/default/a1", branch)
	}
}

func TestCreate_IdempotentOnRestart(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	initRepo(t, repo, "main")

	worktreeHost := filepath.Join(t.TempDir(), "wt")
	if _, err := Create(ctx, repo, worktreeHost, "agent/default/a1"); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	// Simulate the agent having committed something in the worktree.
	if err := os.WriteFile(filepath.Join(worktreeHost, "new.txt"), []byte("work in progress\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runOK(t, worktreeHost, "add", "new.txt")
	runOK(t, worktreeHost, "commit", "-q", "-m", "wip")

	// A second Create (as `restart --from-profile` would trigger) must not
	// touch the existing worktree or destroy that commit.
	base, err := Create(ctx, repo, worktreeHost, "agent/default/a1")
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if base != "main" {
		t.Errorf("base branch on reuse = %q, want main", base)
	}
	log := runOK(t, worktreeHost, "log", "--oneline")
	if !strings.Contains(log, "wip") {
		t.Errorf("the wip commit was lost across a repeated Create call: %s", log)
	}
}

func TestHasUnmergedCommits(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	initRepo(t, repo, "main")

	worktreeHost := filepath.Join(t.TempDir(), "wt")
	if _, err := Create(ctx, repo, worktreeHost, "agent/default/a1"); err != nil {
		t.Fatal(err)
	}

	has, err := HasUnmergedCommits(ctx, worktreeHost, "main")
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("expected no unmerged commits right after creation")
	}

	if err := os.WriteFile(filepath.Join(worktreeHost, "new.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runOK(t, worktreeHost, "add", "new.txt")
	runOK(t, worktreeHost, "commit", "-q", "-m", "add new.txt")

	has, err = HasUnmergedCommits(ctx, worktreeHost, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("expected unmerged commits after committing in the worktree")
	}
}

func TestHasUnmergedCommits_MissingWorktreeIsFalseNotError(t *testing.T) {
	has, err := HasUnmergedCommits(context.Background(), filepath.Join(t.TempDir(), "never-created"), "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if has {
		t.Error("expected false for a worktree path that never existed")
	}
}

func TestLastCommitMessage(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	initRepo(t, repo, "main")

	worktreeHost := filepath.Join(t.TempDir(), "wt")
	if _, err := Create(ctx, repo, worktreeHost, "agent/default/a1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktreeHost, "new.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runOK(t, worktreeHost, "add", "new.txt")
	runOK(t, worktreeHost, "commit", "-q", "-m", "add a feature\n\nlonger description")

	msg, err := LastCommitMessage(ctx, worktreeHost)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(msg, "add a feature") {
		t.Errorf("LastCommitMessage = %q, want it to start with the commit subject", msg)
	}
	if !strings.Contains(msg, "longer description") {
		t.Errorf("LastCommitMessage = %q, want the body included too", msg)
	}
}

func TestDiff(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	initRepo(t, repo, "main")

	worktreeHost := filepath.Join(t.TempDir(), "wt")
	if _, err := Create(ctx, repo, worktreeHost, "agent/default/a1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktreeHost, "new.txt"), []byte("added content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runOK(t, worktreeHost, "add", "new.txt")
	runOK(t, worktreeHost, "commit", "-q", "-m", "add new.txt")

	diff, err := Diff(ctx, worktreeHost, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "new.txt") || !strings.Contains(diff, "added content") {
		t.Errorf("Diff missing expected content: %s", diff)
	}
}

func TestSquashMerge_CleanMergeProducesOneCommitOnBase(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	initRepo(t, repo, "main")

	worktreeHost := filepath.Join(t.TempDir(), "wt")
	if _, err := Create(ctx, repo, worktreeHost, "agent/default/a1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktreeHost, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runOK(t, worktreeHost, "add", "feature.txt")
	runOK(t, worktreeHost, "commit", "-q", "-m", "wip 1")
	if err := os.WriteFile(filepath.Join(worktreeHost, "feature.txt"), []byte("feature v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runOK(t, worktreeHost, "commit", "-q", "-am", "wip 2")

	commit, err := SquashMerge(ctx, repo, worktreeHost, "agent/default/a1", "main", "Add feature\n\nSquashed from two wip commits.")
	if err != nil {
		t.Fatalf("SquashMerge: %v", err)
	}
	if commit == "" {
		t.Fatal("expected a non-empty commit hash")
	}

	// repo (the base checkout) should now have exactly one new commit on
	// top of "initial", not two — the wip history must not leak in.
	log := strings.TrimSpace(runOK(t, repo, "log", "--format=%s"))
	lines := strings.Split(log, "\n")
	if len(lines) != 2 {
		t.Fatalf("log = %v, want exactly 2 commits (initial + one squashed commit)", lines)
	}
	if lines[0] != "Add feature" {
		t.Errorf("top commit subject = %q, want %q", lines[0], "Add feature")
	}
	content, err := os.ReadFile(filepath.Join(repo, "feature.txt"))
	if err != nil {
		t.Fatalf("feature.txt missing from base checkout after merge: %v", err)
	}
	if string(content) != "feature v2\n" {
		t.Errorf("feature.txt content = %q, want the worktree's final version", content)
	}
}

func TestSquashMerge_RejectsWhenRepoNotOnBaseBranch(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	initRepo(t, repo, "main")

	worktreeHost := filepath.Join(t.TempDir(), "wt")
	if _, err := Create(ctx, repo, worktreeHost, "agent/default/a1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktreeHost, "feature.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runOK(t, worktreeHost, "add", "feature.txt")
	runOK(t, worktreeHost, "commit", "-q", "-m", "wip")

	runOK(t, repo, "checkout", "-q", "-b", "other-branch")

	if _, err := SquashMerge(ctx, repo, worktreeHost, "agent/default/a1", "main", "msg"); err == nil {
		t.Fatal("expected an error when repo is not on the base branch")
	}
}

func TestSquashMerge_RejectsWhenRepoDirty(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	initRepo(t, repo, "main")

	worktreeHost := filepath.Join(t.TempDir(), "wt")
	if _, err := Create(ctx, repo, worktreeHost, "agent/default/a1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktreeHost, "feature.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runOK(t, worktreeHost, "add", "feature.txt")
	runOK(t, worktreeHost, "commit", "-q", "-m", "wip")

	if err := os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := SquashMerge(ctx, repo, worktreeHost, "agent/default/a1", "main", "msg"); err == nil {
		t.Fatal("expected an error when repo has uncommitted changes")
	}

	// The dirty file must still be there, untouched.
	if _, err := os.Stat(filepath.Join(repo, "dirty.txt")); err != nil {
		t.Errorf("SquashMerge's precondition check must not have removed the user's own file: %v", err)
	}
}

func TestSquashMerge_ConflictRollsBackCleanlyAndLeavesWorktreeIntact(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	initRepo(t, repo, "main")

	worktreeHost := filepath.Join(t.TempDir(), "wt")
	if _, err := Create(ctx, repo, worktreeHost, "agent/default/a1"); err != nil {
		t.Fatal(err)
	}
	// Conflicting edit in the worktree...
	if err := os.WriteFile(filepath.Join(worktreeHost, "README.md"), []byte("worktree version\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runOK(t, worktreeHost, "commit", "-q", "-am", "worktree edits README")

	// ...and a conflicting edit landed on main in the meantime (e.g. a
	// human committed directly on main after the worktree branched off).
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("main version\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runOK(t, repo, "commit", "-q", "-am", "main edits README")

	preAttemptLog := runOK(t, repo, "log", "--format=%H")

	if _, err := SquashMerge(ctx, repo, worktreeHost, "agent/default/a1", "main", "msg"); err == nil {
		t.Fatal("expected a conflict error")
	}

	// repo must be back to exactly its pre-attempt state: clean, same HEAD.
	status := runOK(t, repo, "status", "--porcelain")
	if strings.TrimSpace(status) != "" {
		t.Errorf("repo not clean after a rolled-back conflict: %q", status)
	}
	postLog := runOK(t, repo, "log", "--format=%H")
	if postLog != preAttemptLog {
		t.Errorf("repo HEAD changed after a rolled-back conflict")
	}

	// The worktree and its branch must be untouched — still there, still
	// has its own commit.
	wtLog := runOK(t, worktreeHost, "log", "--oneline")
	if !strings.Contains(wtLog, "worktree edits README") {
		t.Errorf("worktree's own commit is gone after a failed merge attempt: %s", wtLog)
	}
}

func TestPrune_RemovesWorktreeAndBranch(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	initRepo(t, repo, "main")

	worktreeHost := filepath.Join(t.TempDir(), "wt")
	if _, err := Create(ctx, repo, worktreeHost, "agent/default/a1"); err != nil {
		t.Fatal(err)
	}

	if err := Prune(ctx, repo, worktreeHost, "agent/default/a1"); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	if _, err := os.Stat(worktreeHost); !os.IsNotExist(err) {
		t.Errorf("worktree directory still exists after Prune, stat err = %v", err)
	}
	branches := runOK(t, repo, "branch", "--list", "agent/default/a1")
	if strings.TrimSpace(branches) != "" {
		t.Errorf("branch still exists after Prune: %q", branches)
	}
	if _, err := os.Stat(baseBranchSidecarPath(worktreeHost)); !os.IsNotExist(err) {
		t.Errorf("base-branch sidecar file still exists after Prune")
	}
}

func TestDiscard_ForceRemovesEvenWithUncommittedWorktreeChanges(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	initRepo(t, repo, "main")

	worktreeHost := filepath.Join(t.TempDir(), "wt")
	if _, err := Create(ctx, repo, worktreeHost, "agent/default/a1"); err != nil {
		t.Fatal(err)
	}
	// Uncommitted change in the worktree — a plain `worktree remove`
	// (without --force) would refuse this; Discard must still succeed,
	// since the caller (an explicit --discard-worktree) has already
	// accepted the loss.
	if err := os.WriteFile(filepath.Join(worktreeHost, "uncommitted.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Discard(ctx, repo, worktreeHost, "agent/default/a1"); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if _, err := os.Stat(worktreeHost); !os.IsNotExist(err) {
		t.Errorf("worktree directory still exists after Discard")
	}
}
