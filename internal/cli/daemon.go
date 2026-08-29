package cli

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/thomkin/muro/internal/control"
)

func init() {
	daemonCmd.AddCommand(daemonStartCmd, daemonStopCmd, daemonStatusCmd)
	rootCmd.AddCommand(daemonCmd)
}

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Manage the murod daemon",
}

// daemonStartCmd is the one command that is NOT a control API call
// (IMPLEMENTATION.md §13's documented exception): if murod isn't running,
// there's no socket to call yet. It tries `systemctl start murod` first
// (DESIGN.md §8 ships a system-wide, not --user, systemd unit), then falls
// back to forking murod directly as a detached background process, then
// polls the control socket until it's reachable or a timeout elapses.
var daemonStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start murod",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if isSocketReachable() {
			fmt.Println("murod is already running")
			return nil
		}

		if err := exec.Command("systemctl", "start", "murod").Run(); err != nil {
			// systemctl unavailable, no unit installed, or needs a
			// privilege this invocation doesn't have — fall back to
			// forking murod directly rather than failing outright.
			if startErr := startDetached(); startErr != nil {
				return fmt.Errorf("systemctl start murod failed (%v), and forking murod directly also failed: %w", err, startErr)
			}
		}

		if waitForSocket(5 * time.Second) {
			fmt.Println("murod started")
			return nil
		}
		return fmt.Errorf("started murod but its control socket never became reachable within 5s")
	},
}

func startDetached() error {
	path, err := exec.LookPath("murod")
	if err != nil {
		return fmt.Errorf("murod not found on PATH: %w", err)
	}
	c := exec.Command(path)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // new session, detached from this CLI invocation
	c.Stdin, c.Stdout, c.Stderr = nil, nil, nil
	return c.Start()
}

func waitForSocket(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if isSocketReachable() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return isSocketReachable()
}

func isSocketReachable() bool {
	c, err := control.Dial(control.ResolveSocketPath())
	if err != nil {
		return false
	}
	defer c.Close()
	return true
}

var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop murod",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := control.Dial(control.ResolveSocketPath())
		if err == nil {
			defer c.Close()
			if callErr := c.Call(control.TypeDaemonShutdown, control.DaemonShutdownRequest{}, nil); callErr == nil {
				fmt.Println("murod stopped")
				return nil
			}
		}
		// Socket unreachable, or the shutdown call itself failed — fall
		// back to systemctl.
		if err := exec.Command("systemctl", "stop", "murod").Run(); err != nil {
			return fmt.Errorf("could not stop murod via control API or systemctl: %w", err)
		}
		fmt.Println("murod stopped")
		return nil
	},
}

// daemonStatusCmd is a pure socket-reachability check — "not running" is
// reported, not treated as an error, when the socket can't be dialed.
var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Report whether murod is running",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := control.Dial(control.ResolveSocketPath())
		if err != nil {
			if jsonOutput {
				return RenderJSON(os.Stdout, map[string]any{"running": false})
			}
			fmt.Println("murod: not running")
			return nil
		}
		defer c.Close()

		var resp control.StatusResponse
		callErr := c.Call(control.TypeStatus, control.StatusRequest{}, &resp)
		if jsonOutput {
			out := map[string]any{"running": callErr == nil}
			if callErr == nil {
				out["sandboxes"] = len(resp.Sandboxes)
			}
			return RenderJSON(os.Stdout, out)
		}
		if callErr != nil {
			fmt.Println("murod: socket reachable but not responding correctly:", callErr)
			return nil
		}
		fmt.Printf("murod: running (%d sandbox(es))\n", len(resp.Sandboxes))
		return nil
	},
}
