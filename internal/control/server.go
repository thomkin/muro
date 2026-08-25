package control

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/thomkin/muro/internal/config"
	"github.com/thomkin/muro/internal/sandbox"
	"github.com/thomkin/muro/internal/state"
)

// BrokerStatusChecker is the minimal surface Server needs from the pub/sub
// client to answer broker.status (DESIGN.md §8). Kept as a local interface
// so this package doesn't hard-depend on internal/pubsub's concrete type,
// the same decoupling internal/sandbox.Manager already uses for
// internal/proxy/internal/pubsub. internal/pubsub.Client doesn't implement
// this yet (a Status method wasn't part of that package's task) — Server
// accepts nil here and reports "not configured" rather than panicking
// until that's wired up.
type BrokerStatusChecker interface {
	Status() (connected bool, address string, lastErr error)
}

// Server is murod's control API: a Unix-socket JSON-line server dispatching
// onto a sandbox.Manager and a state.Store (DESIGN.md §7).
type Server struct {
	mgr    *sandbox.Manager
	store  *state.Store
	broker BrokerStatusChecker // may be nil

	// listenerMu guards listener: ListenAndServe sets it from its own
	// goroutine, and Close (called from a different goroutine — a test's
	// cleanup, or handleConn's own daemon.shutdown handler) reads it. Both
	// sides must go through this lock; a bare field here raced under
	// go test -race between net.Listen's write and Close's read.
	listenerMu sync.Mutex
	listener   net.Listener
}

// NewServer constructs a Server. broker may be nil.
func NewServer(mgr *sandbox.Manager, store *state.Store, broker BrokerStatusChecker) *Server {
	return &Server{mgr: mgr, store: store, broker: broker}
}

// ListenAndServe removes any stale socket file at socketPath, listens
// there, sets 0600 permissions (DESIGN.md §6: "owned by the invoking
// user"), and serves connections until Close is called (or a
// daemon.shutdown request is handled — see handleConn). Blocks until the
// listener is closed; returns nil on a clean shutdown.
func (s *Server) ListenAndServe(socketPath string) error {
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale control socket: %w", err)
	}

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen on control socket: %w", err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		ln.Close()
		return fmt.Errorf("chmod control socket: %w", err)
	}
	s.listenerMu.Lock()
	s.listener = ln
	s.listenerMu.Unlock()

	for {
		conn, err := ln.Accept()
		if err != nil {
			// Close() causes Accept to return an error; treat that as a
			// clean shutdown rather than propagating it.
			if isClosedListenerErr(err) {
				return nil
			}
			return fmt.Errorf("accept control connection: %w", err)
		}
		go s.handleConn(conn)
	}
}

// Close stops ListenAndServe's accept loop. Safe to call once; safe to
// call even if ListenAndServe was never started (no-op).
func (s *Server) Close() error {
	s.listenerMu.Lock()
	ln := s.listener
	s.listenerMu.Unlock()
	if ln == nil {
		return nil
	}
	return ln.Close()
}

func isClosedListenerErr(err error) bool {
	// net.Listener.Close causes a subsequent Accept to fail with an error
	// wrapping net.ErrClosed on modern Go; string-matching as a fallback
	// keeps this robust across stdlib versions without importing
	// internal error types.
	if err == nil {
		return false
	}
	return err.Error() == "use of closed network connection" ||
		(len(err.Error()) >= len(net.ErrClosed.Error()) &&
			containsSuffix(err.Error(), net.ErrClosed.Error()))
}

func containsSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

// handleConn owns one client connection for its whole lifetime: it reads
// newline-delimited Requests and writes newline-delimited Responses via a
// single bufio.Reader/net.Conn pair for as long as the connection is a
// plain request/response cycle, and — for sandbox.attach specifically —
// falls through to raw byte-stream passthrough on the SAME reader/conn
// after the JSON handshake (stream.go), so no bytes the bufio.Reader may
// have already buffered ahead of the newline are ever lost.
func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)

	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return // client disconnected (EOF) or a read error — either way, done
		}

		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			writeResponse(conn, Response{OK: false, Error: fmt.Sprintf("malformed request: %v", err)})
			continue
		}

		if req.Type == TypeSandboxAttach {
			s.handleAttach(conn, r, req)
			return // attach owns the connection for the rest of its life (stream.go)
		}

		resp := s.dispatch(req)
		if err := writeResponse(conn, resp); err != nil {
			return
		}
		if req.Type == TypeDaemonShutdown && resp.OK {
			// Give the response time to flush to the client before tearing
			// down the listener out from under any other in-flight
			// connections.
			go func() {
				time.Sleep(50 * time.Millisecond)
				_ = s.Close()
			}()
			return
		}
	}
}

