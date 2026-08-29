// Package gitproxy is the policy engine behind muro's git tool-proxy: a
// sandbox gets no real git binary, only a stub (cmd/muro-toolstub) that
// forwards every invocation to murod over a per-sandbox Unix socket
// (internal/sandbox/toolsocket.go). murod validates the request against a
// two-layer policy — a daemon-global ceiling (config.GitPolicyConfig,
// which subcommands are reachable at all) and a per-profile grant
// (config.GitPolicy, which repos/branches/remotes within those
// subcommands) — and only if allowed, executes the real git on the host,
// unsandboxed, as the real user (so it can use the user's real SSH
// agent/credential helper without ever exposing them to the sandbox).
//
// Path translation is deliberately narrow: only cwd is translated (via the
// profile's existing mounts: table), and git's own `-C <hostRepo>` flag is
// the only thing that needs it. A general mirrored-mount-namespace
// executor was considered and rejected for git specifically — every
// argument accepted by the allowed subcommands below is either a git
// ref/remote name (not a filesystem path at all) or an already-relative
// pathspec that resolves identically once cwd is translated, so there is
// no separate per-argument path-translation problem to solve here.
package gitproxy

import (
	"fmt"
	"path"
	"strings"

	"github.com/thomkin/muro/internal/config"
)

// Request is one tool-proxy invocation forwarded from a sandbox's git stub.
type Request struct {
	Argv []string // e.g. []string{"commit", "-m", "fix bug"} — does NOT include "git" itself
	Cwd  string   // sandbox-side cwd the stub was invoked from
}

// TranslateCwd resolves cwd (a sandbox-side path) to the corresponding
// host path, via the longest mounts entry whose SandboxPath covers it.
// Longest-prefix-wins matters when mounts overlap (e.g. a whole directory
// mounted plus a more specific subdirectory mounted separately elsewhere)
// — the more specific mount is the one that actually determines the host
// path for anything under it.
func TranslateCwd(mounts []config.Mount, cwd string) (hostPath string, ok bool) {
	cwd = path.Clean(cwd)
	bestLen := -1
	var best config.Mount
	for _, m := range mounts {
		sp := path.Clean(m.SandboxPath)
		if cwd != sp && !strings.HasPrefix(cwd, sp+"/") {
			continue
		}
		if len(sp) > bestLen {
			bestLen = len(sp)
			best = m
		}
	}
	if bestLen < 0 {
		return "", false
	}
	sp := path.Clean(best.SandboxPath)
	rest := strings.TrimPrefix(cwd, sp)
	return path.Clean(best.Host + rest), true
}

