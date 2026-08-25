package cli

import (
	"github.com/spf13/cobra"

	"github.com/thomkin/muro/internal/control"
)

var logsFollowFlag bool

func init() {
	logsCmd.Flags().BoolVar(&logsFollowFlag, "follow", false, "stream new output as it happens")
	rootCmd.AddCommand(logsCmd)
}

var logsCmd = &cobra.Command{
	Use:   "logs <agent-name>",
	Short: "Show a sandbox's captured output",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := dialControl()
		if err != nil {
			return err
		}
		defer c.Close()

		ns, name := splitNamespaceName(args[0])
		req := control.LogsRequest{Namespace: ns, Name: name, Follow: logsFollowFlag}
		// The daemon doesn't implement log capture/storage yet
		// (internal/control's dispatch returns OK:false for this request
		// type) — this call correctly surfaces that as an error rather
		// than pretending to stream something that doesn't exist.
		return c.Call(control.TypeLogs, req, nil)
	},
}
