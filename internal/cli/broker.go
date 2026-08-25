package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/thomkin/muro/internal/control"
)

func init() {
	brokerCmd.AddCommand(brokerStatusCmd)
	rootCmd.AddCommand(brokerCmd)
}

var brokerCmd = &cobra.Command{
	Use:   "broker",
	Short: "Inspect the configured MQTT broker connection",
}

var brokerStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Connectivity to the configured MQTT broker",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := dialControl()
		if err != nil {
			return err
		}
		defer c.Close()

		var resp control.BrokerStatusResponse
		if err := c.Call(control.TypeBrokerStatus, control.BrokerStatusRequest{}, &resp); err != nil {
			return err
		}

		if jsonOutput {
			return RenderJSON(os.Stdout, resp)
		}
		if resp.Connected {
			fmt.Printf("connected to %s\n", resp.Address)
		} else if resp.LastError != "" {
			fmt.Printf("not connected: %s\n", resp.LastError)
		} else {
			fmt.Println("not connected")
		}
		return nil
	},
}
