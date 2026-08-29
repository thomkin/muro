package tui

import tea "github.com/charmbracelet/bubbletea"

// keyBytes reconstructs the raw terminal bytes msg was originally parsed
// from, for forwarding to an attached agent while the console has keyboard
// focus. Bubble Tea's own input loop discards the raw bytes once it parses
// a KeyMsg — this rebuilds a faithful-enough encoding for real interactive
// use (typing, arrows, enter, backspace, ctrl+letter, esc) rather than
// perfect fidelity for every key Bubble Tea recognizes: F-keys and
// ctrl+shift+arrow-style combos fall through to nil (silently dropped) —
// rare enough typing into a coding agent's own prompt not to block a first
// pass on.
func keyBytes(msg tea.KeyMsg) []byte {
	var b []byte
	if msg.Alt {
		b = append(b, 0x1b) // standard meta-key convention: ESC-prefix
	}

	if msg.Type == tea.KeyRunes {
		return append(b, []byte(string(msg.Runes))...)
	}

	// Every C0 control key's KeyType VALUE *is* its raw control byte
	// (bubbletea's key.go: keyNUL=0, keySOH=1, ... keyESC=27, ...
	// keyDEL=127) — confirmed against the actual source, no lookup table
	// needed for these.
	if msg.Type >= 0 && msg.Type <= 127 {
		return append(b, byte(msg.Type))
	}

	if seq, ok := negativeKeySequences[msg.Type]; ok {
		return append(b, seq...)
	}
	return nil // unrecognized (F-keys, exotic combos) — dropped, not guessed at
}

// negativeKeySequences covers bubbletea's "other keys" (key.go: KeyType
// values < 0, no numeric relationship to their terminal encoding) that are
// realistic to actually press while typing into an attached agent —
// standard xterm/ANSI escape sequences.
var negativeKeySequences = map[tea.KeyType]string{
	tea.KeyUp:       "\x1b[A",
	tea.KeyDown:     "\x1b[B",
	tea.KeyRight:    "\x1b[C",
	tea.KeyLeft:     "\x1b[D",
	tea.KeyHome:     "\x1b[H",
	tea.KeyEnd:      "\x1b[F",
	tea.KeyPgUp:     "\x1b[5~",
	tea.KeyPgDown:   "\x1b[6~",
	tea.KeyDelete:   "\x1b[3~",
	tea.KeyInsert:   "\x1b[2~",
	tea.KeySpace:    " ",
	tea.KeyShiftTab: "\x1b[Z",
}
