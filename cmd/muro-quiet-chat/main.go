// Command muro-quiet-chat is what a sandbox launches instead of the
// configured agent directly when its profile sets quiet_mode: true
// (config.Profile.QuietMode, internal/sandbox's buildLaunchSpec). Claude
// Code's normal interactive UI shows every tool call, file diff, and raw
// JSON as it works — useful for a coding agent, but noisy clutter for a
// conversational, user-facing profile (a tutor, an assistant) where only
// the reply text matters. This wrapper drives Claude Code's own
// non-interactive print mode (`claude -p ... --output-format json`) one
// turn at a time instead, so the attached pty only ever shows the
// assistant's reply — never a tool_use/tool_result block or a diff.
//
// Session continuity mirrors the shell one-liner claude-base's own
// agent_args already uses (internal/config's example profiles): the first
// turn ever run in this sandbox instance starts a fresh session
// (--session-id), detected by no *.jsonl history existing yet under
// ~/.claude/projects (private per sandbox instance, PrivateDirMounts);
// every turn after that — including every turn following a `muro sandbox
// restart` of the same instance — resumes it (--resume), so the
// conversation (and Claude Code's own memory of it) survives exactly the
// same way an interactively-attached session's would.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/thomkin/muro/internal/sandbox"
)

func main() {
	os.Exit(run())
}

func run() int {
	sessionID := os.Getenv(sandbox.SessionIDEnvVar)
	if sessionID == "" {
		fmt.Fprintf(os.Stderr, "muro-quiet-chat: %s not set — this must be launched by muro, not run directly\n", sandbox.SessionIDEnvVar)
		return 1
	}
	home := os.Getenv("HOME")
	if home == "" {
		fmt.Fprintln(os.Stderr, "muro-quiet-chat: HOME not set")
		return 1
	}
	claudePath := filepath.Join(home, ".local", "bin", "claude")

	// Same detection claude-base's agent_args script uses: any prior
	// session history at all means this sandbox instance has run before
	// (a restart), so even its first turn this run must resume rather than
	// start fresh.
	priorHistory, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", "*.jsonl"))
	freshSession := len(priorHistory) == 0

	fmt.Println("Signora Pizza is ready — type your message and press Enter (Ctrl-D to end the session).")

	scanner := bufio.NewScanner(os.Stdin)
	// A chat message can run well past bufio.Scanner's 64KiB default.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for {
		fmt.Print("\nYou: ")
		if !scanner.Scan() {
			fmt.Println()
			return 0
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		args := []string{"-p", line, "--dangerously-skip-permissions", "--output-format", "json"}
		if freshSession {
			args = append(args, "--session-id", sessionID)
			freshSession = false
		} else {
			args = append(args, "--resume", sessionID)
		}

		// Deliberately not connected to our own Stdout/Stderr: this is what
		// actually hides the tool-call/diff noise — claude's entire
		// interactive UI and every intermediate stream event live only in
		// this captured buffer, never reaching the attached pty. Only what
		// replyText() below prints afterward does.
		cmd := exec.Command(claudePath, args...)
		sp := startSpinner(os.Stdout, "Signora Pizza is thinking")
		out, err := cmd.Output()
		sp.Stop()
		if err != nil {
			fmt.Printf("\n⚠ that turn failed: %s\n", describeErr(err))
			continue
		}
		fmt.Println()
		fmt.Println(replyText(out))
	}
}

// describeErr turns a failed claude invocation into something actually
// diagnosable — exec.Command's own error for a nonzero exit is just "exit
// status 1", which was worse than useless for tracking down a real failure
// (turned out to be muro's own proxy denying api.anthropic.com after a
// daemon restart, internal/sandbox/manager.go's Restart fix — this is what
// would have surfaced that immediately instead of a bare exit code).
// Truncated since claude's stderr can occasionally be long.
func describeErr(err error) string {
	if ee, ok := err.(*exec.ExitError); ok {
		if stderr := strings.TrimSpace(string(ee.Stderr)); stderr != "" {
			const maxLen = 500
			if len(stderr) > maxLen {
				stderr = stderr[:maxLen] + "…"
			}
			return stderr
		}
	}
	return err.Error()
}

// startSpinner prints an animated spinner to w, updating in place, until
// Stop is called — visible feedback that a turn is being processed while
// claude -p runs (which can take several seconds), instead of the terminal
// just sitting silent. Stop leaves the line cleared, so whatever prints
// next starts clean.
type spinnerHandle struct {
	stop    chan struct{}
	stopped chan struct{}
}

func startSpinner(w *os.File, message string) *spinnerHandle {
	h := &spinnerHandle{stop: make(chan struct{}), stopped: make(chan struct{})}
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	go func() {
		defer close(h.stopped)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		i := 0
		fmt.Fprintf(w, "%s %s", frames[0], message)
		for {
			select {
			case <-h.stop:
				fmt.Fprint(w, "\r\033[K") // clear the spinner line entirely
				return
			case <-ticker.C:
				i++
				fmt.Fprintf(w, "\r%s %s", frames[i%len(frames)], message)
			}
		}
	}()
	return h
}

func (h *spinnerHandle) Stop() {
	close(h.stop)
	<-h.stopped // wait for the clearing write to actually happen before anything else prints
}

// printModeResult is the subset of Claude Code's `--output-format json`
// result object this cares about — just enough to extract the final reply
// text, tolerant of fields it doesn't recognize.
type printModeResult struct {
	Result string `json:"result"`
}

// replyText extracts the assistant's final reply from one `claude -p
// --output-format json` invocation's stdout. Falls back to the raw output
// if it doesn't parse as expected, or has no "result" field — better to
// show something unexpected than silently drop a turn's response.
func replyText(out []byte) string {
	var r printModeResult
	if err := json.Unmarshal(out, &r); err == nil && r.Result != "" {
		return r.Result
	}
	return strings.TrimSpace(string(out))
}
