package cli

import (
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"

	"github.com/thomkin/muro/internal/control"
)

func init() {
	sandboxCmd.AddCommand(sandboxListCmd, sandboxShowCmd, sandboxUpdateCmd, sandboxReloadCmd,
		sandboxRestartCmd, sandboxStopCmd, sandboxAttachCmd)
	rootCmd.AddCommand(sandboxCmd, psCmd)
}

var sandboxCmd = &cobra.Command{
	Use:   "sandbox",
	Short: "Manage running sandboxes",
}

var sandboxNamespaceFlag string

var sandboxListCmd = &cobra.Command{
	Use:   "list",
	Short: "List sandboxes",
	Args:  cobra.NoArgs,
	RunE:  runSandboxList,
}

// ps is an alias for `sandbox list`, DESIGN.md §9.
var psCmd = &cobra.Command{
	Use:   "ps",
	Short: "Alias for `sandbox list`",
	Args:  cobra.NoArgs,
	RunE:  runSandboxList,
}

func runSandboxList(cmd *cobra.Command, args []string) error {
	c, err := dialControl()
	if err != nil {
		return err
	}
	defer c.Close()

	var resp control.StatusResponse
	req := control.StatusRequest{Namespace: sandboxNamespaceFlag}
	if err := c.Call(control.TypeStatus, req, &resp); err != nil {
		return err
	}
	if jsonOutput {
		return RenderJSON(os.Stdout, resp)
	}
	return renderSandboxTable(resp.Sandboxes)
}

func init() {
	sandboxListCmd.Flags().StringVar(&sandboxNamespaceFlag, "namespace", "", "restrict to one namespace")
	psCmd.Flags().StringVar(&sandboxNamespaceFlag, "namespace", "", "restrict to one namespace")
}

var sandboxShowCmd = &cobra.Command{
	Use:   "show <agent-name>",
	Short: "Show one sandbox's full detail",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := dialControl()
		if err != nil {
			return err
		}
		defer c.Close()

		ns, name := splitNamespaceName(args[0])
		var view control.SandboxView
		req := control.SandboxShowRequest{Namespace: ns, Name: name}
		if err := c.Call(control.TypeSandboxShow, req, &view); err != nil {
			return err
		}
		if jsonOutput {
			return RenderJSON(os.Stdout, view)
		}
		return renderSandboxTable([]*control.SandboxView{&view})
	},
}

// --- sandbox update ---

var (
	updateProfileFlag   string
	updateAllFlag       bool
	updateMountFlags    []string
	updateAllowURLFlags []string
	updateDenyURLFlags  []string
)

var sandboxUpdateCmd = &cobra.Command{
	Use:   "update [<agent-name>]",
	Short: "Update mounts/URL allowlist on one sandbox, every sandbox from a profile (--profile), or every sandbox (--all)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sel, ns, err := resolveUpdateSelector(args)
		if err != nil {
			return err
		}

		mounts, err := parseMountFlags(updateMountFlags)
		if err != nil {
			return usageErr("%v", err)
		}

		c, err := dialControl()
		if err != nil {
			return err
		}
		defer c.Close()

		req := control.SandboxUpdateRequest{
			Selector:  sel,
			Namespace: ns,
			Mounts:    mounts,
			AllowURLs: updateAllowURLFlags,
			DenyURLs:  updateDenyURLFlags,
		}
		var resp control.SandboxUpdateResponse
		if err := c.Call(control.TypeSandboxUpdate, req, &resp); err != nil {
			return err
		}

		if jsonOutput {
			return RenderJSON(os.Stdout, resp)
		}
		headers := []string{"NAMESPACE/NAME", "APPLIED"}
		rows := make([][]string, 0, len(resp.Results))
		for _, r := range resp.Results {
			applied := "yes"
			if !r.Applied {
				applied = "no (needs `muro sandbox restart`)"
			}
			rows = append(rows, []string{r.Namespace + "/" + r.Name, applied})
		}
		return RenderTable(os.Stdout, headers, rows)
	},
}

