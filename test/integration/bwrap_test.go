//go:build integration

// Package integration exercises the real BwrapIsolator (internal/sandbox's
// bwrap.go) against actual bwrap and actual kernel-enforced isolation —
// this is the whole point of IMPLEMENTATION.md's M3 milestone: proving the
// unit-tested orchestration logic (Manager, tested against a FakeIsolator)
// is backed by real sandboxing, not just correct bookkeeping.
//
// Run with: go test -tags=integration ./test/integration/...
// Requires: bwrap on PATH, unprivileged user namespaces enabled
// (DESIGN.md §8) — both are checked by newIsolator via t.Fatal/t.Skip.
package integration

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thomkin/muro/internal/config"
	"github.com/thomkin/muro/internal/sandbox"
)

// shellMounts is the minimal read-only mount set that gives a sandboxed
// process a working /bin/sh — verified manually against this host's real
// layout (/bin, /lib, /lib64 are symlinks into /usr here, but bind-mounting
// each host path directly works regardless of how the host itself lays
// things out). This is a test-only convenience: real profiles use the
// tools: mechanism (DESIGN.md §10) to expose specific pinned binaries
// instead of bind-mounting the host's general bin/lib directories wholesale
// — that shortcut is only appropriate here, for exercising the isolator
// itself.
func shellMounts() []config.Mount {
	var mounts []config.Mount
	for _, p := range []string{"/usr", "/bin", "/lib", "/lib64", "/sbin"} {
		if _, err := os.Stat(p); err != nil {
			continue // not every path exists on every distro; skip what's absent
		}
		mounts = append(mounts, config.Mount{Host: p, SandboxPath: p, Mode: "ro"})
	}
	return mounts
}

func newIsolator(t *testing.T) *sandbox.BwrapIsolator {
	t.Helper()
	iso, err := sandbox.NewBwrapIsolator()
	if err != nil {
		t.Skipf("bwrap isolator unavailable, skipping integration test: %v", err)
	}
	return iso
}

// runAndWait launches spec, waits for exit (with a timeout so a hung
// sandbox can't hang the test suite), and returns the exit code.
func runAndWait(t *testing.T, iso *sandbox.BwrapIsolator, spec sandbox.LaunchSpec) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h, err := iso.Launch(ctx, spec)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	code, err := h.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	return code
}

