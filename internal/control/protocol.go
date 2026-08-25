// Package control implements murod's control API: a Unix-domain-socket
// server (used by murod) and client (used by cmd/muro and, later, any
// other client such as a future TUI/web dashboard — DESIGN.md §5/§7) that
// speaks newline-delimited JSON request/response envelopes. Every muro CLI
// subcommand except profile management (reads/writes JSON files directly,
// no daemon round-trip) and `daemon start` (there's no socket to call if
// murod isn't running yet) maps to exactly one request Type defined here
// (IMPLEMENTATION.md §7).
package control

import "encoding/json"

// Request is the wire envelope sent by a client, one per newline-delimited
// JSON line.
type Request struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Response is the wire envelope sent back by the server, one per
// newline-delimited JSON line, in reply to exactly one Request (except
// for the sandbox.attach stream-upgrade case, DESIGN.md §12 / stream.go,
// where this one JSON handshake Response is followed by a raw
// bidirectional byte stream on the same connection rather than more
// Request/Response lines).
type Response struct {
	OK      bool            `json:"ok"`
	Error   string          `json:"error,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Request type constants (IMPLEMENTATION.md §7's table).
const (
	TypeStatus         = "status"
	TypeSandboxShow    = "sandbox.show"
	TypeSandboxRun     = "sandbox.run"
	TypeSandboxUpdate  = "sandbox.update"
	TypeSandboxReload  = "sandbox.reload"
	TypeSandboxRestart = "sandbox.restart"
	TypeSandboxStop    = "sandbox.stop"
	TypeSandboxAttach  = "sandbox.attach"
	TypeLogs           = "logs"
	TypeBrokerStatus   = "broker.status"
	TypeDaemonShutdown = "daemon.shutdown"
)

// --- status ---

type StatusRequest struct {
	Namespace string `json:"namespace,omitempty"` // "" = every namespace
}

type StatusResponse struct {
	Sandboxes []*SandboxView `json:"sandboxes"`
}

// --- sandbox.show ---

type SandboxShowRequest struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

// SandboxShowResponse's payload IS a *SandboxView directly (not wrapped),
// matching IMPLEMENTATION.md §7's response-payload column ("Sandbox") for
// this request type.

// --- sandbox.run ---

type SandboxRunRequest struct {
	Profile string `json:"profile"` // profile NAME — the server loads and
	// validates the profile file itself (config.LoadProfile), since murod
	// is what actually needs the *config.Profile to call Manager.Run.
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Agent     string `json:"agent,omitempty"` // optional override of the profile's agent
}

// SandboxRunResponse's payload IS a *SandboxView directly, same as
// sandbox.show.

// --- sandbox.update ---

// UpdateSelector mirrors sandbox.Selector's shape (DESIGN.md §11): exactly
// one of Name, Profile, or All should be set.
type UpdateSelector struct {
	Name    string `json:"name,omitempty"`
	Profile string `json:"profile,omitempty"`
	All     bool   `json:"all,omitempty"`
}

type SandboxUpdateRequest struct {
	Selector  UpdateSelector `json:"selector"`
	Namespace string         `json:"namespace,omitempty"`
	Mounts    []MountView    `json:"mounts,omitempty"`
	AllowURLs []string       `json:"allowURLs,omitempty"`
	DenyURLs  []string       `json:"denyURLs,omitempty"`
}

type UpdateResultView struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Applied   bool   `json:"applied"`
}

type SandboxUpdateResponse struct {
	Results []UpdateResultView `json:"results"`
}

// --- sandbox.reload ---

type SandboxReloadRequest struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

type SandboxReloadResponse struct {
	Applied bool `json:"applied"`
}

// --- sandbox.restart ---

type SandboxRestartRequest struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

type SandboxRestartResponse struct {
	OK bool `json:"ok"`
}

// --- sandbox.stop ---

type SandboxStopRequest struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

type SandboxStopResponse struct {
	OK bool `json:"ok"`
}

// --- sandbox.attach ---

type SandboxAttachRequest struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

// SandboxAttachResponse is the JSON handshake response; if OK, the same
// connection immediately becomes a raw bidirectional byte stream (the
// sandbox's pty) until the client sends sandbox.DetachSequence
// (stream.go).
type SandboxAttachResponse struct {
	OK bool `json:"ok"`
}

// --- logs ---

type LogsRequest struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Follow    bool   `json:"follow,omitempty"`
}

// LogsResponse is the JSON handshake response; if OK, the same connection
// immediately becomes a one-directional raw byte stream (the sandbox's
// captured output, muro-shim's continuous pty capture — DESIGN.md §6) —
// the existing content first, then (if Follow) newly-appended content as
// it happens, until the server reaches EOF-with-no-more-following or the
// client disconnects (stream.go's handleLogs).
type LogsResponse struct {
	OK bool `json:"ok"`
}

// --- broker.status ---

type BrokerStatusRequest struct{}

type BrokerStatusResponse struct {
	Connected bool   `json:"connected"`
	Address   string `json:"address,omitempty"`
	LastError string `json:"lastError,omitempty"`
}

// --- daemon.shutdown ---

type DaemonShutdownRequest struct{}

type DaemonShutdownResponse struct {
	OK bool `json:"ok"`
}

// --- shared view types ---
//
// SandboxView/MountView/ToolView mirror internal/state.Sandbox and
// internal/config.Mount/Tool field-for-field. They exist so this package's
// wire format doesn't hard-depend on internal/state's exact struct
// (which is free to gain fields later without silently changing the wire
// protocol) — server.go converts to/from the real types explicitly.

type MountView struct {
	Host        string `json:"host"`
	SandboxPath string `json:"sandbox_path"`
	Mode        string `json:"mode"`
}

type ToolView struct {
	Host string `json:"host"`
	As   string `json:"as"`
}

type SandboxView struct {
	ID            string      `json:"id"`
	Name          string      `json:"name"`
	Namespace     string      `json:"namespace"`
	Profile       string      `json:"profile"`
	Agent         string      `json:"agent"`
	PID           int         `json:"pid"`
	State         string      `json:"state"`
	StartedAt     string      `json:"started_at"` // RFC3339
	Mounts        []MountView `json:"mounts"`
	Tools         []ToolView  `json:"tools"`
	AllowURLs     []string    `json:"allow_urls"`
	RestartPolicy string      `json:"restart_policy"`
	RestartCount  int         `json:"restart_count"`
}
