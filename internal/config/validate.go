package config

import (
	"fmt"
)

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
	}

	return nil
}
