package sandbox

import (
	"github.com/thomkin/muro/internal/config"
)

// toolRoot is the fixed sandbox-internal directory non-wildcard tools are
// mounted into (DESIGN.md §10) — mirrors internal/config's unexported
// constant of the same name. Duplicated rather than exported from
// internal/config since it's a sandbox-launch concern here, not a
// config-validation concern there; the two must be kept in sync if ever
// changed.
const toolRoot = "/usr/local/bin"

// ResolveMounts validates p (rejecting any tools:/mounts: sandbox-path
// collision via config.ValidateProfile — DESIGN.md §10 requires this be
// rejected outright, never silently resolved) and merges its tools:
// entries into the mount list at their fixed sandbox-internal PATH
// location. From the Isolator's point of view tools are not a separate
// primitive, only from the profile schema's: the returned list is exactly
// what gets passed to Isolator.Launch as LaunchSpec.Mounts.
func ResolveMounts(p *config.Profile) ([]config.Mount, error) {
	if err := config.ValidateProfile(p); err != nil {
		return nil, err
	}

	out := append([]config.Mount(nil), p.Mounts...)
	for _, t := range p.Tools {
		switch t.As {
		case "":
			continue
		case "*":
			out = append(out, config.Mount{Host: config.ExpandHome(t.Host), SandboxPath: toolRoot, Mode: "ro"})
		default:
			out = append(out, config.Mount{Host: config.ExpandHome(t.Host), SandboxPath: toolRoot + "/" + t.As, Mode: "ro"})
		}
	}
	// Expand "~" in every mount's Host AND SandboxPath — the mechanism
	// that makes a profile authored on one machine ("~/.claude/...")
	// resolve correctly on a different one. Validation above already
	// expands "~" internally for its own dangerous-root comparisons, but
	// (deliberately, per its own doc comment) never mutates p.Mounts
	// itself — this is the actual point that turns a literal "~" into a
	// real path before it ever reaches bwrap, which has no shell to do
	// that expansion for it. SandboxPath gets the same treatment as Host:
	// this project's own convention is that a sandbox's $HOME always
	// matches the real host user's, so "~/.claude/settings.json" as a
	// sandbox_path means the same real path on either side.
	for i := range out {
		out[i].Host = config.ExpandHome(out[i].Host)
		out[i].SandboxPath = config.ExpandHome(out[i].SandboxPath)
	}
	return out, nil
}

// mergeMounts appends add to a copy of existing, leaving existing
// untouched — used by Manager.Update to build a candidate mount list to
// validate before anything is actually applied (DESIGN.md §11).
func mergeMounts(existing, add []config.Mount) []config.Mount {
	out := append([]config.Mount(nil), existing...)
	out = append(out, add...)
	return out
}

// cloneGitPolicy deep-copies p's non-worktree repos so a state.Sandbox built
// from a profile at Run time shares no slice memory with the caller's own
// *config.Profile — the same "resolve once, don't alias" precaution
// ResolveMounts and Run's own AllowURLs copy already apply. Worktree: true
// repos are deliberately excluded here — those go through
// WorktreeMounts (worktree.go) instead, which builds their EFFECTIVE
// policy entry (Host rewritten to the generated worktree's own path,
// AllowedBranches replaced with the sandbox's own generated branch, per
// DESIGN.md §15) rather than a straight clone of the profile-declared
// values.
func cloneGitPolicy(p config.GitPolicy) config.GitPolicy {
	var repos []config.GitRepoPolicy
	for _, repo := range p.Repos {
		if repo.Worktree {
			continue
		}
		// "~" expansion (config.ExpandHome), same as every other host path
		// in a profile — a git policy repo authored as "~/projects/foo" on
		// one machine must resolve on whichever machine actually launches
		// this sandbox, not stay a literal, meaningless "~" once it
		// reaches internal/gitproxy's own host-path matching.
		repos = append(repos, config.GitRepoPolicy{
			Host:            config.ExpandHome(repo.Host),
			AllowedBranches: append([]string(nil), repo.AllowedBranches...),
			AllowedRemotes:  append([]string(nil), repo.AllowedRemotes...),
		})
	}
	return config.GitPolicy{Repos: repos}
}

// applyURLDelta returns existing with remove entries dropped and add
// entries appended, de-duplicated, without mutating existing.
func applyURLDelta(existing, add, remove []string) []string {
	removeSet := make(map[string]bool, len(remove))
	for _, u := range remove {
		removeSet[u] = true
	}
	seen := make(map[string]bool, len(existing)+len(add))
	var out []string
	for _, u := range existing {
		if removeSet[u] || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	for _, u := range add {
		if removeSet[u] || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	return out
}
