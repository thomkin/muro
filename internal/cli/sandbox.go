package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/thomkin/muro/internal/control"
	"github.com/thomkin/muro/internal/ptyio"
	"github.com/thomkin/muro/internal/worktree"
)

func init() {
	sandboxCmd.AddCommand(sandboxListCmd, sandboxShowCmd, sandboxUpdateCmd, sandboxReloadCmd,
		sandboxRestartCmd, sandboxStopCmd, sandboxDeleteCmd, sandboxAttachCmd, sandboxMergeCmd)
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

var sandboxRestartFromProfileFlag bool

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
		req := control.SandboxRestartRequest{Namespace: ns, Name: name, FromProfile: sandboxRestartFromProfileFlag}
		if err := c.Call(control.TypeSandboxRestart, req, nil); err != nil {
			return err
		}
		fmt.Printf("restarted %s/%s\n", nsOrDefault(ns), name)
		return nil
	},
}

func init() {
	sandboxRestartCmd.Flags().BoolVar(&sandboxRestartFromProfileFlag, "from-profile", false, "re-resolve mounts/tools/allow_urls/agent/git-policy/audio from this sandbox's own profile before relaunching, instead of reusing what's already stored — picks up a `muro profile edit`/`profile mount add` change without a separate stop+run")
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

var sandboxDeleteYesFlag bool
var sandboxDeleteDiscardWorktreeFlags []string

var sandboxDeleteCmd = &cobra.Command{
	Use:     "delete <agent-name>",
	Aliases: []string{"rm", "remove"},
	Short:   "Permanently remove a stopped sandbox's record, log, and any private session data",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ns, name := splitNamespaceName(args[0])

		if !sandboxDeleteYesFlag {
			confirmed, err := confirmPrompt(fmt.Sprintf(
				"Delete sandbox %s/%s? This permanently removes its record, log, and any private session data. [y/N]: ",
				nsOrDefault(ns), name))
			if err != nil {
				return err
			}
			if !confirmed {
				fmt.Println("aborted")
				return nil
			}
		}

		c, err := dialControl()
		if err != nil {
			return err
		}
		defer c.Close()

		req := control.SandboxDeleteRequest{Namespace: ns, Name: name, DiscardWorktrees: sandboxDeleteDiscardWorktreeFlags}
		if err := c.Call(control.TypeSandboxDelete, req, nil); err != nil {
			return err
		}
		fmt.Printf("deleted %s/%s\n", nsOrDefault(ns), name)
		return nil
	},
}

func init() {
	sandboxDeleteCmd.Flags().BoolVarP(&sandboxDeleteYesFlag, "yes", "y", false, "skip the confirmation prompt")
	// --discard-worktree is deliberately separate from --yes: --yes only
	// confirms deleting metadata/logs, never discarding a git worktree's
	// unmerged commits (DESIGN.md §15) — that needs its own, differently-
	// named, explicit opt-in per worktree so it can never be an accidental
	// side effect of routine sandbox cleanup.
	sandboxDeleteCmd.Flags().StringArrayVar(&sandboxDeleteDiscardWorktreeFlags, "discard-worktree", nil,
		"mount_path of a git worktree whose unmerged commits should be discarded (repeatable) — otherwise delete refuses if any worktree has unmerged commits")
}

