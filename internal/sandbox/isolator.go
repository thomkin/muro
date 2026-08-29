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

	// ToolSocketPath, if set, is the host-side Unix socket murod listens on
	// for this sandbox's git tool-proxy requests (toolsocket.go) —
	// BwrapIsolator mounts it into the sandbox at a fixed internal path
	// (ToolSocketMountPath, bwrap.go). Empty means the tool-proxy is
	// disabled for this sandbox (no git policy configured), in which case
	// neither this mount nor the git stub mount below is added.
	ToolSocketPath string

	// GitStubHostPath, if set, is the host path to the git tool-proxy stub
	// binary (cmd/muro-toolstub) — mounted read-only at GitStubMountPath,
	// shadowing any real git binary. Resolved by BwrapIsolator.Launch
	// itself (via toolstubPath()) when ToolSocketPath is set, not by the
	// caller — a direct Isolator caller normally leaves this empty and
	// lets Launch fill it in.
	GitStubHostPath string

	// AudioRuntimeDir, if set, is the host's $XDG_RUNTIME_DIR — presence
	// (non-empty) signals that this sandbox's Mounts already includes the
	// PipeWire/PulseAudio socket bind mounts (audio.go's AudioMounts,
	// applied by Manager before Launch, same "resolved once by Manager,
	// not by BwrapIsolator" pattern as GitPolicy). BwrapIsolator uses this
	// only to set XDG_RUNTIME_DIR as a default sandbox env var (buildArgs)
	// pointing at the identical path the mounts already landed at — a
	// profile that explicitly sets its own XDG_RUNTIME_DIR in Env still
	// wins, same precedence as HTTP_PROXY/HTTPS_PROXY below. Empty means
	// audio passthrough is disabled for this sandbox.
	AudioRuntimeDir string

	// SessionID, if set, is the sandbox's stable per-instance UUID
	// (state.Sandbox.SessionID) — BwrapIsolator uses this only to set
	// MURO_SESSION_ID as a default sandbox env var (buildArgs), same
	// override precedence as AudioRuntimeDir/HTTP_PROXY above (a profile's
	// own explicit MURO_SESSION_ID in Env still wins).
	SessionID string
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
