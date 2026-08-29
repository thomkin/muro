package config

// GitRepoPolicy scopes what the git tool-proxy allows a sandbox to do
// against one specific host git repository (internal/gitproxy is the
// engine that enforces this; internal/sandbox/toolsocket.go is the
// transport that reaches it).
type GitRepoPolicy struct {
	// Host is the host path to the repo's working tree. For Worktree ==
	// false, this must be covered by (under, or equal to) one of the
	// profile's mounts: entries (enforced by ValidateProfile), since
	// cwd-translation can only ever resolve a sandbox path that lands
	// inside a mounted host path. For Worktree == true, this is instead
	// the SOURCE repo a fresh worktree is created from — it does not need
	// to be covered by mounts:, since the sandbox never sees it directly
	// (DESIGN.md §15).
	Host string `json:"host"`
	// AllowedBranches are glob patterns (path.Match syntax), e.g. "ai" or
	// "ai/*" — a commit or push's destination branch must match at least
	// one of these. Must be left empty when Worktree is true: the
	// worktree's branch is muro-generated (agent/<namespace>/<name>), so
	// there's nothing meaningful for a profile author to declare here —
	// ValidateProfile rejects a non-empty value instead of silently
	// ignoring it (DESIGN.md §15).
	AllowedBranches []string `json:"allowed_branches"`
	// AllowedRemotes are the remote names usable with push/fetch/pull for
	// this repo, e.g. "origin". May legitimately be empty when Worktree is
	// true (no remote access at all, pure local commits) — merging back to
	// the real repo is a host-side muro operation, not a sandbox push.
	AllowedRemotes []string `json:"allowed_remotes"`
	// Worktree opts this repo into DESIGN.md §15's isolation: instead of
	// exposing Host directly, muro creates a fresh `git worktree` on its
	// own branch and mounts THAT at MountPath — the real checkout (and
	// "main"/whatever branch it's on) is never reachable from inside the
	// sandbox. Default false: exactly today's behavior (Host mounted
	// directly via a profile-declared mounts: entry).
	Worktree bool `json:"worktree,omitempty"`
	// MountPath is the sandbox-internal path the generated worktree is
	// mounted at. Required when Worktree is true, rejected (must be empty)
	// otherwise — a non-worktree entry's sandbox path comes from whatever
	// mounts: entry already covers Host, so a separate MountPath here would
	// be redundant.
	MountPath string `json:"mount_path,omitempty"`
}

// GitPolicy is a profile's git tool-proxy configuration. The zero value
// (nil Repos) means default-deny: no repository is reachable through the
// proxy, and — per internal/sandbox's wiring — the git stub isn't even
// mounted into the sandbox at all in that case.
type GitPolicy struct {
	Repos []GitRepoPolicy `json:"repos,omitempty"`
}
