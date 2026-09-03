package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Mount is one directory (or file) bind-mounted into a sandbox at launch.
type Mount struct {
	Host        string `json:"host"`
	SandboxPath string `json:"sandbox_path"`
	Mode        string `json:"mode"` // "ro" | "rw"
}

// Tool is one host executable (or, with As == "*", a whole curated
// toolchain directory) exposed inside a sandbox's restricted PATH
// (DESIGN.md §10).
type Tool struct {
	Host string `json:"host"`
	As   string `json:"as"` // sandbox-visible name, or "*" for a whole directory
}

// Profile is a named, reusable sandbox configuration (SPEC.md §4/§7),
// stored as JSON at <ProfilesDir>/<name>.json.
type Profile struct {
	Name string `json:"name"`
	// Extends names another profile this one inherits from — LoadProfile
	// resolves it recursively and merges the chain (see LoadProfile's doc
	// comment for exact per-field merge rules). The profile's own file on
	// disk only ever stores its own explicit fields; LoadProfileRaw returns
	// exactly that, unresolved — what `profile mount add`/`profile
	// agent-args set`/`profile edit` all read and write, so editing a
	// profile that extends a base never bakes the base's content into the
	// child's own file.
	Extends string `json:"extends,omitempty"`
	Agent   string `json:"agent"`
	// AgentArgs are extra command-line arguments appended after Agent when
	// launching it (e.g. Claude Code's own --dangerously-skip-permissions
	// or --add-dir <path>) — the sandbox's OS-level isolation is the real
	// security boundary, so it's a legitimate, common choice to also tell
	// an agent it doesn't need to re-prompt for permissions it can't
	// actually exceed anyway. Passed as real argv entries to exec, never
	// through a shell, so nothing here is subject to shell injection.
	AgentArgs     []string          `json:"agent_args,omitempty"`
	Mounts        []Mount           `json:"mounts"`
	Tools         []Tool            `json:"tools"`
	AllowURLs     []string          `json:"allow_urls"`
	DenyURLs      []string          `json:"deny_urls"`
	Env           map[string]string `json:"env"`
	RestartPolicy string            `json:"restart_policy"` // never|on-failure|always
	// Git is this profile's git tool-proxy grant (internal/gitproxy) — the
	// per-container half of the global-ceiling/per-container-grant split;
	// DaemonConfig.GitPolicy (daemon.go) is the other half.
	Git GitPolicy `json:"git,omitempty"`
	// Audio opts this sandbox into PipeWire/PulseAudio socket access
	// (internal/sandbox/audio.go) — the host's audio sockets under
	// $XDG_RUNTIME_DIR get bind-mounted into the sandbox at the identical
	// path, so a speech-to-text tool running inside the sandbox can capture
	// microphone input. Off by default: live mic access is privacy-sensitive
	// the same way filesystem/network/git access already are, so it's
	// opt-in, not a default capability every sandbox gets.
	Audio bool `json:"audio,omitempty"`
	// PrivateDirs are sandbox-internal paths that each get a fresh,
	// sandbox-instance-private backing directory (internal/sandbox's
	// PrivateDirMounts) instead of any host location — e.g. Claude Code's
	// own ~/.claude/projects session history, kept isolated per sandbox
	// rather than shared with the host's real ~/.claude/projects (which
	// would leak every OTHER project's history into the sandbox too) or
	// with any other sandbox. Persists across restarts of the SAME
	// sandbox instance (same ID); a fresh `muro run` always starts empty.
	PrivateDirs []string `json:"private_dirs,omitempty"`
	// Instructions is a host path to a markdown file mounted read-only at
	// ~/.claude/CLAUDE.md inside the sandbox (internal/sandbox's
	// InstructionsMount) — Claude Code's own "read at the start of every
	// session" project-instructions mechanism, but mounted at the
	// home-directory level rather than a specific project's, so it applies
	// no matter which directory this sandbox is pointed at. Meant for
	// describing what a specialized profile's agent is FOR (e.g. "you are
	// an FPGA expert; here are the datasheets" or "you are a code
	// reviewer; here are our style rules") — the markdown equivalent of
	// AgentArgs, but content instead of flags.
	Instructions string `json:"instructions,omitempty"`
	// Skills are host paths, each either a SKILL.md file or a directory
	// containing one, mounted read-only under ~/.claude/skills/<name>/
	// inside the sandbox (internal/sandbox's SkillMounts) — Claude Code's
	// own Agent Skills mechanism (.claude/skills/<name>/SKILL.md),
	// available from any project this sandbox is pointed at.
	Skills []string `json:"skills,omitempty"`
	// WorkDir is the sandbox-internal path the agent process starts in
	// (bwrap's --chdir, internal/sandbox/bwrap.go) — every sandbox used to
	// get "/" unconditionally, regardless of where a profile's own mounts:
	// entry puts the actual project (typically /workspace). That silently
	// broke any agent instructions written as paths relative to the
	// project root (e.g. a CLAUDE.md/AGENT.md saying "read progress/foo.json"),
	// since the agent's cwd was never actually inside the project. Empty
	// means "/", preserving every existing profile's behavior exactly.
	WorkDir string `json:"workdir,omitempty"`
	// QuietMode, when true, launches cmd/muro-quiet-chat instead of the
	// agent directly (internal/sandbox's buildLaunchSpec) — a small wrapper
	// that drives Claude Code's own non-interactive print mode
	// (`claude -p ... --output-format json`) turn by turn instead of its
	// normal interactive UI, so an attached session shows only the
	// assistant's reply text, never tool_use/tool_result/diff noise. Meant
	// for a conversational, user-facing profile (a tutor, an assistant)
	// where watching every file edit/tool call is unhelpful clutter rather
	// than useful transparency — a coding-focused profile should leave this
	// off. Agent/AgentArgs are ignored when this is set: the wrapper always
	// invokes Claude Code itself. Off by default, and — like Audio — OR'd
	// across an extends chain: once a base profile turns it on, a child
	// cannot turn it back off (same known, accepted limitation as Audio,
	// for the same reason: no way to distinguish "child didn't mention it"
	// from "child explicitly wants it off" without a bigger schema change).
	QuietMode bool `json:"quiet_mode,omitempty"`
}

