//go:build integration

package integration

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thomkin/muro/internal/config"
	"github.com/thomkin/muro/internal/proxy"
	"github.com/thomkin/muro/internal/sandbox"
	"github.com/thomkin/muro/internal/state"
)

// These tests exercise BwrapIsolator directly (not sandbox.Manager — its
// buildLaunchSpec only supports a bare single-executable Agent with no
// arguments at all, "real agent command construction is a later concern"
// per its own comment, so it can't drive an argv-bearing shell script; a
// pre-existing, separate limitation noted here rather than worked around).
//
// None of these tests capture sandboxed stdout — LaunchSpec has no such
// facility (matching the rest of this codebase; `muro logs` isn't wired to
// anything yet either). Verification instead follows the same pattern
// test/integration/bwrap_test.go already established for mount tests: the
// sandboxed shell script writes its result into a file on a read-write
// mount, and the test reads that file back from the host side afterward.

// waitForProxyUp waits for testProxyAddr to accept connections, but also
// watches errCh for an early ListenAndServe failure (most likely: the port
// is already taken by something else — a real murod running on this dev
// machine, or a leftover process from a previous run) and fails loudly and
// specifically instead of just timing out with a generic message. A
// silently-swallowed bind error here used to make a test's sandboxed script
// unknowingly talk to whatever else was already listening on the port,
// producing a confusing, misleading test failure instead of a clear one —
// confirmed as the actual root cause of exactly that failure during this
// project's own development, hence testProxyAddr being deliberately
// distinct from production's fixed port now too (see its doc comment).
func waitForProxyUp(t *testing.T, errCh <-chan error) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-errCh:
			t.Fatalf("test proxy failed to start on %s: %v", testProxyAddr, err)
		default:
		}
		conn, err := net.DialTimeout("tcp", testProxyAddr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("proxy at %s never came up (leftover process from a previous run using the port?)", testProxyAddr)
}

// startTestProxy starts a REAL internal/proxy.Server — the same package
// cmd/murod wires into production — on testProxyAddr, allowlisting
// allowURLs for every connection (SandboxKeyFunc overridden to a fixed
// key, the same override pattern internal/proxy's own unit tests already
// use; sandbox-identification-by-address has its own coverage there and
// in this package's Manager-based tests elsewhere — what these tests care
// about is the allowlist DECISION, not re-proving address resolution).
func startTestProxy(t *testing.T, allowURLs []string) {
	t.Helper()
	store := state.NewStore(t.TempDir() + "/state.json")
	srv := proxy.NewServer(store)
	const key = "fixed-test-sandbox"
	srv.SandboxKeyFunc = func(string) (string, bool) { return key, true }
	srv.SetAllowlist(key, allowURLs)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe(testProxyAddr) }()
	waitForProxyUp(t, errCh)
}

