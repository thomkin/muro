package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/thomkin/muro/internal/config"
)

func init() {
	profileCmd.AddCommand(profileCreateCmd, profileEditCmd, profileListCmd, profileShowCmd)
	rootCmd.AddCommand(profileCmd)
}

var profileCmd = &cobra.Command{
	Use:   "profile",
	Short: "Manage reusable sandbox profiles (read/written directly as JSON files — no daemon involved, DESIGN.md §9)",
}

var (
	profileAgentFlag     string
	profileMountFlags    []string
	profileToolFlags     []string
	profileAllowURLFlags []string
)

var profileCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a new profile",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		mounts, err := parseMountFlagsConfig(profileMountFlags)
		if err != nil {
			return usageErr("%v", err)
		}
		tools, err := parseToolFlagsConfig(profileToolFlags)
		if err != nil {
			return usageErr("%v", err)
		}

		p := &config.Profile{
			Name:          args[0],
			Agent:         profileAgentFlag,
			Mounts:        mounts,
			Tools:         tools,
			AllowURLs:     profileAllowURLFlags,
			Env:           map[string]string{},
			RestartPolicy: "never",
		}
		if err := config.ValidateProfile(p); err != nil {
			return usageErr("%v", err)
		}
		if err := config.SaveProfile(p); err != nil {
			return err
		}

		path, _ := profileFilePath(p.Name)
		fmt.Printf("wrote %s\n", path)
		return nil
	},
}

func init() {
	profileCreateCmd.Flags().StringVar(&profileAgentFlag, "agent", "", "agent command this profile launches (claude|gemini|custom...)")
	profileCreateCmd.Flags().StringArrayVar(&profileMountFlags, "mount", nil, "host:sandbox_path:mode, repeatable")
	profileCreateCmd.Flags().StringArrayVar(&profileToolFlags, "tool", nil, "host-path[:as], repeatable (DESIGN.md §10)")
	profileCreateCmd.Flags().StringArrayVar(&profileAllowURLFlags, "allow-url", nil, "URL/host to allow, repeatable")
}

var profileEditCmd = &cobra.Command{
	Use:   "edit <name>",
	Short: "Open a profile's JSON file in $EDITOR",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		path, err := profileFilePath(args[0])
		if err != nil {
			return err
		}
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("profile %q: %w", args[0], err)
		}

		editor := os.Getenv("EDITOR")
		if editor == "" {
			editor = "vi"
		}
		c := exec.Command(editor, path)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := c.Run(); err != nil {
			return fmt.Errorf("running %s: %w", editor, err)
		}

		// Re-validate after editing, since the file was hand-edited
		// (DESIGN.md §10: profile JSON files can be hand-edited directly,
		// so validation runs both client- and daemon-side).
		p, err := config.LoadProfile(args[0])
		if err != nil {
			return fmt.Errorf("profile is no longer valid JSON after editing: %w", err)
		}
		if err := config.ValidateProfile(p); err != nil {
			return usageErr("profile is invalid after editing: %v", err)
		}
		return nil
	},
}

var profileListCmd = &cobra.Command{
	Use:   "list",
	Short: "List profile names",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		names, err := config.ListProfiles()
		if err != nil {
			return err
		}
		if jsonOutput {
			return RenderJSON(os.Stdout, names)
		}
		rows := make([][]string, 0, len(names))
		for _, n := range names {
			rows = append(rows, []string{n})
		}
		return RenderTable(os.Stdout, []string{"NAME"}, rows)
	},
}

var profileShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show one profile's full contents",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := config.LoadProfile(args[0])
		if err != nil {
			return err
		}
		if jsonOutput {
			return RenderJSON(os.Stdout, p)
		}
		headers := []string{"NAME", "AGENT", "MOUNTS", "TOOLS", "ALLOW_URLS", "RESTART_POLICY"}
		row := []string{
			p.Name, p.Agent,
			strconv.Itoa(len(p.Mounts)), strconv.Itoa(len(p.Tools)), strconv.Itoa(len(p.AllowURLs)),
			p.RestartPolicy,
		}
		return RenderTable(os.Stdout, headers, [][]string{row})
	},
}

func profileFilePath(name string) (string, error) {
	dir, err := config.ProfilesDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".json"), nil
}
