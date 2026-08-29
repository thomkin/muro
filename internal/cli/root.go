// Package cli implements the muro CLI: a thin client over murod's control
// API (internal/control) plus direct profile-file management
// (internal/config) — DESIGN.md §9. Every subcommand except profile
// management and `daemon start` maps to exactly one control API request
// type (internal/control/protocol.go).
package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/thomkin/muro/internal/control"
)

// Exit codes, IMPLEMENTATION.md §9.
const (
	ExitOK                = 0
	ExitGeneralError      = 1
	ExitUsageError        = 2
	ExitSocketUnreachable = 3
)

// cliError carries an explicit exit code through cobra's plain-error
// RunE contract. Commands that need a specific code (usage problems,
// socket-unreachable) return one via usageErr/socketErr; anything else
// falls back to ExitGeneralError.
type cliError struct {
	code int
	err  error
}

func (e *cliError) Error() string { return e.err.Error() }
func (e *cliError) Unwrap() error { return e.err }

func usageErr(format string, a ...any) error {
	return &cliError{code: ExitUsageError, err: fmt.Errorf(format, a...)}
}

func socketErr(err error) error {
	return &cliError{code: ExitSocketUnreachable, err: fmt.Errorf("murod not running (%w) — try `muro daemon start`", err)}
}

var jsonOutput bool

var rootCmd = &cobra.Command{
	Use:           "muro",
	Short:         "muro — sandboxed multi-agent runtime",
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	rootCmd.PersistentFlags().BoolVar(&jsonOutput, "json", false, "output JSON instead of a table")
}

// Execute runs the CLI and returns the process exit code (IMPLEMENTATION.md
// §9) — cmd/muro/main.go calls this and os.Exits with the result, since a
// bare return from main() always exits 0.
func Execute() int {
	err := rootCmd.Execute()
	if err == nil {
		return ExitOK
	}
	var ce *cliError
	if errors.As(err, &ce) {
		fmt.Fprintln(os.Stderr, "Error:", ce.err)
		return ce.code
	}
	fmt.Fprintln(os.Stderr, "Error:", err)
	return ExitGeneralError
}

// dialControl connects to murod's control socket, or returns a socketErr
// (exit code 3) if it's unreachable.
func dialControl() (*control.Client, error) {
	c, err := control.Dial(control.ResolveSocketPath())
	if err != nil {
		return nil, socketErr(err)
	}
	return c, nil
}

// splitNamespaceName parses a `<agent-name>` or `<namespace>/<agent-name>`
// CLI argument into (namespace, name) — a bare name defaults its
// namespace to "" here (the control server applies the "default" default
// itself, DESIGN.md §9), so an explicit namespace always wins.
func splitNamespaceName(arg string) (namespace, name string) {
	for i := 0; i < len(arg); i++ {
		if arg[i] == '/' {
			return arg[:i], arg[i+1:]
		}
	}
	return "", arg
}