func newUpstreamStub(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fetchViaProxyScript builds a shell command that fetches targetURL
// through HTTP_PROXY (BwrapIsolator always injects this into every
// sandbox's env pointing at murod's/the test's proxy — bwrap.go) using
// nothing but /bin/sh + /dev/tcp (no curl binary inside the minimal
// shellMounts() sandbox), writing whatever it receives to outFile (must
// be inside a read-write mount). The leading sleep gives Stage 2's
// asynchronously-started slirp4netns bridge (network.go) a moment to
// finish before the request fires — the same documented, accepted race
// window as elsewhere in this package.
//
// The connect attempt is wrapped in a subshell `(...)`, not a bare `if
// exec ...; then`: bash's documented behavior for a bare `exec` with only
// redirections (no command) is to exit the *entire non-interactive shell
// immediately* if the redirection fails — bypassing `if`/`else` entirely,
// since there's no subshell boundary to contain the exit. Confirmed via a
// throwaway debug test during this work: an unwrapped `if exec
// 3<>/dev/tcp/...; then ... else ... fi` silently truncated the whole
// script on failure instead of taking the else branch. `timeout 2` bounds
// how long a *blocked* (not merely refused) destination can hang the
// script — nftables DROP (Stage 3, network.go) silently discards packets
// rather than sending a fast RST, so a filtered connection attempt hangs
// for a real TCP retransmission timeout otherwise, not a quick failure.
func fetchViaProxyScript(targetURL, outFile string) []string {
	// Dials 10.0.2.2 (slirpGatewayAddr, internal/sandbox/network.go), not
	// testProxyAddr's literal 127.0.0.1 host — matches exactly what
	// BwrapIsolator itself injects into HTTP_PROXY/HTTPS_PROXY now
	// (bwrap.go's buildArgs), confirmed empirically to be the address that
	// actually reaches the host's real proxy listener through the
	// slirp4netns bridge; 127.0.0.1 never leaves the sandbox's own private
	// loopback interface at all. The port MUST come from testProxyAddr, not
	// be hardcoded to production's 18080 — this bit the test suite directly
	// during this project's own development, once testProxyAddr was changed
	// to a distinct port to stop colliding with a real running murod: the
	// port here silently stayed pinned to the old value.
	_, port, err := net.SplitHostPort(testProxyAddr)
	if err != nil {
		panic("testProxyAddr must be a valid host:port: " + err.Error())
	}
	return []string{"/bin/sh", "-c", fmt.Sprintf(
		`sleep 0.3
timeout 2 sh -c 'exec 3<>/dev/tcp/10.0.2.2/%s && printf "GET %s HTTP/1.0\r\nHost: proxy\r\n\r\n" >&3 && cat <&3 > %s' || echo CONNECT_FAILED > %s`,
		port, targetURL, outFile, outFile,
	)}
}

// runScriptAndReadFile launches cmd via a real BwrapIsolator (Stage 2/3
// active, exactly as production does) with workDir bind-mounted rw at
// /work, waits for it to finish, and returns the content of workDir/out.
func runScriptAndReadFile(t *testing.T, cmd []string, workDir string) string {
	t.Helper()
	iso, err := sandbox.NewBwrapIsolator(testProxyAddr, t.TempDir())
	if err != nil {
		t.Skipf("bwrap isolator unavailable, skipping: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mounts := append(shellMounts(), config.Mount{Host: workDir, SandboxPath: "/work", Mode: "rw"})
	h, err := iso.Launch(ctx, sandbox.LaunchSpec{Mounts: mounts, Cmd: cmd})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(workDir, "out"))
	if err != nil {
		return ""
	}
	return string(data)
}

func TestSandboxReachesAllowedURL_Stage2And3(t *testing.T) {
	upstream := newUpstreamStub(t, "hello-from-upstream")
	startTestProxy(t, []string{upstream.URL})
	workDir := t.TempDir()

	out := runScriptAndReadFile(t, fetchViaProxyScript(upstream.URL, "/work/out"), workDir)
	if !strings.Contains(out, "hello-from-upstream") {
		t.Errorf("expected the allowed upstream's response through the proxy, got: %q", out)
	}
}

func TestSandboxDeniedURL_Stage3(t *testing.T) {
	upstream := newUpstreamStub(t, "should-not-see-this")
	startTestProxy(t, []string{"http://example.invalid"}) // upstream itself is NOT allowlisted
	workDir := t.TempDir()

	out := runScriptAndReadFile(t, fetchViaProxyScript(upstream.URL, "/work/out"), workDir)
	if strings.Contains(out, "should-not-see-this") {
		t.Errorf("the non-allowlisted upstream's response leaked through: %q", out)
	}
}

// TestSandboxOnlyReachesProxy_NoRawBypass proves Stage 3 specifically: a
// bridged sandbox (Stage 2 gives it a working route to the proxy) still
// cannot bypass HTTP_PROXY with a raw connection straight to a real
// external address — the actual point of nftables egress restriction
// (SPEC.md §1: OS-enforced, not the agent's cooperation with HTTP_PROXY).
// No proxy server involved at all here — this is pure network-layer
// reachability, deliberately isolated from allowlist-decision logic.
func TestSandboxOnlyReachesProxy_NoRawBypass(t *testing.T) {
	workDir := t.TempDir()
	script := []string{"/bin/sh", "-c",
		// nftables DROP (Stage 3) silently discards the SYN rather than
		// sending a fast RST, so a genuinely blocked connection attempt
		// hangs for a real TCP retransmission timeout rather than failing
		// quickly — timeout 2 bounds that instead of relying on the
		// surrounding Go context's much longer deadline.
		"sleep 0.3\ntimeout 2 sh -c 'exec 3<>/dev/tcp/1.1.1.1/80' && echo CONNECTED > /work/out || echo BLOCKED > /work/out"}

	out := runScriptAndReadFile(t, script, workDir)
	if !strings.Contains(out, "BLOCKED") {
		t.Errorf("expected a raw connection straight to an external address to be blocked even without any proxy allowlist involved, got: %q", out)
	}
}
