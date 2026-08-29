package sandbox

import (
	"regexp"
	"testing"
)

var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewSessionID_LooksLikeAUUIDv4(t *testing.T) {
	id, err := NewSessionID()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !uuidV4Pattern.MatchString(id) {
		t.Errorf("NewSessionID() = %q, does not match RFC 4122 v4 shape", id)
	}
}

func TestNewSessionID_UniqueAcrossCalls(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id, err := NewSessionID()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if seen[id] {
			t.Fatalf("NewSessionID() produced a duplicate: %q", id)
		}
		seen[id] = true
	}
}
