package sandbox

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/thomkin/muro/internal/config"
)

func runGitOK(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

// initTestRepo mirrors internal/gitproxy/exec_test.go's initRepo — real git,
// not a fake, since toolSocketServer's whole job is wiring a real socket
// protocol on top of gitproxy's real-git-backed engine.
func initTestRepo(t *testing.T, dir, branch string) {
	t.Helper()
	runGitOK(t, dir, "init", "-q", "-b", branch)
	runGitOK(t, dir, "config", "user.email", "test@example.com")
	runGitOK(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitOK(t, dir, "add", "README.md")
	runGitOK(t, dir, "commit", "-q", "-m", "initial")
}

func dialAndRoundtrip(t *testing.T, socketPath string, req ToolExecRequest) ToolExecResponse {
	t.Helper()
	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}

	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var resp ToolExecResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return resp
}

func TestToolSocketServer_StatusAndPolicyRejection(t *testing.T) {
	repoDir := t.TempDir()
	initTestRepo(t, repoDir, "ai")

	mounts := []config.Mount{{Host: repoDir, SandboxPath: "/workspace", Mode: "rw"}}
	policy := config.GitPolicy{Repos: []config.GitRepoPolicy{
		{Host: repoDir, AllowedBranches: []string{"ai", "ai/*"}, AllowedRemotes: []string{"origin"}},
	}}

	socketPath := filepath.Join(t.TempDir(), "tool.sock")
	srv, err := startToolSocket(socketPath, mounts, policy, []string{"status", "commit", "push"})
	if err != nil {
		t.Fatalf("startToolSocket: %v", err)
	}
	defer srv.stop()

	resp := dialAndRoundtrip(t, socketPath, ToolExecRequest{Tool: "git", Argv: []string{"status"}, Cwd: "/workspace"})
	if !resp.OK {
		t.Fatalf("expected status to succeed, got %+v", resp)
	}

	// Unsupported tool.
	resp = dialAndRoundtrip(t, socketPath, ToolExecRequest{Tool: "curl", Argv: []string{"--help"}, Cwd: "/workspace"})
	if resp.OK {
		t.Fatalf("expected curl to be rejected, got %+v", resp)
	}

	// Subcommand outside the daemon ceiling passed to startToolSocket
	// ("log" isn't in the allowlist given above).
	resp = dialAndRoundtrip(t, socketPath, ToolExecRequest{Tool: "git", Argv: []string{"log"}, Cwd: "/workspace"})
	if resp.OK {
		t.Fatalf("expected log to be rejected by the daemon ceiling, got %+v", resp)
	}

	// cwd outside any mount.
	resp = dialAndRoundtrip(t, socketPath, ToolExecRequest{Tool: "git", Argv: []string{"status"}, Cwd: "/nowhere"})
	if resp.OK {
		t.Fatalf("expected rejection for an unmounted cwd, got %+v", resp)
	}
}

func TestToolSocketServer_CommitAndPushEndToEnd(t *testing.T) {
	repoDir := t.TempDir()
	initTestRepo(t, repoDir, "ai")

	remoteDir := t.TempDir()
	runGitOK(t, remoteDir, "init", "-q", "--bare")
	runGitOK(t, repoDir, "remote", "add", "origin", remoteDir)

	mounts := []config.Mount{{Host: repoDir, SandboxPath: "/workspace", Mode: "rw"}}
	policy := config.GitPolicy{Repos: []config.GitRepoPolicy{
		{Host: repoDir, AllowedBranches: []string{"ai", "ai/*"}, AllowedRemotes: []string{"origin"}},
	}}

	socketPath := filepath.Join(t.TempDir(), "tool.sock")
	srv, err := startToolSocket(socketPath, mounts, policy, []string{"status", "add", "commit", "push"})
	if err != nil {
		t.Fatalf("startToolSocket: %v", err)
	}
	defer srv.stop()

	if err := os.WriteFile(filepath.Join(repoDir, "new.txt"), []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp := dialAndRoundtrip(t, socketPath, ToolExecRequest{Tool: "git", Argv: []string{"add", "new.txt"}, Cwd: "/workspace"})
	if !resp.OK || resp.ExitCode != 0 {
		t.Fatalf("add failed: %+v", resp)
	}

	resp = dialAndRoundtrip(t, socketPath, ToolExecRequest{Tool: "git", Argv: []string{"commit", "-m", "add new.txt"}, Cwd: "/workspace"})
	if !resp.OK || resp.ExitCode != 0 {
		t.Fatalf("commit failed: %+v", resp)
	}

	resp = dialAndRoundtrip(t, socketPath, ToolExecRequest{Tool: "git", Argv: []string{"push", "origin", "ai"}, Cwd: "/workspace"})
	if !resp.OK || resp.ExitCode != 0 {
		t.Fatalf("push failed: %+v", resp)
	}

	out, err := exec.Command("git", "-C", remoteDir, "log", "ai", "--oneline").CombinedOutput()
	if err != nil {
		t.Fatalf("verify remote log: %v: %s", err, out)
	}
	if len(out) == 0 {
		t.Fatal("expected the remote's ai branch to have the pushed commit")
	}

	// Policy-violating push (branch not allowed) must be rejected, and must
	// not touch the remote.
	resp = dialAndRoundtrip(t, socketPath, ToolExecRequest{Tool: "git", Argv: []string{"push", "origin", "ai:main"}, Cwd: "/workspace"})
	if resp.OK {
		t.Fatalf("expected push to main to be rejected, got %+v", resp)
	}
	out, err = exec.Command("git", "-C", remoteDir, "branch", "--list", "main").CombinedOutput()
	if err != nil {
		t.Fatalf("check remote branches: %v: %s", err, out)
	}
	if len(out) != 0 {
		t.Fatalf("main branch must not exist on remote after a rejected push, got %q", out)
	}
}
