package cli

import (
	"testing"

	"github.com/thomkin/muro/internal/config"
)

func TestProfileMountAdd_AppendsAndPersists(t *testing.T) {
	t.Setenv("MURO_PROFILES_DIR", t.TempDir())
	p := &config.Profile{Name: "myagent", Mounts: []config.Mount{{Host: "/tmp", SandboxPath: "/workspace", Mode: "ro"}}}
	if err := config.SaveProfile(p); err != nil {
		t.Fatal(err)
	}

	profileMountAddFlags = []string{"/usr/bin:/usr/bin:ro"}
	defer func() { profileMountAddFlags = nil }()
	if err := profileMountAddCmd.RunE(profileMountAddCmd, []string{"myagent"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := config.LoadProfile("myagent")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Mounts) != 2 {
		t.Fatalf("got %d mounts, want 2: %+v", len(got.Mounts), got.Mounts)
	}
	if got.Mounts[1].Host != "/usr/bin" || got.Mounts[1].SandboxPath != "/usr/bin" || got.Mounts[1].Mode != "ro" {
		t.Errorf("added mount = %+v, want /usr/bin -> /usr/bin ro", got.Mounts[1])
	}
}

func TestProfileMountAdd_RequiresMountFlag(t *testing.T) {
	t.Setenv("MURO_PROFILES_DIR", t.TempDir())
	p := &config.Profile{Name: "myagent"}
	if err := config.SaveProfile(p); err != nil {
		t.Fatal(err)
	}

	profileMountAddFlags = nil
	if err := profileMountAddCmd.RunE(profileMountAddCmd, []string{"myagent"}); err == nil {
		t.Fatal("expected an error when --mount is not given")
	}
}

func TestProfileMountAdd_RejectsInvalidProfileAfterAdd(t *testing.T) {
	// A mount that would make ValidateProfile reject the profile (e.g. an
	// rw mount covering a dangerous host root) must not get persisted.
	t.Setenv("MURO_PROFILES_DIR", t.TempDir())
	p := &config.Profile{Name: "myagent"}
	if err := config.SaveProfile(p); err != nil {
		t.Fatal(err)
	}

	profileMountAddFlags = []string{"/etc:/etc:rw"}
	defer func() { profileMountAddFlags = nil }()
	if err := profileMountAddCmd.RunE(profileMountAddCmd, []string{"myagent"}); err == nil {
		t.Fatal("expected a dangerous rw mount to be rejected")
	}

	got, err := config.LoadProfile("myagent")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Mounts) != 0 {
		t.Errorf("rejected mount must not be persisted, got %+v", got.Mounts)
	}
}

func TestProfileMountRemove_RemovesBySandboxPath(t *testing.T) {
	t.Setenv("MURO_PROFILES_DIR", t.TempDir())
	p := &config.Profile{Name: "myagent", Mounts: []config.Mount{
		{Host: "/tmp", SandboxPath: "/workspace", Mode: "ro"},
		{Host: "/usr/bin", SandboxPath: "/usr/bin", Mode: "ro"},
	}}
	if err := config.SaveProfile(p); err != nil {
		t.Fatal(err)
	}

	profileMountRemoveFlags = []string{"/usr/bin"}
	defer func() { profileMountRemoveFlags = nil }()
	if err := profileMountRemoveCmd.RunE(profileMountRemoveCmd, []string{"myagent"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := config.LoadProfile("myagent")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Mounts) != 1 || got.Mounts[0].SandboxPath != "/workspace" {
		t.Errorf("got %+v, want only the /workspace mount left", got.Mounts)
	}
}

func TestProfileMountRemove_RequiresSandboxPathFlag(t *testing.T) {
	t.Setenv("MURO_PROFILES_DIR", t.TempDir())
	p := &config.Profile{Name: "myagent"}
	if err := config.SaveProfile(p); err != nil {
		t.Fatal(err)
	}

	profileMountRemoveFlags = nil
	if err := profileMountRemoveCmd.RunE(profileMountRemoveCmd, []string{"myagent"}); err == nil {
		t.Fatal("expected an error when --sandbox-path is not given")
	}
}