// ResolveRepo finds the repo policy (if any) whose Host covers hostCwd —
// hostCwd may be the repo root itself or any subdirectory beneath it (the
// agent could `cd` into a subdirectory before invoking git, same as a
// normal git workflow).
func ResolveRepo(hostCwd string, repos []config.GitRepoPolicy) (*config.GitRepoPolicy, bool) {
	hostCwd = path.Clean(hostCwd)
	for i := range repos {
		root := path.Clean(repos[i].Host)
		if hostCwd == root || strings.HasPrefix(hostCwd, root+"/") {
			return &repos[i], true
		}
	}
	return nil, false
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func matchesAny(patterns []string, s string) bool {
	for _, p := range patterns {
		if ok, err := path.Match(p, s); err == nil && ok {
			return true
		}
	}
	return false
}

// pushDestBranch extracts the destination branch name from a push
// refspec's right-hand side: "ai" -> "ai", "local:ai" -> "ai", stripping a
// leading "refs/heads/" if present either way.
func pushDestBranch(branchspec string) string {
	dest := branchspec
	if i := strings.IndexByte(branchspec, ':'); i >= 0 {
		dest = branchspec[i+1:]
	}
	return strings.TrimPrefix(dest, "refs/heads/")
}

// Validate is the pure policy check: no subprocess, no I/O beyond reading
// the arguments given. It returns the resolved host repo path and
// subcommand on success, or an error explaining the rejection. Some
// subcommands (commit, push) need additional runtime checks beyond what's
// checkable here (the current branch; what push would actually update) —
// see Handle (orchestrate.go), which calls this first and then layers
// those on top.
func Validate(req Request, mounts []config.Mount, profilePolicy config.GitPolicy, daemonAllowedSubcommands []string) (hostRepo string, subcommand string, err error) {
	if len(req.Argv) == 0 {
		return "", "", fmt.Errorf("empty git invocation")
	}
	subcommand = req.Argv[0]
	args := req.Argv[1:]

	if !containsString(daemonAllowedSubcommands, subcommand) {
		return "", "", fmt.Errorf("git subcommand %q is not permitted by the daemon's git policy", subcommand)
	}

	hostCwd, ok := TranslateCwd(mounts, req.Cwd)
	if !ok {
		return "", "", fmt.Errorf("cwd %q is not inside any mounted path", req.Cwd)
	}

	repo, ok := ResolveRepo(hostCwd, profilePolicy.Repos)
	if !ok {
		return "", "", fmt.Errorf("no git policy is configured for %q", hostCwd)
	}
	hostRepo = repo.Host

	switch subcommand {
	case "status", "show":
		if len(args) != 0 {
			return "", "", fmt.Errorf("git %s takes no arguments in v1", subcommand)
		}
	case "diff":
		if len(args) != 0 && !(len(args) == 1 && args[0] == "--staged") {
			return "", "", fmt.Errorf("git diff only supports no arguments or exactly \"--staged\" in v1")
		}
	case "log":
		if len(args) != 0 {
			return "", "", fmt.Errorf("git log takes no arguments in v1")
		}
	case "add":
		if len(args) == 0 {
			return "", "", fmt.Errorf("git add requires at least one path")
		}
		for _, a := range args {
			if a == "" || strings.HasPrefix(a, "-") {
				return "", "", fmt.Errorf("git add argument %q looks like a flag, which is not permitted", a)
			}
			if strings.HasPrefix(a, "/") {
				return "", "", fmt.Errorf("git add argument %q is an absolute path — only relative pathspecs are supported, since only cwd is translated", a)
			}
		}
	case "commit":
		if len(args) != 2 || args[0] != "-m" {
			return "", "", fmt.Errorf("git commit only supports exactly \"-m <message>\" in v1")
		}
	case "push":
		if len(args) != 2 {
			return "", "", fmt.Errorf("git push requires exactly <remote> <branch> in v1 (no flags, no bare invocation)")
		}
		remote, branchspec := args[0], args[1]
		if strings.HasPrefix(remote, "-") || strings.HasPrefix(branchspec, "-") {
			return "", "", fmt.Errorf("git push arguments must not look like flags")
		}
		if !containsString(repo.AllowedRemotes, remote) {
			return "", "", fmt.Errorf("remote %q is not permitted for this repo", remote)
		}
		dest := pushDestBranch(branchspec)
		if !matchesAny(repo.AllowedBranches, dest) {
			return "", "", fmt.Errorf("push destination branch %q does not match any allowed branch pattern for this repo", dest)
		}
		// This grammar has no room for --force/-f/--force-with-lease
		// anywhere — force-push is structurally impossible in v1 by
		// construction (exactly two positional arguments, neither allowed
		// to start with "-"), not by a runtime flag check. No
		// AllowForcePush config knob exists yet because there is nothing
		// for it to control.
	case "fetch":
		if len(args) > 1 {
			return "", "", fmt.Errorf("git fetch takes at most one argument (a remote name) in v1")
		}
		if len(args) == 1 {
			if strings.HasPrefix(args[0], "-") {
				return "", "", fmt.Errorf("git fetch argument must not look like a flag")
			}
			if !containsString(repo.AllowedRemotes, args[0]) {
				return "", "", fmt.Errorf("remote %q is not permitted for this repo", args[0])
			}
		}
	case "pull":
		if len(args) > 2 {
			return "", "", fmt.Errorf("git pull takes at most two arguments (<remote> <branch>) in v1")
		}
		for _, a := range args {
			if strings.HasPrefix(a, "-") {
				return "", "", fmt.Errorf("git pull arguments must not look like flags")
			}
		}
		if len(args) >= 1 && !containsString(repo.AllowedRemotes, args[0]) {
			return "", "", fmt.Errorf("remote %q is not permitted for this repo", args[0])
		}
	default:
		// Reachable if daemonAllowedSubcommands names a subcommand this
		// package has no per-subcommand grammar for — a daemon.yaml
		// misconfiguration, not a client-triggerable case in practice
		// since defaultGitAllowedSubcommands only lists subcommands
		// handled above.
		return "", "", fmt.Errorf("git subcommand %q has no defined argument grammar in the tool-proxy", subcommand)
	}

	return hostRepo, subcommand, nil
}
