package sandbox

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/thomkin/muro/internal/config"
)

func TestInstructionsMount_EmptyPathReturnsNil(t *testing.T) {
	m, err := InstructionsMount("/home/someone", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m != nil {
		t.Errorf("expected a nil mount for an empty Instructions path, got %+v", m)
	}
}

func TestInstructionsMount_MountsAtHomeClaudeMd(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "AGENT.md")
	if err := os.WriteFile(src, []byte("# You are an FPGA expert\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := InstructionsMount("/home/someone", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m == nil {
		t.Fatal("expected a non-nil mount")
	}
	want := "/home/someone/.claude/CLAUDE.md"
	if m.SandboxPath != want {
		t.Errorf("SandboxPath = %q, want %q", m.SandboxPath, want)
	}
	if m.Host != src || m.Mode != "ro" {
		t.Errorf("got %+v, want Host=%q Mode=ro", m, src)
	}
}

func TestInstructionsMount_MissingFileIsAnError(t *testing.T) {
	_, err := InstructionsMount("/home/someone", "/does/not/exist.md")
	if err == nil {
		t.Fatal("expected an error for a nonexistent instructions file")
	}
}

func TestInstructionsMount_DirectoryIsRejected(t *testing.T) {
	_, err := InstructionsMount("/home/someone", t.TempDir())
	if err == nil {
		t.Fatal("expected an error when Instructions points at a directory, not a file")
	}
}

func TestSkillMounts_SingleFileMountsAsSkillMdInNamedDir(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "deploy.md")
	if err := os.WriteFile(src, []byte("# Deploy skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mounts, err := SkillMounts("/home/someone", []string{src})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mounts) != 1 {
		t.Fatalf("got %d mounts, want 1", len(mounts))
	}
	want := "/home/someone/.claude/skills/deploy/SKILL.md"
	if mounts[0].SandboxPath != want {
		t.Errorf("SandboxPath = %q, want %q", mounts[0].SandboxPath, want)
	}
}

func TestSkillMounts_DirectoryMountsWholesaleAtItsOwnName(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "verify")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# Verify\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mounts, err := SkillMounts("/home/someone", []string{skillDir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mounts) != 1 {
		t.Fatalf("got %d mounts, want 1", len(mounts))
	}
	want := "/home/someone/.claude/skills/verify"
	if mounts[0].SandboxPath != want {
		t.Errorf("SandboxPath = %q, want %q", mounts[0].SandboxPath, want)
	}
}

func TestSkillMounts_MissingPathIsAnError(t *testing.T) {
	_, err := SkillMounts("/home/someone", []string{"/does/not/exist"})
	if err == nil {
		t.Fatal("expected an error for a nonexistent skill path")
	}
}

func TestSkillMounts_MultipleSkillsAllPresent(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.md")
	b := filepath.Join(dir, "b.md")
	if err := os.WriteFile(a, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	mounts, err := SkillMounts("/home/someone", []string{a, b})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mounts) != 2 {
		t.Fatalf("got %d mounts, want 2", len(mounts))
	}
}

func TestBundleDocsMounts_NoBundleDirAtAllReturnsNoMounts(t *testing.T) {
	t.Setenv("MURO_PROFILES_DIR", t.TempDir())
	mounts, err := BundleDocsMounts("never-created")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mounts != nil {
		t.Errorf("expected nil mounts for a profile with no bundle directory at all, got %+v", mounts)
	}
}

// TestBundleDocsMounts_ProfileJSONOnlyReturnsNoMounts is the regression
// test for the bug where SaveProfile always creating the bundle directory
// (just to hold profile.json) was mistaken by BundleDocsMounts for "this
// profile has docs" — every profile, even with zero docs, was getting a
// spurious /agent mount.
func TestBundleDocsMounts_ProfileJSONOnlyReturnsNoMounts(t *testing.T) {
	t.Setenv("MURO_PROFILES_DIR", t.TempDir())
	if err := config.SaveProfile(&config.Profile{Name: "bare"}); err != nil {
		t.Fatal(err)
	}
	mounts, err := BundleDocsMounts("bare")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mounts != nil {
		t.Errorf("expected nil mounts for a bundle dir containing only profile.json, got %+v", mounts)
	}
}

func TestBundleDocsMounts_ExtraDocMountsWholeDirAtAgent(t *testing.T) {
	t.Setenv("MURO_PROFILES_DIR", t.TempDir())
	if err := config.SaveProfile(&config.Profile{Name: "with-docs"}); err != nil {
		t.Fatal(err)
	}
	bundleDir, err := config.ProfileBundleDir("with-docs")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "NOTES.md"), []byte("notes"), 0o644); err != nil {
		t.Fatal(err)
	}

	mounts, err := BundleDocsMounts("with-docs")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mounts) != 1 {
		t.Fatalf("got %d mounts, want 1 (no AGENT.md present): %+v", len(mounts), mounts)
	}
	if mounts[0].Host != bundleDir || mounts[0].SandboxPath != "/agent" || mounts[0].Mode != "rw" {
		t.Errorf("mount = %+v, want Host=%q SandboxPath=/agent Mode=rw", mounts[0], bundleDir)
	}
}

func TestBundleDocsMounts_AgentMdAlsoMountsAtWorkspaceAgentsMd(t *testing.T) {
	t.Setenv("MURO_PROFILES_DIR", t.TempDir())
	if err := config.SaveProfile(&config.Profile{Name: "with-agent-md"}); err != nil {
		t.Fatal(err)
	}
	bundleDir, err := config.ProfileBundleDir("with-agent-md")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "AGENT.md"), []byte("# instructions"), 0o644); err != nil {
		t.Fatal(err)
	}

	mounts, err := BundleDocsMounts("with-agent-md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mounts) != 2 {
		t.Fatalf("got %d mounts, want 2 (whole dir at /agent + AGENT.md at /workspace/AGENTS.md): %+v", len(mounts), mounts)
	}
	if mounts[0].SandboxPath != "/agent" {
		t.Errorf("mounts[0] = %+v, want SandboxPath=/agent", mounts[0])
	}
	wantAgentsMd := filepath.Join(bundleDir, "AGENT.md")
	if mounts[1].Host != wantAgentsMd || mounts[1].SandboxPath != "/workspace/AGENTS.md" || mounts[1].Mode != "rw" {
		t.Errorf("mounts[1] = %+v, want Host=%q SandboxPath=/workspace/AGENTS.md Mode=rw", mounts[1], wantAgentsMd)
	}
}