// ProfileBundleDir returns the directory holding one profile's whole
// bundle: its profile.json plus any docs alongside it (AGENT.md, and
// whatever else — internal/sandbox mounts this whole directory at /agent
// inside the sandbox, and specifically surfaces AGENT.md, if present, at
// /workspace/AGENTS.md too). Every profile is a directory, uniformly,
// whether or not it happens to have any docs — <ProfilesDir>/<name>/,
// never a mix of loose files and same-named directories.
//
// Rejects a name that could escape ProfilesDir (SECURITY_REVIEW.md finding
// #2's bug class — flagged during that fix's own review as present here
// too, under the identical unsanitized-concatenation pattern, but out of
// that fix's scope since it named SandboxLogPath specifically; closed here
// using the same ValidSandboxName validator rather than a second,
// possibly-drifting implementation).
func ProfileBundleDir(name string) (string, error) {
	if err := ValidSandboxName("profile name", name); err != nil {
		return "", err
	}
	dir, err := ProfilesDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

func profilePath(name string) (string, error) {
	bundleDir, err := ProfileBundleDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(bundleDir, "profile.json"), nil
}

// LoadProfileRaw reads and parses <ProfilesDir>/<name>/profile.json
// exactly as stored on disk — it does NOT resolve Extends, and does NOT default an
// empty RestartPolicy (that defaulting happens once, at the end, in
// LoadProfile — applying it here too would make every profile's raw
// RestartPolicy read as "never" even when unset, which would silently
// defeat a child profile's ability to inherit its base's actual policy).
// Use this for anything that reads a profile in order to MODIFY and
// re-save it: profile mount add/remove, profile agent-args set, profile
// edit's post-edit validation.
func LoadProfileRaw(name string) (*Profile, error) {
	path, err := profilePath(name)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var p Profile
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parsing profile %q: %w", name, err)
	}
	return &p, nil
}

