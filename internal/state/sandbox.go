package state

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/thomkin/muro/internal/config"
)

// SandboxState is the lifecycle state of a sandbox.
type SandboxState string

const (
	StateRunning          SandboxState = "running"
	StateStopped          SandboxState = "stopped"
	StateReloadPending    SandboxState = "reload-pending"
	StateCrashed          SandboxState = "crashed"
	StateRestarting       SandboxState = "restarting"
	StateRestartExhausted SandboxState = "restart-exhausted"
)

// Sandbox is the daemon's live record of one running (or previously running)
// sandbox. ID is an internal storage key only — CLI-facing addressing uses
// Namespace/Name (DESIGN.md §9).
type Sandbox struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Namespace     string         `json:"namespace"`
	Profile       string         `json:"profile"`
	Agent         string         `json:"agent"`
	PID           int            `json:"pid"`
	State         SandboxState   `json:"state"`
	StartedAt     time.Time      `json:"started_at"`
	Mounts        []config.Mount `json:"mounts"`
	Tools         []config.Tool  `json:"tools"`
	AllowURLs     []string       `json:"allow_urls"`
	RestartPolicy string         `json:"restart_policy"`
	RestartCount  int            `json:"restart_count"`

	// ShimSocket, NetAddr, and SlirpPID let a restarted murod reconstruct
	// a live Handle for a sandbox it didn't itself launch (Stage 3 shim
	// lifecycle, internal/sandbox/shim.go) — PID alone is only enough to
	// check liveness (state.Reconcile), not to re-attach or correctly
	// tear the sandbox's network bridge down. ShimSocket is the Unix
	// socket path the sandbox's persistent shim process listens on for
	// attach; NetAddr is its bridged outbound loopback address
	// (internal/sandbox/network.go); SlirpPID is its slirp4netns bridge
	// process's PID, needed because a reconstructed Handle was never that
	// process's parent and so has no wait4(2)-based way to manage it,
	// only signal-by-PID.
	ShimSocket string `json:"shim_socket,omitempty"`
	NetAddr    string `json:"net_addr,omitempty"`
	SlirpPID   int    `json:"slirp_pid,omitempty"`

	// InjectSocket is the Unix socket path muro-shim listens on for pty
	// input injection (internal/sandbox/inject.go) — the inbound half of
	// the MQTT agent-to-agent bridge (DESIGN.md §8). Persisted, like
	// ShimSocket, so the daemon's inbox-message listener can dial it
	// directly from the Store without needing a live Handle at all — this
	// is what lets an inbox subscription re-delivering a message after a
	// murod restart still work even though Reattach (internal/sandbox/
	// bwrap.go) never repopulates this field on its reconstructed Handle.
	InjectSocket string `json:"inject_socket,omitempty"`
}

// Clone returns a deep copy of sb — a shallow struct copy would still share
// the underlying arrays of Mounts/Tools/AllowURLs with the original, so a
// caller mutating a "copy" could still race with or corrupt the Store's
// internal state. Store uses Clone on every value crossing its API boundary
// (in via Put, out via Get/List) so callers never hold a pointer aliased to
// the Store's own map entries, however they mutate it (internal/sandbox's
// Manager found this the hard way via go test -race).
func (sb *Sandbox) Clone() *Sandbox {
	if sb == nil {
		return nil
	}
	out := *sb
	out.Mounts = append([]config.Mount(nil), sb.Mounts...)
	out.Tools = append([]config.Tool(nil), sb.Tools...)
	out.AllowURLs = append([]string(nil), sb.AllowURLs...)
	return &out
}

// NewID generates a unique internal sandbox id, e.g. "sb_8f2a1c9d".
func NewID() (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate sandbox id: %w", err)
	}
	return "sb_" + hex.EncodeToString(buf), nil
}

// key is the Store's internal map key for a sandbox: "namespace/name".
func key(namespace, name string) string {
	return namespace + "/" + name
}
