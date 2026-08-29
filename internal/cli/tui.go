package cli

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/thomkin/muro/internal/tui"
)

func init() {
	rootCmd.AddCommand(tuiCmd)
}

var tuiCmd = &cobra.Command{
	Use:   "tui",
	Short: "Interactive terminal UI: see running agents, launch new ones from a profile, attach and switch between them",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		p := tea.NewProgram(tui.NewModel(), tea.WithAltScreen(), tea.WithMouseCellMotion())
		_, err := p.Run()
		return err
	},
}
