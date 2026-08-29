package tui

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/hinshun/vt10x"

	"github.com/thomkin/muro/internal/control"
)

// vt10x's text-attribute bits (Glyph.Mode) are unexported in the library
// itself (state.go's attrReverse/attrUnderline/attrBold/... consts) —
// replicated here by exact bit VALUE, pinned to
// github.com/hinshun/vt10x@v0.0.0-20220301184237-5011da428d02's state.go
// (`attrReverse = 1 << iota`, in this declaration order). A future vt10x
// release renumbering these would silently misrender text attributes (bold
// text stops looking bold, etc.), not panic or fail a build — a known,
// accepted fragility given the library exports no public names for them.
const (
	vtAttrReverse = 1 << iota
	vtAttrUnderline
	vtAttrBold
	vtAttrGfx
	vtAttrItalic
	vtAttrBlink
	vtAttrWrap
)

const (
	paneDefaultCols = 80
	paneDefaultRows = 24
)

// session is the single live attach connection + virtual terminal backing
// the right-hand pane. There is never more than one alive at once across
// the whole muro tui process — muro's control server allows exactly one
// attacher per sandbox (DESIGN.md §12), and this package never opens a
// second one for a different sandbox before closing whichever is already
// open (switchTo, model.go).
type session struct {
	namespace, name string
	client          *control.Client
	writer          io.Writer
	term            vt10x.Terminal
	// done is closed by the background reader goroutine once it ends —
	// EOF (server-side detach, e.g. Ctrl-P Ctrl-Q recognized by
	// internal/control/stream.go's detachScanner, completely unchanged;
	// or the sandbox process exiting) or a real connection error. Live()
	// is how the rest of this package notices a session ended without
	// needing to inject a *tea.Program reference into openSession to push
	// a message — the pane's own periodic redraw tick just checks it.
	done chan struct{}

	// historyMu/history back the pane's scrollback (model.go's
	// Model.scrollBack, viewSplitPane): a capped copy of every raw byte
	// this session has received, alongside (not instead of) feeding the
	// live vt10x.Terminal. vt10x itself keeps no history beyond its
	// current screen — scrolling back means re-parsing an earlier PREFIX
	// of this buffer into a throwaway vt10x.Terminal and rendering
	// whatever screen that produces (renderScrollback) — an
	// approximation good enough for the common case (a shell's linear
	// output) and not really a meaningful concept at all for a
	// full-screen redrawing app, matching how real terminals also
	// suspend scrollback during alt-screen mode.
	historyMu sync.Mutex
	history   []byte
}

// historyCap bounds how much raw output a session retains for scrollback —
// generous for a real interactive shell/agent session without letting a
// runaway output producer grow this without bound.
const historyCap = 512 * 1024

func (s *session) appendHistory(p []byte) {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	s.history = append(s.history, p...)
	if len(s.history) > historyCap {
		s.history = s.history[len(s.history)-historyCap:]
	}
}

// historySnapshot returns a copy of the captured history, safe to read
// (e.g. to replay into a throwaway vt10x.Terminal) without racing the
// background reader goroutine's own appends.
func (s *session) historySnapshot() []byte {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	return append([]byte(nil), s.history...)
}

// Live reports whether the background reader goroutine is still running —
// false once the attach stream has ended, for any reason.
func (s *session) Live() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// openSession dials and attaches to namespace/name, returning a *session
// whose vt10x.Terminal is fed by a background goroutine for as long as the
// connection stays open — ended by a server-side detach (Ctrl-P Ctrl-Q),
// the sandbox exiting, or Close. cols/rows size the virtual terminal to
// the pane's current rendered area; the real sandbox pty itself is NOT
// resized to match — no resize-forwarding protocol exists in muro yet
// (DESIGN.md, deliberately deferred) — so a pane larger than the pty's own
// default size (80x24, matching `muro sandbox attach`'s existing behavior)
// just renders blank past that edge, and a smaller pane crops.
func openSession(namespace, name string, cols, rows int) (*session, error) {
	c, err := control.Dial(control.ResolveSocketPath())
	if err != nil {
		return nil, err
	}
	r, w, err := c.Attach(namespace, name)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	if cols <= 0 {
		cols = paneDefaultCols
	}
	if rows <= 0 {
		rows = paneDefaultRows
	}
	term := vt10x.New(vt10x.WithSize(cols, rows))
	done := make(chan struct{})
	sess := &session{namespace: namespace, name: name, client: c, writer: w, term: term, done: done}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := r.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				_, _ = term.Write(chunk)
				sess.appendHistory(chunk)
			}
			if rerr != nil {
				break
			}
		}
		close(done)
	}()

	return sess, nil
}

// Write sends bytes typed by the operator straight to the attached agent —
// used only while the console has keyboard focus (model.go).
func (s *session) Write(p []byte) (int, error) {
	return s.writer.Write(p)
}

// Close ends the attach connection — the same effect Ctrl-P Ctrl-Q has
// server-side (internal/control/stream.go's detachScanner), just triggered
// by the client closing its end instead of sending the detach byte
// sequence. The sandbox itself keeps running; only the attach stream ends.
func (s *session) Close() {
	if s.client != nil {
		_ = s.client.Close()
	}
}

