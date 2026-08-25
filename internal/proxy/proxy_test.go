package proxy

import (
	"bufio"
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
