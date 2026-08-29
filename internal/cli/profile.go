package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/thomkin/muro/internal/config"
)

func init() {
	profileMountCmd.AddCommand(profileMountAddCmd, profileMountRemoveCmd)
	profileCmd.AddCommand(profileCreateCmd, profileEditCmd, profileListCmd, profileShowCmd, profileMountCmd, profileAgentArgsCmd, profileInstructionsCmd, profileSkillsCmd)
	rootCmd.AddCommand(profileCmd)
}

var profileCmd = &cobra.Command{
	Use:   "profile",
	Short: "Manage reusable sandbox profiles (read/written directly as JSON files — no daemon involved, DESIGN.md §9)",
}

var (
	profileAgentFlag        string
	profileAgentArgFlags    []string
	profileMountFlags       []string
	profileToolFlags        []string
	profileAllowURLFlags    []string
	profileAudioFlag        bool
	profileExtendsFlag      string
	profileInstructionsFlag string
	profileSkillFlags       []string
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
			Name:      args[0],
			Extends:   profileExtendsFlag,
			Agent:     profileAgentFlag,
			AgentArgs: profileAgentArgFlags,
			Mounts:    mounts,
			Tools:     tools,
			AllowURLs: profileAllowURLFlags,
			Env:       map[string]string{},
			// RestartPolicy deliberately left unset here, not hardcoded to
			// "never" — LoadProfile only defaults an unset policy once, at
			// the very end of extends resolution; hardcoding it on every
			// created profile would make it always win over an --extends
			// base's own policy, defeating that part of inheritance.
			Audio:        profileAudioFlag,
			Instructions: profileInstructionsFlag,
			Skills:       profileSkillFlags,
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
	profileCreateCmd.Flags().StringVar(&profileExtendsFlag, "extends", "", "base profile to inherit from — this profile's own fields layer on top (see `muro profile show --raw` vs plain `show`)")
	profileCreateCmd.Flags().StringArrayVar(&profileAgentArgFlags, "agent-arg", nil, "extra argv passed to the agent command, repeatable and in order (e.g. --agent-arg --dangerously-skip-permissions) — the sandbox's own OS-level isolation is the real security boundary, so telling the agent it doesn't need to re-prompt for permissions it can't exceed anyway is a legitimate, common choice")
	profileCreateCmd.Flags().StringArrayVar(&profileMountFlags, "mount", nil, "host:sandbox_path:mode, repeatable")
	profileCreateCmd.Flags().StringArrayVar(&profileToolFlags, "tool", nil, "host-path[:as], repeatable (DESIGN.md §10)")
	profileCreateCmd.Flags().StringArrayVar(&profileAllowURLFlags, "allow-url", nil, "URL/host to allow, repeatable")
	profileCreateCmd.Flags().BoolVar(&profileAudioFlag, "audio", false, "grant this sandbox access to the host's PipeWire/PulseAudio socket (opt-in, off by default)")
	profileCreateCmd.Flags().StringVar(&profileInstructionsFlag, "instructions", "", "path to a markdown file mounted at ~/.claude/CLAUDE.md inside the sandbox — read by Claude Code at the start of every session, regardless of which directory this sandbox is pointed at")
	profileCreateCmd.Flags().StringArrayVar(&profileSkillFlags, "skill", nil, "path to a SKILL.md file or a skill directory, repeatable — mounted under ~/.claude/skills/ (Claude Code's Agent Skills mechanism)")
}

var profileAgentArgsCmd = &cobra.Command{
	Use:   "agent-args",
	Short: "Manage the extra argv passed to an existing profile's agent command",
}

var profileAgentArgsSetFlags []string

var profileAgentArgsSetCmd = &cobra.Command{
	Use:   "set <name>",
	Short: "Replace the extra argv passed to an existing profile's agent command",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := config.LoadProfileRaw(args[0])
		if err != nil {
			return err
		}
		p.AgentArgs = profileAgentArgsSetFlags
		if err := config.ValidateProfile(p); err != nil {
			return usageErr("%v", err)
		}
		if err := config.SaveProfile(p); err != nil {
			return err
		}
		fmt.Printf("set %d agent-arg(s) on profile %q — takes effect on the next `muro run` from it, or an already-running sandbox via `muro sandbox restart --from-profile`\n", len(p.AgentArgs), p.Name)
		return nil
	},
}

func init() {
	profileAgentArgsSetCmd.Flags().StringArrayVar(&profileAgentArgsSetFlags, "agent-arg", nil, "extra argv passed to the agent command, repeatable and in order — replaces the profile's whole current list, including clearing it if omitted entirely")
	profileAgentArgsCmd.AddCommand(profileAgentArgsSetCmd)
}

var profileInstructionsCmd = &cobra.Command{
	Use:   "instructions",
	Short: "Manage the markdown file mounted at ~/.claude/CLAUDE.md for an existing profile",
}

var profileInstructionsSetFlag string

var profileInstructionsSetCmd = &cobra.Command{
	Use:   "set <name>",
	Short: "Set (or clear, with --file \"\") the instructions markdown file for a profile",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := config.LoadProfileRaw(args[0])
		if err != nil {
			return err
		}
		p.Instructions = profileInstructionsSetFlag
		if err := config.ValidateProfile(p); err != nil {
			return usageErr("%v", err)
		}
		if err := config.SaveProfile(p); err != nil {
			return err
		}
		if p.Instructions == "" {
			fmt.Printf("cleared instructions on profile %q\n", p.Name)
		} else {
			fmt.Printf("set instructions on profile %q to %q — takes effect on the next `muro run` from it, or an already-running sandbox via `muro sandbox restart --from-profile`\n", p.Name, p.Instructions)
		}
		return nil
	},
}

