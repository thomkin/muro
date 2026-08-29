package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/thomkin/muro/internal/config"
)

// InstructionsMount resolves a profile's Instructions markdown file (if
// set) into a bind mount at ~/.claude/CLAUDE.md inside the sandbox, using
// hostHome (the real host user's home directory, os.UserHomeDir() at
// launch time) — the same "$HOME-relative sandbox path matches the host's"
// convention the rest of this project already relies on for Claude's other
// config files. Mounted at the home level rather than a specific
// project's, so it applies no matter which directory this sandbox is
// pointed at — the point of a specialized profile (e.g. "FPGA expert")
// having ONE consistent set of instructions.
//
// Returns a nil mount (not an error) if instructionsPath is empty — no
// Instructions configured is the normal case, not a failure.
func InstructionsMount(hostHome, instructionsPath string) (*config.Mount, error) {
	if instructionsPath == "" {
		return nil, nil
	}
	info, err := os.Stat(instructionsPath)
	if err != nil {
		return nil, fmt.Errorf("instructions file %q: %w", instructionsPath, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("instructions %q is a directory, not a markdown file", instructionsPath)
	}
	target := filepath.Join(hostHome, ".claude", "CLAUDE.md")
	return &config.Mount{Host: instructionsPath, SandboxPath: target, Mode: "ro"}, nil
}

// BundleDocsMounts resolves a profile's own bundle directory
// (<ProfilesDir>/<profileName>/, config.ProfileBundleDir) into sandbox
// mounts:
//   - the WHOLE directory at /agent, read-write — edits made from inside
//     the sandbox (by the agent, or by a human attached to it) write
//     straight back to this same host directory, the one canonical copy
//     every launch of this profile shares, not a private per-instance
//     snapshot. Visible, not a dotfile — this is meant to be something a
//     person browsing the sandbox (or the host) immediately recognizes,
//     not something hidden away.
//   - if a file named exactly AGENT.md exists in that directory, it is
//     ALSO specifically mounted at /workspace/AGENTS.md, read-write — the
//     universal "read at the start of every session" filename Claude Code
//     (and other AGENTS.md-aware tools) already recognize natively, no
//     per-agent-type logic needed here at all.
//
// Purely convention-driven: no profile field controls this, it's entirely
// filesystem presence. A profile with no bundle directory (or an existing
// but empty one) gets no mounts at all here — not an error, just the
// normal case for a profile with no docs.
func BundleDocsMounts(profileName string) ([]config.Mount, error) {
	bundleDir, err := config.ProfileBundleDir(profileName)
	if err != nil {
		return nil, err
	}
	entries, readErr := os.ReadDir(bundleDir)
	if readErr != nil {
		return nil, nil // no bundle directory at all — nothing to mount, not an error
	}
	// SaveProfile always creates this directory, just to hold profile.json
	// — so "the directory exists" is never a valid signal for "has docs"
	// on its own. Only mount /agent if there's something in it BESIDES
	// profile.json; a profile with no docs must get no mount here at all.
	hasDocs := false
	for _, e := range entries {
		if e.Name() != "profile.json" {
			hasDocs = true
			break
		}
	}
	if !hasDocs {
		return nil, nil
	}

	mounts := []config.Mount{{Host: bundleDir, SandboxPath: "/agent", Mode: "rw"}}

	entryFile := filepath.Join(bundleDir, "AGENT.md")
	if fi, err := os.Stat(entryFile); err == nil && !fi.IsDir() {
		mounts = append(mounts, config.Mount{Host: entryFile, SandboxPath: "/workspace/AGENTS.md", Mode: "rw"})
	}
	return mounts, nil
}

// SkillMounts resolves a profile's Skills paths into bind mounts under
// ~/.claude/skills/<name>/ inside the sandbox (same hostHome convention as
// InstructionsMount). Each entry may be either a directory (mounted
// wholesale at skills/<dir-name>/) or a single *.md file (mounted at
// skills/<file-name-without-extension>/SKILL.md, matching Claude Code's
// own one-skill-per-directory convention even for a profile author who
// only wrote a single file).
func SkillMounts(hostHome string, skillPaths []string) ([]config.Mount, error) {
	var mounts []config.Mount
	for _, sp := range skillPaths {
		info, err := os.Stat(sp)
		if err != nil {
			return nil, fmt.Errorf("skill %q: %w", sp, err)
		}
		base := filepath.Base(sp)
		if info.IsDir() {
			mounts = append(mounts, config.Mount{
				Host:        sp,
				SandboxPath: filepath.Join(hostHome, ".claude", "skills", base),
				Mode:        "ro",
			})
			continue
		}
		skillName := strings.TrimSuffix(base, filepath.Ext(base))
		mounts = append(mounts, config.Mount{
			Host:        sp,
			SandboxPath: filepath.Join(hostHome, ".claude", "skills", skillName, "SKILL.md"),
			Mode:        "ro",
		})
	}
	return mounts, nil
}
