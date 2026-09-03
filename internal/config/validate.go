package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// dangerousHostRoots are host paths that must never be exposed read-write
// to a sandbox, in either direction: mounting one of these exactly, an
// ancestor of one (e.g. mounting a parent of StateDir contains StateDir),
// OR a path narrower than one (e.g. mounting muro's own state.json
// specifically, a file inside StateDir) as "rw" would let a sandboxed
// process modify system files or muro's own config/state out from under
// murod itself. Checked both ways —
// pathCoversOrEquals(hostAbs, dangerous) || pathCoversOrEquals(dangerous,
// hostAbs) below — since a narrower mount landing INSIDE one of these is
// just as much a problem as a broader one containing it: confirmed by
// direct reproduction that a check catching only "hostAbs covers
// dangerous" let a mount of state.json itself through undetected —
// exactly the scenario this validation exists to prevent.
//
// "/" is deliberately NOT in this list — see dangerousHostRootsExactOnly,
// below, for why it needs different (one-directional) treatment.
//
// Read-only exposure of these is a much smaller concern (test/integration's
// own shellMounts() legitimately mounts /usr, /bin, /lib, /lib64 read-only
// to give a sandbox a working shell) and is not restricted here.
//
// This guards against accidental/tooling-generated over-broad profiles,
// not the sandboxed agent itself — mounts are fixed at launch
// (Isolator.UpdateMounts never allows a live remount), so there is no
// runtime escalation path this closes for an already-running sandbox.
func dangerousHostRoots() []string {
	roots := []string{"/etc", "/usr", "/bin", "/lib", "/proc", "/sys", "/dev"}
	if cfgDir, err := ConfigDir(); err == nil && cfgDir != "" {
		roots = append(roots, cfgDir)
	}
	if stateDir, err := StateDir(); err == nil && stateDir != "" {
		roots = append(roots, stateDir)
	}
	return roots
}

// dangerousHostRootsExactOnly are checked one-directionally only
// (pathCoversOrEquals(hostAbs, root) — "hostAbs is root, or something
// broader containing it"), never the symmetric "any overlap" test
// dangerousHostRoots' entries use:
//   - "/" — the symmetric test is meaningless here and actively wrong:
//     pathCoversOrEquals("/", anything) is true for literally every
//     absolute path by definition (root covers everything), so checking
//     "does '/' cover hostAbs" would reject every single mount, not just
//     root itself. Confirmed by direct reproduction — this exact bug
//     briefly broke every legitimate mount in this package's own tests
//     before being caught. Root only needs its one-directional check
//     (mounting "/" itself); no other absolute path can be broader than
//     it, and any dangerous subpath of "/" not separately listed
//     (/etc, /usr, StateDir, ...) isn't dangerous enough to warrant
//     blocking arbitrary siblings under it.
//   - the operator's home directory — a mount NARROWER than home (e.g.
//     ~/projects/foo, the normal, expected case) is legitimate and must
//     stay allowed; only the home directory itself, or something broader
//     containing it, is rejected.
func dangerousHostRootsExactOnly() []string {
	roots := []string{"/"}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		roots = append(roots, home)
	}
	return roots
}

// sandboxScaffoldPaths are the fixed paths BwrapIsolator.buildArgs already
// sets up for every sandbox (--proc /proc --dev /dev --tmpfs /tmp,
// internal/sandbox/bwrap.go) — a profile mount landing on one of these
// sandbox-internal paths would override that restricted scaffolding,
// regardless of the mount's own mode.
func sandboxScaffoldPaths() []string {
	return []string{"/proc", "/dev", "/tmp"}
}

// ExpandHome resolves a leading "~" the same way a shell would, using the
// REAL CURRENT user's home directory (os.UserHomeDir(), evaluated at call
// time, on whatever machine this runs) — the mechanism that makes a
// profile authored on one machine ("~/.claude/...") resolve correctly on a
// different one, rather than needing every profile to hardcode
// "/home/thomas/...". Applied everywhere a profile carries a host or
// sandbox path: Mount.Host, Mount.SandboxPath, Tool.Host, Instructions,
// Skills entries, and GitRepoPolicy.Host (internal/sandbox's ResolveMounts
// and friends) — as well as here, for validation's own dangerous-root
// comparisons, which is what this used to be scoped to exclusively before
// the wider expansion existed.
func ExpandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~/"))
}

// pathCoversOrEquals reports whether mounting/using `outer` would expose
// `inner` — either the same path, or `outer` is an ancestor directory of
// `inner`. Both are filepath.Clean'd first and compared on path-component
// boundaries (so "/etc" does not also match "/etcfoo").
func pathCoversOrEquals(outer, inner string) bool {
	if !filepath.IsAbs(outer) || !filepath.IsAbs(inner) {
		// Only absolute (or home-expanded) paths are checked — a relative
		// host path's actual target depends on murod's CWD at launch time,
		// which this validation has no way to resolve; existing behavior
		// for relative paths is unchanged.
		return false
	}
	outer = filepath.Clean(outer)
	inner = filepath.Clean(inner)
	if outer == string(filepath.Separator) {
		return true // root covers everything
	}
	if outer == inner {
		return true
	}
	return strings.HasPrefix(inner, outer+string(filepath.Separator))
}