func TestReadOnlyMountIsVisible(t *testing.T) {
	iso := newIsolator(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	mounts := append(shellMounts(), config.Mount{Host: dir, SandboxPath: "/workspace", Mode: "ro"})
	code := runAndWait(t, iso, sandbox.LaunchSpec{
		Mounts: mounts,
		Cmd:    []string{"/bin/sh", "-c", "test -f /workspace/marker"},
	})
	if code != 0 {
		t.Errorf("expected exit 0 (marker file visible via explicit mount), got %d", code)
	}
}

func TestUnmountedHostPathIsNotVisible_DenyByDefault(t *testing.T) {
	iso := newIsolator(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "secret"), []byte("shh"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	// Deliberately do NOT mount dir anywhere in the sandbox.
	code := runAndWait(t, iso, sandbox.LaunchSpec{
		Mounts: shellMounts(),
		Cmd:    []string{"/bin/sh", "-c", "test -e " + dir + "/secret"},
	})
	if code == 0 {
		t.Errorf("expected non-zero exit: an unmounted host path must not be visible inside the sandbox, but it was")
	}

	// Also confirm a well-known host path that's never explicitly mounted
	// (only /usr, /bin, /lib, /lib64, /sbin are) is invisible too.
	code = runAndWait(t, iso, sandbox.LaunchSpec{
		Mounts: shellMounts(),
		Cmd:    []string{"/bin/sh", "-c", "test -e /etc/passwd"},
	})
	if code == 0 {
		t.Errorf("expected /etc/passwd to be invisible (never mounted), but it was visible")
	}
}

func TestReadOnlyMountRejectsWrite(t *testing.T) {
	iso := newIsolator(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("original"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	mounts := append(shellMounts(), config.Mount{Host: dir, SandboxPath: "/workspace", Mode: "ro"})
	code := runAndWait(t, iso, sandbox.LaunchSpec{
		Mounts: mounts,
		Cmd:    []string{"/bin/sh", "-c", "echo modified >> /workspace/file"},
	})
	if code == 0 {
		t.Errorf("expected non-zero exit: write to a ro mount must fail, but it succeeded")
	}

	got, err := os.ReadFile(filepath.Join(dir, "file"))
	if err != nil {
		t.Fatalf("read back host file: %v", err)
	}
	if string(got) != "original" {
		t.Errorf("host file was modified despite ro mount: got %q", got)
	}
}

func TestReadWriteMountAllowsWrite(t *testing.T) {
	iso := newIsolator(t)

	dir := t.TempDir()
	mounts := append(shellMounts(), config.Mount{Host: dir, SandboxPath: "/workspace", Mode: "rw"})
	code := runAndWait(t, iso, sandbox.LaunchSpec{
		Mounts: mounts,
		Cmd:    []string{"/bin/sh", "-c", "echo written > /workspace/out"},
	})
	if code != 0 {
		t.Fatalf("expected exit 0 (rw mount allows write), got %d", code)
	}

	got, err := os.ReadFile(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatalf("expected the sandbox's write to be visible on the host rw mount: %v", err)
	}
	if string(got) != "written\n" {
		t.Errorf("unexpected content: %q", got)
	}
}

func TestNetworkIsIsolated(t *testing.T) {
	iso := newIsolator(t)

	// /dev/tcp is a bash-ism for a raw TCP connect attempt; --unshare-net
	// gives the sandbox no route to anything, so this must fail fast
	// rather than actually reaching the internet.
	code := runAndWait(t, iso, sandbox.LaunchSpec{
		Mounts: shellMounts(),
		Cmd:    []string{"/bin/sh", "-c", "exec 3<>/dev/tcp/1.1.1.1/80"},
	})
	if code == 0 {
		t.Errorf("expected the sandboxed process to have no network access, but the connection attempt succeeded")
	}
}

func TestUpdateMounts_ReportsNotAppliedForRealDelta(t *testing.T) {
	iso := newIsolator(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h, err := iso.Launch(ctx, sandbox.LaunchSpec{
		Mounts: shellMounts(),
		Cmd:    []string{"/bin/sh", "-c", "sleep 5"},
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer iso.Stop(h)

	applied, err := iso.UpdateMounts(h, []config.Mount{{Host: "/tmp", SandboxPath: "/extra", Mode: "ro"}})
	if err != nil {
		t.Fatalf("UpdateMounts: %v", err)
	}
	if applied {
		t.Errorf("expected applied=false for a real mount delta (bwrap can't live-remount), got true")
	}

	applied, err = iso.UpdateMounts(h, nil)
	if err != nil {
		t.Fatalf("UpdateMounts (empty delta): %v", err)
	}
	if !applied {
		t.Errorf("expected applied=true for an empty/no-op delta, got false")
	}
}

func TestStop_TerminatesSandbox(t *testing.T) {
	iso := newIsolator(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h, err := iso.Launch(ctx, sandbox.LaunchSpec{
		Mounts: shellMounts(),
		Cmd:    []string{"/bin/sh", "-c", "sleep 30"},
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	start := time.Now()
	if err := iso.Stop(h); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 6*time.Second {
		t.Errorf("Stop took %v, expected termination well under the 5s SIGKILL grace period plus overhead", elapsed)
	}

	code, err := h.Wait()
	if err != nil {
		t.Fatalf("Wait after Stop: %v", err)
	}
	if code == 0 {
		t.Errorf("expected a non-zero exit code from a killed sleep 30, got 0")
	}
}

func TestPTY_LaunchProducesUsablePseudoTerminal(t *testing.T) {
	iso := newIsolator(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h, err := iso.Launch(ctx, sandbox.LaunchSpec{
		Mounts: shellMounts(),
		Cmd:    []string{"/bin/sh", "-c", "echo pty-hello"},
		PTY:    true,
	})
	if err != nil {
		t.Fatalf("Launch with PTY: %v", err)
	}

	pty, ok := h.Stdio()
	if !ok {
		t.Fatal("expected Stdio() ok=true for a PTY launch")
	}

	buf := make([]byte, 256)
	pty.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := pty.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("read from pty: %v", err)
	}
	got := string(buf[:n])
	if !contains(got, "pty-hello") {
		t.Errorf("expected pty output to contain %q, got %q", "pty-hello", got)
	}

	if _, err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i+len(substr) <= len(s); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}
