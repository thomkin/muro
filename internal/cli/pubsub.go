package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/spf13/cobra"

	"github.com/thomkin/muro/internal/sandbox"
)

// pubsub publish is deliberately NOT a thin client over murod's control API
// (unlike every other command in this package — see root.go's package
// comment): it's meant to run FROM INSIDE a sandbox, where the control
// socket isn't reachable at all (bwrap's mount namespace only exposes what
// a profile explicitly mounts in). Instead it dials
// sandbox.AgentSocketMountPath, the fixed path every pub/sub-enabled
// sandbox gets its own dedicated agent socket mounted at
// (internal/sandbox/bwrap.go), and speaks the narrow newline-JSON protocol
// internal/sandbox/agentsocket.go defines — reusing its exact
// AgentPublishRequest/AgentPublishResponse types rather than redefining an
// equivalent shape here, so the two sides of this protocol can never
// silently drift apart.

var (
	pubsubToFlag        string
	pubsubBroadcastFlag string
)

func init() {
	pubsubPublishCmd.Flags().StringVar(&pubsubToFlag, "to", "", "target agent, \"namespace/name\" or bare \"name\" (defaults to your own namespace) — mutually exclusive with --broadcast")
	pubsubPublishCmd.Flags().StringVar(&pubsubBroadcastFlag, "broadcast", "", "free-form topic under your own namespace — mutually exclusive with --to")
	pubsubCmd.AddCommand(pubsubPublishCmd)
	rootCmd.AddCommand(pubsubCmd)
}

var pubsubCmd = &cobra.Command{
	Use:   "pubsub",
	Short: "Agent-to-agent MQTT messaging (must be run from inside a sandbox)",
}

var pubsubPublishCmd = &cobra.Command{
	Use:   "publish <message>",
	Short: "Publish a message to another agent's inbox or to a broadcast topic",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if (pubsubToFlag == "") == (pubsubBroadcastFlag == "") {
			return usageErr("exactly one of --to or --broadcast must be set")
		}

		conn, err := net.DialTimeout("unix", sandbox.AgentSocketMountPath, 5*time.Second)
		if err != nil {
			return &cliError{code: ExitSocketUnreachable, err: fmt.Errorf("agent socket unreachable at %s (are you running this inside a muro sandbox with pub/sub enabled?): %w", sandbox.AgentSocketMountPath, err)}
		}
		defer conn.Close()

		req := sandbox.AgentPublishRequest{
			To:        pubsubToFlag,
			Broadcast: pubsubBroadcastFlag,
			Message:   args[0],
		}
		data, err := json.Marshal(req)
		if err != nil {
			return err
		}
		data = append(data, '\n')

		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := conn.Write(data); err != nil {
			return fmt.Errorf("write to agent socket: %w", err)
		}

		line, err := bufio.NewReader(conn).ReadBytes('\n')
		if err != nil {
			return fmt.Errorf("read response from agent socket: %w", err)
		}

		var resp sandbox.AgentPublishResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			return fmt.Errorf("malformed response from agent socket: %w", err)
		}
		if !resp.OK {
			return fmt.Errorf("publish rejected: %s", resp.Error)
		}

		if jsonOutput {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(resp)
		}
		fmt.Fprintln(cmd.OutOrStdout(), "published")
		return nil
	},
}
