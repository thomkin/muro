package proxy

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/thomkin/muro/internal/state"
)

// hopHeaders are stripped when forwarding a request/response, per RFC 7230
// §6.1 — they're meaningful only for one hop of the connection, not for the
// proxied destination.
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Transfer-Encoding",
	"TE", "Trailer", "Upgrade", "Proxy-Authenticate", "Proxy-Authorization",
}

// Server is murod's embedded URL-allowlist proxy (DESIGN.md §6.2): plain
// HTTP is filtered by the full request URL; HTTPS/CONNECT is filtered by
// destination host+port only (never terminated, never path-inspected) —
// see handleCONNECT's doc comment for exactly how. Default policy is
// deny-all: an unidentified sandbox, or one with no registered Allowlist,
// gets nothing.
type Server struct {
	store *state.Store

	mu    sync.RWMutex
	rules map[string]*Allowlist // keyed by sandbox key (whatever internal/sandbox.Manager passes to SetAllowlist — the sandbox's internal ID, not "namespace/name"; see RegisterSandboxAddr)

	// addrIndex maps a sandbox's assigned outbound loopback address (e.g.
	// "127.0.0.5" — internal/sandbox's Stage 2 networking, one distinct
	// 127.0.0.0/8 address per sandbox) to the same sandbox key rules is
	// keyed by. This is what makes the default SandboxKeyFunc below a real
	// implementation rather than the placeholder it started as: once a
	// sandbox's bridged traffic arrives with a distinguishable source
	// address, resolving "which sandbox is this" is just this lookup.
	addrIndex map[string]string

	// SandboxKeyFunc resolves which sandbox an incoming connection belongs
	// to, given its remote address string (net/http's r.RemoteAddr, or a
	// hijacked net.Conn's RemoteAddr().String()). The default (set by
	// NewServer) strips the port and looks the host up in addrIndex,
	// populated via RegisterSandboxAddr — DESIGN.md §6.2's "murod resolves
	// the calling sandbox by which loopback/namespace a connection arrived
	// on," now real rather than a placeholder. Still overridable directly
	// (tests set it to a fixed key without needing a real bridge).
	SandboxKeyFunc func(remoteAddr string) (key string, ok bool)

	logger *slog.Logger

	// ReadHeaderTimeout/ReadTimeout/WriteTimeout/IdleTimeout configure the
	// http.Server ListenAndServe constructs — the slow-loris mitigation
	// (SECURITY_REVIEW.md finding #3): without them, a sandboxed process
	// can open many connections and send headers (or a CONNECT line) at a
	// trickle or never, tying up a goroutine/fd per connection indefinitely
	// and denying network access to every other sandbox this proxy serves.
	// Exported as fields (not hardcoded in ListenAndServe) so tests can
	// shrink them to keep a slow-loris regression test fast; NewServer sets
	// production-sane defaults. ReadHeaderTimeout is what actually matters
	// for the pre-hijack slow-loris case; ReadTimeout/WriteTimeout mainly
	// protect the plain-HTTP (non-CONNECT) path — a hijacked CONNECT tunnel
	// has any inherited deadline explicitly cleared in handleCONNECT right
	// after Hijack, specifically so a long-lived legitimate tunnel is never
	// at risk of being killed by these once relaying is underway (see that
	// function's comment; net/http's Hijacker docs say a hijacked
	// connection "may have read or write deadlines already set... it is
	// the caller's responsibility to set or clear those as needed").
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
}

// NewServer creates a Server backed by store for denied-request logging
// (DESIGN.md §5's denied-URL event log). Register a per-sandbox allowlist
// with SetAllowlist before traffic for that sandbox is expected to pass,
// and its bridged address with RegisterSandboxAddr so SandboxKeyFunc's
// default implementation can actually identify it.
func NewServer(store *state.Store) *Server {
	s := &Server{
		store:     store,
		rules:     make(map[string]*Allowlist),
		addrIndex: make(map[string]string),
		logger:    slog.Default(),

		// Production defaults (SECURITY_REVIEW.md finding #3).
		// ReadHeaderTimeout: generous for any legitimate client (headers
		// arrive in one write, essentially instantly) but bounds a
		// deliberately slow/idle one. ReadTimeout/WriteTimeout: only
		// meaningfully apply to the plain-HTTP path in practice (see
		// handleCONNECT's deadline-clearing). IdleTimeout: bounds an idle
		// keep-alive connection waiting for its next request; does not
		// apply to an actively-relaying hijacked tunnel, which net/http no
		// longer tracks as "idle" (or at all) once hijacked.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	s.SandboxKeyFunc = s.resolveSandboxKeyByAddr
	return s
}

// resolveSandboxKeyByAddr is the real default SandboxKeyFunc: strip the
// port from remoteAddr and look the resulting host up in addrIndex. An
// unrecognized address (no sandbox ever registered it, or none was
// registered at all — e.g. in a test using a fake Isolator) correctly
// returns ok=false, which the deny-all default then refuses, same as
// before this was wired up for real.
func (s *Server) resolveSandboxKeyByAddr(remoteAddr string) (string, bool) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr // remoteAddr with no port at all — try it as-is
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	key, ok := s.addrIndex[host]
	return key, ok
}

