package sandbox

import (
	"context"
	"io"

	"github.com/thomkin/muro/internal/config"
)

// LaunchSpec is everything an Isolator needs to start one sandboxed
// process.
type LaunchSpec struct {
	Mounts []config.Mount
	Tools  []config.Tool
	Env    map[string]string
	Cmd    []string
	PTY    bool

	// SandboxID identifies which sandbox this is launching (state.Sandbox's
	// ID, set by Manager before calling Launch). BwrapIsolator uses it to
	// name the surviving shim process's per-sandbox socket/status-file
	// directory (shim.go) — it doesn't need to be globally meaningful
	// beyond "a stable name for this sandbox's on-disk runtime files".
	SandboxID string

	// LogPath, if set, is where muro-shim continuously appends the
	// sandbox's pty output for the whole life of the sandbox — independent
	// of whether anything is attached and independent of murod's own
	// process lifetime, so `muro logs` has no gap across a murod restart
	// (DESIGN.md §6's <namespace>__<name>.log convention;
	// config.SandboxLogPath computes this same path from the CLI/control
	// side without needing it persisted anywhere). Empty means no log
	// capture — only used by direct Isolator callers/tests that don't go
	// through Manager, which always sets it.
	LogPath string

	// AgentSocketPath, if set, is the host-side Unix socket murod listens
	// on for this sandbox's outbound MQTT publish requests (the
	// agent-to-agent bridge, agentsocket.go) — BwrapIsolator mounts it
	// into the sandbox at a fixed internal path (AgentSocketMountPath,
	// bwrap.go) so `muro pubsub publish` can reach it. Empty means the
	// bridge is disabled for this sandbox (Manager.pubStateDir unset —
	// pub/sub not configured at all), in which case no such mount is
	// added and the sandbox simply has no agent socket.
	AgentSocketPath string
}

// Isolator is the sandboxing backend (DESIGN.md §6.1). v1 wraps the bwrap
// binary (bwrap.go, a later milestone); a native Go namespace
// implementation can be swapped in later without touching Manager, the
// proxy, or pub/sub, since nothing outside this package depends on which
// Isolator is in use.
type Isolator interface {
	Launch(ctx context.Context, spec LaunchSpec) (Handle, error)
	UpdateMounts(h Handle, mounts []config.Mount) (applied bool, err error)
	Stop(h Handle) error
}

// Handle is a live reference to one launched sandbox process.
type Handle interface {
	PID() int
	Wait() (exitCode int, err error)

	// Stdio returns a fresh, independent connection to the sandbox's pty
	// each time it's called (not a single long-held file) — real
	// implementations (bwrapHandle) dial a persistent shim process's Unix
	// socket rather than returning an in-process pty fd, precisely so the
	// pty master survives across a murod restart (see shim.go). The
	// returned value additionally supports SetReadDeadline when the
	// concrete type allows it (net.Conn always does) — callers that want
	// non-blocking polling should type-assert for that optional
	// capability (matching this package's networkAddrProvider pattern)
	// rather than assuming it.
	Stdio() (pty io.ReadWriteCloser, ok bool)
}
