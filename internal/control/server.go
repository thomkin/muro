package control

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/thomkin/muro/internal/config"
	"github.com/thomkin/muro/internal/sandbox"
	"github.com/thomkin/muro/internal/state"
	"github.com/thomkin/muro/internal/worktree"
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

	// requestIdleTimeout overrides the default requestIdleTimeout constant
	// (below) when non-zero — exists so a test can exercise real timeout
	// behavior (a connection that sends nothing gets disconnected) without
	// the test suite actually waiting out the production 30s value.
	requestIdleTimeout time.Duration
}

// NewServer constructs a Server. broker may be nil.
func NewServer(mgr *sandbox.Manager, store *state.Store, broker BrokerStatusChecker) *Server {
	return &Server{mgr: mgr, store: store, broker: broker}
}

func (s *Server) idleTimeout() time.Duration {
	if s.requestIdleTimeout > 0 {
		return s.requestIdleTimeout
	}
	return requestIdleTimeout
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

// maxRequestLineSize bounds a single request line before it's ever handed
// to json.Unmarshal — every real control API payload (profile paths, mount
// lists, a handful of strings) is at most a few KB; 4MiB is generous
// headroom for that while still bounding worst-case memory growth from a
// malformed or hostile line that never terminates in '\n'. bufio.Reader's
// own ReadBytes has no such limit — it grows its internal buffer without
// bound chasing the delimiter, which readLineLimited (below) exists to cap.
const maxRequestLineSize = 4 << 20

// requestIdleTimeout bounds how long handleConn's request loop will block
// waiting for a client to send its next request line. In this protocol's
// actual usage every real client (cmd/muro's Client.Call) sends exactly one
// request within milliseconds of connecting and then either reads its
// response or upgrades to a stream (attach/logs, which have their own,
// separately-reasoned-about blocking reads once the request line itself has
// been read — this deadline only ever governs waiting for THAT first line).
// Without this, a connection that's accepted but never sends anything (a
// stray/confused process, or a deliberately silent one) pins a goroutine and
// its underlying fd on this daemon indefinitely; 30s is far more than any
// legitimate client ever needs and still short enough that this can't
// meaningfully accumulate.
const requestIdleTimeout = 30 * time.Second

// readLineLimited reads one '\n'-terminated line from r, the same contract
// as bufio.Reader.ReadBytes('\n'), except it errors out once more than
// maxSize bytes have been read without finding the delimiter, rather than
// growing without bound. Uses r.ReadByte() in a loop (not ReadSlice/
// ReadBytes) specifically so this stays on the SAME *bufio.Reader instance
// handleAttach/handleLogs need to keep reading from afterward (any bytes
// buffered ahead of the delimiter must not be lost) — switching to
// bufio.Scanner for its built-in Buffer() size cap would have meant a
// second, disconnected buffer with no clean way to hand back what it read
// ahead.
func readLineLimited(r *bufio.Reader, maxSize int) ([]byte, error) {
	var line []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			return line, err
		}
		line = append(line, b)
		if b == '\n' {
			return line, nil
		}
		if len(line) > maxSize {
			return nil, fmt.Errorf("request line exceeds maximum size of %d bytes", maxSize)
		}
	}
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
		_ = conn.SetReadDeadline(time.Now().Add(s.idleTimeout()))
		line, err := readLineLimited(r, maxRequestLineSize)
		if err != nil {
			return // client disconnected (EOF), idle timeout, oversized line, or a read error — either way, done
		}
		// Reads inside handleAttach/handleLogs govern their own blocking
		// behavior once a stream upgrade begins (an attach session
		// legitimately waits indefinitely for the next keystroke) — clear
		// the deadline before handing off so this timeout can't fire mid
		// session.
		_ = conn.SetReadDeadline(time.Time{})

		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			writeResponse(conn, Response{OK: false, Error: fmt.Sprintf("malformed request: %v", err)})
			continue
		}

		if req.Type == TypeSandboxAttach {
			s.handleAttach(conn, r, req)
			return // attach owns the connection for the rest of its life (stream.go)
		}
		if req.Type == TypeLogs {
			s.handleLogs(conn, req)
			return // logs owns the connection for the rest of its life too (stream.go) — read-only, but still a stream, not a single response
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
	case TypeSandboxDelete:
		return s.handleSandboxDelete(req.Payload)
	case TypeSandboxMerge:
		return s.handleSandboxMerge(req.Payload)
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

// validateNamespaceName rejects a namespace/name pair that could be used
// to construct a path outside its intended directory (most directly
// config.SandboxLogPath) — applied uniformly at every request handler that
// accepts these from a client, not just the one (sandbox.run, via
// Manager.Run -> the Store -> SandboxLogPath) that actually creates new
// state; the daemon shouldn't trust the CLI is the only possible client of
// its control API (DESIGN.md's own framing of this as a general protocol).
// namespace may be empty (defaultNS's "default" applies later); name is
// always required.
func validateNamespaceName(namespace, name string) error {
	if err := config.ValidSandboxName("name", name); err != nil {
		return err
	}
	if namespace != "" {
		if err := config.ValidSandboxName("namespace", namespace); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) handleSandboxShow(payload json.RawMessage) Response {
	var req SandboxShowRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return errResp(fmt.Errorf("bad sandbox.show payload: %w", err))
	}
	if err := validateNamespaceName(req.Namespace, req.Name); err != nil {
		return errResp(err)
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

	if err := validateNamespaceName(req.Namespace, req.Name); err != nil {
		return errResp(err)
	}

	profile, err := config.LoadProfile(req.Profile)
	if err != nil {
		return errResp(fmt.Errorf("load profile %q: %w", req.Profile, err))
	}
	if req.Agent != "" {
		profile.Agent = req.Agent
	}
	if len(req.AgentArgs) > 0 {
		profile.AgentArgs = req.AgentArgs
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
	if req.Selector.Name != "" {
		if err := config.ValidSandboxName("name", req.Selector.Name); err != nil {
			return errResp(err)
		}
	}
	if req.Namespace != "" {
		if err := config.ValidSandboxName("namespace", req.Namespace); err != nil {
			return errResp(err)
		}
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
	if err := validateNamespaceName(req.Namespace, req.Name); err != nil {
		return errResp(err)
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
	if err := validateNamespaceName(req.Namespace, req.Name); err != nil {
		return errResp(err)
	}
	if err := s.mgr.Restart(defaultNS(req.Namespace), req.Name, req.FromProfile); err != nil {
		return errResp(err)
	}
	return okResp(SandboxRestartResponse{OK: true})
}

func (s *Server) handleSandboxStop(payload json.RawMessage) Response {
	var req SandboxStopRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return errResp(fmt.Errorf("bad sandbox.stop payload: %w", err))
	}
	if err := validateNamespaceName(req.Namespace, req.Name); err != nil {
		return errResp(err)
	}
	if err := s.mgr.Stop(defaultNS(req.Namespace), req.Name); err != nil {
		return errResp(err)
	}
	return okResp(SandboxStopResponse{OK: true})
}

func (s *Server) handleSandboxDelete(payload json.RawMessage) Response {
	var req SandboxDeleteRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return errResp(fmt.Errorf("bad sandbox.delete payload: %w", err))
	}
	if err := validateNamespaceName(req.Namespace, req.Name); err != nil {
		return errResp(err)
	}
	if err := s.mgr.Delete(defaultNS(req.Namespace), req.Name, req.DiscardWorktrees); err != nil {
		return errResp(err)
	}
	return okResp(SandboxDeleteResponse{OK: true})
}

func (s *Server) handleSandboxMerge(payload json.RawMessage) Response {
	var req SandboxMergeRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return errResp(fmt.Errorf("bad sandbox.merge payload: %w", err))
	}
	if err := validateNamespaceName(req.Namespace, req.Name); err != nil {
		return errResp(err)
	}
	if req.MountPath == "" {
		return errResp(fmt.Errorf("sandbox.merge: mount_path is required"))
	}
	commit, err := s.mgr.Merge(defaultNS(req.Namespace), req.Name, req.MountPath, req.Message)
	if err != nil {
		return errResp(err)
	}
	return okResp(SandboxMergeResponse{OK: true, Commit: commit})
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
	var worktrees []WorktreeView
	for _, wt := range sb.Worktrees {
		has, _ := worktree.HasUnmergedCommits(context.Background(), wt.Host, wt.BaseBranch) // best-effort: a check failure just shows as false, doesn't hide the sandbox
		worktrees = append(worktrees, WorktreeView{
			MountPath:          wt.MountPath,
			Host:               wt.Host,
			Branch:             wt.Branch,
			BaseBranch:         wt.BaseBranch,
			HasUnmergedCommits: has,
		})
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
		Worktrees:     worktrees,
	}
}