// resolveUpdateSelector implements DESIGN.md §11's selector: exactly one
// of a positional <agent-name>, --profile, or --all.
func resolveUpdateSelector(args []string) (control.UpdateSelector, string, error) {
	set := 0
	if len(args) == 1 {
		set++
	}
	if updateProfileFlag != "" {
		set++
	}
	if updateAllFlag {
		set++
	}
	if set == 0 {
		return control.UpdateSelector{}, "", usageErr("sandbox update requires exactly one of: <agent-name>, --profile, --all")
	}
	if set > 1 {
		return control.UpdateSelector{}, "", usageErr("sandbox update accepts only one of: <agent-name>, --profile, --all — not more than one at a time")
	}

	if len(args) == 1 {
		ns, name := splitNamespaceName(args[0])
		return control.UpdateSelector{Name: name}, ns, nil
	}
	if updateProfileFlag != "" {
		return control.UpdateSelector{Profile: updateProfileFlag}, sandboxNamespaceFlag, nil
	}
	return control.UpdateSelector{All: true}, sandboxNamespaceFlag, nil
}

func init() {
	sandboxUpdateCmd.Flags().StringVar(&updateProfileFlag, "profile", "", "target every running sandbox launched from this profile")
	sandboxUpdateCmd.Flags().BoolVar(&updateAllFlag, "all", false, "target every running sandbox, regardless of profile")
	sandboxUpdateCmd.Flags().StringVar(&sandboxNamespaceFlag, "namespace", "", "scope --profile/--all to one namespace")
	sandboxUpdateCmd.Flags().StringArrayVar(&updateMountFlags, "mount", nil, "host:sandbox_path:mode, repeatable")
	sandboxUpdateCmd.Flags().StringArrayVar(&updateAllowURLFlags, "allow-url", nil, "URL/host to add to the allowlist, repeatable")
	sandboxUpdateCmd.Flags().StringArrayVar(&updateDenyURLFlags, "deny-url", nil, "URL/host to remove from the allowlist, repeatable")
}

var sandboxReloadCmd = &cobra.Command{
	Use:   "reload <agent-name>",
	Short: "Apply pending config live where possible",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := dialControl()
		if err != nil {
			return err
		}
		defer c.Close()

		ns, name := splitNamespaceName(args[0])
		var resp control.SandboxReloadResponse
		req := control.SandboxReloadRequest{Namespace: ns, Name: name}
		if err := c.Call(control.TypeSandboxReload, req, &resp); err != nil {
			return err
		}
		if jsonOutput {
			return RenderJSON(os.Stdout, resp)
		}
		if resp.Applied {
			fmt.Println("applied live")
		} else {
			fmt.Println("still pending — needs `muro sandbox restart`")
		}
		return nil
	},
}

var sandboxRestartCmd = &cobra.Command{
	Use:   "restart <agent-name>",
	Short: "Apply everything, including non-hot-reloadable mounts",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := dialControl()
		if err != nil {
			return err
		}
		defer c.Close()

		ns, name := splitNamespaceName(args[0])
		req := control.SandboxRestartRequest{Namespace: ns, Name: name}
		if err := c.Call(control.TypeSandboxRestart, req, nil); err != nil {
			return err
		}
		fmt.Printf("restarted %s/%s\n", nsOrDefault(ns), name)
		return nil
	},
}

var sandboxStopCmd = &cobra.Command{
	Use:   "stop <agent-name>",
	Short: "Stop a sandbox",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := dialControl()
		if err != nil {
			return err
		}
		defer c.Close()

		ns, name := splitNamespaceName(args[0])
		req := control.SandboxStopRequest{Namespace: ns, Name: name}
		if err := c.Call(control.TypeSandboxStop, req, nil); err != nil {
			return err
		}
		fmt.Printf("stopped %s/%s\n", nsOrDefault(ns), name)
		return nil
	},
}

var sandboxAttachCmd = &cobra.Command{
	Use:   "attach <agent-name>",
	Short: "Take over the sandbox's interactive session (Ctrl-P Ctrl-Q to detach without stopping it)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := dialControl()
		if err != nil {
			return err
		}
		defer c.Close()

		ns, name := splitNamespaceName(args[0])
		r, w, err := c.Attach(ns, name)
		if err != nil {
			return err
		}

		restore, err := setRawMode(os.Stdin)
		if err != nil {
			// Not every environment has a real terminal on stdin (e.g. a
			// test harness, or output piped) — proceed without raw mode
			// rather than failing outright; interactive control just won't
			// be byte-perfect.
			restore = func() {}
		}
		defer restore()

		// Restore the terminal on Ctrl-C/SIGTERM too, so a killed `muro
		// sandbox attach` never leaves the user's terminal in raw mode.
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt)
		defer signal.Stop(sigCh)
		go func() {
			<-sigCh
			restore()
			os.Exit(ExitGeneralError)
		}()

		return pumpAttach(os.Stdin, os.Stdout, r, w)
	},
}

func nsOrDefault(ns string) string {
	if ns == "" {
		return "default"
	}
	return ns
}
