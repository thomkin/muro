package sandbox

// ShimSpec is the launch contract between BwrapIsolator.Launch and the
// muro-shim binary (cmd/muro-shim): Launch writes one of these as JSON to
// a temp file and passes its path as the shim's sole argument. The shim
// reads it, deletes the file, and never needs to see LaunchSpec or any
// other muro internals — this is the entire, intentionally narrow
// boundary between the two processes.
type ShimSpec struct {
	BwrapPath  string   `json:"bwrap_path"`
	Args       []string `json:"args"`
	PTY        bool     `json:"pty"`
	SocketPath string   `json:"socket_path"`        // Unix socket the shim listens on for attach connections
	StatusPath string   `json:"status_path"`        // where the shim records bwrap's exit status once it's known
	LogPath    string   `json:"log_path,omitempty"` // where the shim continuously appends pty output; empty disables log capture

	// InjectSocketPath, if set (only when PTY is true), is a SECOND Unix
	// socket the shim listens on for automated pty input injection — the
	// MQTT inbox bridge (internal/sandbox's inbox-listener) dials this to
	// deliver an arriving message as if a human had typed it. Deliberately
	// a separate socket from SocketPath/attach, not a shared path: attach's
	// handleAttachConn claims ptyBroadcaster's exclusive "current" slot and
	// clears it on disconnect, which a short-lived injector connection
	// would wrongly steal from (and then wrongly clear) a real, concurrent
	// human attach session. The injection handler never touches
	// ptyBroadcaster at all — it only ever writes to ptmx directly.
	InjectSocketPath string `json:"inject_socket_path,omitempty"`
}

// ShimStatus is written atomically (temp file + rename, same pattern
// internal/state.Store uses) by muro-shim to ShimSpec.StatusPath the
// moment its bwrap child exits — this is how a Handle reconstructed after
// a murod restart (which was never the shim's parent and so can't
// exec.Cmd.Wait it) recovers the real exit code instead of only knowing
// "the shim process is gone".
type ShimStatus struct {
	ExitCode int    `json:"exit_code"`
	Err      string `json:"err,omitempty"` // set instead of a meaningful ExitCode if the shim couldn't determine one
}

// ShimReadyOK / ShimReadyErrPrefix are the two possible lines muro-shim
// writes to its inherited ready-fd (fd 3) once it knows whether bwrap
// started: "OK <pid>\n" (bwrap's outer PID) or "ERR <message>\n". Kept as
// exported constants/helpers rather than each side hand-rolling the
// format, so BwrapIsolator.Launch's parser and the shim's writer can't
// silently drift apart.
const (
	shimReadyOKPrefix  = "OK "
	shimReadyErrPrefix = "ERR "
)
