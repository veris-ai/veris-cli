// Package ui is how the veris command talks to a person: marks on a line,
// aligned tables, a spinner while something takes a while, and the prompts a
// command asks when it was not told enough.
//
// Everything here writes to Out, which is stderr in the binary. stdout is the
// machine's: the child's output, the raw body behind --json. Keeping the two
// apart is what lets `veris env list --json | jq` work while a person still
// sees the progress beside it.
package ui

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"golang.org/x/term"
)

// UI carries the answers to "may I colour this", "may I ask" and "should I say
// it at all". They are plain fields so a command, or a test, can set them
// outright instead of arranging an environment to be detected.
type UI struct {
	Out       io.Writer // stderr
	In        io.Reader // stdin; prompts read from it (tests feed key sequences)
	Color     bool      // ANSI colour on Out
	TTY       bool      // prompts allowed: In is a terminal
	Quiet     bool      // -q: Success/Info/Detail/Link/Next are suppressed; Fail and Warn still print
	AssumeYes bool      // --yes: Confirm returns true without prompting and echoes "question y"

	// reader wraps In once so a prompt that read ahead of its own line does
	// not lose the rest for the next one.
	readerOnce sync.Once
	reader     *bufio.Reader
}

// New builds a UI for out and in, detecting what they are. TTY is whether in
// is a terminal; Color is whether out is one, unless NO_COLOR is set or TERM
// is dumb. Neither detection touches a writer that is not a file, so a buffer
// in a test is simply "not a terminal".
func New(out io.Writer, in io.Reader) *UI {
	return &UI{
		Out:   out,
		In:    in,
		TTY:   isTerminal(in),
		Color: colorEnabled(isTerminal(out), os.LookupEnv),
	}
}

// colorEnabled is the NO_COLOR convention (https://no-color.org): the variable
// being set, even to nothing, turns colour off. TERM=dumb is the older spelling
// of the same wish. lookupEnv is os.LookupEnv, injected so the rule is testable
// without a terminal or a mutated environment.
func colorEnabled(outIsTerminal bool, lookupEnv func(string) (string, bool)) bool {
	if !outIsTerminal {
		return false
	}
	if _, set := lookupEnv("NO_COLOR"); set {
		return false
	}
	t, _ := lookupEnv("TERM")
	return t != "dumb"
}

