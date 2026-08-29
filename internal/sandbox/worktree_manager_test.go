package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thomkin/muro/internal/config"
	"github.com/thomkin/muro/internal/state"
)

// runOK, initGitRepo mirror internal/gitproxy/exec_test.go's own helpers —
// exercising real git subprocesses against real temp repos, this
// codebase's established preference over mocking git.
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

func initGitRepo(t *testing.T, dir, branch string) {
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

func testWorktreeProfile(name, repoHost, mountPath string) *config.Profile {
	p := testProfile(name)
	p.Git = config.GitPolicy{Repos: []config.GitRepoPolicy{
		{Host: repoHost, Worktree: true, MountPath: mountPath},
	}}
	return p
}

func TestRun_WorktreeRepoProducesWorktreeMountAndState(t *testing.T) {
	mgr, iso, _ := newTestManager(t)
	stateDir := t.TempDir()
	mgr.SetStateDir(stateDir)

	repo := t.TempDir()
	initGitRepo(t, repo, "main")

	p := testWorktreeProfile("p1", repo, "/workspace/foo")
	sb, err := mgr.Run(p, "agent-1", "default")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}

	if len(sb.Worktrees) != 1 {
		t.Fatalf("sb.Worktrees = %+v, want 1 entry", sb.Worktrees)
	}
	wt := sb.Worktrees[0]
	if wt.MountPath != "/workspace/foo" {
		t.Errorf("MountPath = %q, want /workspace/foo", wt.MountPath)
	}
	if wt.Branch != "agent/default/agent-1" {
		t.Errorf("Branch = %q, want agent/default/agent-1", wt.Branch)
	}
	if wt.BaseBranch != "main" {
		t.Errorf("BaseBranch = %q, want main", wt.BaseBranch)
	}
	wantHostPrefix := filepath.Join(stateDir, "sandboxes", sb.ID, "worktrees")
	if !strings.HasPrefix(wt.Host, wantHostPrefix) {
		t.Errorf("worktree Host = %q, want prefix %q (not next to the real repo)", wt.Host, wantHostPrefix)
	}
	if _, err := os.Stat(filepath.Join(wt.Host, "README.md")); err != nil {
		t.Errorf("worktree not actually checked out: %v", err)
	}

	found := false
	for _, m := range iso.launched[len(iso.launched)-1].Mounts {
		if m.SandboxPath == "/workspace/foo" {
			found = true
			if m.Host != wt.Host {
				t.Errorf("mount Host = %q, want the worktree path %q, not the real repo", m.Host, wt.Host)
			}
			if m.Mode != "rw" {
				t.Errorf("worktree mount mode = %q, want rw", m.Mode)
			}
		}
	}
	if !found {
		t.Errorf("expected a mount at /workspace/foo, got %+v", iso.launched[len(iso.launched)-1].Mounts)
	}

	if len(sb.GitPolicy.Repos) != 1 {
		t.Fatalf("sb.GitPolicy.Repos = %+v, want 1 entry", sb.GitPolicy.Repos)
	}
	gp := sb.GitPolicy.Repos[0]
	if gp.Host != wt.Host {
		t.Errorf("GitPolicy.Repos[0].Host = %q, want the worktree path %q, not the real repo %q", gp.Host, wt.Host, repo)
	}
	if len(gp.AllowedBranches) != 1 || gp.AllowedBranches[0] != "agent/default/agent-1" {
		t.Errorf("GitPolicy.Repos[0].AllowedBranches = %v, want exactly [agent/default/agent-1]", gp.AllowedBranches)
	}
}

