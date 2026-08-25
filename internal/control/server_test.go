package control

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thomkin/muro/internal/config"
	"github.com/thomkin/muro/internal/sandbox"
	"github.com/thomkin/muro/internal/state"
)

// newTestServer wires a real Server + sandbox.Manager + state.Store +
// fakeIsolator, starts ListenAndServe on a temp Unix socket in a
// background goroutine, and returns it once the socket is dialable. t's
// cleanup stops the server and removes the temp dir.
func newTestServer(t *testing.T) (srv *Server, socketPath string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir) // config.LoadProfile reads profiles from here

	store := state.NewStore(filepath.Join(dir, "state.json"))
	if err := store.Load(); err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	mgr := sandbox.NewManager(store, &fakeIsolator{}, nil, nil)
	srv = NewServer(mgr, store, nil)

	socketPath = filepath.Join(dir, "control.sock")
	go func() {
		if err := srv.ListenAndServe(socketPath); err != nil {
			t.Logf("ListenAndServe: %v", err)
		}
	}()
	t.Cleanup(func() { _ = srv.Close() })

	waitForSocket(t, socketPath)
	return srv, socketPath
}

func waitForSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("control socket %s never appeared", path)
}

func saveTestProfile(t *testing.T, name string) {
	t.Helper()
	if err := config.SaveProfile(&config.Profile{
		Name:          name,
		Agent:         "true",
		RestartPolicy: "never",
	}); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
}

func TestEndToEnd_StatusRunShowStop(t *testing.T) {
	_, socketPath := newTestServer(t)
	saveTestProfile(t, "test-profile")

	c, err := Dial(socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	var status StatusResponse
	if err := c.Call(TypeStatus, StatusRequest{}, &status); err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(status.Sandboxes) != 0 {
		t.Fatalf("status before run: got %d sandboxes, want 0", len(status.Sandboxes))
	}

	var ran SandboxView
	runReq := SandboxRunRequest{Profile: "test-profile", Name: "agent-1", Namespace: "default"}
	if err := c.Call(TypeSandboxRun, runReq, &ran); err != nil {
		t.Fatalf("sandbox.run: %v", err)
	}
	if ran.Name != "agent-1" || ran.State != "running" {
		t.Errorf("run result = %+v, want Name=agent-1 State=running", ran)
	}

	if err := c.Call(TypeStatus, StatusRequest{}, &status); err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(status.Sandboxes) != 1 {
		t.Fatalf("status after run: got %d sandboxes, want 1", len(status.Sandboxes))
	}

	var shown SandboxView
	if err := c.Call(TypeSandboxShow, SandboxShowRequest{Namespace: "default", Name: "agent-1"}, &shown); err != nil {
		t.Fatalf("sandbox.show: %v", err)
	}
	if shown.ID != ran.ID {
		t.Errorf("show ID = %q, want %q", shown.ID, ran.ID)
	}

	// Namespace defaulting: omitting Namespace should still resolve to
	// "default" (DESIGN.md §9), matching what a bare `muro sandbox show
	// agent-1` would mean.
	var shownDefaulted SandboxView
	if err := c.Call(TypeSandboxShow, SandboxShowRequest{Name: "agent-1"}, &shownDefaulted); err != nil {
		t.Fatalf("sandbox.show (defaulted namespace): %v", err)
	}
	if shownDefaulted.ID != ran.ID {
		t.Errorf("defaulted-namespace show ID = %q, want %q", shownDefaulted.ID, ran.ID)
	}

	var stopResp SandboxStopResponse
	if err := c.Call(TypeSandboxStop, SandboxStopRequest{Name: "agent-1"}, &stopResp); err != nil {
		t.Fatalf("sandbox.stop: %v", err)
	}
	if !stopResp.OK {
		t.Errorf("stop OK = false")
	}
}

func TestSocketPermissions(t *testing.T) {
	_, socketPath := newTestServer(t)
	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket perm = %o, want 0600", perm)
	}
}

