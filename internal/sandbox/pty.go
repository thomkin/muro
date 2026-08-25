package sandbox

import (
	"sync"
	"time"
)

// DetachSequence is the fixed escape sequence (Ctrl-P Ctrl-Q, matching
// docker attach's convention so it's not a new thing to memorize) that
// detaches an attached terminal from a sandbox without killing the agent
// process (DESIGN.md §12).
const DetachSequence = "\x10\x11"

// attachRegistry tracks, per sandbox, whether a terminal is currently
// attached. Exactly one attacher is allowed at a time (DESIGN.md §12) — a
// second attach attempt is rejected outright, never queued or
// multiplexed, since letting two terminals both drive raw input into the
// same interactive session is a footgun this avoids rather than solves
// cleverly.
type attachRegistry struct {
	mu       sync.Mutex
	attached map[string]time.Time
}

func newAttachRegistry() *attachRegistry {
	return &attachRegistry{attached: make(map[string]time.Time)}
}

// TryAttach attempts to claim the attach slot for key. If it's already
// claimed, already is true and since reports when the existing attach
// started; the caller must reject the new attach rather than queuing it.
func (r *attachRegistry) TryAttach(key string) (already bool, since time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.attached[key]; ok {
		return true, t
	}
	r.attached[key] = time.Now()
	return false, time.Time{}
}

// Detach releases the attach slot for key. Detaching a key that isn't
// attached is a no-op, not an error — Manager.Restart/Stop call this
// unconditionally to force-detach any existing session (DESIGN.md §12: a
// restart re-execs behind a brand-new pty, so any existing attach session
// is stale the moment restart runs).
func (r *attachRegistry) Detach(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.attached, key)
}

// detachScanner watches a stream of client input for DetachSequence,
// split across arbitrarily-sized writes. internal/control/stream.go's raw
// pty passthrough loop (a different package, wired up later) feeds each
// chunk of client input through Feed to know when to stop forwarding
// bytes and detach instead.
type detachScanner struct {
	matched int
}

// Feed reports whether DetachSequence was completed by the end of buf.
// Match state carries over between calls so the sequence can span writes.
func (s *detachScanner) Feed(buf []byte) bool {
	for _, b := range buf {
		switch {
		case b == DetachSequence[s.matched]:
			s.matched++
			if s.matched == len(DetachSequence) {
				s.matched = 0
				return true
			}
		case b == DetachSequence[0]:
			s.matched = 1
		default:
			s.matched = 0
		}
	}
	return false
}
