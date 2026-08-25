package proxy

import "testing"

func TestAllowsHTTP_FullURLMatch(t *testing.T) {
	a := NewAllowlist([]string{"http://example.com/api"})

	cases := []struct {
		url  string
		want bool
	}{
		{"http://example.com/api", true},
		{"http://example.com/api/sub", true},
		{"http://example.com/other", false},
		{"http://other.com/api", false},
		{"https://example.com/api", false}, // scheme mismatch
	}
	for _, c := range cases {
		if got := a.AllowsHTTP(c.url); got != c.want {
			t.Errorf("AllowsHTTP(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

func TestAllowsHTTP_HostOnlyRuleAllowsAnyPath(t *testing.T) {
	a := NewAllowlist([]string{"http://example.com"})

	if !a.AllowsHTTP("http://example.com/anything/at/all") {
		t.Error("host-only rule should allow any path")
	}
}

func TestAllowsHost_HostAndPortMatch(t *testing.T) {
	a := NewAllowlist([]string{"https://api.anthropic.com"})

	if !a.AllowsHost("api.anthropic.com:443") {
		t.Error("expected api.anthropic.com:443 to be allowed (default https port)")
	}
	if a.AllowsHost("api.anthropic.com:8443") {
		t.Error("expected a non-default port to be denied")
	}
	if a.AllowsHost("evil.com:443") {
		t.Error("expected a different host to be denied")
	}
}

func TestAllowsHost_IgnoresPathAndScheme(t *testing.T) {
	// DESIGN.md §6.2/§14: HTTPS is filtered by host+port only, even if the
	// original rule had a path or was declared as http://.
	a := NewAllowlist([]string{"http://example.com/some/path"})

	if !a.AllowsHost("example.com:80") {
		t.Error("AllowsHost must ignore PathPrefix entirely")
	}
}

func TestAllowsHost_BareHostNoScheme(t *testing.T) {
	a := NewAllowlist([]string{"example.com"})

	if !a.AllowsHost("example.com:443") {
		t.Error("bare host entry (no scheme) should match any port for AllowsHost")
	}
	if !a.AllowsHost("example.com") {
		t.Error("bare host entry should match a bare-host query defaulting to :443")
	}
}

func TestSwap_ReplacesRulesLive(t *testing.T) {
	a := NewAllowlist([]string{"https://old.example.com"})
	if !a.AllowsHost("old.example.com:443") {
		t.Fatal("precondition: old rule should be allowed before swap")
	}

	a.Swap([]string{"https://new.example.com"})

	if a.AllowsHost("old.example.com:443") {
		t.Error("old rule should no longer match after Swap")
	}
	if !a.AllowsHost("new.example.com:443") {
		t.Error("new rule should match after Swap")
	}
}

func TestNewAllowlist_EmptyDeniesEverything(t *testing.T) {
	a := NewAllowlist(nil)
	if a.AllowsHost("anything.com:443") {
		t.Error("empty allowlist must deny all HTTPS hosts")
	}
	if a.AllowsHTTP("http://anything.com/") {
		t.Error("empty allowlist must deny all HTTP requests")
	}
}

func TestParseRule_MalformedEntrySkipped(t *testing.T) {
	// A malformed entry (a scheme with no host at all) must not take down
	// the rest of the allowlist.
	a := NewAllowlist([]string{"https://good.example.com", "https://"})
	if !a.AllowsHost("good.example.com:443") {
		t.Error("well-formed entry should still be usable despite a malformed sibling")
	}
}
