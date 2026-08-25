package cli

import (
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/thomkin/muro/internal/control"
)

func init() {
	rootCmd.AddCommand(statusCmd)
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show every sandbox across all namespaces (table: id, agent, state, uptime, mounts, urls, pending-reload)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := dialControl()
		if err != nil {
			return err
		}
		defer c.Close()

		var resp control.StatusResponse
		if err := c.Call(control.TypeStatus, control.StatusRequest{}, &resp); err != nil {
			return err
		}

		if jsonOutput {
			return RenderJSON(os.Stdout, resp)
		}
		return renderSandboxTable(resp.Sandboxes)
	},
}

func renderSandboxTable(sbs []*control.SandboxView) error {
	headers := []string{"NAMESPACE/NAME", "AGENT", "STATE", "STARTED", "MOUNTS", "URLS", "RESTARTS"}
	rows := make([][]string, 0, len(sbs))
	for _, sb := range sbs {
		rows = append(rows, []string{
			sb.Namespace + "/" + sb.Name,
			sb.Agent,
			sb.State,
			sb.StartedAt,
			strconv.Itoa(len(sb.Mounts)),
			strconv.Itoa(len(sb.AllowURLs)),
			strconv.Itoa(sb.RestartCount),
		})
	}
	return RenderTable(os.Stdout, headers, rows)
}
