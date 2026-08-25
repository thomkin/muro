package sandbox

import (
	"context"
	"io"
	"sync"

	"github.com/thomkin/muro/internal/config"
)

// fakeIsolator is an entirely in-memory Isolator for unit-testing Manager
// without any real process or bwrap dependency (IMPLEMENTATION.md §10/§12
// M2).
type fakeIsolator struct {
	mu       sync.Mutex
	nextPID  int
	launched []LaunchSpec
	handles  []*fakeHandle // one per Launch call, in order — tests use this to reach into a specific launch's fakeHandle (e.g. to simulate a crash via finish, or to flip updateApplies)
}

func newFakeIsolator() *fakeIsolator {
	return &fakeIsolator{nextPID: 1000}
}

func (f *fakeIsolator) Launch(_ context.Context, spec LaunchSpec) (Handle, error) {
	f.mu.Lock()
	f.nextPID++
	pid := f.nextPID
	f.launched = append(f.launched, spec)
	h := &fakeHandle{
		pid:           pid,
		done:          make(chan struct{}),
		updateApplies: true, // hot-apply succeeds by default; tests override to exercise reload-pending
	}
	f.handles = append(f.handles, h)
	f.mu.Unlock()

	return h, nil
}

// lastHandle returns the most recently launched fakeHandle, or nil if
// nothing has been launched yet.
func (f *fakeIsolator) lastHandle() *fakeHandle {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.handles) == 0 {
		return nil
	}
	return f.handles[len(f.handles)-1]
}

// handleCount returns how many times Launch has been called so far —
// tests use this to wait for/assert a restart actually relaunched.
func (f *fakeIsolator) handleCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.handles)
}

func (f *fakeIsolator) UpdateMounts(h Handle, mounts []config.Mount) (bool, error) {
	fh := h.(*fakeHandle)
	fh.mu.Lock()
	defer fh.mu.Unlock()
	fh.mounts = mounts
	return fh.updateApplies, nil
}

func (f *fakeIsolator) Stop(h Handle) error {
	fh := h.(*fakeHandle)
	fh.finish(0, nil)
	return nil
}

// fakeHandle is a Handle whose exit is entirely test-controlled via
// finish/crash, so restart_policy timing tests never depend on a real
// process or wall-clock exit.
type fakeHandle struct {
	pid  int
	done chan struct{}

	mu            sync.Mutex
	exitCode      int
	exitErr       error
	finished      bool
	mounts        []config.Mount
	updateApplies bool // whether UpdateMounts reports the change as hot-applied; set true by fakeIsolator.Launch, tests may flip it
}

func (h *fakeHandle) PID() int { return h.pid }

func (h *fakeHandle) Wait() (int, error) {
	<-h.done
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.exitCode, h.exitErr
}

func (h *fakeHandle) Stdio() (io.ReadWriteCloser, bool) {
	// No real pty backs a fakeHandle; attach-exclusivity tests only need
	// Attach to succeed in claiming the slot, not a working terminal.
	return nil, true
}

// finish marks the handle exited with the given code/error, waking any
// Wait() caller. Safe to call more than once; only the first call has an
// effect.
func (h *fakeHandle) finish(exitCode int, err error) {
	h.mu.Lock()
	if h.finished {
		h.mu.Unlock()
		return
	}
	h.finished = true
	h.exitCode = exitCode
	h.exitErr = err
	h.mu.Unlock()
	close(h.done)
}