// LoadProfile reads <name>.json and, if it sets Extends, recursively
// resolves and merges the whole chain — base profile's fields first, this
// profile's own layered on top — before returning the fully effective
// config. This is what `muro run` and `muro profile show` use.
//
// Per-field merge rules:
//   - list fields (Mounts, Tools, AllowURLs, DenyURLs, Git.Repos,
//     PrivateDirs): the base's entries, then this profile's own,
//     concatenated. A mount/tool at the same sandbox path in both is not
//     specially deduplicated here — bwrap itself resolves the LATER of two
//     mounts at the same path as the effective one (the same mechanism the
//     git tool-proxy stub already relies on to shadow a real git binary),
//     so a child profile "overriding" a base mount just works by listing
//     its own version after the base's.
//   - single-value fields (Agent, AgentArgs, RestartPolicy): this
//     profile's own value if set/non-empty, else inherited from the base.
//   - Env: map-merged, this profile's own keys win on collision.
//   - Audio: OR'd — if the base turns it on, a child cannot turn it back
//     off. There's no way to distinguish "child didn't mention it" from
//     "child explicitly wants it off" without a bigger (pointer-typed)
//     schema change; a known, accepted limitation rather than a bug.
//
// A cycle (A extends B extends A, directly or transitively) is rejected
// with a clear error instead of infinite-looping.
func LoadProfile(name string) (*Profile, error) {
	p, err := loadProfileResolved(name, nil)
	if err != nil {
		return nil, err
	}
	if p.RestartPolicy == "" {
		p.RestartPolicy = "never"
	}
	return p, nil
}

func loadProfileResolved(name string, seen map[string]bool) (*Profile, error) {
	if seen == nil {
		seen = make(map[string]bool)
	}
	if seen[name] {
		return nil, fmt.Errorf("profile %q: extends cycle detected", name)
	}
	seen[name] = true

	p, err := LoadProfileRaw(name)
	if err != nil {
		return nil, err
	}
	if p.Extends == "" {
		return p, nil
	}
	base, err := loadProfileResolved(p.Extends, seen)
	if err != nil {
		return nil, fmt.Errorf("profile %q: resolve extends %q: %w", name, p.Extends, err)
	}
	return mergeProfiles(base, p), nil
}

func mergeProfiles(base, child *Profile) *Profile {
	merged := &Profile{
		Name:          child.Name,
		Agent:         firstNonEmpty(child.Agent, base.Agent),
		AgentArgs:     child.AgentArgs,
		Mounts:        append(append([]Mount(nil), base.Mounts...), child.Mounts...),
		Tools:         append(append([]Tool(nil), base.Tools...), child.Tools...),
		AllowURLs:     append(append([]string(nil), base.AllowURLs...), child.AllowURLs...),
		DenyURLs:      append(append([]string(nil), base.DenyURLs...), child.DenyURLs...),
		Env:           mergeEnv(base.Env, child.Env),
		RestartPolicy: firstNonEmpty(child.RestartPolicy, base.RestartPolicy),
		Git:           GitPolicy{Repos: append(append([]GitRepoPolicy(nil), base.Git.Repos...), child.Git.Repos...)},
		Audio:         base.Audio || child.Audio,
		PrivateDirs:   append(append([]string(nil), base.PrivateDirs...), child.PrivateDirs...),
		Instructions:  firstNonEmpty(child.Instructions, base.Instructions),
		Skills:        append(append([]string(nil), base.Skills...), child.Skills...),
		WorkDir:       firstNonEmpty(child.WorkDir, base.WorkDir),
		QuietMode:     base.QuietMode || child.QuietMode,
	}
	if len(child.AgentArgs) == 0 {
		merged.AgentArgs = base.AgentArgs
	}
	return merged
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func mergeEnv(base, child map[string]string) map[string]string {
	if len(base) == 0 && len(child) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(child))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range child {
		out[k] = v
	}
	return out
}

// SaveProfile writes p to <ProfilesDir>/<p.Name>/profile.json atomically:
// it writes to a temp file in the same directory and renames it into
// place, so a reader (or a crash mid-write) never observes a partially
// written profile. Creating the bundle directory itself never disturbs any
// docs already sitting in it (MkdirAll on an existing directory is a
// no-op) — this is also what a fresh `profile mount add`/`agent-args set`
// etc. round-trip goes through, so it must never touch sibling files.
func SaveProfile(p *Profile) error {
	if p.Name == "" {
		return fmt.Errorf("profile has no name")
	}
	bundleDir, err := ProfileBundleDir(p.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(bundleDir, ".tmp-profile-*.json")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	// Clean up the temp file if we return before the rename succeeds.
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	finalPath := filepath.Join(bundleDir, "profile.json")
	return os.Rename(tmpPath, finalPath)
}

// ListProfiles returns the names of all profiles in ProfilesDir — every
// subdirectory that actually contains a profile.json, so a stray unrelated
// directory someone drops in ProfilesDir doesn't show up as a phantom
// profile. Returns an empty slice, not an error, if the directory doesn't
// exist yet.
func ListProfiles() ([]string, error) {
	dir, err := ProfilesDir()
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "profile.json")); err != nil {
			continue
		}
		names = append(names, e.Name())
	}
	return names, nil
}