// RegisterSandboxAddr records that addr (a sandbox's Stage 2 outbound
// loopback address, e.g. "127.0.0.5") belongs to sandboxKey, so the
// default SandboxKeyFunc can resolve future connections from that address
// back to the right allowlist. This is the ProxyUpdater method
// internal/sandbox.Manager calls (by duck typing against its own local
// ProxyUpdater interface, which this package does not import) whenever a
// Handle exposes a network address — see internal/sandbox/network.go's
// networkAddrProvider.
func (s *Server) RegisterSandboxAddr(sandboxKey, addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addrIndex[addr] = sandboxKey
}

// ListenAndServe starts the proxy on addr and blocks. addr is expected to
// be a loopback address per sandbox network namespace (DESIGN.md §6.2);
// this package itself is transport-agnostic about that.
func (s *Server) ListenAndServe(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("proxy: listen %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler:           http.HandlerFunc(s.handle),
		ReadHeaderTimeout: s.ReadHeaderTimeout,
		ReadTimeout:       s.ReadTimeout,
		WriteTimeout:      s.WriteTimeout,
		IdleTimeout:       s.IdleTimeout,
	}
	return srv.Serve(ln)
}

// SetAllowlist hot-swaps the allowlist for sandboxKey (DESIGN.md §6.3/§9 —
// always live, no restart needed). This is the ProxyUpdater method
// internal/sandbox.Manager calls by duck typing against its own local
// ProxyUpdater interface — this package does not import internal/sandbox.
func (s *Server) SetAllowlist(sandboxKey string, allowURLs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.rules[sandboxKey]; ok {
		a.Swap(allowURLs)
		return
	}
	s.rules[sandboxKey] = NewAllowlist(allowURLs)
}

func (s *Server) allowlistFor(sandboxKey string) (*Allowlist, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.rules[sandboxKey]
	return a, ok
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		s.handleCONNECT(w, r)
		return
	}
	s.handleHTTP(w, r)
}

// handleHTTP proxies a plain-HTTP request, matched against the calling
// sandbox's allowlist by the full request URL (scheme+host+port+path) —
// DESIGN.md §6.2: unlike HTTPS, the proxy sees the whole thing in
// cleartext, so it enforces the whole thing.
func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	key, ok := s.SandboxKeyFunc(r.RemoteAddr)

	fullURL := r.URL.String()
	if r.URL.Scheme == "" {
		fullURL = "http://" + r.Host + r.URL.RequestURI()
	}

	var allowed bool
	if ok {
		if a, found := s.allowlistFor(key); found {
			allowed = a.AllowsHTTP(fullURL)
		}
	}
	if !allowed {
		s.deny(w, key, r.Host, fullURL, http.StatusForbidden)
		return
	}

	s.forwardHTTP(w, r)
}

func (s *Server) deny(w http.ResponseWriter, sandboxKey, host, url string, status int) {
	ns, name := splitKey(sandboxKey)
	if s.store != nil {
		_ = s.store.RecordDenied(ns, name, host, url)
	}
	s.logger.Info("proxy: denied", "sandbox", sandboxKey, "host", host, "url", url)
	http.Error(w, "muro: destination not in allowlist", status)
}

// splitKey splits a "namespace/name" sandbox key. An empty or malformed
// key (SandboxKeyFunc returned ok=false, so key is "") splits to
// ("", "") — RecordDenied still gets called with empty fields rather than
// skipped, so an unidentifiable-sandbox denial is still visible in the
// event log rather than silently dropped.
func splitKey(key string) (namespace, name string) {
	i := strings.IndexByte(key, '/')
	if i < 0 {
		return "", key
	}
	return key[:i], key[i+1:]
}

