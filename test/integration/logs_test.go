//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thomkin/muro/internal/sandbox"
)

// SandboxID values in this file are deliberately short. A t.TempDir() path
// already embeds the test function's full name, and BwrapIsolator derives
// the shim's Unix socket path from stateDir/sandboxes/<SandboxID>/shim.sock
// — combined with a long, descriptive SandboxID this silently exceeds the
// ~108-byte sockaddr_un limit. net.Listen then fails, and since that
// failure only reaches a log.Printf (cmd/muro-shim/main.go) writing to a
// stderr that's /dev/null in the real daemon-spawned case, EVERYTHING
// gated on the listener succeeding — not just the socket, but log capture
// too (main.go's `if ln != nil` block covers both) — silently no-ops with
// no visible error anywhere. Confirmed by direct reproduction while
// writing these tests: a verbose SandboxID here reliably reproduced
// exactly the "log file never created, no error" symptom this file's
// tests would otherwise exist to catch. Real production SandboxIDs
// (state.NewID(), "sb_" + 8 hex chars) are short enough to never approach
// this, so it isn't a functional bug worth changing shim behavior over —
// but it's a real, sharp-edged trap for exactly the kind of manual/ad hoc
// testing this project has repeatedly hit this session, so it's documented
// here rather than left to be silently rediscovered again later.

// TestLogCapture_ContinuousWithoutAttach proves the core mechanism `muro
// logs` depends on: muro-shim drains and logs a sandbox's pty output for
// its whole life, independent of whether anything is ever attached to it.
// Before this was fixed, the shim only read the pty when a client happened
// to be connected, which would silently lose all output produced while
// unattended — exactly the common case, since a sandbox spends nearly all
// its life with nobody attached.
func TestLogCapture_ContinuousWithoutAttach(t *testing.T) {
	iso := newIsolator(t)
	logPath := filepath.Join(t.TempDir(), "sandbox.log")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	h, err := iso.Launch(ctx, sandbox.LaunchSpec{
		Mounts:    shellMounts(),
		Cmd:       []string{"/bin/sh", "-c", "for i in 1 2 3 4; do echo tick-$i; sleep 1; done"},
		PTY:       true,
		SandboxID: "log-test-1",
		LogPath:   logPath,
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer iso.Stop(h)

	// Deliberately never call h.Stdio() — the whole point is proving output
	// is captured with nobody reading the live stream at all.
	code, err := h.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if code != 0 {
		t.Fatalf("sandboxed script exited %d, want 0", code)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	content := string(data)
	for _, want := range []string{"tick-1", "tick-2", "tick-3", "tick-4"} {
		if !strings.Contains(content, want) {
			t.Errorf("log file missing %q; full content:\n%s", want, content)
		}
	}
}

// TestLogCapture_IsIncrementalNotBatchedAtExit proves the log file grows
// WHILE the sandbox is still running, not just once at process exit — a
// naive implementation that only flushed captured output when the process
// terminated would pass a weaker "check the final content" test but would
// still make `muro logs` (without --follow) show nothing for a long-running
// sandbox until it stops, and would defeat `--follow` entirely. This is the
// same incremental-capture behavior a real murod restart mid-sandbox-life
// depends on (verified separately, manually, end-to-end against the real
// murod/muro binaries: kill murod mid-run, wait several seconds with it
// down, restart it, confirm the log has no gap for exactly that window) —
// this test isolates and locks in the underlying mechanism so a future
// regression here would be caught without needing the full daemon stack.
func TestLogCapture_IsIncrementalNotBatchedAtExit(t *testing.T) {
	iso := newIsolator(t)
	logPath := filepath.Join(t.TempDir(), "sandbox.log")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	h, err := iso.Launch(ctx, sandbox.LaunchSpec{
		Mounts:    shellMounts(),
		Cmd:       []string{"/bin/sh", "-c", "for i in 1 2 3 4 5; do echo tick-$i; sleep 1; done"},
		PTY:       true,
		SandboxID: "log-test-2",
		LogPath:   logPath,
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer iso.Stop(h)

	// Poll mid-run (the sandbox takes ~5s total) rather than waiting for
	// exit — this is what actually distinguishes incremental capture from
	// a batch-at-exit implementation.
	deadline := time.Now().Add(4 * time.Second)
	sawEarlyTick := false
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(logPath)
		if err == nil && strings.Contains(string(data), "tick-1") {
			sawEarlyTick = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !sawEarlyTick {
		t.Fatal("tick-1 never appeared in the log file while the sandbox was still running — logging appears to be batched at exit, not incremental")
	}

	// The process is still running at this point (its full loop takes ~5s
	// and well under that has elapsed) — confirm that directly rather than
	// assuming it, then let it finish normally.
	select {
	case <-ctx.Done():
		t.Fatal("context expired before the sandbox finished")
	default:
	}

	code, err := h.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if code != 0 {
		t.Fatalf("sandboxed script exited %d, want 0", code)
	}
}
