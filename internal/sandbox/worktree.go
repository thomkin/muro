package sandbox

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/thomkin/muro/internal/config"
	"github.com/thomkin/muro/internal/state"
	"github.com/thomkin/muro/internal/worktree"
)

// worktreesBase returns the host directory under which every one of
// sandboxID's git worktrees live — mirrors privateDirsBase's shape
// (privatedirs.go), the established "everything owned by this sandbox
// lives under StateDir, namespaced by sandbox ID" convention (DESIGN.md
// §15: deliberately not next to the real repo).
func worktreesBase(stateDir, sandboxID string) string {
	return filepath.Join(stateDir, "sandboxes", sandboxID, "worktrees")
}

// worktreeBranch is the single, muro-generated branch every worktree for a
// given sandbox uses, for every repo it has — deterministic from the
// sandbox's own namespace/name, never author-configurable (DESIGN.md §15
// refinement 1: a profile author can't know this name in advance, so
// letting them declare allowed_branches themselves would be a footgun, not
// a feature).
func worktreeBranch(namespace, name string) string {
	return "agent/" + namespace + "/" + name
}

// WorktreeMounts resolves every worktree: true repo in repos into: the
// bind mount for its generated worktree (Host = the worktree's own real
// path, NOT repo.Host — that's what's actually mounted at MountPath, and
// what the git tool-proxy's cwd-translation must resolve against), the
// EFFECTIVE GitRepoPolicy entry the tool-proxy should enforce (Host
// rewritten the same way, AllowedBranches replaced with exactly this
// sandbox's own generated branch), and a state.WorktreeInfo recording
// enough to later show/merge/delete-guard it. Idempotent via
// internal/worktree.Create: a restart reusing the same sandboxID's own
// worktree never re-creates or disturbs it.
func WorktreeMounts(ctx context.Context, stateDir, sandboxID, namespace, name string, repos []config.GitRepoPolicy) ([]config.Mount, []config.GitRepoPolicy, []state.WorktreeInfo, error) {
	var mounts []config.Mount
	var effectiveRepos []config.GitRepoPolicy
	var infos []state.WorktreeInfo

	branch := worktreeBranch(namespace, name)
	for _, repo := range repos {
		if !repo.Worktree {
			continue
		}
		repoHost := config.ExpandHome(repo.Host)
		worktreeHost := filepath.Join(worktreesBase(stateDir, sandboxID), filepath.Base(repo.MountPath))

		baseBranch, err := worktree.Create(ctx, repoHost, worktreeHost, branch)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("create worktree for %q: %w", repo.Host, err)
		}

		mounts = append(mounts, config.Mount{Host: worktreeHost, SandboxPath: repo.MountPath, Mode: "rw"})
		effectiveRepos = append(effectiveRepos, config.GitRepoPolicy{
			Host:            worktreeHost,
			AllowedBranches: []string{branch},
			AllowedRemotes:  append([]string(nil), repo.AllowedRemotes...),
		})
		infos = append(infos, state.WorktreeInfo{
			MountPath:  repo.MountPath,
			Host:       worktreeHost,
			RepoHost:   repoHost,
			Branch:     branch,
			BaseBranch: baseBranch,
		})
	}
	return mounts, effectiveRepos, infos, nil
}
