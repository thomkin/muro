package cli

import "testing"

func TestSplitMountFlag_ValidROAndRW(t *testing.T) {
	host, sandboxPath, mode, err := splitMountFlag("/home/me/repo:/workspace:rw")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "/home/me/repo" || sandboxPath != "/workspace" || mode != "rw" {
		t.Errorf("got (%q, %q, %q)", host, sandboxPath, mode)
	}

	_, _, mode, err = splitMountFlag("/a:/b:ro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mode != "ro" {
		t.Errorf("mode = %q, want ro", mode)
	}
}

func TestSplitMountFlag_WrongPartCountRejected(t *testing.T) {
	if _, _, _, err := splitMountFlag("/a:/b"); err == nil {
		t.Error("expected error for missing mode component")
	}
	if _, _, _, err := splitMountFlag("/a:/b:rw:extra"); err == nil {
		t.Error("expected error for too many components")
	}
}

func TestSplitMountFlag_InvalidModeRejected(t *testing.T) {
	if _, _, _, err := splitMountFlag("/a:/b:readwrite"); err == nil {
		t.Error("expected error for invalid mode")
	}
}

func TestSplitToolFlag_ExplicitAs(t *testing.T) {
	host, as, err := splitToolFlag("/usr/bin/git:git")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "/usr/bin/git" || as != "git" {
		t.Errorf("got (%q, %q)", host, as)
	}
}

func TestSplitToolFlag_DefaultsAsToBasename(t *testing.T) {
	host, as, err := splitToolFlag("/usr/bin/node")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "/usr/bin/node" || as != "node" {
		t.Errorf("got (%q, %q), want (/usr/bin/node, node)", host, as)
	}
}

func TestSplitToolFlag_WildcardDirectory(t *testing.T) {
	host, as, err := splitToolFlag("~/.muro/toolchains/claude-default/bin:*")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if as != "*" {
		t.Errorf("as = %q, want *", as)
	}
	_ = host
}

func TestSplitToolFlag_MissingHostRejected(t *testing.T) {
	if _, _, err := splitToolFlag(":git"); err == nil {
		t.Error("expected error for missing host path")
	}
}

func TestParseMountFlags_MultipleValid(t *testing.T) {
	out, err := parseMountFlags([]string{"/a:/w1:ro", "/b:/w2:rw"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d entries, want 2", len(out))
	}
	if out[0].Mode != "ro" || out[1].Mode != "rw" {
		t.Errorf("modes = %q, %q", out[0].Mode, out[1].Mode)
	}
}

func TestParseMountFlags_PropagatesFirstError(t *testing.T) {
	_, err := parseMountFlags([]string{"/a:/w1:ro", "malformed"})
	if err == nil {
		t.Error("expected error to propagate from a malformed entry")
	}
}

func TestParseMountFlagsConfig_MatchesControlVariant(t *testing.T) {
	out, err := parseMountFlagsConfig([]string{"/a:/w:rw"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 || out[0].Host != "/a" || out[0].SandboxPath != "/w" || out[0].Mode != "rw" {
		t.Errorf("got %+v", out)
	}
}

func TestParseToolFlagsConfig_Multiple(t *testing.T) {
	out, err := parseToolFlagsConfig([]string{"/usr/bin/git", "/usr/bin/node:node"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 2 || out[0].As != "git" || out[1].As != "node" {
		t.Errorf("got %+v", out)
	}
}