func init() {
	profileInstructionsSetCmd.Flags().StringVar(&profileInstructionsSetFlag, "file", "", "path to a markdown file (omit or pass \"\" to clear)")
	profileInstructionsCmd.AddCommand(profileInstructionsSetCmd)
}

var profileSkillsCmd = &cobra.Command{
	Use:   "skills",
	Short: "Manage the skill files/directories mounted under ~/.claude/skills/ for an existing profile",
}

var profileSkillsSetFlags []string

var profileSkillsSetCmd = &cobra.Command{
	Use:   "set <name>",
	Short: "Replace the skills list for a profile",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := config.LoadProfileRaw(args[0])
		if err != nil {
			return err
		}
		p.Skills = profileSkillsSetFlags
		if err := config.ValidateProfile(p); err != nil {
			return usageErr("%v", err)
		}
		if err := config.SaveProfile(p); err != nil {
			return err
		}
		fmt.Printf("set %d skill(s) on profile %q — takes effect on the next `muro run` from it, or an already-running sandbox via `muro sandbox restart --from-profile`\n", len(p.Skills), p.Name)
		return nil
	},
}

func init() {
	profileSkillsSetCmd.Flags().StringArrayVar(&profileSkillsSetFlags, "skill", nil, "path to a SKILL.md file or a skill directory, repeatable — replaces the profile's whole current list, including clearing it if omitted entirely")
	profileSkillsCmd.AddCommand(profileSkillsSetCmd)
}

var profileMountCmd = &cobra.Command{
	Use:   "mount",
	Short: "Add or remove mounts on an existing profile without hand-editing its JSON file",
}

var profileMountAddFlags []string

var profileMountAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Add one or more mounts to a profile",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(profileMountAddFlags) == 0 {
			return usageErr("--mount is required, repeatable (host:sandbox_path:mode)")
		}
		p, err := config.LoadProfileRaw(args[0])
		if err != nil {
			return err
		}
		newMounts, err := parseMountFlagsConfig(profileMountAddFlags)
		if err != nil {
			return usageErr("%v", err)
		}

		p.Mounts = append(p.Mounts, newMounts...)
		if err := config.ValidateProfile(p); err != nil {
			return usageErr("%v", err)
		}
		if err := config.SaveProfile(p); err != nil {
			return err
		}
		fmt.Printf("added %d mount(s) to profile %q — takes effect on the next `muro run` from it; an already-running sandbox won't see this until it's stopped and re-run (or given the same mount via `muro sandbox update` + `restart`)\n", len(newMounts), p.Name)
		return nil
	},
}

var profileMountRemoveFlags []string

var profileMountRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove mounts from a profile by their sandbox-side path",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(profileMountRemoveFlags) == 0 {
			return usageErr("--sandbox-path is required, repeatable")
		}
		p, err := config.LoadProfileRaw(args[0])
		if err != nil {
			return err
		}

		removeSet := make(map[string]bool, len(profileMountRemoveFlags))
		for _, sp := range profileMountRemoveFlags {
			removeSet[sp] = true
		}
		kept := make([]config.Mount, 0, len(p.Mounts))
		removed := 0
		for _, m := range p.Mounts {
			if removeSet[m.SandboxPath] {
				removed++
				continue
			}
			kept = append(kept, m)
		}
		p.Mounts = kept

		if err := config.ValidateProfile(p); err != nil {
			return usageErr("%v", err)
		}
		if err := config.SaveProfile(p); err != nil {
			return err
		}
		fmt.Printf("removed %d mount(s) from profile %q — takes effect on the next `muro run` from it; an already-running sandbox keeps its existing mounts until it's stopped and re-run\n", removed, p.Name)
		return nil
	},
}

func init() {
	profileMountAddCmd.Flags().StringArrayVar(&profileMountAddFlags, "mount", nil, "host:sandbox_path:mode, repeatable")
	profileMountRemoveCmd.Flags().StringArrayVar(&profileMountRemoveFlags, "sandbox-path", nil, "sandbox-side path of a mount to remove, repeatable")
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

		if err := openInEditor(path); err != nil {
			return err
		}

		// Re-validate after editing, since the file was hand-edited
		// (DESIGN.md §10: profile JSON files can be hand-edited directly,
		// so validation runs both client- and daemon-side). Two checks:
		// the raw file itself must still be valid JSON, and — separately —
		// its whole extends chain (if any) must still resolve and the
		// FULLY MERGED result must validate, since a merge can introduce
		// issues (e.g. a mount collision) neither half has on its own.
		if _, err := config.LoadProfileRaw(args[0]); err != nil {
			return fmt.Errorf("profile is no longer valid JSON after editing: %w", err)
		}
		p, err := config.LoadProfile(args[0])
		if err != nil {
			return fmt.Errorf("profile's extends chain no longer resolves after editing: %w", err)
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

var profileShowRawFlag bool

var profileShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show one profile's full contents (the effective, extends-resolved config by default)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var p *config.Profile
		var err error
		if profileShowRawFlag {
			p, err = config.LoadProfileRaw(args[0])
		} else {
			p, err = config.LoadProfile(args[0])
		}
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

func init() {
	profileShowCmd.Flags().BoolVar(&profileShowRawFlag, "raw", false, "show exactly what this profile's own file declares, without resolving extends")
}

func profileFilePath(name string) (string, error) {
	dir, err := config.ProfilesDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".json"), nil
}
