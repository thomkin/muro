package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveLoadProfile_RoundTrip(t *testing.T) {
	t.Setenv("MURO_PROFILES_DIR", t.TempDir())

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
	bundleDir := filepath.Join(dir, "claude-default")
	wantPath := filepath.Join(bundleDir, "profile.json")
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("expected profile file at %s: %v", wantPath, err)
	}

	// No leftover temp files after a successful save.
	entries, err := os.ReadDir(bundleDir)
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
	t.Setenv("MURO_PROFILES_DIR", t.TempDir())

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
	t.Setenv("MURO_PROFILES_DIR", t.TempDir())
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
	t.Setenv("MURO_PROFILES_DIR", dir)

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
	t.Setenv("MURO_PROFILES_DIR", t.TempDir())
	if _, err := LoadProfile("../../etc/passwd"); err == nil {
		t.Fatal("expected an error loading a profile with a path-traversal name")
	}
}

func TestListProfiles(t *testing.T) {
	t.Setenv("MURO_PROFILES_DIR", t.TempDir())

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
	t.Setenv("MURO_PROFILES_DIR", t.TempDir())
	if _, err := LoadProfile("does-not-exist"); err == nil {
		t.Error("expected an error loading a nonexistent profile")
	}
}

func TestLoadProfile_ExtendsMergesListFieldsBaseFirst(t *testing.T) {
	t.Setenv("MURO_PROFILES_DIR", t.TempDir())
	base := &Profile{
		Name:      "base",
		Mounts:    []Mount{{Host: "/usr", SandboxPath: "/usr", Mode: "ro"}},
		Tools:     []Tool{{Host: "/usr/bin/git", As: "git"}},
		AllowURLs: []string{"https://api.anthropic.com"},
	}
	if err := SaveProfile(base); err != nil {
		t.Fatal(err)
	}
	child := &Profile{
		Name:      "child",
		Extends:   "base",
		Mounts:    []Mount{{Host: "/tmp", SandboxPath: "/workspace", Mode: "rw"}},
		AllowURLs: []string{"https://github.com"},
	}
	if err := SaveProfile(child); err != nil {
		t.Fatal(err)
	}

	resolved, err := LoadProfile("child")
	if err != nil {
		t.Fatalf("LoadProfile error: %v", err)
	}
	if len(resolved.Mounts) != 2 || resolved.Mounts[0].SandboxPath != "/usr" || resolved.Mounts[1].SandboxPath != "/workspace" {
		t.Errorf("Mounts = %+v, want base's entry first, then child's", resolved.Mounts)
	}
	if len(resolved.Tools) != 1 || resolved.Tools[0].As != "git" {
		t.Errorf("Tools = %+v, want inherited from base", resolved.Tools)
	}
	if len(resolved.AllowURLs) != 2 || resolved.AllowURLs[0] != "https://api.anthropic.com" || resolved.AllowURLs[1] != "https://github.com" {
		t.Errorf("AllowURLs = %v, want base's then child's", resolved.AllowURLs)
	}
	if resolved.Name != "child" {
		t.Errorf("Name = %q, want the child's own name preserved after merge", resolved.Name)
	}
}

