package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// openInEditor runs $EDITOR (falling back to vi) on path, blocking until it
// exits, with stdin/stdout/stderr connected to the terminal — shared by
// `profile edit` (opens a profile's JSON file directly) and `sandbox merge`
// (opens a temp file holding the proposed merge commit message), so both
// commands' "open something in the user's editor and wait" behavior can
// never drift apart.
func openInEditor(path string) error {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	// $EDITOR conventionally may include arguments (e.g. "code --wait" — VS
	// Code specifically needs --wait, since without it `code <path>` returns
	// immediately instead of blocking until the file is closed, which would
	// make whatever re-reads the file afterward see a still-unedited
	// version). exec.Command's first arg is a single binary name, not a
	// shell command line, so this has to be split first.
	editorParts := strings.Fields(editor)
	c := exec.Command(editorParts[0], append(editorParts[1:], path)...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("running %s: %w", editor, err)
	}
	return nil
}
