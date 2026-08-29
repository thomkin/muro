package gitproxy

import (
	"context"

	"github.com/thomkin/muro/internal/config"
)

// Result is what Handle returns to the socket layer (internal/sandbox/
// toolsocket.go), which relays it to the sandbox's git stub. OK is false
// only for a policy rejection or a genuine infrastructure failure (git not
// found, etc.) — a nonzero ExitCode from a real, permitted git invocation
// is still OK: true, exactly the way running git locally would report a
// nonzero exit without that being an "error" from the shell's point of
// view.
type Result struct {
	OK       bool
	Error    string // set when !OK — policy rejection reason, or a git-level failure summary
	Stdout   string
	Stderr   string
	ExitCode int
}

// Handle validates req, then — subcommand-dependent — performs the extra
// pre-flight checks Validate alone can't (branch check for commit,
// dry-run+porcelain check for push) before actually executing. This
// mirrors how internal/sandbox/agentsocket.go distinguishes "rejected"
// (a Result/Response with an Error string) from a genuine Go error: there
// is no error return here at all — every outcome, including "git isn't
// installed," is expressed as a Result so the socket handler always has
// exactly one thing to marshal back.
func Handle(ctx context.Context, req Request, mounts []config.Mount, profilePolicy config.GitPolicy, daemonAllowedSubcommands []string) Result {
	hostRepo, subcommand, err := Validate(req, mounts, profilePolicy, daemonAllowedSubcommands)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}

	switch subcommand {
	case "commit":
		repo, _ := ResolveRepo(hostRepo, profilePolicy.Repos) // hostRepo == repo.Host by construction of Validate
		if err := CheckCurrentBranch(ctx, hostRepo, repo.AllowedBranches); err != nil {
			return Result{OK: false, Error: err.Error()}
		}
		return runResult(RunGit(ctx, hostRepo, req.Argv))

	case "push":
		repo, _ := ResolveRepo(hostRepo, profilePolicy.Repos)
		dryStdout, dryStderr, dryExit, dryErr := RunGit(ctx, hostRepo, pushArgsWithDryRun(req.Argv))
		if dryErr != nil {
			return Result{OK: false, Error: "push dry-run: " + dryErr.Error()}
		}
		if dryExit != 0 {
			return Result{OK: false, Error: "push dry-run failed: " + dryStderr}
		}
		updates, perr := ParsePushPorcelain(dryStdout)
		if perr != nil {
			return Result{OK: false, Error: "push dry-run: " + perr.Error()}
		}
		if err := CheckPushPlan(updates, repo.AllowedBranches); err != nil {
			return Result{OK: false, Error: err.Error()}
		}
		return runResult(RunGit(ctx, hostRepo, req.Argv))

	default:
		return runResult(RunGit(ctx, hostRepo, req.Argv))
	}
}

func runResult(stdout, stderr string, exitCode int, err error) Result {
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	return Result{OK: true, Stdout: stdout, Stderr: stderr, ExitCode: exitCode}
}
