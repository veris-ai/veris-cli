package ui

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// ErrInterrupted is returned when the user leaves a prompt with Ctrl-C or
// closes stdin under it. The caller exits without printing a failure: the
// user chose this.
var ErrInterrupted = errors.New("interrupted")

// NoTTYError is returned by every prompt when stdin is not a terminal. A
// command that would have asked names the flag that answers instead, so a
// script author sees exactly what to add.
type NoTTYError struct{ FlagHint string }

func (e *NoTTYError) Error() string {
	return "Error: Interactive prompt requires a TTY. Pass " + e.FlagHint + " instead."
}

// Option is one choice in Select or MultiSelect. Value is what the command
// receives; Label is what the user reads; Detail is shown dimmed beside it.
type Option struct{ Value, Label, Detail string }

// input wraps In once. Prompts read line by line and raw mode reads byte by
// byte, and both must share one buffer or a key read ahead by one prompt
// would be lost to the next.
func (u *UI) input() *bufio.Reader {
	u.readerOnce.Do(func() {
		if r, ok := u.In.(*bufio.Reader); ok {
			u.reader = r
			return
		}
		u.reader = bufio.NewReader(u.In)
	})
	return u.reader
}

// readLine reads one answer. EOF with nothing typed is the user closing
// stdin, which is the same answer as Ctrl-C.
func (u *UI) readLine() (string, error) {
	line, err := u.input().ReadString('\n')
	if err != nil && line == "" {
		if errors.Is(err, io.EOF) {
			return "", ErrInterrupted
		}
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// Confirm asks a yes/no question. Empty input takes def. With --yes the
// question is answered on the user's behalf and the answer is echoed, so the
// transcript still shows what was agreed to.
func (u *UI) Confirm(question string, def bool, flagHint string) (bool, error) {
	if u.AssumeYes {
		if !u.Quiet {
			fmt.Fprintf(u.Out, "%s y\n", question)
		}
		return true, nil
	}
	if !u.TTY {
		return false, &NoTTYError{FlagHint: flagHint}
	}
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	for {
		fmt.Fprintf(u.Out, "%s %s ", question, hint)
		line, err := u.readLine()
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		fmt.Fprintln(u.Out, "Please answer y or n.")
	}
}

// Input asks for a line of text. Empty input takes def.
func (u *UI) Input(label, def, flagHint string) (string, error) {
	if !u.TTY {
		return "", &NoTTYError{FlagHint: flagHint}
	}
	if def != "" {
		fmt.Fprintf(u.Out, "? %s (%s): ", label, def)
	} else {
		fmt.Fprintf(u.Out, "? %s: ", label)
	}
	line, err := u.readLine()
	if err != nil {
		return "", err
	}
	if line = strings.TrimSpace(line); line == "" {
		return def, nil
	}
	return line, nil
}

// Select asks the user to pick one of opts. On a terminal that supports raw
// mode the list is live: arrows move, typing filters, Enter confirms. Anywhere
// else that a prompt is allowed (a test's forced TTY, a terminal that refuses
// raw mode) the list is numbered and one line is read.
func (u *UI) Select(label string, opts []Option, flagHint string) (Option, error) {
	if !u.TTY {
		return Option{}, &NoTTYError{FlagHint: flagHint}
	}
	if len(opts) == 0 {
		return Option{}, errors.New("nothing to choose from")
	}
	s := newSelector(label, opts, false, nil)
	if err := u.interactRaw(s); err == nil {
		return s.chosen()[0], nil
	} else if !errors.Is(err, errNoRawMode) {
		return Option{}, err
	}
	return u.selectLine(s)
}

// MultiSelect asks the user to pick any number of opts; preselected names, by
// Value, those that start ticked. The result keeps the order of opts.
func (u *UI) MultiSelect(label string, opts []Option, preselected []string, flagHint string) ([]Option, error) {
	if !u.TTY {
		return nil, &NoTTYError{FlagHint: flagHint}
	}
	if len(opts) == 0 {
		return nil, errors.New("nothing to choose from")
	}
	s := newSelector(label, opts, true, preselected)
	if err := u.interactRaw(s); err == nil {
		return s.chosen(), nil
	} else if !errors.Is(err, errNoRawMode) {
		return nil, err
	}
	return u.multiSelectLine(s)
}

// --- the list, shared by both modes -----------------------------------------

// selector is the state of one Select or MultiSelect: the options, what has
// been typed to narrow them, where the cursor is and what is ticked.
type selector struct {
	label    string
	opts     []Option
	multi    bool
	filter   string
	visible  []int // indices into opts that match filter, prefix matches first
	cursor   int   // index into visible
	top      int   // first visible row on screen
	selected map[int]bool
}

func newSelector(label string, opts []Option, multi bool, preselected []string) *selector {
	s := &selector{label: label, opts: opts, multi: multi, selected: map[int]bool{}}
	for _, v := range preselected {
		for i, o := range opts {
			if o.Value == v {
				s.selected[i] = true
			}
		}
	}
	s.refilter()
	return s
}

// refilter recomputes which options the filter allows, case-insensitively.
// Options whose label starts with the filter come before those that merely
// contain it, so typing "st" lands on "staging" before "test". The cursor
// follows the option it was on when that option survives.
func (s *selector) refilter() {
	var was = -1
	if s.cursor < len(s.visible) {
		was = s.visible[s.cursor]
	}
	q := strings.ToLower(s.filter)
	var prefix, within []int
	for i, o := range s.opts {
		l := strings.ToLower(o.Label)
		switch {
		case q == "" || strings.HasPrefix(l, q):
			prefix = append(prefix, i)
		case strings.Contains(l, q):
			within = append(within, i)
		}
	}
	s.visible = append(prefix, within...)
	s.cursor = 0
	for j, i := range s.visible {
		if i == was {
			s.cursor = j
		}
	}
}

func (s *selector) move(delta int) {
	if len(s.visible) == 0 {
		return
	}
	s.cursor = (s.cursor + delta + len(s.visible)) % len(s.visible)
}

func (s *selector) toggle() {
	if !s.multi || s.cursor >= len(s.visible) {
		return
	}
	i := s.visible[s.cursor]
	if s.selected[i] {
		delete(s.selected, i)
	} else {
		s.selected[i] = true
	}
}

func (s *selector) count() int { return len(s.selected) }

// chosen is the answer: the ticked options in list order, or in single mode
// the one under the cursor.
func (s *selector) chosen() []Option {
	if !s.multi {
		if s.cursor >= len(s.visible) {
			return nil
		}
		return []Option{s.opts[s.visible[s.cursor]]}
	}
	var out []Option
	for i, o := range s.opts {
		if s.selected[i] {
			out = append(out, o)
		}
	}
	return out
}

// match resolves a typed answer in line mode: a 1-based index into opts or
// an exact Value. -1 when neither.
func (s *selector) match(answer string) int {
	answer = strings.TrimSpace(answer)
	if n, err := strconv.Atoi(answer); err == nil {
		if n >= 1 && n <= len(s.opts) {
			return n - 1
		}
		return -1
	}
	for i, o := range s.opts {
		if o.Value == answer {
			return i
		}
	}
	return -1
}

// --- raw mode ----------------------------------------------------------------

// errNoRawMode says the terminal cannot be put in raw mode, or In is not a
// terminal at all, so the line-mode list is used instead.
var errNoRawMode = errors.New("no raw mode")

// maxRows is how many options are on screen at once; the list scrolls to
// keep the cursor inside the window.
const maxRows = 10

// interactRaw runs the live list when In is a terminal that grants raw mode.
func (u *UI) interactRaw(s *selector) error {
	f, ok := u.In.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return errNoRawMode
	}
	fd := int(f.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return errNoRawMode
	}
	// A failed restore on the way out has nothing left to tell it to.
	defer func() { _ = term.Restore(fd, state) }()
	return u.interact(u.input(), s)
}

type keyKind int

const (
	keyRune keyKind = iota
	keyEnter
	keyUp
	keyDown
	keyLeft
	keyRight
	keyBackspace
	keySpace
	keyCtrlC
)

type key struct {
	kind keyKind
	r    rune
}

// parseKeys turns bytes read so far into keys -- the arrow sequences, the
// control characters a prompt cares about, and printable runes -- and hands
// back whatever tail cannot be judged yet: an escape whose sequence has not
// finished arriving. A terminal, and a slow link in particular, may deliver
// ESC in one read and "[B" in the next, and typing "[B" into the filter is
// not what the user pressed. A lone escape and every finished sequence that
// is not an arrow are dropped rather than typed.
//
// ESC '[' opens a CSI sequence: parameter bytes 0x30-0x3F, intermediate
// bytes 0x20-0x2F, then one final byte 0x40-0x7E. ESC 'O' opens the SS3 form
// terminals in application-cursor mode use for the arrows.
func parseKeys(b []byte) (keys []key, rest []byte) {
	for len(b) > 0 {
		switch {
		case b[0] == 0x1b:
			n, k, ok := parseEscape(b)
			if !ok {
				return keys, b
			}
			if k != nil {
				keys = append(keys, *k)
			}
			b = b[n:]
		case b[0] == 0x03:
			keys = append(keys, key{kind: keyCtrlC})
			b = b[1:]
		case b[0] == '\r' || b[0] == '\n':
			keys = append(keys, key{kind: keyEnter})
			b = b[1:]
		case b[0] == 0x7f || b[0] == 0x08:
			keys = append(keys, key{kind: keyBackspace})
			b = b[1:]
		case b[0] == ' ':
			keys = append(keys, key{kind: keySpace})
			b = b[1:]
		case b[0] < 0x20:
			b = b[1:]
		default:
			r, n := utf8.DecodeRune(b)
			if r != utf8.RuneError {
				keys = append(keys, key{kind: keyRune, r: r})
			}
			b = b[n:]
		}
	}
	return keys, nil
}

// parseEscape reads one escape sequence at the head of b. It reports how
// many bytes it spans and the key it means (nil for one the prompt ignores),
// or ok=false when b ends before the sequence does.
func parseEscape(b []byte) (n int, k *key, ok bool) {
	if len(b) < 2 {
		return 0, nil, false
	}
	switch b[1] {
	case '[':
		i := 2
		for i < len(b) && b[i] >= 0x20 && b[i] <= 0x3f {
			i++
		}
		if i >= len(b) {
			return 0, nil, false
		}
		final := b[i]
		if final < 0x40 || final > 0x7e {
			// Not a CSI final byte: the ESC and '[' were something else,
			// and the byte is left for the next round.
			return i, nil, true
		}
		// Only the bare arrows count; "\x1b[1;5A" (a modified arrow) and
		// every other final byte are dropped.
		if i == 2 {
			return 3, arrowKey(final), true
		}
		return i + 1, nil, true
	case 'O':
		if len(b) < 3 {
			return 0, nil, false
		}
		return 3, arrowKey(b[2]), true
	}
	// A lone escape, or ESC followed by an ordinary byte (alt-x): the
	// escape is dropped and the byte is read on its own.
	return 1, nil, true
}

func arrowKey(final byte) *key {
	switch final {
	case 'A':
		return &key{kind: keyUp}
	case 'B':
		return &key{kind: keyDown}
	case 'C':
		return &key{kind: keyRight}
	case 'D':
		return &key{kind: keyLeft}
	}
	return nil
}

// interact is the key loop, over any reader so a test can feed it key
// sequences. Lines end in "\r\n" because raw mode has turned off the
// terminal's newline translation.
func (u *UI) interact(in io.Reader, s *selector) error {
	drawn := s.render(u)
	buf := make([]byte, 64)
	// pending is the tail of the last read that parseKeys could not judge:
	// an escape sequence still arriving. It is prepended to the next read.
	// An escape on its own does nothing, so holding one until the next
	// key arrives loses nothing.
	var pending []byte
	for {
		n, err := in.Read(buf)
		if err != nil {
			u.erase(drawn)
			if errors.Is(err, io.EOF) {
				return ErrInterrupted
			}
			return err
		}
		keys, rest := parseKeys(append(pending, buf[:n]...))
		pending = append(pending[:0], rest...)
		for _, k := range keys {
			switch k.kind {
			case keyCtrlC:
				u.erase(drawn)
				fmt.Fprint(u.Out, "^C\r\n")
				return ErrInterrupted
			case keyEnter:
				if !s.multi && len(s.visible) == 0 {
					continue // nothing under the cursor to confirm
				}
				u.erase(drawn)
				u.summary(s)
				return nil
			case keyUp:
				s.move(-1)
			case keyDown:
				s.move(1)
			case keyLeft, keyRight:
				s.toggle()
			case keySpace:
				if s.multi {
					s.toggle()
				} else {
					s.filter += " "
					s.refilter()
				}
			case keyBackspace:
				if s.filter != "" {
					_, size := utf8.DecodeLastRuneInString(s.filter)
					s.filter = s.filter[:len(s.filter)-size]
					s.refilter()
				}
			case keyRune:
				s.filter += string(k.r)
				s.refilter()
			}
		}
		u.erase(drawn)
		drawn = s.render(u)
	}
}

// erase moves the cursor back to where the list began and clears from there
// to the end of the screen.
func (u *UI) erase(lines int) {
	if lines > 0 {
		fmt.Fprintf(u.Out, "\033[%dA", lines)
	}
	fmt.Fprint(u.Out, "\r\033[J")
}

// summary is what stays on screen once the list is gone: the question and
// its answer, one line.
func (u *UI) summary(s *selector) {
	names := make([]string, 0, len(s.chosen()))
	for _, o := range s.chosen() {
		names = append(names, o.Label)
	}
	answer := strings.Join(names, ", ")
	if answer == "" {
		answer = "none"
	}
	fmt.Fprintf(u.Out, "? %s %s\r\n", s.label, u.paint(ansiCyan, answer))
}

// render draws the list and reports how many lines it took, so the next
// redraw knows how far to go back.
func (s *selector) render(u *UI) int {
	// The window shrinks back first: a filter that just removed rows above
	// the cursor would otherwise leave the window past the end of the list,
	// hiding matches above it with nothing on screen to say so.
	if s.top > len(s.visible)-maxRows {
		s.top = max(0, len(s.visible)-maxRows)
	}
	if s.cursor < s.top {
		s.top = s.cursor
	}
	if s.cursor >= s.top+maxRows {
		s.top = s.cursor - maxRows + 1
	}
	lines := 0
	fmt.Fprintf(u.Out, "? %s %s\r\n", s.label, s.filter)
	lines++
	if len(s.visible) == 0 {
		fmt.Fprint(u.Out, u.paint(ansiDim, "  (no match)")+"\r\n")
		lines++
	}
	if s.top > 0 {
		fmt.Fprint(u.Out, u.paint(ansiDim, fmt.Sprintf("  … %d above", s.top))+"\r\n")
		lines++
	}
	for j := s.top; j < len(s.visible) && j < s.top+maxRows; j++ {
		i := s.visible[j]
		o := s.opts[i]
		var row strings.Builder
		if j == s.cursor {
			row.WriteString(u.paint(ansiCyan, "❯ "))
		} else {
			row.WriteString("  ")
		}
		if s.multi {
			if s.selected[i] {
				row.WriteString("◼ ")
			} else {
				row.WriteString("◻ ")
			}
		}
		row.WriteString(o.Label)
		if o.Detail != "" {
			row.WriteString("  " + u.paint(ansiDim, o.Detail))
		}
		fmt.Fprint(u.Out, row.String()+"\r\n")
		lines++
	}
	if below := len(s.visible) - (s.top + maxRows); below > 0 {
		fmt.Fprint(u.Out, u.paint(ansiDim, fmt.Sprintf("  … %d more", below))+"\r\n")
		lines++
	}
	hint := "↑/↓ move · type to filter · enter confirm"
	if s.multi {
		hint = fmt.Sprintf("%d selected · type to filter · space toggle · enter confirm", s.count())
	}
	fmt.Fprint(u.Out, u.paint(ansiDim, hint)+"\r\n")
	lines++
	return lines
}

// --- line mode ---------------------------------------------------------------

// listLine prints the numbered options for line mode.
func (u *UI) listLine(s *selector) {
	fmt.Fprintf(u.Out, "? %s\n", s.label)
	for i, o := range s.opts {
		mark := ""
		if s.multi {
			mark = "◻ "
			if s.selected[i] {
				mark = "◼ "
			}
		}
		line := fmt.Sprintf("  %d) %s%s", i+1, mark, o.Label)
		if o.Detail != "" {
			line += "  " + u.paint(ansiDim, o.Detail)
		}
		fmt.Fprintln(u.Out, line)
	}
}

// selectLine is Select without raw mode: a numbered list and one answer, a
// number or an exact value.
func (u *UI) selectLine(s *selector) (Option, error) {
	u.listLine(s)
	for {
		fmt.Fprintf(u.Out, "  choice [1-%d]: ", len(s.opts))
		line, err := u.readLine()
		if err != nil {
			return Option{}, err
		}
		if i := s.match(line); i >= 0 {
			return s.opts[i], nil
		}
		fmt.Fprintf(u.Out, "  %q is not a number 1-%d or one of the values\n",
			strings.TrimSpace(line), len(s.opts))
	}
}

// multiSelectLine is MultiSelect without raw mode: the numbered list and one
// answer naming every choice, comma- or space-separated. An empty answer
// keeps what was preselected; anything else replaces it.
func (u *UI) multiSelectLine(s *selector) ([]Option, error) {
	u.listLine(s)
	for {
		fmt.Fprintf(u.Out, "  choices [comma-separated; enter keeps %d selected]: ", s.count())
		line, err := u.readLine()
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(line) == "" {
			return s.chosen(), nil
		}
		picked := map[int]bool{}
		bad := ""
		for _, tok := range strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
			i := s.match(tok)
			if i < 0 {
				bad = tok
				break
			}
			picked[i] = true
		}
		if bad != "" {
			fmt.Fprintf(u.Out, "  %q is not a number 1-%d or one of the values\n", bad, len(s.opts))
			continue
		}
		s.selected = picked
		return s.chosen(), nil
	}
}