func writeResponse(conn net.Conn, resp Response) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = conn.Write(data)
	return err
}

// defaultNS applies DESIGN.md §9's addressing default: a bare name (no
// explicit namespace) resolves within "default". sandbox.Manager's
// per-name methods (Reload/Restart/Stop/Attach) don't apply this default
// themselves — Manager.Run and its Selector matching do — so dispatch
// applies it uniformly here for every per-name request, matching what a
// bare `muro sandbox ...` invocation should mean.
func defaultNS(ns string) string {
	if ns == "" {
		return "default"
	}
	return ns
}

// dispatch routes one Request to the right Manager/Store call and turns
// the result (or error) into a Response. sandbox.attach is handled
// separately in handleConn/handleAttach since it isn't a single
// request/response exchange.
func (s *Server) dispatch(req Request) Response {
	switch req.Type {
	case TypeStatus:
		return s.handleStatus(req.Payload)
	case TypeSandboxShow:
		return s.handleSandboxShow(req.Payload)
	case TypeSandboxRun:
		return s.handleSandboxRun(req.Payload)
	case TypeSandboxUpdate:
		return s.handleSandboxUpdate(req.Payload)
	case TypeSandboxReload:
		return s.handleSandboxReload(req.Payload)
	case TypeSandboxRestart:
		return s.handleSandboxRestart(req.Payload)
	case TypeSandboxStop:
		return s.handleSandboxStop(req.Payload)
	case TypeLogs:
		return Response{OK: false, Error: "logs --follow is not implemented yet: log capture/storage isn't wired up"}
	case TypeBrokerStatus:
		return s.handleBrokerStatus()
	case TypeDaemonShutdown:
		return Response{OK: true, Payload: mustMarshal(DaemonShutdownResponse{OK: true})}
	default:
		return Response{OK: false, Error: fmt.Sprintf("unknown request type %q", req.Type)}
	}
}

func errResp(err error) Response {
	return Response{OK: false, Error: err.Error()}
}

func okResp(payload any) Response {
	return Response{OK: true, Payload: mustMarshal(payload)}
}

func mustMarshal(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		// Every payload type here is a plain struct of strings/ints/slices
		// built by this package — a marshal failure would be a bug in this
		// file, not a runtime condition callers need to handle.
		panic(fmt.Sprintf("control: marshal response payload: %v", err))
	}
	return data
}

func (s *Server) handleStatus(payload json.RawMessage) Response {
	var req StatusRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return errResp(fmt.Errorf("bad status payload: %w", err))
	}
	sbs := s.store.List(req.Namespace)
	views := make([]*SandboxView, 0, len(sbs))
	for _, sb := range sbs {
		views = append(views, toView(sb))
	}
	return okResp(StatusResponse{Sandboxes: views})
}

func (s *Server) handleSandboxShow(payload json.RawMessage) Response {
	var req SandboxShowRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return errResp(fmt.Errorf("bad sandbox.show payload: %w", err))
	}
	ns := defaultNS(req.Namespace)
	sb, ok := s.store.Get(ns, req.Name)
	if !ok {
		return errResp(fmt.Errorf("sandbox %s/%s not found", ns, req.Name))
	}
	return okResp(toView(sb))
}

func (s *Server) handleSandboxRun(payload json.RawMessage) Response {
	var req SandboxRunRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return errResp(fmt.Errorf("bad sandbox.run payload: %w", err))
	}

	profile, err := config.LoadProfile(req.Profile)
	if err != nil {
		return errResp(fmt.Errorf("load profile %q: %w", req.Profile, err))
	}
	if req.Agent != "" {
		profile.Agent = req.Agent
	}

	sb, err := s.mgr.Run(profile, req.Name, req.Namespace)
	if err != nil {
		return errResp(err)
	}
	return okResp(toView(sb))
}

