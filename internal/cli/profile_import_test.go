package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thomkin/muro/internal/config"
)

func TestImportOneBundle_ValidBundleImported(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MURO_PROFILES_DIR", "")
	t.Setenv("HOME", t.TempDir())

	bundleDir := filepath.Join(t.TempDir(), "base")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "profile.json"), []byte(`{"name":"imported-base","agent":"/bin/sh"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "AGENT.md"), []byte("# hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	name, skip := importOneBundle(bundleDir, false)
	if skip != "" {
		t.Fatalf("unexpected skip: %s", skip)
	}
	if name != "imported-base" {
		t.Errorf("name = %q, want imported-base", name)
	}

	got, err := config.LoadProfileRaw("imported-base")
	if err != nil {
		t.Fatalf("profile was not actually saved: %v", err)
	}
	if got.Agent != "/bin/sh" {
		t.Errorf("Agent = %q, want /bin/sh", got.Agent)
	}

	destDir, err := config.ProfileBundleDir("imported-base")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destDir, "AGENT.md")); err != nil {
		t.Errorf("expected AGENT.md to have been copied alongside profile.json: %v", err)
	}
}

func TestImportOneBundle_MissingProfileJSONSkipped(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MURO_PROFILES_DIR", "")
	t.Setenv("HOME", t.TempDir())

	bundleDir := t.TempDir() // empty, no profile.json
	_, skip := importOneBundle(bundleDir, false)
	if skip == "" {
		t.Fatal("expected a skip reason for a directory with no profile.json, got none")
	}
}

func TestImportOneBundle_InvalidJSONSkipped(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MURO_PROFILES_DIR", "")
	t.Setenv("HOME", t.TempDir())

	bundleDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundleDir, "profile.json"), []byte(`not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, skip := importOneBundle(bundleDir, false)
	if skip == "" {
		t.Fatal("expected a skip reason for invalid JSON, got none")
	}
}

func TestImportOneBundle_DangerousMountRejectedByValidation(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MURO_PROFILES_DIR", "")
	t.Setenv("HOME", t.TempDir())

	bundleDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundleDir, "profile.json"), []byte(`{"name":"dangerous","agent":"/bin/sh","mounts":[{"host":"/etc","sandbox_path":"/etc","mode":"rw"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, skip := importOneBundle(bundleDir, false)
	if skip == "" {
		t.Fatal("expected a skip reason for a dangerous rw mount, got none")
	}
	if _, err := config.LoadProfileRaw("dangerous"); err == nil {
		t.Error("a bundle that failed validation must not be saved")
	}
}

func TestImportOneBundle_ExistingProfileNotOverwrittenByDefault(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MURO_PROFILES_DIR", "")
	t.Setenv("HOME", t.TempDir())
	if err := config.SaveProfile(&config.Profile{Name: "existing", Agent: "/original/agent"}); err != nil {
		t.Fatal(err)
	}

	bundleDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundleDir, "profile.json"), []byte(`{"name":"existing","agent":"/new/agent"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, skip := importOneBundle(bundleDir, false)
	if skip == "" {
		t.Fatal("expected a skip reason for an existing profile without --overwrite, got none")
	}
	got, _ := config.LoadProfileRaw("existing")
	if got.Agent != "/original/agent" {
		t.Errorf("Agent = %q, want the original untouched", got.Agent)
	}
}

func TestImportOneBundle_OverwriteFlagReplacesExistingAndItsDocs(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MURO_PROFILES_DIR", "")
	t.Setenv("HOME", t.TempDir())
	if err := config.SaveProfile(&config.Profile{Name: "existing", Agent: "/original/agent"}); err != nil {
		t.Fatal(err)
	}
	oldDestDir, err := config.ProfileBundleDir("existing")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDestDir, "STALE.md"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	bundleDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundleDir, "profile.json"), []byte(`{"name":"existing","agent":"/new/agent"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "AGENT.md"), []byte("fresh"), 0o644); err != nil {
		t.Fatal(err)
	}

	name, skip := importOneBundle(bundleDir, true)
	if skip != "" {
		t.Fatalf("unexpected skip with --overwrite: %s", skip)
	}
	if name != "existing" {
		t.Errorf("name = %q, want existing", name)
	}
	got, _ := config.LoadProfileRaw("existing")
	if got.Agent != "/new/agent" {
		t.Errorf("Agent = %q, want overwritten to /new/agent", got.Agent)
	}
	if _, err := os.Stat(filepath.Join(oldDestDir, "STALE.md")); !os.IsNotExist(err) {
		t.Errorf("expected the old bundle's stale doc to be gone after --overwrite, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(oldDestDir, "AGENT.md")); err != nil {
		t.Errorf("expected the new AGENT.md to have been copied in: %v", err)
	}
}

// TestProfileImportCmd_RealGitCloneFindsNestedBundles is a genuine
// end-to-end test: a real local git repo (git supports a plain filesystem
// path as the clone URL, no network needed) with a profiles/ subdirectory
// holding two profile bundles (each its own subdirectory with
// profile.json + docs), one of which is invalid — proving the actual
// command wires together cloning, profiles/-first discovery, per-bundle
// directory iteration, and skip-don't-abort behavior, not just
// importOneBundle in isolation.
func TestProfileImportCmd_RealGitCloneFindsNestedBundles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH, skipping")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("MURO_PROFILES_DIR", "")
	t.Setenv("HOME", t.TempDir())

	repoDir := t.TempDir()
	profilesDir := filepath.Join(repoDir, "profiles")

	goodBundle := filepath.Join(profilesDir, "code-reviewer")
	if err := os.MkdirAll(goodBundle, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(goodBundle, "profile.json"), []byte(`{"name":"from-repo","agent":"/bin/sh"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(goodBundle, "AGENT.md"), []byte("# reviewer\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	badBundle := filepath.Join(profilesDir, "broken")
	if err := os.MkdirAll(badBundle, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badBundle, "profile.json"), []byte(`not valid json`), 0o644); err != nil {
		t.Fatal(err)
	}

	// A loose file directly under profiles/ (not a bundle directory) must
	// never be treated as one.
	if err := os.WriteFile(filepath.Join(profilesDir, "stray.json"), []byte(`{"name":"should-not-import"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "--quiet", "-b", "main")
	runGit("add", ".")
	runGit("commit", "--quiet", "-m", "initial")

	profileImportRefFlag = ""
	profileImportOverwriteFlag = false
	if err := profileImportCmd.RunE(profileImportCmd, []string{repoDir}); err != nil {
		t.Fatalf("import RunE error: %v", err)
	}

	if _, err := config.LoadProfileRaw("from-repo"); err != nil {
		t.Errorf("expected \"from-repo\" to have been imported: %v", err)
	}
	destDir, _ := config.ProfileBundleDir("from-repo")
	if _, err := os.Stat(filepath.Join(destDir, "AGENT.md")); err != nil {
		t.Errorf("expected AGENT.md to have been imported alongside profile.json: %v", err)
	}
	if _, err := config.LoadProfileRaw("should-not-import"); err == nil {
		t.Error("a loose file directly under profiles/ must not be imported as a bundle")
	}
}