// confirmPrompt prints prompt and reads a y/yes (case-insensitive) answer
// from stdin, defaulting to "no" for anything else including a bare Enter —
// deleting a sandbox is destructive, so silence or ambiguity must never be
// read as consent. Refuses outright (rather than blocking forever) when
// stdin isn't an interactive terminal, since there's no one there to answer
// — callers in that situation should pass --yes explicitly instead.
func confirmPrompt(prompt string) (bool, error) {
	if !isTerminal(int(os.Stdin.Fd())) {
		return false, usageErr("refusing to delete without confirmation in a non-interactive session — pass --yes")
	}
	fmt.Print(prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false, nil // EOF/closed stdin reads as "no", not an error — same "never assume consent" reasoning
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

// isTerminal reports whether fd refers to a real terminal — the same
// TCGETS-success probe ptyio.SetRawMode uses internally, factored out here
// since this file needs the same check without also wanting raw mode.
func isTerminal(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	return err == nil
}

var sandboxMergeRepoFlag string

// sandboxMergeCmd squash-merges one of a sandbox's git worktrees
// (DESIGN.md §15) back into its base branch. The diff/proposed-message
// preview and $EDITOR step run entirely client-side — a deliberate,
// established exception to "the CLI has no business logic" (profile edit
// already does the same for a profile's JSON, §9) — since it's a
// read-only preview on the same host, same user, no daemon round-trip
// needed for it. The actual merge (mutating the real repo) is still
// murod's job alone (sandbox.merge), the same way every other mutation
// in this file goes through the daemon rather than the CLI doing it
// directly.
var sandboxMergeCmd = &cobra.Command{
	Use:   "merge <agent-name>",
	Short: "Squash-merge a sandbox's git worktree back into its base branch",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ns, name := splitNamespaceName(args[0])

		c, err := dialControl()
		if err != nil {
			return err
		}
		defer c.Close()

		var view control.SandboxView
		showReq := control.SandboxShowRequest{Namespace: ns, Name: name}
		if err := c.Call(control.TypeSandboxShow, showReq, &view); err != nil {
			return err
		}

		wt, err := selectWorktree(view.Worktrees, sandboxMergeRepoFlag, ns, name)
		if err != nil {
			return err
		}
		if !wt.HasUnmergedCommits {
			return usageErr("worktree %q has no commits to merge", wt.MountPath)
		}

		ctx := context.Background()
		diff, err := worktree.Diff(ctx, wt.Host, wt.BaseBranch)
		if err != nil {
			return fmt.Errorf("read worktree diff: %w", err)
		}
		draftMsg, err := worktree.LastCommitMessage(ctx, wt.Host)
		if err != nil {
			return fmt.Errorf("read worktree's last commit message: %w", err)
		}

		message, err := editMergeMessage(draftMsg, diff, wt.Branch, wt.BaseBranch)
		if err != nil {
			return err
		}

		var mergeResp control.SandboxMergeResponse
		mergeReq := control.SandboxMergeRequest{Namespace: ns, Name: name, MountPath: wt.MountPath, Message: message}
		if err := c.Call(control.TypeSandboxMerge, mergeReq, &mergeResp); err != nil {
			return err
		}
		fmt.Printf("merged %s/%s worktree %s into %s: %s\n", nsOrDefault(ns), name, wt.MountPath, wt.BaseBranch, mergeResp.Commit)
		return nil
	},
}

func init() {
	sandboxMergeCmd.Flags().StringVar(&sandboxMergeRepoFlag, "repo", "", "mount_path of the worktree to merge (required if the sandbox has more than one)")
}

// selectWorktree picks which of a sandbox's worktrees `sandbox merge`
// should act on: the explicit --repo flag if given, or the sandbox's only
// worktree if there's exactly one — an ambiguous or empty selection is a
// usage error listing what's actually available, not a guess.
func selectWorktree(worktrees []control.WorktreeView, repoFlag, ns, name string) (*control.WorktreeView, error) {
	if len(worktrees) == 0 {
		return nil, usageErr("sandbox %s/%s has no git worktrees", nsOrDefault(ns), name)
	}
	if repoFlag != "" {
		for i := range worktrees {
			if worktrees[i].MountPath == repoFlag {
				return &worktrees[i], nil
			}
		}
		return nil, usageErr("sandbox %s/%s has no worktree at mount_path %q", nsOrDefault(ns), name, repoFlag)
	}
	if len(worktrees) > 1 {
		paths := make([]string, len(worktrees))
		for i, w := range worktrees {
			paths[i] = w.MountPath
		}
		return nil, usageErr("sandbox %s/%s has multiple worktrees, pick one with --repo: %s",
			nsOrDefault(ns), name, strings.Join(paths, ", "))
	}
	return &worktrees[0], nil
}

// editMergeMessage writes draftMsg followed by diff as "# "-prefixed
// comment lines (exactly `git commit`'s own editor convention — the
// operator already knows how to read this) to a temp file, opens it in
// $EDITOR, and returns the non-comment lines with surrounding blank lines
// trimmed. An error if the result is blank — a merge always needs a real
// message, never a silently empty one.
func editMergeMessage(draftMsg, diff, branch, baseBranch string) (string, error) {
	f, err := os.CreateTemp("", "muro-merge-msg-*.txt")
	if err != nil {
		return "", fmt.Errorf("create temp file for merge message: %w", err)
	}
	tmpPath := f.Name()
	defer os.Remove(tmpPath)

	var b strings.Builder
	b.WriteString(draftMsg)
	if !strings.HasSuffix(draftMsg, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("\n# Merging ")
	b.WriteString(branch)
	b.WriteString(" into ")
	b.WriteString(baseBranch)
	b.WriteString(". Lines starting with # are ignored.\n#\n")
	for _, line := range strings.Split(diff, "\n") {
		b.WriteString("# ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return "", fmt.Errorf("write temp file for merge message: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("write temp file for merge message: %w", err)
	}

	if err := openInEditor(tmpPath); err != nil {
		return "", err
	}

	edited, err := os.ReadFile(tmpPath)
	if err != nil {
		return "", fmt.Errorf("read back merge message: %w", err)
	}
	var kept []string
	for _, line := range strings.Split(string(edited), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		kept = append(kept, line)
	}
	message := strings.TrimSpace(strings.Join(kept, "\n"))
	if message == "" {
		return "", usageErr("merge commit message is empty — aborting")
	}
	return message, nil
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

		restore, err := ptyio.SetRawMode(os.Stdin)
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

		return ptyio.Pump(os.Stdin, os.Stdout, r, w)
	},
}

func nsOrDefault(ns string) string {
	if ns == "" {
		return "default"
	}
	return ns
}
