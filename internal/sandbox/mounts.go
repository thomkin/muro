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
			out = append(out, config.Mount{Host: t.Host, SandboxPath: toolRoot, Mode: "ro"})
		default:
			out = append(out, config.Mount{Host: t.Host, SandboxPath: toolRoot + "/" + t.As, Mode: "ro"})
		}
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
