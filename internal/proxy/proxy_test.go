package proxy

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thomkin/muro/internal/state"
)

func newTestStore(t *testing.T) *state.Store {
	t.Helper()
	return state.NewStore(filepath.Join(t.TempDir(), "state.json"))
}

func TestHandleHTTP_Allowed(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("backend-ok"))
	}))
	defer backend.Close()

	store := newTestStore(t)
	srv := NewServer(store)
	srv.SandboxKeyFunc = func(string) (string, bool) { return "default/test", true }
	srv.SetAllowlist("default/test", []string{backend.URL})

	proxyTS := httptest.NewServer(http.HandlerFunc(srv.handle))
	defer proxyTS.Close()

	client := clientThroughProxy(t, proxyTS.URL)

	resp, err := client.Get(backend.URL)
	if err != nil {
		t.Fatalf("allowed request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != "backend-ok" {
		t.Errorf("body = %q, want %q", body, "backend-ok")
	}
}

func TestHandleHTTP_Denied(t *testing.T) {
	store := newTestStore(t)
	srv := NewServer(store)
	srv.SandboxKeyFunc = func(string) (string, bool) { return "default/test", true }
	srv.SetAllowlist("default/test", []string{"http://only-this-host.example.com"})

	proxyTS := httptest.NewServer(http.HandlerFunc(srv.handle))
	defer proxyTS.Close()

	client := clientThroughProxy(t, proxyTS.URL)

	resp, err := client.Get("http://not-allowed.example.com/somewhere")
	if err != nil {
		t.Fatalf("denied request transport error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestHandleHTTP_UnidentifiedSandboxDenied(t *testing.T) {
	store := newTestStore(t)
	srv := NewServer(store) // default SandboxKeyFunc always returns ok=false

	proxyTS := httptest.NewServer(http.HandlerFunc(srv.handle))
	defer proxyTS.Close()

	client := clientThroughProxy(t, proxyTS.URL)
	resp, err := client.Get("http://anywhere.example.com/")
	if err != nil {
		t.Fatalf("request transport error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for an unidentified sandbox", resp.StatusCode)
	}
}

func clientThroughProxy(t *testing.T, proxyURLStr string) *http.Client {
	t.Helper()
	proxyURL, err := url.Parse(proxyURLStr)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
}

func TestHandleCONNECT_Denied(t *testing.T) {
	store := newTestStore(t)
	srv := NewServer(store)
	srv.SandboxKeyFunc = func(string) (string, bool) { return "default/test", true }
	srv.SetAllowlist("default/test", []string{"https://allowed.example.com"})

	proxyTS := httptest.NewServer(http.HandlerFunc(srv.handle))
	defer proxyTS.Close()

	conn, err := net.Dial("tcp", proxyTS.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	fmt.Fprintf(conn, "CONNECT disallowed.example.com:443 HTTP/1.1\r\nHost: disallowed.example.com:443\r\n\r\n")

	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	statusLine, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.Contains(statusLine, "403") {
		t.Errorf("status line = %q, want 403", statusLine)
	}
}

func TestHandleCONNECT_Allowed(t *testing.T) {
	// A real listener so the proxy can actually dial a backend once it
	// decides to allow the tunnel.
	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer backendLn.Close()
	go func() {
		for {
			c, err := backendLn.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	_, backendPort, err := net.SplitHostPort(backendLn.Addr().String())
	if err != nil {
		t.Fatalf("split backend addr: %v", err)
	}
	backendHostPort := "127.0.0.1:" + backendPort

	store := newTestStore(t)
	srv := NewServer(store)
	srv.SandboxKeyFunc = func(string) (string, bool) { return "default/test", true }
	srv.SetAllowlist("default/test", []string{"https://127.0.0.1:" + backendPort})

	proxyTS := httptest.NewServer(http.HandlerFunc(srv.handle))
	defer proxyTS.Close()

	conn, err := net.Dial("tcp", proxyTS.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", backendHostPort, backendHostPort)

	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	statusLine, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.Contains(statusLine, "200") {
		t.Errorf("status line = %q, want 200 Connection Established", statusLine)
	}
	// Deliberately send nothing further and let defer conn.Close() run —
	// the server's SNI peek should see an immediate EOF and degrade
	// gracefully rather than waiting out its full deadline.
}

// TestHandleCONNECT_SNIMismatchDenied exercises the domain-fronting defense
// (SECURITY_REVIEW.md finding #5): a CONNECT target that IS allowlisted (so
// the tunnel gets hijacked), but where the client then presents a TLS
// ClientHello with a DIFFERENT SNI hostname than the CONNECT target. The
// connection must be closed, not relayed — this is what stops a client
// from claiming an allowed destination in the CONNECT line while actually
// routing to a disallowed one via SNI once the tunnel is established.
func TestHandleCONNECT_SNIMismatchDenied(t *testing.T) {
	// A real listener so the proxy can actually dial a backend once the
	// CONNECT-target check passes — the SNI cross-check only runs AFTER
	// that first check already succeeded.
	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer backendLn.Close()
	go func() {
		for {
			c, err := backendLn.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	_, backendPort, err := net.SplitHostPort(backendLn.Addr().String())
	if err != nil {
		t.Fatalf("split backend addr: %v", err)
	}
	backendHostPort := "127.0.0.1:" + backendPort

	store := newTestStore(t)
	srv := NewServer(store)
	srv.SandboxKeyFunc = func(string) (string, bool) { return "default/test", true }
	// Allows the CONNECT target itself (what lets the tunnel get hijacked
	// at all) — deliberately does NOT allow "evil.example.com", the SNI the
	// client will present once the tunnel is up.
	srv.SetAllowlist("default/test", []string{"https://127.0.0.1:" + backendPort})

	proxyTS := httptest.NewServer(http.HandlerFunc(srv.handle))
	defer proxyTS.Close()

	conn, err := net.Dial("tcp", proxyTS.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", backendHostPort, backendHostPort)

	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	statusLine, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.Contains(statusLine, "200") {
		t.Fatalf("status line = %q, want 200 Connection Established", statusLine)
	}

	// Present a real TLS ClientHello (same construction sni_test.go uses)
	// with an SNI hostname that deliberately differs from the CONNECT
	// target — domain fronting.
	go func() {
		conf := &tls.Config{ServerName: "evil.example.com", InsecureSkipVerify: true} //nolint:gosec // test only, never a real handshake
		_ = tls.Client(conn, conf).Handshake()                                        // expected to fail/hang; we only need the ClientHello it sends
	}()

	// The server must close the connection rather than relay — a
	// subsequent read must fail (EOF/reset), not return data or hang.
	deadline := time.Now().Add(5 * time.Second)
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) {
		if err := conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
			t.Fatalf("SetReadDeadline: %v", err)
		}
		n, rerr := conn.Read(buf)
		if n > 0 {
			t.Fatalf("received %d bytes after presenting a mismatched SNI — domain-fronting defense did not close the tunnel", n)
		}
		if rerr != nil {
			if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
				continue // our own short per-iteration deadline, not a server response yet
			}
			return // connection closed by the server — SNI mismatch correctly denied
		}
	}
	t.Fatal("connection was still open 5s after a mismatched SNI was presented — expected the server to close it")
}

// TestListenAndServe_ReadHeaderTimeoutDisconnectsSlowClient exercises the
// slow-loris mitigation (SECURITY_REVIEW.md finding #3): a client that
// opens a connection and never completes its request line/headers must be
// disconnected within a bounded time, not held open indefinitely — which,
// before this fix, it was (no timeouts were configured on the proxy's
// http.Server at all).
func TestListenAndServe_ReadHeaderTimeoutDisconnectsSlowClient(t *testing.T) {
	store := newTestStore(t)
	srv := NewServer(store)
	srv.ReadHeaderTimeout = 200 * time.Millisecond // shrink from the 10s production default so this test is fast

	// httptest.NewUnstartedServer + assigning srv's timeout to Config
	// before Start avoids a listen/close/re-listen port race while still
	// exercising the exact mechanism ListenAndServe wires up (an
	// http.Server with ReadHeaderTimeout set to Server.ReadHeaderTimeout).
	proxyTS := httptest.NewUnstartedServer(http.HandlerFunc(srv.handle))
	proxyTS.Config.ReadHeaderTimeout = srv.ReadHeaderTimeout
	proxyTS.Start()
	defer proxyTS.Close()

	conn, err := net.Dial("tcp", proxyTS.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	// Send nothing at all — a slow-loris client that never completes its
	// request line/headers.
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 1)
	_, err = conn.Read(buf)
	if err == nil {
		t.Fatal("expected the connection to be closed by ReadHeaderTimeout, but a read succeeded")
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("connection was still open after 2s — ReadHeaderTimeout (200ms) did not disconnect the idle client: %v", err)
	}
	// Any other error (io.EOF, connection reset, etc.) indicates the
	// server closed the connection, as expected.
}
