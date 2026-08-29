//go:build integration

// This file lives in package sandbox itself (not test/integration, unlike
// this codebase's other real-bwrap tests) because it needs startToolSocket,
// an unexported constructor with no other legitimate external caller
// (production code only ever reaches it through Manager.startToolBridge).
// Run with: go test -tags=integration ./internal/sandbox/...
// Requires: bwrap, and muro-toolstub built and on PATH (`make build` puts
// it in bin/, which the Makefile's test-integration target already
// prepends to PATH for exactly this reason).
package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/thomkin/muro/internal/config"
)

func runGitOKIntegration(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

// TestGitToolProxy_EndToEndThroughRealSandbox proves the whole chain works
// for real: a bwrap sandbox with NO real git binary, only the mounted
// muro-toolstub stub at the sandbox's git PATH location, running
// `git commit -m` then `git push` — which must reach the real host repo
// and the real bare "remote" via murod's tool socket and the real
// gitproxy engine, exactly as a live agent inside the sandbox would
// experience it.
func TestGitToolProxy_EndToEndThroughRealSandbox(t *testing.T) {
	iso, err := NewBwrapIsolator(testProxyAddrForIntegration, t.TempDir())
	if err != nil {
		t.Skipf("bwrap isolator unavailable, skipping: %v", err)
	}
	if _, err := exec.LookPath("muro-toolstub"); err != nil {
		t.Skip("muro-toolstub not on PATH — run via `make build && make test-integration`")
	}

	repoDir := t.TempDir()
	runGitOKIntegration(t, repoDir, "init", "-q", "-b", "ai")
	runGitOKIntegration(t, repoDir, "config", "user.email", "test@example.com")
	runGitOKIntegration(t, repoDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitOKIntegration(t, repoDir, "add", "README.md")
	runGitOKIntegration(t, repoDir, "commit", "-q", "-m", "initial")

	remoteDir := t.TempDir()
	runGitOKIntegration(t, remoteDir, "init", "-q", "--bare")
	runGitOKIntegration(t, repoDir, "remote", "add", "origin", remoteDir)

	mounts := []config.Mount{{Host: repoDir, SandboxPath: "/workspace", Mode: "rw"}}
	mounts = append(mounts, shellMountsForIntegration()...)
	policy := config.GitPolicy{Repos: []config.GitRepoPolicy{
		{Host: repoDir, AllowedBranches: []string{"ai", "ai/*"}, AllowedRemotes: []string{"origin"}},
	}}

	toolSocketPath := filepath.Join(t.TempDir(), "tool.sock")
	srv, err := startToolSocket(toolSocketPath, mounts, policy, []string{"status", "add", "commit", "push"})
	if err != nil {
		t.Fatalf("startToolSocket: %v", err)
	}
	defer srv.stop()

	if err := os.WriteFile(filepath.Join(repoDir, "new.txt"), []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// The sandboxed shell needs to run from /workspace for the stub's cwd
	// translation to resolve; bwrap's --chdir is fixed to "/" in buildArgs,
	// so the script itself cds first.
	// PATH is pinned to toolRoot only (matching the tools: mechanism's real
	// convention, DESIGN.md §10) so this test deterministically exercises
	// the mounted stub rather than depending on whatever order the host's
	// own PATH happens to search shellMountsForIntegration's /usr/bin in —
	// bwrap does not clear/restrict env on its own (confirmed: no
	// --clearenv, no built-in PATH override in buildArgs), so without
	// this a sandbox that also has /usr mounted (as this test's shell
	// support requires) could resolve a real system git ahead of the
	// stub depending on host PATH ordering. A real profile normally
	// avoids this entirely by not wholesale-mounting /usr at all — see
	// shellMounts' own doc comment (test/integration/bwrap_test.go) for
	// why that mount is a test-only convenience, not a production
	// pattern.
	env := map[string]string{"PATH": toolRoot}
	spec := LaunchSpec{
		Mounts:         mounts,
		ToolSocketPath: toolSocketPath,
		Env:            env,
		Cmd:            []string{"/bin/sh", "-c", "{ cd /workspace && git add new.txt && git commit -m 'add new.txt' && git push origin ai; } >/workspace/debug.log 2>&1"},
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
		debugOut, _ := os.ReadFile(filepath.Join(repoDir, "debug.log"))
		t.Fatalf("expected the sandboxed git commit+push (via the stub) to succeed, exit code %d\nsandbox output:\n%s", code, debugOut)
	}

	out, err := exec.Command("git", "-C", remoteDir, "log", "ai", "--oneline").CombinedOutput()
	if err != nil {
		t.Fatalf("verify remote log: %v: %s", err, out)
	}
	if len(out) == 0 {
		t.Fatal("expected the remote's ai branch to have the commit pushed from inside the sandbox")
	}

	// A policy-violating push, run the same way, must fail (nonzero exit)
	// and must not touch the remote's main branch (which doesn't exist).
	spec2 := LaunchSpec{
		Mounts:         mounts,
		ToolSocketPath: toolSocketPath,
		Env:            env,
		Cmd:            []string{"/bin/sh", "-c", "cd /workspace && git push origin ai:main"},
	}
	h2, err := iso.Launch(ctx, spec2)
	if err != nil {
		t.Fatalf("Launch (policy violation case): %v", err)
	}
	code2, err := h2.Wait()
	if err != nil {
		t.Fatalf("Wait (policy violation case): %v", err)
	}
	if code2 == 0 {
		t.Fatal("expected the policy-violating push (to main) to fail")
	}
	out, err = exec.Command("git", "-C", remoteDir, "branch", "--list", "main").CombinedOutput()
	if err != nil {
		t.Fatalf("check remote branches: %v: %s", err, out)
	}
	if len(out) != 0 {
		t.Fatalf("main branch must not exist on remote after a rejected push, got %q", out)
	}
}

// testProxyAddrForIntegration/shellMountsForIntegration duplicate
// test/integration/bwrap_test.go's testProxyAddr/shellMounts (that
// package can't be imported here — this file needs unexported package
// sandbox internals instead, see the file-level comment above — so a
// small, independently-reasoned-about duplicate is preferable to
// restructuring either package's test layout for one test).
const testProxyAddrForIntegration = "127.0.0.1:18080"

// shellMountsForIntegration deliberately does NOT wholesale-mount /usr the
// way test/integration/bwrap_test.go's shellMounts() does: /usr is the
// parent of toolRoot ("/usr/local/bin", GitStubMountPath's target), and
// bwrap cannot create a new mount point (the git stub) inside a directory
// tree it has already bound as read-only — confirmed by direct
// reproduction (bwrap: "Can't create file at /usr/local/bin/git: Read-only
// file system" with a wholesale /usr mount present), the same collision
// class this codebase already hit once before with a --tool mount
// (bwrap.go's own git history). Binding /usr/bin, /usr/lib, etc.
// individually — not /usr itself — sidesteps it: they're sibling
// subdirectories of an auto-created /usr, not an ancestor of
// /usr/local/bin.
func shellMountsForIntegration() []config.Mount {
	var mounts []config.Mount
	subdirs := []string{"/usr/bin", "/usr/lib", "/usr/lib64", "/usr/sbin"}
	for _, p := range subdirs {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		mounts = append(mounts, config.Mount{Host: p, SandboxPath: p, Mode: "ro"})
		// Also bind the same content at the traditional /bin,/lib,/lib64,
		// /sbin locations (this host merges them into /usr via symlinks,
		// per `ls -ld /bin` -> "usr/bin", so /bin/sh as an absolute path
		// needs real content at sandbox /bin too, not just /usr/bin).
		alt := "/" + p[len("/usr/"):]
		mounts = append(mounts, config.Mount{Host: p, SandboxPath: alt, Mode: "ro"})
	}
	return mounts
}
