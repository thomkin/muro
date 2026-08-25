package state

import (
	"sync"
	"time"
)

// DeniedEvent records one network request the proxy blocked (SPEC.md §6.2).
type DeniedEvent struct {
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	Host      string    `json:"host"`
	URL       string    `json:"url"`
	Timestamp time.Time `json:"timestamp"`
}

// Ring is a fixed-capacity, concurrency-safe ring buffer of DeniedEvents.
// It overwrites the oldest entry once full (DESIGN.md §5's capped event log).
type Ring struct {
	mu   sync.Mutex
	buf  []DeniedEvent
	cap  int
	next int // index the next Push writes to
	size int // number of valid entries currently stored (<= cap)
}

// NewRing creates a Ring holding at most cap entries.
func NewRing(cap int) *Ring {
	if cap <= 0 {
		cap = 1
	}
	return &Ring{buf: make([]DeniedEvent, cap), cap: cap}
}

// Push records a new event, evicting the oldest one if the ring is full.
func (r *Ring) Push(e DeniedEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = e
	r.next = (r.next + 1) % r.cap
	if r.size < r.cap {
		r.size++
	}
}

// Last returns up to n most recent events, ordered oldest-to-newest (the
// same order they'd read in a log file, oldest at the top).
func (r *Ring) Last(n int) []DeniedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()

	if n > r.size {
		n = r.size
	}
	if n <= 0 {
		return nil
	}

	out := make([]DeniedEvent, n)
	// oldest of the requested n is (size-n) steps back from "next".
	start := (r.next - n + r.cap) % r.cap
	for i := 0; i < n; i++ {
		out[i] = r.buf[(start+i)%r.cap]
	}
	return out
}

// Snapshot returns all currently stored events, oldest-to-newest. Used when
// serializing the full registry to state.json.
func (r *Ring) Snapshot() []DeniedEvent {
	r.mu.Lock()
	size := r.size
	r.mu.Unlock()
	return r.Last(size)
}

// LoadSnapshot replaces the ring's contents with the given events (assumed
// oldest-to-newest, already capped to the ring's capacity by the caller).
// Used when restoring from a state.json snapshot on startup.
func (r *Ring) LoadSnapshot(events []DeniedEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next = 0
	r.size = 0
	for i := range r.buf {
		r.buf[i] = DeniedEvent{}
	}
	for _, e := range events {
		r.buf[r.next] = e
		r.next = (r.next + 1) % r.cap
		if r.size < r.cap {
			r.size++
		}
	}
}
