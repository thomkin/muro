package cli

import (
	"io"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
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
		stream, err := c.Logs(ns, name, logsFollowFlag)
		if err != nil {
			return err
		}

		if logsFollowFlag {
			// Ctrl-C should end a --follow session cleanly, not leave the
			// process to be killed mid-copy. Closing the connection
			// unblocks io.Copy below with a read error, which we then
			// treat as a normal end of stream, not a command failure.
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt)
			defer signal.Stop(sigCh)
			go func() {
				<-sigCh
				_ = c.Close()
			}()
		}

		_, err = io.Copy(os.Stdout, stream)
		if err != nil && !logsFollowFlag {
			// Without --follow the copy is expected to run to a clean EOF;
			// any error here is a real problem worth surfacing.
			return err
		}
		// With --follow, the stream only ever "ends" via Ctrl-C (above) or
		// the server/daemon going away — both show up as an io.Copy error
		// but are the expected way this command finishes, not a failure.
		return nil
	},
}