func TestRestart_FromProfileReusesWorktreeAndPreservesCommits(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	stateDir := t.TempDir()
	mgr.SetStateDir(stateDir)

	repo := t.TempDir()
	initGitRepo(t, repo, "main")

	p := testWorktreeProfile("p1", repo, "/workspace/foo")
	if err := config.SaveProfile(p); err != nil {
		t.Fatalf("SaveProfile error: %v", err)
	}
	sb, err := mgr.Run(p, "agent-1", "default")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	worktreeHost := sb.Worktrees[0].Host

	// Simulate the agent committing inside its worktree (what the git
	// tool-proxy would do on its behalf).
	if err := os.WriteFile(filepath.Join(worktreeHost, "feature.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runOK(t, worktreeHost, "add", "feature.txt")
	runOK(t, worktreeHost, "commit", "-q", "-m", "wip commit")

	if err := mgr.Restart("default", "agent-1", true); err != nil {
		t.Fatalf("Restart(fromProfile=true) error: %v", err)
	}

	after, ok := mgr.store.Get("default", "agent-1")
	if !ok {
		t.Fatal("sandbox missing after restart")
	}
	if len(after.Worktrees) != 1 || after.Worktrees[0].Host != worktreeHost {
		t.Fatalf("worktree path changed across restart: %+v", after.Worktrees)
	}
	log := runOK(t, worktreeHost, "log", "--oneline")
	if !strings.Contains(log, "wip commit") {
		t.Errorf("restart destroyed the agent's commit: %s", log)
	}
}

func TestDelete_RefusesWhenWorktreeHasUnmergedCommits(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	mgr.SetStateDir(t.TempDir())

	repo := t.TempDir()
	initGitRepo(t, repo, "main")

	p := testWorktreeProfile("p1", repo, "/workspace/foo")
	sb, err := mgr.Run(p, "agent-1", "default")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	worktreeHost := sb.Worktrees[0].Host
	if err := os.WriteFile(filepath.Join(worktreeHost, "feature.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runOK(t, worktreeHost, "add", "feature.txt")
	runOK(t, worktreeHost, "commit", "-q", "-m", "wip commit")

	if err := mgr.Stop("default", "agent-1"); err != nil {
		t.Fatalf("Stop error: %v", err)
	}
	if err := mgr.Delete("default", "agent-1", nil); err == nil {
		t.Fatal("expected Delete to refuse a sandbox with an unmerged worktree")
	}
	if _, ok := mgr.store.Get("default", "agent-1"); !ok {
		t.Error("sandbox record should still exist after a refused Delete")
	}
	if _, err := os.Stat(worktreeHost); err != nil {
		t.Errorf("worktree should still exist after a refused Delete: %v", err)
	}
}

func TestDelete_DiscardWorktreeFlagAllowsDeletion(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	mgr.SetStateDir(t.TempDir())

	repo := t.TempDir()
	initGitRepo(t, repo, "main")

	p := testWorktreeProfile("p1", repo, "/workspace/foo")
	sb, err := mgr.Run(p, "agent-1", "default")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	worktreeHost := sb.Worktrees[0].Host
	if err := os.WriteFile(filepath.Join(worktreeHost, "feature.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runOK(t, worktreeHost, "add", "feature.txt")
	runOK(t, worktreeHost, "commit", "-q", "-m", "wip commit")

	if err := mgr.Stop("default", "agent-1"); err != nil {
		t.Fatalf("Stop error: %v", err)
	}
	if err := mgr.Delete("default", "agent-1", []string{"/workspace/foo"}); err != nil {
		t.Fatalf("Delete with --discard-worktree error: %v", err)
	}
	if _, ok := mgr.store.Get("default", "agent-1"); ok {
		t.Error("sandbox record should be gone after Delete")
	}
	if _, err := os.Stat(worktreeHost); !os.IsNotExist(err) {
		t.Errorf("expected worktree to be discarded, stat err = %v", err)
	}
}

func TestDelete_PrunesWorktreeAutomaticallyWhenNothingUnmerged(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	mgr.SetStateDir(t.TempDir())

	repo := t.TempDir()
	initGitRepo(t, repo, "main")

	p := testWorktreeProfile("p1", repo, "/workspace/foo")
	sb, err := mgr.Run(p, "agent-1", "default")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	worktreeHost := sb.Worktrees[0].Host // no commits made — nothing to lose

	if err := mgr.Stop("default", "agent-1"); err != nil {
		t.Fatalf("Stop error: %v", err)
	}
	if err := mgr.Delete("default", "agent-1", nil); err != nil {
		t.Fatalf("Delete error: %v", err)
	}
	if _, err := os.Stat(worktreeHost); !os.IsNotExist(err) {
		t.Errorf("expected an unused worktree to be pruned automatically on delete, stat err = %v", err)
	}
}

func TestMerge_SquashMergesAndRemovesWorktreeFromSandbox(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	mgr.SetStateDir(t.TempDir())

	repo := t.TempDir()
	initGitRepo(t, repo, "main")

	p := testWorktreeProfile("p1", repo, "/workspace/foo")
	sb, err := mgr.Run(p, "agent-1", "default")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	worktreeHost := sb.Worktrees[0].Host
	if err := os.WriteFile(filepath.Join(worktreeHost, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runOK(t, worktreeHost, "add", "feature.txt")
	runOK(t, worktreeHost, "commit", "-q", "-m", "add feature")

	if err := mgr.Stop("default", "agent-1"); err != nil {
		t.Fatalf("Stop error: %v", err)
	}

	commit, err := mgr.Merge("default", "agent-1", "/workspace/foo", "Add feature")
	if err != nil {
		t.Fatalf("Merge error: %v", err)
	}
	if commit == "" {
		t.Error("expected a non-empty commit hash")
	}

	after, ok := mgr.store.Get("default", "agent-1")
	if !ok {
		t.Fatal("sandbox missing after merge")
	}
	if len(after.Worktrees) != 0 {
		t.Errorf("expected Worktrees to be empty after merge, got %+v", after.Worktrees)
	}
	for _, m := range after.Mounts {
		if m.SandboxPath == "/workspace/foo" {
			t.Errorf("expected the worktree mount to be removed after merge, still found: %+v", m)
		}
	}
	if _, err := os.Stat(worktreeHost); !os.IsNotExist(err) {
		t.Errorf("expected worktree to be pruned after merge, stat err = %v", err)
	}
	content, err := os.ReadFile(filepath.Join(repo, "feature.txt"))
	if err != nil {
		t.Fatalf("feature.txt missing from real repo after merge: %v", err)
	}
	if string(content) != "feature\n" {
		t.Errorf("feature.txt content = %q, want the merged version", content)
	}

	// Delete should now succeed cleanly — nothing left to protect.
	if err := mgr.Delete("default", "agent-1", nil); err != nil {
		t.Fatalf("Delete after merge error: %v", err)
	}
}

func TestMerge_RunningSandboxMarkedReloadPending(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	mgr.SetStateDir(t.TempDir())

	repo := t.TempDir()
	initGitRepo(t, repo, "main")

	p := testWorktreeProfile("p1", repo, "/workspace/foo")
	sb, err := mgr.Run(p, "agent-1", "default")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	worktreeHost := sb.Worktrees[0].Host
	if err := os.WriteFile(filepath.Join(worktreeHost, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runOK(t, worktreeHost, "add", "feature.txt")
	runOK(t, worktreeHost, "commit", "-q", "-m", "add feature")

	// Sandbox is left RUNNING (not stopped) for this merge.
	if _, err := mgr.Merge("default", "agent-1", "/workspace/foo", "Add feature"); err != nil {
		t.Fatalf("Merge error: %v", err)
	}

	after, ok := mgr.store.Get("default", "agent-1")
	if !ok {
		t.Fatal("sandbox missing after merge")
	}
	if after.State != state.StateReloadPending {
		t.Errorf("State = %q, want reload-pending after merging a mount out from under a running sandbox", after.State)
	}
}

func TestMerge_NoCommitsIsAnError(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	mgr.SetStateDir(t.TempDir())

	repo := t.TempDir()
	initGitRepo(t, repo, "main")

	p := testWorktreeProfile("p1", repo, "/workspace/foo")
	if _, err := mgr.Run(p, "agent-1", "default"); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	if _, err := mgr.Merge("default", "agent-1", "/workspace/foo", "msg"); err == nil {
		t.Fatal("expected an error merging a worktree with no commits")
	}
}
