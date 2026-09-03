package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestKeyBytes_PlainRunes(t *testing.T) {
	msg := tea.KeyMsg(tea.Key{Type: tea.KeyRunes, Runes: []rune("hi")})
	got := keyBytes(msg)
	if string(got) != "hi" {
		t.Errorf("keyBytes = %q, want %q", got, "hi")
	}
}

func TestKeyBytes_Enter(t *testing.T) {
	msg := tea.KeyMsg(tea.Key{Type: tea.KeyEnter})
	got := keyBytes(msg)
	if string(got) != "\r" {
		t.Errorf("keyBytes(Enter) = %q, want CR", got)
	}
}

func TestKeyBytes_Backspace(t *testing.T) {
	msg := tea.KeyMsg(tea.Key{Type: tea.KeyBackspace})
	got := keyBytes(msg)
	if len(got) != 1 || got[0] != 127 {
		t.Errorf("keyBytes(Backspace) = %v, want [127]", got)
	}
}

func TestKeyBytes_Tab(t *testing.T) {
	msg := tea.KeyMsg(tea.Key{Type: tea.KeyTab})
	got := keyBytes(msg)
	if string(got) != "\t" {
		t.Errorf("keyBytes(Tab) = %q, want tab", got)
	}
}

func TestKeyBytes_Esc(t *testing.T) {
	msg := tea.KeyMsg(tea.Key{Type: tea.KeyEsc})
	got := keyBytes(msg)
	if len(got) != 1 || got[0] != 0x1b {
		t.Errorf("keyBytes(Esc) = %v, want [0x1b]", got)
	}
}

// This is the key regression case for the whole console-focus feature:
// Ctrl-P Ctrl-Q must be forwarded as ordinary raw bytes (0x10, 0x11) just
// like any other keystroke — internal/control/stream.go's detachScanner
// recognizes the sequence server-side, exactly as it already does for
// `muro sandbox attach`. If keyBytes ever special-cased these two keys
// (e.g. dropped them, or intercepted them client-side) detach would
// silently stop working from inside the split-pane console.
func TestKeyBytes_CtrlPCtrlQForwardedAsRawBytes(t *testing.T) {
	p := keyBytes(tea.KeyMsg(tea.Key{Type: tea.KeyCtrlP}))
	if len(p) != 1 || p[0] != 0x10 {
		t.Errorf("keyBytes(Ctrl-P) = %v, want [0x10]", p)
	}
	q := keyBytes(tea.KeyMsg(tea.Key{Type: tea.KeyCtrlQ}))
	if len(q) != 1 || q[0] != 0x11 {
		t.Errorf("keyBytes(Ctrl-Q) = %v, want [0x11]", q)
	}
}

func TestKeyBytes_CtrlC(t *testing.T) {
	msg := tea.KeyMsg(tea.Key{Type: tea.KeyCtrlC})
	got := keyBytes(msg)
	if len(got) != 1 || got[0] != 0x03 {
		t.Errorf("keyBytes(Ctrl-C) = %v, want [0x03]", got)
	}
}

func TestKeyBytes_Arrows(t *testing.T) {
	cases := map[tea.KeyType]string{
		tea.KeyUp:    "\x1b[A",
		tea.KeyDown:  "\x1b[B",
		tea.KeyRight: "\x1b[C",
		tea.KeyLeft:  "\x1b[D",
	}
	for kt, want := range cases {
		got := keyBytes(tea.KeyMsg(tea.Key{Type: kt}))
		if string(got) != want {
			t.Errorf("keyBytes(%v) = %q, want %q", kt, got, want)
		}
	}
}

func TestKeyBytes_AltPrefixesEsc(t *testing.T) {
	msg := tea.KeyMsg(tea.Key{Type: tea.KeyRunes, Runes: []rune("x"), Alt: true})
	got := keyBytes(msg)
	if string(got) != "\x1bx" {
		t.Errorf("keyBytes(alt+x) = %q, want ESC-prefixed x", got)
	}
}

func TestKeyBytes_UnrecognizedKeyReturnsNil(t *testing.T) {
	got := keyBytes(tea.KeyMsg(tea.Key{Type: tea.KeyF1}))
	if got != nil {
		t.Errorf("keyBytes(F1) = %v, want nil (unrecognized, dropped)", got)
	}
}

// TestKeyBytes_CtrlLeftReturnsNil locks in the zero-collision claim
// model.go's handleKey relies on for repurposing Ctrl-Left as a
// client-side "back to the list" shortcut: since it was never forwarded to
// the attached agent in the first place (only plain arrows are in
// negativeKeySequences), intercepting it client-side takes nothing away
// from what the agent used to receive.
func TestKeyBytes_CtrlLeftReturnsNil(t *testing.T) {
	got := keyBytes(tea.KeyMsg(tea.Key{Type: tea.KeyCtrlLeft}))
	if got != nil {
		t.Errorf("keyBytes(ctrl+left) = %v, want nil (never forwarded, so repurposing it client-side is collision-free)", got)
	}
}
