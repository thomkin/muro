package ptyio

import (
	"bytes"
	"io"
	"os"
	"testing"
	"time"
)

// TestPump_RelaysBothDirections is the plain happy-path: bytes written to
// in reach remoteW, and bytes written to remoteR reach out.
func TestPump_RelaysBothDirections(t *testing.T) {
	inR, inW, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatal(pipeErr)
	}
	defer inW.Close()
	remoteR, remoteRW := io.Pipe()
	var outBuf, remoteWBuf bytes.Buffer

	errCh := make(chan error, 1)
	go func() { errCh <- Pump(inR, &outBuf, remoteR, &remoteWBuf) }()

	if _, err := inW.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := remoteRW.Write([]byte("world")); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if remoteWBuf.Len() == 5 && outBuf.Len() == 5 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := remoteWBuf.String(); got != "hello" {
		t.Errorf("remoteW got %q, want %q", got, "hello")
	}
	if got := outBuf.String(); got != "world" {
		t.Errorf("out got %q, want %q", got, "world")
	}

	remoteRW.Close()
	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
		t.Fatal("Pump did not return after the remote side closed")
	}
}

// TestPump_CancelsPendingReadOnInAfterRemoteEnds is the regression test for
// the bug found while building `muro tui`: when remoteR ends first (the
// normal detach case), Pump used to return immediately while its OTHER
// goroutine stayed blocked reading `in` forever — for `muro sandbox
// attach`, harmless (the process exits right after); for `muro tui`, this
// silently stole every subsequent keystroke from Bubble Tea's own
// re-initialized stdin reader after resuming (confirmed by direct
// reproduction: no key, including quit, worked again after a single
// attach/detach cycle). Pump must now actually stop reading `in` once it
// returns — verified here by confirming a byte written to `in` AFTER Pump
// has returned is never relayed to remoteW.
func TestPump_CancelsPendingReadOnInAfterRemoteEnds(t *testing.T) {
	inR, inW, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatal(pipeErr)
	}
	defer inR.Close()
	defer inW.Close()
	remoteR, remoteRW := io.Pipe()
	var outBuf, remoteWBuf bytes.Buffer

	errCh := make(chan error, 1)
	go func() { errCh <- Pump(inR, &outBuf, remoteR, &remoteWBuf) }()

	// Simulate the remote (sandbox) ending the stream first, e.g. a detach
	// — Pump must return even though nothing has been written to `in` yet,
	// leaving its in-reading goroutine still blocked at that moment.
	remoteRW.Close()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Pump returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Pump did not return after the remote side ended")
	}

	// Give the fix a moment to actually cancel the leaked goroutine's
	// pending Read before we probe it.
	time.Sleep(100 * time.Millisecond)

	if _, err := inW.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	if remoteWBuf.Len() != 0 {
		t.Errorf("a byte written to `in` after Pump returned was still relayed to remoteW (%q) — "+
			"the in-reading goroutine was not actually canceled, it's still silently consuming input",
			remoteWBuf.String())
	}
}
