package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// defaultEventLogCap matches daemon.yaml's documented default (DESIGN.md §5).
// NewStore doesn't take a cap parameter (IMPLEMENTATION.md §5's signature),
// so this is the value used; wire daemon.yaml's event_log_cap through here
// once cmd/murod is assembled if a non-default cap is needed.
const defaultEventLogCap = 200

// snapshot is the on-disk shape of state.json: the full sandbox registry
// plus the denied-event ring, oldest-to-newest. It is a persistence/
// crash-recovery snapshot only — murod's in-memory Store is authoritative
// (DESIGN.md §5).
type snapshot struct {
	Sandboxes    []*Sandbox    `json:"sandboxes"`
	DeniedEvents []DeniedEvent `json:"denied_events"`
}

// Store is murod's in-memory sandbox registry, write-through persisted to a
// JSON file on every mutation (DESIGN.md §5). Store is safe for concurrent
// use.
type Store struct {
	mu        sync.RWMutex
	path      string
	sandboxes map[string]*Sandbox // keyed by "namespace/name"
	events    *Ring
}

// NewStore creates a Store backed by the state file at path. Call Load to
// populate it from an existing file before use.
func NewStore(path string) *Store {
	return &Store{
		path:      path,
		sandboxes: make(map[string]*Sandbox),
		events:    NewRing(defaultEventLogCap),
	}
}

// Load reads the state file at Store's path, if it exists, and populates
// the in-memory registry. A missing file is not an error — that's just a
// fresh install with nothing persisted yet.
func (s *Store) Load() error {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read state file: %w", err)
	}

	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("parse state file: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sandboxes = make(map[string]*Sandbox, len(snap.Sandboxes))
	for _, sb := range snap.Sandboxes {
		s.sandboxes[key(sb.Namespace, sb.Name)] = sb
	}
	s.events.LoadSnapshot(snap.DeniedEvents)
	return nil
}

// persist writes the full in-memory registry to disk atomically
// (write-to-temp-file + rename, DESIGN.md §5). Caller must hold at least a
// read lock — persist itself only reads s.sandboxes/s.events, both of which
// are safe to snapshot under RLock since events has its own internal lock.
func (s *Store) persist() error {
	snap := snapshot{
		Sandboxes:    make([]*Sandbox, 0, len(s.sandboxes)),
		DeniedEvents: s.events.Snapshot(),
	}
	for _, sb := range s.sandboxes {
		snap.Sandboxes = append(snap.Sandboxes, sb)
	}

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".state-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp state file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp state file: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("rename temp state file into place: %w", err)
	}
	return nil
}

// Put inserts or replaces a sandbox record and persists the registry.
func (s *Store) Put(sb *Sandbox) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sandboxes[key(sb.Namespace, sb.Name)] = sb
	return s.persist()
}

// Get looks up a sandbox by namespace/name.
func (s *Store) Get(namespace, name string) (*Sandbox, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sb, ok := s.sandboxes[key(namespace, name)]
	return sb, ok
}

// List returns all sandboxes in namespace, or every sandbox across all
// namespaces if namespace is "".
func (s *Store) List(namespace string) []*Sandbox {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Sandbox, 0, len(s.sandboxes))
	for _, sb := range s.sandboxes {
		if namespace == "" || sb.Namespace == namespace {
			out = append(out, sb)
		}
	}
	return out
}

// Delete removes a sandbox record and persists the registry. Deleting a
// name that doesn't exist is a no-op, not an error.
func (s *Store) Delete(namespace, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sandboxes, key(namespace, name))
	return s.persist()
}

// RecordDenied appends a denied-network event for the given sandbox and
// persists the registry (DESIGN.md §5's capped denied-URL event log).
func (s *Store) RecordDenied(namespace, name, host, url string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events.Push(DeniedEvent{
		Namespace: namespace,
		Name:      name,
		Host:      host,
		URL:       url,
		Timestamp: time.Now(),
	})
	return s.persist()
}