// toolRoot is the fixed sandbox-internal directory non-wildcard tools are
// mounted into (DESIGN.md §10): a tool declared `{"as": "git"}` lands at
// toolRoot+"/git" inside the sandbox, and the sandbox's PATH is set to
// exactly this directory.
const toolRoot = "/usr/local/bin"

var validRestartPolicies = map[string]bool{
	"never":      true,
	"on-failure": true,
	"always":     true,
}

// ValidSandboxName rejects a sandbox namespace or name value that could be
// used to construct a path outside its intended directory — most directly
// internal/config.SandboxLogPath, which builds a filename by concatenating
// namespace+"__"+name with no sanitization of its own, but this is the
// single, shared validation point every request accepting a namespace/name
// pair from a client (CLI or control API) should call, rather than relying
// on each individual path-consuming function downstream to separately
// guard against it. Deliberately permissive otherwise: letters, digits,
// hyphens, underscores, and dots (other than a leading dot, or "..") are
// all accepted, since real sandbox/namespace names are expected to look
// like ordinary identifiers, not need much restricting beyond "no path
// traversal or separators."
func ValidSandboxName(kind, s string) error {
	if s == "" {
		return fmt.Errorf("%s must not be empty", kind)
	}
	if strings.ContainsAny(s, "/\\") {
		return fmt.Errorf("%s %q must not contain a path separator", kind, s)
	}
	if strings.Contains(s, "..") {
		return fmt.Errorf("%s %q must not contain \"..\"", kind, s)
	}
	if strings.HasPrefix(s, ".") {
		return fmt.Errorf("%s %q must not start with \".\"", kind, s)
	}
	return nil
}