func TestLoadProfile_ExtendsChildOverridesLaterMountAtSameSandboxPath(t *testing.T) {
	// A child mount at the same sandbox_path as a base mount must come
	// AFTER the base's in the merged list, since that's what makes it
	// actually win — bwrap resolves the LATER of two mounts at the same
	// path as the effective one.
	t.Setenv("MURO_PROFILES_DIR", t.TempDir())
	if err := SaveProfile(&Profile{
		Name:   "base",
		Mounts: []Mount{{Host: "/host/base/workspace", SandboxPath: "/workspace", Mode: "ro"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := SaveProfile(&Profile{
		Name:    "child",
		Extends: "base",
		Mounts:  []Mount{{Host: "/host/child/workspace", SandboxPath: "/workspace", Mode: "rw"}},
	}); err != nil {
		t.Fatal(err)
	}

	resolved, err := LoadProfile("child")
	if err != nil {
		t.Fatalf("LoadProfile error: %v", err)
	}
	if len(resolved.Mounts) != 2 {
		t.Fatalf("Mounts = %+v, want both entries present (last one wins at launch time, not deduplicated here)", resolved.Mounts)
	}
	last := resolved.Mounts[len(resolved.Mounts)-1]
	if last.Host != "/host/child/workspace" || last.Mode != "rw" {
		t.Errorf("last mount = %+v, want the child's own override last in the list", last)
	}
}

func TestLoadProfile_ExtendsSingleValueFieldsChildWinsWhenSet(t *testing.T) {
	t.Setenv("MURO_PROFILES_DIR", t.TempDir())
	if err := SaveProfile(&Profile{
		Name:          "base",
		Agent:         "/base/agent",
		AgentArgs:     []string{"--base-flag"},
		RestartPolicy: "always",
		WorkDir:       "/base-workdir",
	}); err != nil {
		t.Fatal(err)
	}
	if err := SaveProfile(&Profile{
		Name:      "child",
		Extends:   "base",
		Agent:     "/child/agent",
		AgentArgs: []string{"--child-flag"},
		WorkDir:   "/child-workdir",
		// RestartPolicy deliberately left unset — must inherit "always" from base.
	}); err != nil {
		t.Fatal(err)
	}

	resolved, err := LoadProfile("child")
	if err != nil {
		t.Fatalf("LoadProfile error: %v", err)
	}
	if resolved.Agent != "/child/agent" {
		t.Errorf("Agent = %q, want child's own value to win", resolved.Agent)
	}
	if len(resolved.AgentArgs) != 1 || resolved.AgentArgs[0] != "--child-flag" {
		t.Errorf("AgentArgs = %v, want child's own value (full replace, not merged)", resolved.AgentArgs)
	}
	if resolved.RestartPolicy != "always" {
		t.Errorf("RestartPolicy = %q, want inherited \"always\" from base since child left it unset", resolved.RestartPolicy)
	}
	if resolved.WorkDir != "/child-workdir" {
		t.Errorf("WorkDir = %q, want child's own value to win", resolved.WorkDir)
	}
}

func TestLoadProfile_ExtendsWorkDirInheritedWhenChildLeavesItUnset(t *testing.T) {
	t.Setenv("MURO_PROFILES_DIR", t.TempDir())
	if err := SaveProfile(&Profile{Name: "base", WorkDir: "/workspace"}); err != nil {
		t.Fatal(err)
	}
	if err := SaveProfile(&Profile{Name: "child", Extends: "base"}); err != nil {
		t.Fatal(err)
	}

	resolved, err := LoadProfile("child")
	if err != nil {
		t.Fatalf("LoadProfile error: %v", err)
	}
	if resolved.WorkDir != "/workspace" {
		t.Errorf("WorkDir = %q, want inherited \"/workspace\" from base since child left it unset", resolved.WorkDir)
	}
}

func TestLoadProfile_ExtendsEnvMapMergedChildWinsOnCollision(t *testing.T) {
	t.Setenv("MURO_PROFILES_DIR", t.TempDir())
	if err := SaveProfile(&Profile{
		Name: "base",
		Env:  map[string]string{"SHARED": "base-value", "BASE_ONLY": "b"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := SaveProfile(&Profile{
		Name:    "child",
		Extends: "base",
		Env:     map[string]string{"SHARED": "child-value", "CHILD_ONLY": "c"},
	}); err != nil {
		t.Fatal(err)
	}

	resolved, err := LoadProfile("child")
	if err != nil {
		t.Fatalf("LoadProfile error: %v", err)
	}
	want := map[string]string{"SHARED": "child-value", "BASE_ONLY": "b", "CHILD_ONLY": "c"}
	for k, v := range want {
		if resolved.Env[k] != v {
			t.Errorf("Env[%q] = %q, want %q (full env: %v)", k, resolved.Env[k], v, resolved.Env)
		}
	}
}

func TestLoadProfile_ExtendsAudioIsORed(t *testing.T) {
	t.Setenv("MURO_PROFILES_DIR", t.TempDir())
	if err := SaveProfile(&Profile{Name: "base", Audio: true}); err != nil {
		t.Fatal(err)
	}
	if err := SaveProfile(&Profile{Name: "child", Extends: "base"}); err != nil {
		t.Fatal(err)
	}
	resolved, err := LoadProfile("child")
	if err != nil {
		t.Fatalf("LoadProfile error: %v", err)
	}
	if !resolved.Audio {
		t.Error("expected Audio to be inherited (true) from base")
	}
}

func TestLoadProfile_ExtendsInstructionsChildWinsWhenSet(t *testing.T) {
	t.Setenv("MURO_PROFILES_DIR", t.TempDir())
	if err := SaveProfile(&Profile{Name: "base", Instructions: "/base/AGENT.md"}); err != nil {
		t.Fatal(err)
	}
	if err := SaveProfile(&Profile{Name: "child", Extends: "base"}); err != nil {
		t.Fatal(err)
	}
	resolved, err := LoadProfile("child")
	if err != nil {
		t.Fatalf("LoadProfile error: %v", err)
	}
	if resolved.Instructions != "/base/AGENT.md" {
		t.Errorf("Instructions = %q, want inherited from base", resolved.Instructions)
	}

	if err := SaveProfile(&Profile{Name: "child2", Extends: "base", Instructions: "/child/AGENT.md"}); err != nil {
		t.Fatal(err)
	}
	resolved2, err := LoadProfile("child2")
	if err != nil {
		t.Fatalf("LoadProfile error: %v", err)
	}
	if resolved2.Instructions != "/child/AGENT.md" {
		t.Errorf("Instructions = %q, want child's own value to win over base's", resolved2.Instructions)
	}
}

func TestLoadProfile_ExtendsSkillsConcatenated(t *testing.T) {
	t.Setenv("MURO_PROFILES_DIR", t.TempDir())
	if err := SaveProfile(&Profile{Name: "base", Skills: []string{"/base/skill-a.md"}}); err != nil {
		t.Fatal(err)
	}
	if err := SaveProfile(&Profile{Name: "child", Extends: "base", Skills: []string{"/child/skill-b.md"}}); err != nil {
		t.Fatal(err)
	}
	resolved, err := LoadProfile("child")
	if err != nil {
		t.Fatalf("LoadProfile error: %v", err)
	}
	if len(resolved.Skills) != 2 || resolved.Skills[0] != "/base/skill-a.md" || resolved.Skills[1] != "/child/skill-b.md" {
		t.Errorf("Skills = %v, want base's then child's", resolved.Skills)
	}
}

func TestLoadProfile_ExtendsCycleRejected(t *testing.T) {
	t.Setenv("MURO_PROFILES_DIR", t.TempDir())
	if err := SaveProfile(&Profile{Name: "a", Extends: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := SaveProfile(&Profile{Name: "b", Extends: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProfile("a"); err == nil {
		t.Fatal("expected an extends cycle to be rejected, got nil error")
	}
}

func TestLoadProfile_ExtendsMissingBaseIsAClearError(t *testing.T) {
	t.Setenv("MURO_PROFILES_DIR", t.TempDir())
	if err := SaveProfile(&Profile{Name: "child", Extends: "does-not-exist"}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProfile("child"); err == nil {
		t.Fatal("expected an error resolving a nonexistent base, got nil")
	}
}

func TestLoadProfileRaw_DoesNotResolveExtends(t *testing.T) {
	// The whole point of LoadProfileRaw: mutation commands (profile mount
	// add/remove, profile agent-args set) must never see the base's
	// content merged in, or saving would bake it permanently into the
	// child's own file.
	t.Setenv("MURO_PROFILES_DIR", t.TempDir())
	if err := SaveProfile(&Profile{
		Name:   "base",
		Mounts: []Mount{{Host: "/usr", SandboxPath: "/usr", Mode: "ro"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := SaveProfile(&Profile{Name: "child", Extends: "base"}); err != nil {
		t.Fatal(err)
	}

	raw, err := LoadProfileRaw("child")
	if err != nil {
		t.Fatalf("LoadProfileRaw error: %v", err)
	}
	if len(raw.Mounts) != 0 {
		t.Errorf("LoadProfileRaw Mounts = %+v, want empty — base's mounts must not appear in the raw read", raw.Mounts)
	}
	if raw.Extends != "base" {
		t.Errorf("Extends = %q, want \"base\" preserved in the raw read", raw.Extends)
	}
}

func TestLoadProfileRaw_DoesNotDefaultRestartPolicy(t *testing.T) {
	// LoadProfile's "default empty RestartPolicy to never" step must only
	// happen once, at the very end of resolution — if LoadProfileRaw also
	// defaulted it, a child profile with RestartPolicy left unset would
	// never actually inherit its base's real policy (it would always read
	// as the already-defaulted "never" before the merge even runs).
	t.Setenv("MURO_PROFILES_DIR", t.TempDir())
	if err := SaveProfile(&Profile{Name: "bare"}); err != nil {
		t.Fatal(err)
	}
	raw, err := LoadProfileRaw("bare")
	if err != nil {
		t.Fatal(err)
	}
	if raw.RestartPolicy != "" {
		t.Errorf("LoadProfileRaw RestartPolicy = %q, want empty (undefaulted)", raw.RestartPolicy)
	}
	// But the public LoadProfile entry point must still default it, for a
	// profile with no base at all — preserving the existing contract.
	resolved, err := LoadProfile("bare")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.RestartPolicy != "never" {
		t.Errorf("LoadProfile RestartPolicy = %q, want defaulted \"never\"", resolved.RestartPolicy)
	}
}