func (s *Server) handleSandboxUpdate(payload json.RawMessage) Response {
	var req SandboxUpdateRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return errResp(fmt.Errorf("bad sandbox.update payload: %w", err))
	}

	sel := sandbox.Selector{
		Name:      req.Selector.Name,
		Namespace: req.Namespace,
		Profile:   req.Selector.Profile,
		All:       req.Selector.All,
	}
	mounts := make([]config.Mount, 0, len(req.Mounts))
	for _, m := range req.Mounts {
		mounts = append(mounts, config.Mount{Host: m.Host, SandboxPath: m.SandboxPath, Mode: m.Mode})
	}
	delta := sandbox.ConfigDelta{
		AddMounts: mounts,
		AllowURLs: req.AllowURLs,
		DenyURLs:  req.DenyURLs,
	}

	results, err := s.mgr.Update(sel, delta)
	if err != nil {
		return errResp(err)
	}
	views := make([]UpdateResultView, 0, len(results))
	for _, r := range results {
		views = append(views, UpdateResultView{Namespace: r.Namespace, Name: r.Name, Applied: r.Applied})
	}
	return okResp(SandboxUpdateResponse{Results: views})
}

func (s *Server) handleSandboxReload(payload json.RawMessage) Response {
	var req SandboxReloadRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return errResp(fmt.Errorf("bad sandbox.reload payload: %w", err))
	}
	ns := defaultNS(req.Namespace)
	if err := s.mgr.Reload(ns, req.Name); err != nil {
		return errResp(err)
	}
	// Manager.Reload doesn't itself report whether the pending change went
	// live — derive it the same way DESIGN.md §6.3 defines "applied": the
	// sandbox is no longer StateReloadPending.
	applied := true
	if sb, ok := s.store.Get(ns, req.Name); ok {
		applied = sb.State != state.StateReloadPending
	}
	return okResp(SandboxReloadResponse{Applied: applied})
}

func (s *Server) handleSandboxRestart(payload json.RawMessage) Response {
	var req SandboxRestartRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return errResp(fmt.Errorf("bad sandbox.restart payload: %w", err))
	}
	if err := s.mgr.Restart(defaultNS(req.Namespace), req.Name); err != nil {
		return errResp(err)
	}
	return okResp(SandboxRestartResponse{OK: true})
}

func (s *Server) handleSandboxStop(payload json.RawMessage) Response {
	var req SandboxStopRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return errResp(fmt.Errorf("bad sandbox.stop payload: %w", err))
	}
	if err := s.mgr.Stop(defaultNS(req.Namespace), req.Name); err != nil {
		return errResp(err)
	}
	return okResp(SandboxStopResponse{OK: true})
}

func (s *Server) handleBrokerStatus() Response {
	if s.broker == nil {
		return okResp(BrokerStatusResponse{Connected: false, LastError: "broker not configured"})
	}
	connected, address, lastErr := s.broker.Status()
	resp := BrokerStatusResponse{Connected: connected, Address: address}
	if lastErr != nil {
		resp.LastError = lastErr.Error()
	}
	return okResp(resp)
}

func toView(sb *state.Sandbox) *SandboxView {
	mounts := make([]MountView, 0, len(sb.Mounts))
	for _, m := range sb.Mounts {
		mounts = append(mounts, MountView{Host: m.Host, SandboxPath: m.SandboxPath, Mode: m.Mode})
	}
	tools := make([]ToolView, 0, len(sb.Tools))
	for _, t := range sb.Tools {
		tools = append(tools, ToolView{Host: t.Host, As: t.As})
	}
	return &SandboxView{
		ID:            sb.ID,
		Name:          sb.Name,
		Namespace:     sb.Namespace,
		Profile:       sb.Profile,
		Agent:         sb.Agent,
		PID:           sb.PID,
		State:         string(sb.State),
		StartedAt:     sb.StartedAt.Format(time.RFC3339),
		Mounts:        mounts,
		Tools:         tools,
		AllowURLs:     sb.AllowURLs,
		RestartPolicy: sb.RestartPolicy,
		RestartCount:  sb.RestartCount,
	}
}