// scrollCutoff returns the byte offset into history to replay up through
// so the resulting screen approximates "what was on screen linesBack real
// lines before the live end of the buffer" — found by walking backward
// from the end counting '\n' bytes, since vt10x itself tracks no line
// index of its own. linesBack <= 0 means "no scrollback, use the whole
// buffer" (equivalent to the live screen).
func scrollCutoff(history []byte, linesBack int) int {
	if linesBack <= 0 {
		return len(history)
	}
	newlines := 0
	for i := len(history) - 1; i >= 0; i-- {
		if history[i] == '\n' {
			newlines++
			if newlines > linesBack {
				return i + 1
			}
		}
	}
	return 0
}

// renderScrollback replays history[:scrollCutoff(history, linesBack)] into
// a throwaway vt10x.Terminal the same size as the live one and renders its
// resulting screen — see session.history's doc comment for why this is an
// approximation, not a perfect reconstruction, and why that's an accepted
// tradeoff.
func renderScrollback(history []byte, linesBack, width, height int) string {
	term := vt10x.New(vt10x.WithSize(width, height))
	cutoff := scrollCutoff(history, linesBack)
	_, _ = term.Write(history[:cutoff])
	return renderPane(term, width, height)
}

// renderPane draws term's current cell grid into a width x height block of
// styled text. term.Cell(x, y) panics for x/y outside the terminal's own
// Size() — vt10x has no bounds checking of its own — so this always clamps
// to min(pane, terminal) rather than risking that panic; see openSession's
// doc comment for why the two sizes can differ at all.
func renderPane(term vt10x.View, width, height int) string {
	term.Lock()
	defer term.Unlock()

	termCols, termRows := term.Size()
	var b strings.Builder
	for y := 0; y < height; y++ {
		if y > 0 {
			b.WriteByte('\n')
		}
		if y >= termRows {
			continue
		}
		for x := 0; x < width && x < termCols; x++ {
			b.WriteString(renderGlyph(term.Cell(x, y)))
		}
	}
	return b.String()
}

func renderGlyph(g vt10x.Glyph) string {
	ch := g.Char
	if ch == 0 {
		ch = ' '
	}
	style := lipgloss.NewStyle()
	if fg, ok := vtColor(g.FG); ok {
		style = style.Foreground(fg)
	}
	if bg, ok := vtColor(g.BG); ok {
		style = style.Background(bg)
	}
	if g.Mode&vtAttrBold != 0 {
		style = style.Bold(true)
	}
	if g.Mode&vtAttrUnderline != 0 {
		style = style.Underline(true)
	}
	if g.Mode&vtAttrItalic != 0 {
		style = style.Italic(true)
	}
	if g.Mode&vtAttrBlink != 0 {
		style = style.Blink(true)
	}
	if g.Mode&vtAttrReverse != 0 {
		style = style.Reverse(true)
	}
	return style.Render(string(ch))
}

// vtColor maps a vt10x Color to a lipgloss color. false means "no explicit
// color" — vt10x's DefaultFG/DefaultBG/DefaultCursor sentinels are >=
// 1<<24 specifically so a caller can tell "use the terminal's own default"
// apart from a real color, which is what letting lipgloss leave this
// unset (rather than forcing literal black) achieves.
func vtColor(c vt10x.Color) (lipgloss.Color, bool) {
	if c >= 1<<24 {
		return "", false
	}
	if c < 256 {
		return lipgloss.Color(fmt.Sprintf("%d", uint32(c))), true
	}
	// 24-bit true color, encoded as r<<16|g<<8|b (vt10x's SGR 38;2/48;2
	// handling, state.go).
	r := (uint32(c) >> 16) & 0xFF
	g := (uint32(c) >> 8) & 0xFF
	b := uint32(c) & 0xFF
	return lipgloss.Color(fmt.Sprintf("#%02x%02x%02x", r, g, b)), true
}

// paneRedrawInterval drives how often the console pane is redrawn from its
// active session's vt10x.Terminal — independent of, and much faster than,
// the status-list poll (commands.go's pollInterval). A fixed-rate tick is
// simpler and more robust than signaling a Bubble Tea message per pty
// byte-chunk received, and naturally coalesces bursty output into one
// redraw.
const paneRedrawInterval = 80 * time.Millisecond

type paneTickMsg time.Time

func paneTickCmd() tea.Cmd {
	return tea.Tick(paneRedrawInterval, func(t time.Time) tea.Msg { return paneTickMsg(t) })
}

// sessionOpenedMsg is switchToCmd's result. target is namespace+"/"+name —
// carried alongside sess/err so a stale response (superseded by a faster
// later switch while this one was still dialing) can be recognized and
// discarded by the caller instead of clobbering a newer session.
type sessionOpenedMsg struct {
	target string
	sess   *session
	err    error
}

func switchToCmd(namespace, name string, cols, rows int) tea.Cmd {
	target := namespace + "/" + name
	return func() tea.Msg {
		sess, err := openSession(namespace, name, cols, rows)
		return sessionOpenedMsg{target: target, sess: sess, err: err}
	}
}