// forwardHTTP relays an allowed plain-HTTP request to its real destination
// and copies the response back, stripping hop-by-hop headers both ways.
func (s *Server) forwardHTTP(w http.ResponseWriter, r *http.Request) {
	outReq := r.Clone(r.Context())
	outReq.RequestURI = "" // required by http.Transport for outbound requests
	if outReq.URL.Scheme == "" {
		outReq.URL.Scheme = "http"
	}
	if outReq.URL.Host == "" {
		outReq.URL.Host = r.Host
	}
	stripHopHeaders(outReq.Header)

	resp, err := http.DefaultTransport.RoundTrip(outReq)
	if err != nil {
		http.Error(w, "muro: upstream request failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	stripHopHeaders(resp.Header)
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func stripHopHeaders(h http.Header) {
	for _, hh := range hopHeaders {
		h.Del(hh)
	}
}

// handleCONNECT proxies an HTTPS (or any CONNECT-tunneled) request.
// DESIGN.md §6.2: TLS is never terminated, so the request path is never
// seen — enforcement is by destination host+port only. Two checks apply:
//
//  1. The CONNECT target itself (e.g. "api.anthropic.com:443" from the
//     request line, in r.Host) is checked against the allowlist BEFORE
//     anything is accepted. If it's not allowed, this responds with a
//     plain HTTP 403 — no hijack, no TLS involved at all. This is both the
//     common case and the cheap, easily-testable path.
//  2. If the target is allowed, the connection is hijacked and a
//     "200 Connection Established" is sent (required before the client
//     will start its TLS handshake over the tunnel). The proxy then peeks
//     the first bytes the client sends — the TLS ClientHello — and
//     cross-checks its SNI hostname against the allowlist too, as a
//     defense against a client claiming one CONNECT target while actually
//     presenting a different (disallowed) SNI to the real destination. If
//     SNI extraction fails or times out (non-TLS traffic, or an unusually
//     slow/fragmented ClientHello), this degrades gracefully and proceeds
//     using the already-validated CONNECT target rather than hanging or
//     dropping legitimate traffic.
//
// Once both checks pass, any bytes already peeked are replayed to the real
// backend first, then the two connections are relayed byte-for-byte in
// both directions, untouched, until either side closes — DESIGN.md §6.2's
// "no CA, no decryption, just pass it through."
func (s *Server) handleCONNECT(w http.ResponseWriter, r *http.Request) {
	key, ok := s.SandboxKeyFunc(r.RemoteAddr)
	target := r.Host // CONNECT's request-target, e.g. "api.anthropic.com:443"

	var allowed bool
	var allowlist *Allowlist
	if ok {
		if a, found := s.allowlistFor(key); found {
			allowlist = a
			allowed = a.AllowsHost(target)
		}
	}
	if !allowed {
		s.deny(w, key, target, target, http.StatusForbidden)
		return
	}

	hj, hijackable := w.(http.Hijacker)
	if !hijackable {
		http.Error(w, "muro: proxy does not support CONNECT on this listener", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	defer clientConn.Close()

	// A hijacked connection may already have a read/write deadline armed by
	// the server (ReadTimeout/WriteTimeout, if set) from before Hijack was
	// called — net/http's Hijacker docs are explicit that clearing it is
	// the caller's responsibility. Without this, a legitimate long-lived
	// CONNECT tunnel (a real, ongoing HTTPS session to an allowed
	// destination) would be silently killed mid-relay once that deadline
	// elapsed, entirely unrelated to whether the tunnel is still actively
	// in use.
	_ = clientConn.SetReadDeadline(time.Time{})
	_ = clientConn.SetWriteDeadline(time.Time{})

	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}

	peeked := s.peekAndCrossCheckSNI(clientConn, allowlist, key, target)
	if peeked == nil {
		return // SNI cross-check denied; connection already closed and denial logged
	}

	backendConn, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		return
	}
	defer backendConn.Close()

	if len(peeked) > 0 {
		if _, err := backendConn.Write(peeked); err != nil {
			return
		}
	}

	relay(clientConn, backendConn)
}

// peekAndCrossCheckSNI reads up to a small deadline/cap worth of bytes from
// conn looking for a complete TLS ClientHello, and if one is found, checks
// its SNI hostname (at the CONNECT target's port) against allowlist. It
// returns the bytes read so far (to be replayed to the real backend) on
// success or graceful degradation, or nil if the SNI cross-check explicitly
// denied the connection — in which case the connection has already been
// closed by the caller and the denial has already been recorded.
func (s *Server) peekAndCrossCheckSNI(conn net.Conn, allowlist *Allowlist, sandboxKey, target string) []byte {
	const maxPeek = 16 * 1024
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	defer conn.SetReadDeadline(time.Time{})

	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for len(buf) < maxPeek {
		n, err := conn.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if host, perr := ExtractSNI(buf); perr == nil {
				_, port, splitErr := net.SplitHostPort(target)
				if splitErr != nil {
					port = "443"
				}
				if !allowlist.AllowsHost(host + ":" + port) {
					ns, name := splitKey(sandboxKey)
					if s.store != nil {
						_ = s.store.RecordDenied(ns, name, host, "https://"+host+"/")
					}
					s.logger.Info("proxy: denied (SNI mismatch)", "sandbox", sandboxKey, "connect_target", target, "sni", host)
					return nil
				}
				return buf
			}
		}
		if err != nil {
			// Timeout, EOF, or bytes that didn't parse as a ClientHello
			// within the cap — degrade gracefully and proceed using the
			// already-validated CONNECT target rather than dropping
			// legitimate non-TLS-over-CONNECT traffic.
			return buf
		}
	}
	return buf
}

// relay copies bytes in both directions between a and b until either side
// closes, untouched — DESIGN.md §6.2's "no CA, no decryption."
func relay(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
}
