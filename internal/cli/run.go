package cli

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/thomkin/muro/internal/config"
	"github.com/thomkin/muro/internal/control"
)

var (
	runProfileFlag   string
	runNameFlag      string
	runNamespaceFlag string
	runAgentFlag     string
	runAgentArgFlags []string
)

func init() {
	runCmd.Flags().StringVar(&runProfileFlag, "profile", "", "profile to launch from (required)")
	runCmd.Flags().StringVar(&runNameFlag, "name", "", "agent name, unique within its namespace (required)")
	runCmd.Flags().StringVar(&runNamespaceFlag, "namespace", "", "namespace (default \"default\")")
	runCmd.Flags().StringVar(&runAgentFlag, "agent", "", "override the profile's agent command")
	runCmd.Flags().StringArrayVar(&runAgentArgFlags, "agent-arg", nil, "override the profile's whole agent_args list for this run, repeatable and in order")
	_ = runCmd.MarkFlagRequired("profile")
	_ = runCmd.MarkFlagRequired("name")
	rootCmd.AddCommand(runCmd)
}

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Launch a new sandboxed agent instance from a profile",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := config.ValidSandboxName("--name", runNameFlag); err != nil {
			return usageErr("%v", err)
		}
		if runNamespaceFlag != "" {
			if err := config.ValidSandboxName("--namespace", runNamespaceFlag); err != nil {
				return usageErr("%v", err)
			}
		}

		c, err := dialControl()
		if err != nil {
			return err
		}
		defer c.Close()

		req := control.SandboxRunRequest{
			Profile:   runProfileFlag,
			Name:      runNameFlag,
			Namespace: runNamespaceFlag,
			Agent:     runAgentFlag,
			AgentArgs: runAgentArgFlags,
		}
		var view control.SandboxView
		if err := c.Call(control.TypeSandboxRun, req, &view); err != nil {
			return err
		}

		if jsonOutput {
			return RenderJSON(os.Stdout, view)
		}
		return renderSandboxTable([]*control.SandboxView{&view})
	},
}