// ValidateProfile checks a profile for internal consistency before it's
// used to launch or reload a sandbox:
//
//   - restart_policy, if set, must be one of never|on-failure|always
//     (empty is allowed and treated as "never" by LoadProfile).
//   - no tools: entry may resolve to the same sandbox-internal path as a
//     mounts: entry (DESIGN.md §10) — this is rejected rather than
//     silently resolved, since silently letting one win would undermine
//     the whole point of the tools: allowlist.
//
// A wildcard tool (As == "*") mounts a whole directory as the sandbox's
// tool root rather than a single named path, so it cannot collide with a
// specific mounts: target and is not part of the collision check.
func ValidateProfile(p *Profile) error {
	if p.Name == "" {
		return fmt.Errorf("profile: name is required")
	}

	policy := p.RestartPolicy
	if policy == "" {
		policy = "never"
	}
	if !validRestartPolicies[policy] {
		return fmt.Errorf("profile %q: invalid restart_policy %q (must be never, on-failure, or always)", p.Name, p.RestartPolicy)
	}

	if p.WorkDir != "" && !strings.HasPrefix(p.WorkDir, "/") {
		return fmt.Errorf("profile %q: workdir %q must be an absolute sandbox-internal path", p.Name, p.WorkDir)
	}

	toolTargets := make(map[string]Tool, len(p.Tools))
	for _, t := range p.Tools {
		if t.As == "" || t.As == "*" {
			continue
		}
		target := toolRoot + "/" + t.As
		if existing, ok := toolTargets[target]; ok {
			return fmt.Errorf("profile %q: tools entries %q and %q both resolve to sandbox path %q",
				p.Name, existing.Host, t.Host, target)
		}
		toolTargets[target] = t
	}

	for _, m := range p.Mounts {
		if t, ok := toolTargets[m.SandboxPath]; ok {
			return fmt.Errorf("profile %q: mount %q and tool %q (as %q) both target sandbox path %q",
				p.Name, m.Host, t.Host, t.As, m.SandboxPath)
		}

		if m.Mode == "rw" {
			hostAbs := ExpandHome(m.Host)
			for _, dangerous := range dangerousHostRoots() {
				// Symmetric: either direction of overlap is rejected. A
				// mount narrower than a dangerous root (e.g. StateDir's own
				// state.json) is just as much a problem as one that
				// contains the root entirely.
				if pathCoversOrEquals(hostAbs, dangerous) || pathCoversOrEquals(dangerous, hostAbs) {
					return fmt.Errorf("profile %q: mount %q -> %q is read-write and overlaps %q — "+
						"mounting this path read-write would let the sandbox modify it; "+
						"use mode \"ro\" if read access is all that's needed",
						p.Name, m.Host, m.SandboxPath, dangerous)
				}
			}
			// "/" and home are checked one-directionally only, not via the
			// symmetric loop above — see dangerousHostRootsExactOnly's doc
			// comment for why (a symmetric check against "/" would reject
			// every mount; a symmetric check against home would reject
			// legitimate subdirectory mounts like ~/projects/foo).
			for _, exact := range dangerousHostRootsExactOnly() {
				if pathCoversOrEquals(hostAbs, exact) {
					return fmt.Errorf("profile %q: mount %q -> %q is read-write and covers %q — "+
						"mount a narrower subdirectory instead, or use mode \"ro\" if read access is all that's needed",
						p.Name, m.Host, m.SandboxPath, exact)
				}
			}
		}

		for _, scaffold := range sandboxScaffoldPaths() {
			if pathCoversOrEquals(m.SandboxPath, scaffold) {
				return fmt.Errorf("profile %q: mount %q -> %q would override the sandbox's own %q scaffolding "+
					"(bwrap sets this up for every sandbox); choose a different sandbox_path",
					p.Name, m.Host, m.SandboxPath, scaffold)
			}
		}
	}

	privateDirSeen := make(map[string]bool, len(p.PrivateDirs))
	for _, pd := range p.PrivateDirs {
		if privateDirSeen[pd] {
			return fmt.Errorf("profile %q: private_dirs entry %q is listed more than once", p.Name, pd)
		}
		privateDirSeen[pd] = true
		if t, ok := toolTargets[pd]; ok {
			return fmt.Errorf("profile %q: private_dirs entry %q and tool %q (as %q) both target the same sandbox path",
				p.Name, pd, t.Host, t.As)
		}
		for _, m := range p.Mounts {
			if m.SandboxPath == pd {
				return fmt.Errorf("profile %q: private_dirs entry %q and mount %q both target the same sandbox path",
					p.Name, pd, m.Host)
			}
		}
	}

	for _, repo := range p.Git.Repos {
		if repo.Worktree {
			if repo.MountPath == "" {
				return fmt.Errorf("profile %q: git policy repo %q has worktree: true but no mount_path — "+
					"a worktree needs a sandbox-internal path to be mounted at",
					p.Name, repo.Host)
			}
			if len(repo.AllowedBranches) != 0 {
				return fmt.Errorf("profile %q: git policy repo %q has worktree: true and also sets "+
					"allowed_branches %v — this is computed automatically from the sandbox's own "+
					"agent/<namespace>/<name> branch; leave allowed_branches empty for a worktree entry",
					p.Name, repo.Host, repo.AllowedBranches)
			}
			if t, ok := toolTargets[repo.MountPath]; ok {
				return fmt.Errorf("profile %q: git policy repo %q's mount_path %q and tool %q (as %q) both target the same sandbox path",
					p.Name, repo.Host, repo.MountPath, t.Host, t.As)
			}
			for _, m := range p.Mounts {
				if m.SandboxPath == repo.MountPath {
					return fmt.Errorf("profile %q: git policy repo %q's mount_path %q and mount %q both target the same sandbox path — "+
						"muro generates this mount itself from the worktree, a hand-declared mounts: entry here would be redundant",
						p.Name, repo.Host, repo.MountPath, m.Host)
				}
			}
			if privateDirSeen[repo.MountPath] {
				return fmt.Errorf("profile %q: git policy repo %q's mount_path %q and a private_dirs entry both target the same sandbox path",
					p.Name, repo.Host, repo.MountPath)
			}
			continue
		}

		if repo.MountPath != "" {
			return fmt.Errorf("profile %q: git policy repo %q sets mount_path %q but worktree is not true — "+
				"mount_path only applies to a worktree: true entry",
				p.Name, repo.Host, repo.MountPath)
		}

		hostAbs := ExpandHome(repo.Host)
		covered := false
		for _, m := range p.Mounts {
			if pathCoversOrEquals(ExpandHome(m.Host), hostAbs) {
				covered = true
				break
			}
		}
		if !covered {
			return fmt.Errorf("profile %q: git policy repo %q is not covered by any mounts: entry — "+
				"the git tool-proxy translates a sandbox's cwd to a host path via the mounts: table, "+
				"so a repo outside every mounted path could never be reached",
				p.Name, repo.Host)
		}
		if len(repo.AllowedBranches) == 0 {
			return fmt.Errorf("profile %q: git policy repo %q has no allowed_branches — "+
				"an empty-but-present repo entry allows nothing; omit the repo entirely instead if that's the intent",
				p.Name, repo.Host)
		}
		if len(repo.AllowedRemotes) == 0 {
			return fmt.Errorf("profile %q: git policy repo %q has no allowed_remotes — "+
				"an empty-but-present repo entry allows nothing; omit the repo entirely instead if that's the intent",
				p.Name, repo.Host)
		}
	}

	// Two worktree entries must not target the same mount_path.
	seenWorktreeMountPaths := make(map[string]string, len(p.Git.Repos))
	for _, repo := range p.Git.Repos {
		if !repo.Worktree {
			continue
		}
		if existing, ok := seenWorktreeMountPaths[repo.MountPath]; ok {
			return fmt.Errorf("profile %q: git policy repos %q and %q both set mount_path %q",
				p.Name, existing, repo.Host, repo.MountPath)
		}
		seenWorktreeMountPaths[repo.MountPath] = repo.Host
	}

	return nil
}
