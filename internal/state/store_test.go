package state

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/thomkin/muro/internal/config"
)

func testSandbox(namespace, name string) *Sandbox {
	id, err := NewID()
	if err != nil {
		panic(err)
	}
	return &Sandbox{
		ID:        id,
		Name:      name,
		Namespace: namespace,
		Profile:   "claude-default",
		Agent:     "claude",
		PID:       12345,
		State:     StateRunning,
		StartedAt: time.Now().Truncate(time.Second),
		Mounts: []config.Mount{
			{Host: "~/projects/myrepo", SandboxPath: "/workspace", Mode: "rw"},
		},
		Tools: []config.Tool{
			{Host: "/usr/bin/git", As: "git"},
		},
		AllowURLs:     []string{"https://api.anthropic.com"},
		RestartPolicy: "never",
	}
}

func TestStorePutGetListDelete(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(filepath.Join(dir, "state.json"))

	sb1 := testSandbox("default", "claude-1")
	sb2 := testSandbox("default", "claude-2")
	sb3 := testSandbox("other", "claude-1") // same name, different namespace

	for _, sb := range []*Sandbox{sb1, sb2, sb3} {
		if err := s.Put(sb); err != nil {
			t.Fatalf("Put(%s/%s) error: %v", sb.Namespace, sb.Name, err)
		}
	}

	got, ok := s.Get("default", "claude-1")
	if !ok {
		t.Fatal("Get(default, claude-1) not found")
	}
	if got.PID != sb1.PID {
		t.Errorf("Get(default, claude-1).PID = %d, want %d", got.PID, sb1.PID)
	}

	if _, ok := s.Get("default", "does-not-exist"); ok {
		t.Error("Get(default, does-not-exist) found a sandbox, want not found")
	}

	all := s.List("")
	if len(all) != 3 {
		t.Fatalf("List(\"\") len = %d, want 3", len(all))
	}

	defaultNS := s.List("default")
	if len(defaultNS) != 2 {
		t.Fatalf("List(default) len = %d, want 2", len(defaultNS))
	}

	if err := s.Delete("default", "claude-1"); err != nil {
		t.Fatalf("Delete error: %v", err)
	}
	if _, ok := s.Get("default", "claude-1"); ok {
		t.Error("Get after Delete still found the sandbox")
	}
	if len(s.List("")) != 2 {
		t.Errorf("List(\"\") after Delete len = %d, want 2", len(s.List("")))
	}

	// Deleting something that doesn't exist is a no-op, not an error.
	if err := s.Delete("default", "never-existed"); err != nil {
		t.Errorf("Delete of nonexistent sandbox returned error: %v", err)
	}
}

func TestStorePersistAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s := NewStore(path)
	sb := testSandbox("default", "claude-1")
	if err := s.Put(sb); err != nil {
		t.Fatalf("Put error: %v", err)
	}
	if err := s.RecordDenied("default", "claude-1", "evil.example.com", "https://evil.example.com/x"); err != nil {
		t.Fatalf("RecordDenied error: %v", err)
	}

	// Fresh Store pointed at the same file.
	s2 := NewStore(path)
	if err := s2.Load(); err != nil {
		t.Fatalf("Load error: %v", err)
	}

	got, ok := s2.Get("default", "claude-1")
	if !ok {
		t.Fatal("after Load, sandbox not found")
	}
	if got.ID != sb.ID || got.PID != sb.PID || got.Profile != sb.Profile {
		t.Errorf("after Load, sandbox = %+v, want match for %+v", got, sb)
	}
	if len(got.Mounts) != 1 || got.Mounts[0].SandboxPath != "/workspace" {
		t.Errorf("after Load, Mounts = %+v, want one entry at /workspace", got.Mounts)
	}
	if len(got.Tools) != 1 || got.Tools[0].As != "git" {
		t.Errorf("after Load, Tools = %+v, want one 'git' entry", got.Tools)
	}

	events := s2.events.Last(10)
	if len(events) != 1 || events[0].Host != "evil.example.com" {
		t.Errorf("after Load, denied events = %+v, want one evil.example.com entry", events)
	}
}

func TestStoreLoadMissingFileIsNotError(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(filepath.Join(dir, "does-not-exist.json"))
	if err := s.Load(); err != nil {
		t.Fatalf("Load on missing file returned error: %v", err)
	}
	if len(s.List("")) != 0 {
		t.Errorf("List after Load on missing file = %v, want empty", s.List(""))
	}
}

func TestStorePersistIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s := NewStore(path)

	if err := s.Put(testSandbox("default", "claude-1")); err != nil {
		t.Fatalf("Put error: %v", err)
	}

	// No leftover temp files after a successful persist.
	entries, err := filepath.Glob(filepath.Join(dir, ".state-*.json.tmp"))
	if err != nil {
		t.Fatalf("glob error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("leftover temp files after persist: %v", entries)
	}
}