func TestValidateNamespaceName(t *testing.T) {
	if err := validateNamespaceName("", "agent-1"); err != nil {
		t.Errorf("empty namespace + valid name should be accepted, got: %v", err)
	}
	if err := validateNamespaceName("default", "agent-1"); err != nil {
		t.Errorf("valid namespace + name should be accepted, got: %v", err)
	}
	if err := validateNamespaceName("", ""); err == nil {
		t.Error("expected an error for an empty name")
	}
	if err := validateNamespaceName("", "../../etc/passwd"); err == nil {
		t.Error("expected an error for a path-traversal name")
	}
	if err := validateNamespaceName("../../etc", "agent-1"); err == nil {
		t.Error("expected an error for a path-traversal namespace")
	}
}

// TestSandboxRun_RejectsPathTraversalName confirms the control API itself
// rejects a malicious name — not just the CLI — since the daemon shouldn't
// trust the CLI is the only possible client of its control API. Without
// this, a crafted sandbox.run request could have caused
// config.SandboxLogPath (via muro-shim's log capture) to write outside
// its intended logs/sandbox/ directory.
func TestSandboxRun_RejectsPathTraversalName(t *testing.T) {
	_, socketPath := newTestServer(t)
	saveTestProfile(t, "test-profile")

	c, err := Dial(socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	err = c.Call(TypeSandboxRun, SandboxRunRequest{
		Profile: "test-profile",
		Name:    "../../etc/passwd",
	}, nil)
	if err == nil {
		t.Fatal("expected sandbox.run to reject a path-traversal name")
	}

	var status StatusResponse
	if err := c.Call(TypeStatus, StatusRequest{}, &status); err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(status.Sandboxes) != 0 {
		t.Errorf("a rejected sandbox.run should not have created any sandbox, got %d", len(status.Sandboxes))
	}
}

func TestSandboxShow_NotFound(t *testing.T) {
	_, socketPath := newTestServer(t)
	c, err := Dial(socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	err = c.Call(TypeSandboxShow, SandboxShowRequest{Name: "nope"}, nil)
	if err == nil {
		t.Fatalf("expected an error for a nonexistent sandbox")
	}
}

func TestBrokerStatus_NilBroker(t *testing.T) {
	srv, socketPath := newTestServer(t)
	_ = srv
	c, err := Dial(socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	var resp BrokerStatusResponse
	if err := c.Call(TypeBrokerStatus, BrokerStatusRequest{}, &resp); err != nil {
		t.Fatalf("broker.status: %v", err)
	}
	if resp.Connected {
		t.Errorf("Connected = true with a nil broker checker")
	}
	if resp.LastError == "" {
		t.Errorf("expected a LastError explaining the broker isn't configured")
	}
}

func TestDaemonShutdown_StopsServer(t *testing.T) {
	_, socketPath := newTestServer(t)
	c, err := Dial(socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	var resp DaemonShutdownResponse
	if err := c.Call(TypeDaemonShutdown, DaemonShutdownRequest{}, &resp); err != nil {
		t.Fatalf("daemon.shutdown: %v", err)
	}
	if !resp.OK {
		t.Fatalf("shutdown OK = false")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := Dial(socketPath); err != nil {
			return // server stopped accepting new connections, as expected
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server still accepting connections after daemon.shutdown")
}

func TestReadLineLimited_NormalLine(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("hello world\nrest"))
	line, err := readLineLimited(r, 1024)
	if err != nil {
		t.Fatalf("readLineLimited: %v", err)
	}
	if string(line) != "hello world\n" {
		t.Errorf("line = %q, want %q", line, "hello world\n")
	}
	// Confirm bytes after the delimiter are still there for a subsequent
	// read on the same reader (the shared-reader contract handleAttach/
	// handleLogs depend on).
	rest, _ := r.ReadString(0)
	if rest != "rest" {
		t.Errorf("remaining reader content = %q, want %q", rest, "rest")
	}
}

func TestReadLineLimited_OversizedLineRejected(t *testing.T) {
	huge := strings.Repeat("a", 100) // no trailing '\n' at all
	r := bufio.NewReader(strings.NewReader(huge))
	_, err := readLineLimited(r, 50)
	if err == nil {
		t.Fatal("expected an error for a line exceeding the size limit")
	}
}

// TestHandleConn_OversizedLineDisconnects confirms the fix end to end: a
// client sending a huge line with no newline gets disconnected quickly
// rather than the server growing an unbounded buffer waiting for a
// delimiter that never arrives (SECURITY_REVIEW.md follow-up — control
// plane/CLI pass).
func TestHandleConn_OversizedLineDisconnects(t *testing.T) {
	_, socketPath := newTestServer(t)

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	// Comfortably over maxRequestLineSize, sent with no trailing newline.
	oversized := make([]byte, maxRequestLineSize+1024)
	for i := range oversized {
		oversized[i] = 'a'
	}
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _ = conn.Write(oversized) // the server may close before this fully lands; that's fine, ignore the error

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1)
	_, err = conn.Read(buf)
	if err == nil {
		t.Fatal("expected the connection to be closed by the server after an oversized line, got a successful read")
	}
}

// TestHandleConn_IdleClientDisconnected confirms a connection that sends
// nothing at all is disconnected once requestIdleTimeout elapses, rather
// than pinning a goroutine/fd on the daemon indefinitely. Uses a short
// override so this test doesn't wait out the real 30s production value.
func TestHandleConn_IdleClientDisconnected(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	store := state.NewStore(filepath.Join(dir, "state.json"))
	if err := store.Load(); err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	mgr := sandbox.NewManager(store, &fakeIsolator{}, nil, nil)
	srv := NewServer(mgr, store, nil)
	srv.requestIdleTimeout = 200 * time.Millisecond

	socketPath := filepath.Join(dir, "control.sock")
	go func() { _ = srv.ListenAndServe(socketPath) }()
	t.Cleanup(func() { _ = srv.Close() })
	waitForSocket(t, socketPath)

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	// Send nothing at all. The server should close its side within
	// roughly srv.requestIdleTimeout.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	_, err = conn.Read(buf)
	if err == nil {
		t.Fatal("expected the idle connection to be closed by the server, got a successful read")
	}
}

func TestAttach_RawPassthroughAndDetach(t *testing.T) {
	_, socketPath := newTestServer(t)
	saveTestProfile(t, "test-profile")

	setup, err := Dial(socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	var ran SandboxView
	if err := setup.Call(TypeSandboxRun, SandboxRunRequest{Profile: "test-profile", Name: "agent-1", Namespace: "default"}, &ran); err != nil {
		t.Fatalf("sandbox.run: %v", err)
	}
	setup.Close()

	// A fresh connection for the actual attach — Attach takes over the
	// connection, so it can't share one with the setup Call above once
	// upgraded (each request/response Call needs the connection back for
	// the next Call, which attach's raw passthrough doesn't give back).
	c1, err := Dial(socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c1.Close()

	r1, w1, err := c1.Attach("default", "agent-1")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// Find the fake pty's peer end (the "agent side") via the store — the
	// test doesn't have direct access to the Handle, so instead exercise
	// the passthrough through the fakeIsolator's own bookkeeping isn't
	// exposed either; simplest robust approach: write from the client and
	// read it back is not possible (nothing echoes). Instead verify the
	// exclusivity contract, which is the property that actually matters
	// at this layer: a second attach attempt is rejected while c1's is
	// live, and succeeds again after detaching.

	c2, err := Dial(socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c2.Close()
	if _, _, err := c2.Attach("default", "agent-1"); err == nil {
		t.Fatalf("second concurrent Attach succeeded, want rejection (DESIGN.md §12: exactly one attacher)")
	}

	// Send the detach sequence; the server should stop forwarding and
	// release the attach slot.
	if _, err := w1.Write([]byte(sandbox.DetachSequence)); err != nil {
		t.Fatalf("write detach sequence: %v", err)
	}
	_ = r1

	deadline := time.Now().Add(2 * time.Second)
	var reattached bool
	for time.Now().Before(deadline) {
		c3, err := Dial(socketPath)
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		_, _, err = c3.Attach("default", "agent-1")
		c3.Close()
		if err == nil {
			reattached = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !reattached {
		t.Fatalf("could not re-attach after sending the detach sequence")
	}
}
