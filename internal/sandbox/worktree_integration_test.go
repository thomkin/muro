//go:build integration

// This file lives in package sandbox itself, not test/integration, for the
// same reason toolproxy_integration_test.go does: it needs startToolSocket,
// an unexported constructor.
//
// Run with: go test -tags=integration ./internal/sandbox/...
// Requires: bwrap, and muro-toolstub built and on PATH.
package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/thomkin/muro/internal/config"
	"github.com/thomkin/muro/internal/worktree"
)

// TestWorktreeGitProxy_EndToEndThroughRealSandbox proves DESIGN.md §15's
// actual isolation boundary for real: a bwrap sandbox with NO real git
// binary and NO .git of any kind mounted — only the worktree's own checked-
// out files at /workspace, plus the muro-toolstub stub — can still `git
// add`/`git commit` successfully (because the real git process that
// performs those operations runs on the HOST, inside murod's own gitproxy,
// which necessarily DOES have normal access to the worktree's .git pointer
// and the real repo's shared .git/worktrees metadata — that's how git
// worktrees work, and it's the same trusted-host-side-execution model
// toolproxy_integration_test.go already exercises for a plain repo) — while
// the SANDBOXED PROCESS ITSELF, which has neither a git binary nor any .git
// path mounted, can verifiably not reach the real repo's own working-tree
// directory at all.
func TestWorktreeGitProxy_EndToEndThroughRealSandbox(t *testing.T) {
	iso, err := NewBwrapIsolator(testProxyAddrForIntegration, t.TempDir())
	if err != nil {
		t.Skipf("bwrap isolator unavailable, skipping: %v", err)
	}
	if _, err := exec.LookPath("muro-toolstub"); err != nil {
		t.Skip("muro-toolstub not on PATH — run via `make build && make test-integration`")
	}

	repoDir := t.TempDir()
	runGitOKIntegration(t, repoDir, "init", "-q", "-b", "main")
	runGitOKIntegration(t, repoDir, "config", "user.email", "test@example.com")
	runGitOKIntegration(t, repoDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitOKIntegration(t, repoDir, "add", "README.md")
	runGitOKIntegration(t, repoDir, "commit", "-q", "-m", "initial")

	worktreeHost := filepath.Join(t.TempDir(), "wt")
	branch := "agent/default/it-test"
	baseBranch, err := worktree.Create(context.Background(), repoDir, worktreeHost, branch)
	if err != nil {
		t.Fatalf("worktree.Create: %v", err)
	}
	if baseBranch != "main" {
		t.Fatalf("baseBranch = %q, want main", baseBranch)
	}

	// Exactly what WorktreeMounts (worktree.go) produces: the worktree
	// mounted at the sandbox path, and a GitRepoPolicy whose Host is the
	// WORKTREE's own real path (not repoDir) with AllowedBranches locked to
	// this sandbox's own generated branch.
	mounts := []config.Mount{{Host: worktreeHost, SandboxPath: "/workspace", Mode: "rw"}}
	mounts = append(mounts, shellMountsForIntegration()...)
	policy := config.GitPolicy{Repos: []config.GitRepoPolicy{
		{Host: worktreeHost, AllowedBranches: []string{branch}, AllowedRemotes: nil},
	}}

	toolSocketPath := filepath.Join(t.TempDir(), "tool.sock")
	srv, err := startToolSocket(toolSocketPath, mounts, policy, []string{"status", "add", "commit"})
	if err != nil {
		t.Fatalf("startToolSocket: %v", err)
	}
	defer srv.stop()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	env := map[string]string{"PATH": toolRoot}
	script := "{ cd /workspace" +
		" && echo 'feature work' > feature.txt" +
		" && git add feature.txt" +
		" && git commit -m 'add feature from inside the sandbox'" +
		" && test ! -e " + repoDir +
		" ; } >/workspace/debug.log 2>&1"
	spec := LaunchSpec{
		Mounts:         mounts,
		ToolSocketPath: toolSocketPath,
		Env:            env,
		Cmd:            []string{"/bin/sh", "-c", script},
	}

	h, err := iso.Launch(ctx, spec)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	code, err := h.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if code != 0 {
		debugOut, _ := os.ReadFile(filepath.Join(worktreeHost, "debug.log"))
		t.Fatalf("expected the sandboxed commit (via the stub) to succeed AND the real repo to be unreachable, exit code %d\nsandbox output:\n%s", code, debugOut)
	}

	// The host must see the commit — the real git process (murod's
	// gitproxy, unsandboxed) wrote it into the real worktree, not a
	// sandbox-private copy.
	has, err := worktree.HasUnmergedCommits(context.Background(), worktreeHost, "main")
	if err != nil {
		t.Fatalf("HasUnmergedCommits: %v", err)
	}
	if !has {
		t.Fatal("expected the commit made through the sandboxed tool-proxy to be visible from the host afterward")
	}

	// And squash-merging it back (host-side, exactly what `muro sandbox
	// merge` does) must produce the expected content on the real repo.
	commit, err := worktree.SquashMerge(context.Background(), repoDir, worktreeHost, branch, "main", "Add feature")
	if err != nil {
		t.Fatalf("SquashMerge: %v", err)
	}
	if commit == "" {
		t.Fatal("expected a non-empty commit hash")
	}
	content, err := os.ReadFile(filepath.Join(repoDir, "feature.txt"))
	if err != nil {
		t.Fatalf("feature.txt missing from the real repo after merge: %v", err)
	}
	if string(content) != "feature work\n" {
		t.Errorf("feature.txt content = %q, want the sandbox's committed content", content)
	}
}
