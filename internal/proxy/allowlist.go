package proxy

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
)

// Rule is one allowed network destination, parsed from a profile's
// allow_urls entry. HTTPS/CONNECT traffic is only ever matched by Host+Port
// (DESIGN.md §6.2/§14 — no TLS termination, so the path is never actually
// enforced for HTTPS even if a path happened to be present in the original
// URL string); PathPrefix is only consulted by AllowsHTTP for plain-HTTP
// requests.
type Rule struct {
	Scheme     string // "http", "https", or "" if the entry had no scheme (bare host)
	Host       string // lowercased hostname, no port
	Port       string // explicit or scheme-defaulted port; "" only when Scheme is also ""
	PathPrefix string // HTTP-only path-prefix match; "" matches any path
}

// Allowlist is a per-sandbox, hot-swappable set of Rules. Reads (the
// Allows* methods) are lock-free; Swap atomically replaces the whole
// ruleset (DESIGN.md §6.3/§9 hot-reload — no restart needed).
type Allowlist struct {
	rules atomic.Pointer[[]Rule]
}

// NewAllowlist parses urls (as they appear in a profile's allow_urls) into
// an Allowlist. Default policy is deny-all: an empty or nil urls list
// produces an Allowlist that allows nothing (DESIGN.md §6.2).
func NewAllowlist(urls []string) *Allowlist {
	a := &Allowlist{}
	a.Swap(urls)
	return a
}

// Swap atomically replaces the entire ruleset. A url entry that fails to
// parse is skipped rather than failing the whole allowlist — internal/config
// validation is the place that should catch a malformed allow_urls entry
// before it ever reaches a running sandbox; being lenient here just avoids
// one bad string taking down a sandbox's entire network access.
func (a *Allowlist) Swap(urls []string) {
	rules := make([]Rule, 0, len(urls))
	for _, u := range urls {
		r, err := parseRule(u)
		if err != nil {
			continue
		}
		rules = append(rules, r)
	}
	a.rules.Store(&rules)
}

func parseRule(raw string) (Rule, error) {
	if !strings.Contains(raw, "://") {
		host, port, err := splitHostPortMaybe(raw)
		if err != nil {
			return Rule{}, err
		}
		return Rule{Host: strings.ToLower(host), Port: port}, nil
	}

	u, err := url.Parse(raw)
	if err != nil {
		return Rule{}, fmt.Errorf("parse allow_urls entry %q: %w", raw, err)
	}
	if u.Host == "" {
		return Rule{}, fmt.Errorf("parse allow_urls entry %q: no host", raw)
	}
	host, port, err := splitHostPortMaybe(u.Host)
	if err != nil {
		return Rule{}, err
	}
	if port == "" {
		port = defaultPort(u.Scheme)
	}
	return Rule{
		Scheme:     u.Scheme,
		Host:       strings.ToLower(host),
		Port:       port,
		PathPrefix: u.Path,
	}, nil
}

func splitHostPortMaybe(hostport string) (host, port string, err error) {
	if strings.Contains(hostport, ":") {
		return net.SplitHostPort(hostport)
	}
	return hostport, "", nil
}

func defaultPort(scheme string) string {
	switch scheme {
	case "https":
		return "443"
	case "http":
		return "80"
	default:
		return ""
	}
}

// AllowsHTTP reports whether a plain-HTTP request to fullURL (an
// absolute-URI as seen on a proxy request line, e.g.
// "http://example.com/some/path") is allowed: full scheme+host+port+path
// match against the ruleset.
func (a *Allowlist) AllowsHTTP(fullURL string) bool {
	rules := a.rules.Load()
	if rules == nil {
		return false
	}

	u, err := url.Parse(fullURL)
	if err != nil || u.Host == "" {
		return false
	}
	host, port, err := splitHostPortMaybe(u.Host)
	if err != nil {
		return false
	}
	if port == "" {
		port = defaultPort(u.Scheme)
		if port == "" {
			port = "80"
		}
	}
	host = strings.ToLower(host)

	for _, r := range *rules {
		if r.Scheme != "" && r.Scheme != u.Scheme {
			continue
		}
		if r.Host != host {
			continue
		}
		if r.Port != "" && r.Port != port {
			continue
		}
		if r.PathPrefix != "" && !strings.HasPrefix(u.Path, r.PathPrefix) {
			continue
		}
		return true
	}
	return false
}

// AllowsHost reports whether a CONNECT/HTTPS request to hostport (e.g.
// "api.anthropic.com:443", or a bare host with no port) is allowed. Per
// DESIGN.md §6.2/§14, HTTPS is filtered by hostname+port only — no path or
// scheme matching is ever applied here, even for a rule that had a path or
// an "http://" scheme in the original profile entry.
func (a *Allowlist) AllowsHost(hostport string) bool {
	rules := a.rules.Load()
	if rules == nil {
		return false
	}

	host, port, err := splitHostPortMaybe(hostport)
	if err != nil {
		host, port = hostport, ""
	}
	if port == "" {
		port = "443"
	}
	host = strings.ToLower(host)

	for _, r := range *rules {
		if r.Host != host {
			continue
		}
		if r.Port != "" && r.Port != port {
			continue
		}
		return true
	}
	return false
}
