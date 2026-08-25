package sandbox

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thomkin/muro/internal/config"
)

// PubsubPublisher is the minimal surface Manager needs to let a sandboxed
// process publish an MQTT message via its agent socket (closing DESIGN.md
// §8's gap: "sandboxes do not connect to MQTT directly — they publish/
// subscribe via the daemon's control API"). Kept as a local interface, the
// same decoupling ProxyUpdater/EventPublisher already use, so this package
// doesn't depend on internal/pubsub — topic construction (SPEC.md §8's
// namespace/name-rooted scheme) is the caller's (cmd/murod's adapter)
// responsibility, not this package's; by the time either method is called,
// the same-namespace scoping below has already been enforced, so the
// implementation just needs to build the right topic and publish.
type PubsubPublisher interface {
	// PublishInbox delivers message to namespace/name's inbox. Called only
	// after agentSocketServer has already confirmed namespace matches the
	// calling sandbox's own namespace.
	PublishInbox(namespace, name, message string) error
	// PublishBroadcast delivers message to a free-form topic under
	// namespace (always the calling sandbox's own namespace).
	PublishBroadcast(namespace, topic, message string) error
}

// AgentPublishRequest is the one message shape a sandbox's agent socket
// accepts (via `muro pubsub publish`), newline-terminated JSON — mirroring
// internal/control's request/response framing for consistency, but this is
// a deliberately separate, much narrower protocol. Identity is established
// by which sandbox's dedicated socket a connection arrives on, never by a
// self-reported field in the payload — there is no "from" field here.
type AgentPublishRequest struct {
	To        string `json:"to,omitempty"`        // "namespace/name" — mutually exclusive with Broadcast
	Broadcast string `json:"broadcast,omitempty"` // topic name — mutually exclusive with To
	Message   string `json:"message"`
}

type AgentPublishResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// maxAgentRequestSize bounds a single agent-socket request — generous for
// any realistic inter-agent message while still capping worst-case memory
// growth from a malformed or runaway sender, the same reasoning
// internal/control/server.go's maxRequestLineSize already documents (that
// one caps at 4MiB for the general control API; agent messages are
// expected to be much smaller chat-style text, so this is tighter).
const maxAgentRequestSize = 256 << 10

// agentSocketServer is the per-sandbox listener for the outbound half of
// the MQTT bridge — started by Manager (startAgentSocket) BEFORE Launch,
// using the same host-side path (AgentSocketPath, bwrap.go) BwrapIsolator
// mounts into the sandbox, so the listener is always up before the
// sandboxed process could possibly connect.
type agentSocketServer struct {
	namespace string          // the OWNING sandbox's namespace — same-namespace-only enforcement compares against this
	publisher PubsubPublisher // may be nil (pub/sub configured but broker unreachable) — publish attempts get a clear error, not a launch failure
	path      string
	listener  net.Listener
}

// startAgentSocket listens on path (creating its parent directory and
// setting 0600 permissions, matching internal/control/server.go's and
// cmd/muro-shim's established socket-setup pattern) and starts serving.
// publisher may be nil — the listener still starts so a connecting client
// gets a clear "broker not connected" response rather than the mount
// existing but nothing on the other end.
func startAgentSocket(path, namespace string, publisher PubsubPublisher) (*agentSocketServer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create agent socket dir: %w", err)
	}
	os.Remove(path) // stale socket from a previous run at this same path, if any
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on agent socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("chmod agent socket: %w", err)
	}
	s := &agentSocketServer{namespace: namespace, publisher: publisher, path: path, listener: ln}
	go s.acceptLoop()
	return s, nil
}

func (s *agentSocketServer) stop() {
	_ = s.listener.Close()
	_ = os.Remove(s.path)
}

func (s *agentSocketServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return // listener closed (sandbox stopping) or a real error either way — stop accepting
		}
		go s.handleConn(conn)
	}
}

func (s *agentSocketServer) handleConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	r := bufio.NewReader(conn)
	data, err := readLineLimitedBytes(r, maxAgentRequestSize)
	if err != nil {
		writeAgentResponse(conn, AgentPublishResponse{Error: fmt.Sprintf("read request: %v", err)})
		return
	}

	var req AgentPublishRequest
	if err := json.Unmarshal(data, &req); err != nil {
		writeAgentResponse(conn, AgentPublishResponse{Error: fmt.Sprintf("malformed request: %v", err)})
		return
	}

	writeAgentResponse(conn, s.publish(req))
}

func (s *agentSocketServer) publish(req AgentPublishRequest) AgentPublishResponse {
	hasTo := req.To != ""
	hasBroadcast := req.Broadcast != ""
	if hasTo == hasBroadcast { // both set, or neither
		return AgentPublishResponse{Error: "exactly one of to/broadcast must be set"}
	}
	if s.publisher == nil {
		return AgentPublishResponse{Error: "pub/sub broker not connected"}
	}

	if hasBroadcast {
		if err := s.publisher.PublishBroadcast(s.namespace, req.Broadcast, req.Message); err != nil {
			return AgentPublishResponse{Error: err.Error()}
		}
		return AgentPublishResponse{OK: true}
	}

	// hasTo: split "namespace/name" the same way the CLI does
	// (internal/cli/root.go's splitNamespaceName) — a bare name (no "/")
	// defaults to this sandbox's OWN namespace, matching that same
	// default-namespace convention throughout the rest of muro.
	ns, name := splitToTarget(req.To)
	if ns == "" {
		ns = s.namespace
	}
	if ns != s.namespace {
		// SPEC.md §8's namespace scoping, enforced here: a sandbox may
		// only message another agent within its own namespace. Rejected
		// before ever reaching the publisher, not left to it.
		return AgentPublishResponse{Error: fmt.Sprintf("cannot publish to namespace %q from namespace %q — cross-namespace inbox delivery is not permitted", ns, s.namespace)}
	}
	if err := config.ValidSandboxName("name", name); err != nil {
		return AgentPublishResponse{Error: err.Error()}
	}
	if err := s.publisher.PublishInbox(ns, name, req.Message); err != nil {
		return AgentPublishResponse{Error: err.Error()}
	}
	return AgentPublishResponse{OK: true}
}

// splitToTarget parses a "namespace/name" or bare "name" To value — a
// package-local copy of internal/cli/root.go's splitNamespaceName (that
// package can't be imported here — internal/cli depends on internal/
// sandbox transitively via internal/control, not the other way around).
func splitToTarget(arg string) (namespace, name string) {
	if i := strings.IndexByte(arg, '/'); i >= 0 {
		return arg[:i], arg[i+1:]
	}
	return "", arg
}

func writeAgentResponse(conn net.Conn, resp AgentPublishResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}
	data = append(data, '\n')
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _ = conn.Write(data)
}

// readLineLimitedBytes reads one '\n'-terminated line from r, erroring out
// once more than maxSize bytes have been read without finding the
// delimiter rather than growing without bound — the same pattern internal/
// control/server.go's readLineLimited establishes, duplicated here (not
// imported — internal/control depends on internal/sandbox, not the
// reverse, so importing it here would be a cycle) rather than factored into
// a shared package for two small, independently-reasoned-about call sites.
func readLineLimitedBytes(r *bufio.Reader, maxSize int) ([]byte, error) {
	var line []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			return line, err
		}
		if b == '\n' {
			return line, nil
		}
		line = append(line, b)
		if len(line) > maxSize {
			return nil, fmt.Errorf("request exceeds maximum size of %d bytes", maxSize)
		}
	}
}
