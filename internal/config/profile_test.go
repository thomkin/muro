package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveLoadProfile_RoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	p := &Profile{
		Name:  "claude-default",
		Agent: "claude",
		Mounts: []Mount{
			{Host: "~/projects/myrepo", SandboxPath: "/workspace", Mode: "rw"},
		},
		Tools: []Tool{
			{Host: "/usr/bin/git", As: "git"},
			{Host: "~/.muro/toolchains/claude-default/bin", As: "*"},
		},
		AllowURLs:     []string{"https://api.anthropic.com", "https://github.com"},
		DenyURLs:      []string{},
		Env:           map[string]string{},
		RestartPolicy: "never",
	}

	if err := SaveProfile(p); err != nil {
		t.Fatalf("SaveProfile() error: %v", err)
	}

	dir, err := ProfilesDir()
	if err != nil {
		t.Fatalf("ProfilesDir() error: %v", err)
	}
	wantPath := filepath.Join(dir, "claude-default.json")
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("expected profile file at %s: %v", wantPath, err)
	}

	// No leftover temp files after a successful save.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("leftover temp file after SaveProfile: %s", e.Name())
		}
	}

	got, err := LoadProfile("claude-default")
	if err != nil {
		t.Fatalf("LoadProfile() error: %v", err)
	}
	if got.Name != p.Name || got.Agent != p.Agent {
		t.Errorf("LoadProfile() = %+v, want name/agent to match %+v", got, p)
	}
	if len(got.Mounts) != 1 || got.Mounts[0].SandboxPath != "/workspace" {
		t.Errorf("Mounts round-trip mismatch: %+v", got.Mounts)
	}
	if len(got.Tools) != 2 || got.Tools[1].As != "*" {
		t.Errorf("Tools round-trip mismatch: %+v", got.Tools)
	}
	if len(got.AllowURLs) != 2 {
		t.Errorf("AllowURLs round-trip mismatch: %+v", got.AllowURLs)
	}

	// Confirm the file on disk is valid, complete JSON (the atomic
	// write-temp+rename should never leave a partial file behind).
	raw, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var roundTripped Profile
	if err := json.Unmarshal(raw, &roundTripped); err != nil {
		t.Errorf("saved profile file is not valid JSON: %v", err)
	}
}

func TestLoadProfile_DefaultsRestartPolicy(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	p := &Profile{Name: "no-policy", Agent: "gemini"}
	if err := SaveProfile(p); err != nil {
		t.Fatalf("SaveProfile() error: %v", err)
	}

	got, err := LoadProfile("no-policy")
	if err != nil {
		t.Fatalf("LoadProfile() error: %v", err)
	}
	if got.RestartPolicy != "never" {
		t.Errorf("RestartPolicy = %q, want default %q", got.RestartPolicy, "never")
	}
}

func TestSaveProfile_RequiresName(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := SaveProfile(&Profile{}); err == nil {
		t.Error("expected an error saving a profile with no name")
	}
}

// TestSaveProfile_RejectsPathTraversalName and TestLoadProfile_RejectsPathTraversalName
// cover the same bug class as SECURITY_REVIEW.md finding #2
// (SandboxLogPath's unsanitized namespace/name concatenation) found present
// here too during that fix's review: profilePath built
// filepath.Join(dir, name+".json") with no validation, so a name containing
// "../" could escape ProfilesDir entirely.
func TestSaveProfile_RejectsPathTraversalName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	err := SaveProfile(&Profile{Name: "../../etc/evil"})
	if err == nil {
		t.Fatal("expected an error saving a profile with a path-traversal name")
	}

	// Confirm nothing was actually written outside dir.
	if _, statErr := os.Stat(filepath.Join(dir, "..", "..", "etc", "evil.json")); statErr == nil {
		t.Fatal("SaveProfile wrote a file outside ProfilesDir despite returning an error")
	}
}

func TestLoadProfile_RejectsPathTraversalName(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := LoadProfile("../../etc/passwd"); err == nil {
		t.Fatal("expected an error loading a profile with a path-traversal name")
	}
}

func TestListProfiles(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if names, err := ListProfiles(); err != nil || len(names) != 0 {
		t.Fatalf("ListProfiles() on empty dir = %v, %v; want empty, nil", names, err)
	}

	for _, name := range []string{"a", "b", "c"} {
		if err := SaveProfile(&Profile{Name: name}); err != nil {
			t.Fatalf("SaveProfile(%q): %v", name, err)
		}
	}

	names, err := ListProfiles()
	if err != nil {
		t.Fatalf("ListProfiles() error: %v", err)
	}
	if len(names) != 3 {
		t.Fatalf("ListProfiles() = %v, want 3 entries", names)
	}
}

func TestLoadProfile_MissingFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := LoadProfile("does-not-exist"); err == nil {
		t.Error("expected an error loading a nonexistent profile")
	}
}
