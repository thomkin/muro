package state

import "testing"

func TestRingPushAndLast(t *testing.T) {
	r := NewRing(3)

	// Empty ring.
	if got := r.Last(5); len(got) != 0 {
		t.Fatalf("Last on empty ring = %v, want empty", got)
	}

	r.Push(DeniedEvent{Host: "a"})
	r.Push(DeniedEvent{Host: "b"})

	got := r.Last(5) // asking for more than available
	want := []string{"a", "b"}
	if len(got) != len(want) {
		t.Fatalf("Last(5) len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i, h := range want {
		if got[i].Host != h {
			t.Errorf("Last(5)[%d].Host = %q, want %q", i, got[i].Host, h)
		}
	}
}

func TestRingOverflowEvictsOldest(t *testing.T) {
	r := NewRing(3)
	r.Push(DeniedEvent{Host: "a"})
	r.Push(DeniedEvent{Host: "b"})
	r.Push(DeniedEvent{Host: "c"})
	r.Push(DeniedEvent{Host: "d"}) // evicts "a"

	got := r.Last(10)
	want := []string{"b", "c", "d"}
	if len(got) != len(want) {
		t.Fatalf("Last(10) len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i, h := range want {
		if got[i].Host != h {
			t.Errorf("Last(10)[%d].Host = %q, want %q", i, got[i].Host, h)
		}
	}
}

func TestRingLastN(t *testing.T) {
	r := NewRing(5)
	for _, h := range []string{"a", "b", "c", "d", "e"} {
		r.Push(DeniedEvent{Host: h})
	}

	got := r.Last(2)
	want := []string{"d", "e"}
	if len(got) != len(want) {
		t.Fatalf("Last(2) len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i, h := range want {
		if got[i].Host != h {
			t.Errorf("Last(2)[%d].Host = %q, want %q", i, got[i].Host, h)
		}
	}
}

func TestRingSnapshotAndLoadSnapshot(t *testing.T) {
	r := NewRing(3)
	r.Push(DeniedEvent{Host: "a"})
	r.Push(DeniedEvent{Host: "b"})
	r.Push(DeniedEvent{Host: "c"})
	r.Push(DeniedEvent{Host: "d"}) // ring is now [b, c, d]

	snap := r.Snapshot()

	r2 := NewRing(3)
	r2.LoadSnapshot(snap)

	got := r2.Last(10)
	want := []string{"b", "c", "d"}
	if len(got) != len(want) {
		t.Fatalf("after LoadSnapshot, Last(10) len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i, h := range want {
		if got[i].Host != h {
			t.Errorf("after LoadSnapshot, Last(10)[%d].Host = %q, want %q", i, got[i].Host, h)
		}
	}

	// Ring should still behave correctly (respect capacity) after loading.
	r2.Push(DeniedEvent{Host: "e"})
	got = r2.Last(10)
	want = []string{"c", "d", "e"}
	if len(got) != len(want) {
		t.Fatalf("after LoadSnapshot+Push, Last(10) len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i, h := range want {
		if got[i].Host != h {
			t.Errorf("after LoadSnapshot+Push, Last(10)[%d].Host = %q, want %q", i, got[i].Host, h)
		}
	}
}