func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	if !ok || f == nil {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// ANSI sequences. Plain escape codes rather than a colour library: four
// colours and a reset are not worth a dependency.
const (
	ansiReset  = "\033[0m"
	ansiRed    = "\033[31m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiCyan   = "\033[36m"
	ansiDim    = "\033[2m"
)

func (u *UI) paint(code, s string) string {
	if !u.Color {
		return s
	}
	return code + s + ansiReset
}

func (u *UI) line(s string) {
	fmt.Fprintln(u.Out, s)
}

// Success prints "✓ …" in green.
func (u *UI) Success(format string, a ...any) {
	if u.Quiet {
		return
	}
	u.line(u.paint(ansiGreen, "✓") + " " + fmt.Sprintf(format, a...))
}

// Fail prints "✗ …" in red. It is never suppressed: a failure the user asked
// not to hear about is still a failure they have to act on.
func (u *UI) Fail(format string, a ...any) {
	u.line(u.paint(ansiRed, "✗") + " " + fmt.Sprintf(format, a...))
}

// Warn prints "! …" in yellow. Not suppressed by Quiet, for the same reason as
// Fail.
func (u *UI) Warn(format string, a ...any) {
	u.line(u.paint(ansiYellow, "!") + " " + fmt.Sprintf(format, a...))
}

// Link prints "→ url", dimmed: a place to go, not a result.
func (u *UI) Link(url string) {
	if u.Quiet {
		return
	}
	u.line(u.paint(ansiDim, "→ "+url))
}

// Next prints "→ Next: cmd", the one command that follows this one.
func (u *UI) Next(cmd string) {
	if u.Quiet {
		return
	}
	u.line("→ Next: " + cmd)
}

// Info prints a plain line.
func (u *UI) Info(format string, a ...any) {
	if u.Quiet {
		return
	}
	u.line(fmt.Sprintf(format, a...))
}

// Detail prints a line indented under the one before it.
func (u *UI) Detail(format string, a ...any) {
	if u.Quiet {
		return
	}
	u.line("  " + fmt.Sprintf(format, a...))
}

// Table prints header and rows as columns two spaces apart. The header is
// printed as given -- no upper-casing, no underline -- so a caller that wants
// "NAME" writes "NAME". A nil header prints rows alone.
func (u *UI) Table(header []string, rows [][]string) {
	w := tabwriter.NewWriter(u.Out, 0, 0, 2, ' ', 0)
	if len(header) > 0 {
		fmt.Fprintln(w, strings.Join(header, "\t"))
	}
	for _, row := range rows {
		fmt.Fprintln(w, strings.Join(row, "\t"))
	}
	_ = w.Flush()
}

// MaskKey renders an API key as its prefix and an ellipsis: enough to tell two
// keys apart (the prefix carries the kind and the first characters of the id),
// never enough to use. Nothing in the CLI prints a whole key, and this is the
// one function that decides what a key looks like when printed.
func MaskKey(k string) string {
	switch {
	case len(k) > 12:
		return k[:12] + "…"
	case len(k) >= 5:
		return k[:4] + "…"
	default:
		return "…"
	}
}

// --- spinner ----------------------------------------------------------------

// spinnerFrames is the braille spinner; spinnerInterval is how fast it turns.
// The interval is a variable so a test can turn it quickly.
var (
	spinnerFrames   = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	spinnerInterval = 80 * time.Millisecond
)

// Spinner shows that something is still happening. On a terminal it animates
// on one line and is erased by Stop, leaving no trace; anywhere else -- a CI
// log, a pipe -- it prints its label once, because a log full of frames is
// worse than no spinner at all.
type Spinner struct {
	u      *UI
	mu     sync.Mutex
	label  string
	done   chan struct{}
	exited chan struct{}
	live   bool // animating: TTY and not quiet
}

// Spinner starts a spinner labelled label. Call Stop before printing anything
// else on Out, or the next line lands beside the frame.
func (u *UI) Spinner(label string) *Spinner {
	s := &Spinner{u: u, label: label}
	if u.Quiet {
		return s
	}
	if !u.TTY {
		fmt.Fprintln(u.Out, label)
		return s
	}
	s.live = true
	s.done = make(chan struct{})
	s.exited = make(chan struct{})
	go s.spin()
	return s
}

func (s *Spinner) spin() {
	defer close(s.exited)
	t := time.NewTicker(spinnerInterval)
	defer t.Stop()
	i := 0
	s.draw(spinnerFrames[i])
	for {
		select {
		case <-s.done:
			// Erase the frame; whatever prints next starts on a clean line.
			fmt.Fprint(s.u.Out, "\r\033[K")
			return
		case <-t.C:
			i = (i + 1) % len(spinnerFrames)
			s.draw(spinnerFrames[i])
		}
	}
}

func (s *Spinner) draw(frame string) {
	s.mu.Lock()
	label := s.label
	s.mu.Unlock()
	fmt.Fprintf(s.u.Out, "\r\033[K%s %s", s.u.paint(ansiCyan, frame), label)
}

// Update changes the label. Off a terminal the new label prints as its own
// line, so a log still shows each stage the run went through.
func (s *Spinner) Update(label string) {
	s.mu.Lock()
	changed := label != s.label
	s.label = label
	s.mu.Unlock()
	if s.live || s.u.Quiet || !changed {
		return
	}
	fmt.Fprintln(s.u.Out, label)
}

// Stop ends the animation and clears the line. Safe to call more than once,
// and returns only after the last frame has been erased, so the caller's next
// line cannot race a frame.
func (s *Spinner) Stop() {
	if !s.live {
		return
	}
	s.mu.Lock()
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	s.mu.Unlock()
	<-s.exited
}
