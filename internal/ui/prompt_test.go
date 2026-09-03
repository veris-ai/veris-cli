package ui

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

// tty is a UI whose stdin is the given text and which is allowed to prompt,
// though In is not a terminal -- so Select and MultiSelect take the line-mode
// path, and Confirm and Input read lines as they always do.
func tty(in string) (*UI, *bytes.Buffer) {
	var out bytes.Buffer
	return &UI{Out: &out, In: strings.NewReader(in), TTY: true}, &out
}

var three = []Option{
	{Value: "alpha", Label: "Alpha", Detail: "first"},
	{Value: "beta", Label: "Beta"},
	{Value: "gamma", Label: "Gamma", Detail: "third"},
}

func TestNoTTYError(t *testing.T) {
	var out bytes.Buffer
	u := &UI{Out: &out, In: strings.NewReader("y\n")}
	const want = "Error: Interactive prompt requires a TTY. Pass --yes instead."

	check := func(name string, err error) {
		t.Helper()
		var nt *NoTTYError
		if !errors.As(err, &nt) {
			t.Fatalf("%s: got %v, want *NoTTYError", name, err)
		}
		if nt.FlagHint != "--yes" || err.Error() != want {
			t.Errorf("%s: %q, want %q", name, err.Error(), want)
		}
	}
	_, err := u.Confirm("Delete?", false, "--yes")
	check("Confirm", err)
	_, err = u.Input("Name", "", "--yes")
	check("Input", err)
	_, err = u.Select("Pick", three, "--yes")
	check("Select", err)
	_, err = u.MultiSelect("Pick", three, nil, "--yes")
	check("MultiSelect", err)
	if out.Len() != 0 {
		t.Errorf("a refused prompt printed %q", out.String())
	}
}

func TestConfirmAssumeYes(t *testing.T) {
	var out bytes.Buffer
	// In is nil on purpose: --yes must not read stdin at all.
	u := &UI{Out: &out, AssumeYes: true}
	ok, err := u.Confirm("Delete environment dev?", false, "--yes")
	if err != nil || !ok {
		t.Fatalf("Confirm = %v, %v; want true, nil", ok, err)
	}
	if got := out.String(); got != "Delete environment dev? y\n" {
		t.Fatalf("echo %q", got)
	}

	out.Reset()
	u.Quiet = true
	if ok, err := u.Confirm("Again?", false, "--yes"); err != nil || !ok || out.Len() != 0 {
		t.Fatalf("quiet --yes: %v, %v, printed %q", ok, err, out.String())
	}
}

func TestConfirm(t *testing.T) {
	cases := []struct {
		in      string
		def     bool
		want    bool
		wantErr error
		output  string
	}{
		{"y\n", false, true, nil, "Delete? [y/N] "},
		{"Y\n", false, true, nil, "Delete? [y/N] "},
		{"yes\n", false, true, nil, "Delete? [y/N] "},
		{" n \n", true, false, nil, "Delete? [Y/n] "},
		{"no\n", true, false, nil, "Delete? [Y/n] "},
		{"\n", false, false, nil, "Delete? [y/N] "},
		{"\n", true, true, nil, "Delete? [Y/n] "},
		{"y", false, true, nil, "Delete? [y/N] "}, // no trailing newline still answers
		{"maybe\ny\n", false, true, nil, "Delete? [y/N] Please answer y or n.\nDelete? [y/N] "},
		{"", false, false, ErrInterrupted, "Delete? [y/N] "},
		{"maybe\n", false, false, ErrInterrupted, "Delete? [y/N] Please answer y or n.\nDelete? [y/N] "},
	}
	for _, c := range cases {
		u, out := tty(c.in)
		got, err := u.Confirm("Delete?", c.def, "--yes")
		if !errors.Is(err, c.wantErr) || got != c.want {
			t.Errorf("Confirm(%q, def=%v) = %v, %v; want %v, %v", c.in, c.def, got, err, c.want, c.wantErr)
		}
		if out.String() != c.output {
			t.Errorf("Confirm(%q) printed %q, want %q", c.in, out.String(), c.output)
		}
	}
}

func TestInput(t *testing.T) {
	cases := []struct {
		in, def, want string
		wantErr       error
		output        string
	}{
		{"checkout-svc\n", "", "checkout-svc", nil, "? Project name: "},
		{"\n", "veris-proxy", "veris-proxy", nil, "? Project name (veris-proxy): "},
		{"  spaced  \n", "", "spaced", nil, "? Project name: "},
		{"typed", "def", "typed", nil, "? Project name (def): "},
		{"", "def", "", ErrInterrupted, "? Project name (def): "},
	}
	for _, c := range cases {
		u, out := tty(c.in)
		got, err := u.Input("Project name", c.def, "--name")
		if !errors.Is(err, c.wantErr) || got != c.want {
			t.Errorf("Input(%q, def=%q) = %q, %v; want %q, %v", c.in, c.def, got, err, c.want, c.wantErr)
		}
		if out.String() != c.output {
			t.Errorf("Input(%q) printed %q, want %q", c.in, out.String(), c.output)
		}
	}
}

func TestPromptsShareOneReader(t *testing.T) {
	// Two prompts on one stdin: the first must not swallow the second's line.
	u, _ := tty("y\nname\n")
	if ok, err := u.Confirm("Go?", false, "--yes"); err != nil || !ok {
		t.Fatalf("Confirm: %v, %v", ok, err)
	}
	if got, err := u.Input("Name", "", "--name"); err != nil || got != "name" {
		t.Fatalf("Input: %q, %v", got, err)
	}
}

func TestSelectLineMode(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string // Value
		wantErr error
		output  []string // fragments, in order
	}{
		{"index", "2\n", "beta", nil, []string{"? Environment\n", "  1) Alpha  first\n", "  2) Beta\n", "  3) Gamma  third\n", "  choice [1-3]: "}},
		{"value", "gamma\n", "gamma", nil, nil},
		{"index without newline", "1", "alpha", nil, nil},
		{"bad number then good", "9\n1\n", "alpha", nil, []string{`"9" is not a number 1-3 or one of the values`, "choice [1-3]: "}},
		{"label is not a value", "Beta\nbeta\n", "beta", nil, []string{`"Beta" is not a number`}},
		{"zero", "0\n3\n", "gamma", nil, []string{`"0" is not a number`}},
		{"eof", "", "", ErrInterrupted, nil},
		{"eof after junk", "x\n", "", ErrInterrupted, nil},
	}
	for _, c := range cases {
		u, out := tty(c.in)
		got, err := u.Select("Environment", three, "--env")
		if !errors.Is(err, c.wantErr) || got.Value != c.want {
			t.Errorf("%s: Select = %+v, %v; want value %q, %v", c.name, got, err, c.want, c.wantErr)
		}
		rest := out.String()
		for _, frag := range c.output {
			i := strings.Index(rest, frag)
			if i < 0 {
				t.Errorf("%s: output %q lacks %q (in order)", c.name, out.String(), frag)
				break
			}
			rest = rest[i+len(frag):]
		}
	}

	u, _ := tty("1\n")
	if _, err := u.Select("Nothing", nil, "--env"); err == nil {
		t.Error("Select with no options succeeded")
	}
}

func TestMultiSelectLineMode(t *testing.T) {
	values := func(opts []Option) []string {
		var v []string
		for _, o := range opts {
			v = append(v, o.Value)
		}
		return v
	}
	cases := []struct {
		name        string
		in          string
		preselected []string
		want        []string
		wantErr     error
		output      []string
	}{
		{"enter keeps preselected", "\n", []string{"gamma", "alpha"}, []string{"alpha", "gamma"}, nil,
			[]string{"  1) ◼ Alpha  first\n", "  2) ◻ Beta\n", "  3) ◼ Gamma  third\n", "enter keeps 2 selected]: "}},
		{"enter with nothing preselected", "\n", nil, nil, nil, []string{"enter keeps 0 selected"}},
		{"indices replace", "3,1\n", []string{"beta"}, []string{"alpha", "gamma"}, nil, nil},
		{"values and spaces", "beta gamma\n", nil, []string{"beta", "gamma"}, nil, nil},
		{"mixed with a repeat", "1, alpha ,2\n", nil, []string{"alpha", "beta"}, nil, nil},
		{"bad token then good", "1,zeta\n2\n", nil, []string{"beta"}, nil, []string{`"zeta" is not a number 1-3`}},
		{"unknown preselected ignored", "\n", []string{"nope"}, nil, nil, nil},
		{"eof", "", []string{"alpha"}, nil, ErrInterrupted, nil},
	}
	for _, c := range cases {
		u, out := tty(c.in)
		got, err := u.MultiSelect("Services", three, c.preselected, "--service")
		if !errors.Is(err, c.wantErr) || !reflect.DeepEqual(values(got), c.want) {
			t.Errorf("%s: MultiSelect = %v, %v; want %v, %v", c.name, values(got), err, c.want, c.wantErr)
		}
		rest := out.String()
		for _, frag := range c.output {
			i := strings.Index(rest, frag)
			if i < 0 {
				t.Errorf("%s: output %q lacks %q (in order)", c.name, out.String(), frag)
				break
			}
			rest = rest[i+len(frag):]
		}
	}

	u, _ := tty("1\n")
	if _, err := u.MultiSelect("Nothing", nil, nil, "--service"); err == nil {
		t.Error("MultiSelect with no options succeeded")
	}
}

func TestInteractRawNeedsTerminal(t *testing.T) {
	u, _ := tty("")
	if err := u.interactRaw(newSelector("x", three, false, nil)); !errors.Is(err, errNoRawMode) {
		t.Fatalf("interactRaw off a terminal = %v, want errNoRawMode", err)
	}
}

func TestParseKeys(t *testing.T) {
	cases := []struct {
		in   string
		want []key
		rest string // what an unfinished sequence leaves for the next read
	}{
		{"\x1b[A\x1b[B\x1b[C\x1b[D", []key{{kind: keyUp}, {kind: keyDown}, {kind: keyRight}, {kind: keyLeft}}, ""},
		{"\x1bOA\x1bOB\x1bOC\x1bOD", []key{{kind: keyUp}, {kind: keyDown}, {kind: keyRight}, {kind: keyLeft}}, ""}, // application-cursor mode
		{"\r", []key{{kind: keyEnter}}, ""},
		{"\n", []key{{kind: keyEnter}}, ""},
		{"\x03", []key{{kind: keyCtrlC}}, ""},
		{"\x7f\x08", []key{{kind: keyBackspace}, {kind: keyBackspace}}, ""},
		{" ", []key{{kind: keySpace}}, ""},
		{"ab", []key{{kind: keyRune, r: 'a'}, {kind: keyRune, r: 'b'}}, ""},
		{"é", []key{{kind: keyRune, r: 'é'}}, ""},
		{"\x1b[Z", nil, ""},                           // an unknown sequence is dropped, not typed
		{"\x1b[3~", nil, ""},                          // Delete: parameter byte then '~', nothing leaks
		{"\x1b[1;5A", nil, ""},                        // Ctrl-Up is not Up, and ";5A" is not typed
		{"\x1b[H\x1b[F", nil, ""},                     // Home, End
		{"\x1b[?25l", nil, ""},                        // a private-mode sequence, whole
		{"\x1bOP", nil, ""},                           // F1 in SS3 form
		{"\x1bx", []key{{kind: keyRune, r: 'x'}}, ""}, // alt-x: the escape goes, the letter stays
		{"\x01\x1a", nil, ""},                         // other control characters are ignored
		{"\x1b[Ax\r", []key{{kind: keyUp}, {kind: keyRune, r: 'x'}, {kind: keyEnter}}, ""},
		{"\x1b[3~\x7f", []key{{kind: keyBackspace}}, ""},
		// Unfinished sequences wait for the next read.
		{"\x1b", nil, "\x1b"},
		{"\x1b[", nil, "\x1b["},
		{"\x1b[1;5", nil, "\x1b[1;5"},
		{"\x1bO", nil, "\x1bO"},
		{"a\x1b[", []key{{kind: keyRune, r: 'a'}}, "\x1b["},
	}
	for _, c := range cases {
		got, rest := parseKeys([]byte(c.in))
		if !reflect.DeepEqual(got, c.want) || string(rest) != c.rest {
			t.Errorf("parseKeys(%q) = %+v, %q; want %+v, %q", c.in, got, rest, c.want, c.rest)
		}
	}
}

// chunked hands the key loop its bytes one read at a time, the way a
// terminal delivers keystrokes, so a sequence split across reads is covered.
type chunked struct{ parts []string }

func (c *chunked) Read(p []byte) (int, error) {
	if len(c.parts) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.parts[0])
	c.parts = c.parts[1:]
	return n, nil
}

func TestInteractSelect(t *testing.T) {
	cases := []struct {
		name    string
		keys    []string
		want    string // Value
		wantErr error
		output  []string // fragments of the final summary or last frame
	}{
		{"enter takes the first", []string{"\r"}, "alpha", nil, []string{"? Environment Alpha\r\n"}},
		{"down twice", []string{"\x1b[B", "\x1b[B", "\r"}, "gamma", nil, nil},
		{"wraps past the end", []string{"\x1b[B\x1b[B\x1b[B", "\r"}, "alpha", nil, nil},
		{"up wraps to the end", []string{"\x1b[A", "\r"}, "gamma", nil, nil},
		{"typing filters by prefix", []string{"g", "a", "\r"}, "gamma", nil, nil},
		{"filter is case-insensitive and substring", []string{"MM", "\r"}, "gamma", nil, nil},
		{"prefix beats substring", []string{"a", "\r"}, "alpha", nil, nil}, // Alpha (prefix) before Beta, Gamma (contain a)
		{"backspace widens again", []string{"gx", "\x7f", "\r"}, "gamma", nil, nil},
		{"enter on no match is ignored", []string{"zzz", "\r", "\x7f\x7f\x7f", "\r"}, "alpha", nil, []string{"(no match)"}},
		{"cursor follows the option through a filter", []string{"\x1b[B", "b", "\r"}, "beta", nil, nil},
		{"an arrow split across reads is still an arrow", []string{"\x1b", "[B", "\r"}, "beta", nil, nil},
		{"an arrow split three ways", []string{"\x1b", "[", "B\r"}, "beta", nil, nil},
		{"a lone escape before a letter is dropped", []string{"\x1b", "g", "\r"}, "gamma", nil, nil},
		{"delete does not type into the filter", []string{"g", "\x1b[3~", "\r"}, "gamma", nil, nil},
		{"ctrl-c", []string{"\x1b[B", "\x03"}, "", ErrInterrupted, []string{"^C\r\n"}},
		{"stdin closed", []string{}, "", ErrInterrupted, nil},
	}
	for _, c := range cases {
		var out bytes.Buffer
		u := &UI{Out: &out, TTY: true}
		s := newSelector("Environment", three, false, nil)
		err := u.interact(&chunked{c.keys}, s)
		if !errors.Is(err, c.wantErr) {
			t.Errorf("%s: interact = %v, want %v", c.name, err, c.wantErr)
			continue
		}
		if err == nil {
			if got := s.chosen(); len(got) != 1 || got[0].Value != c.want {
				t.Errorf("%s: chose %+v, want %q", c.name, got, c.want)
			}
		}
		for _, frag := range c.output {
			if !strings.Contains(out.String(), frag) {
				t.Errorf("%s: output %q lacks %q", c.name, out.String(), frag)
			}
		}
	}
}

func TestInteractMultiSelect(t *testing.T) {
	values := func(opts []Option) []string {
		var v []string
		for _, o := range opts {
			v = append(v, o.Value)
		}
		return v
	}
	cases := []struct {
		name        string
		keys        []string
		preselected []string
		want        []string
		output      []string
	}{
		{"enter keeps preselected", []string{"\r"}, []string{"beta"}, []string{"beta"}, []string{"? Services Beta\r\n"}},
		{"space toggles on", []string{" ", "\x1b[B", " ", "\r"}, nil, []string{"alpha", "beta"}, nil},
		{"space toggles off", []string{" ", "\r"}, []string{"alpha"}, nil, []string{"? Services none\r\n"}},
		{"right and left toggle too", []string{"\x1b[C", "\x1b[B", "\x1b[D", "\r"}, nil, []string{"alpha", "beta"}, nil},
		// The cursor stays on Gamma when the filter clears, so Down wraps to Alpha.
		{"filter then toggle keeps list order", []string{"gam", " ", "\x7f\x7f\x7f", "\x1b[B", " ", "\r"}, nil, []string{"alpha", "gamma"}, nil},
		{"enter with no match still confirms", []string{"zzz", "\r"}, []string{"gamma"}, []string{"gamma"}, nil},
	}
	for _, c := range cases {
		var out bytes.Buffer
		u := &UI{Out: &out, TTY: true}
		s := newSelector("Services", three, true, c.preselected)
		if err := u.interact(&chunked{c.keys}, s); err != nil {
			t.Errorf("%s: interact = %v", c.name, err)
			continue
		}
		if got := values(s.chosen()); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: chose %v, want %v", c.name, got, c.want)
		}
		for _, frag := range c.output {
			if !strings.Contains(out.String(), frag) {
				t.Errorf("%s: output %q lacks %q", c.name, out.String(), frag)
			}
		}
	}
}

func TestRender(t *testing.T) {
	var out bytes.Buffer
	u := &UI{Out: &out, TTY: true}
	s := newSelector("Services", three, true, []string{"beta"})
	s.move(1)
	lines := s.render(u)
	want := "" +
		"? Services \r\n" +
		"  ◻ Alpha  first\r\n" +
		"❯ ◼ Beta\r\n" +
		"  ◻ Gamma  third\r\n" +
		"1 selected · type to filter · space toggle · enter confirm\r\n"
	if out.String() != want {
		t.Fatalf("render:\n%q\nwant:\n%q", out.String(), want)
	}
	if lines != 5 {
		t.Fatalf("render reported %d lines, want 5", lines)
	}

	out.Reset()
	single := newSelector("Environment", three, false, nil)
	single.filter = "a"
	single.refilter()
	single.render(u)
	got := out.String()
	if !strings.HasPrefix(got, "? Environment a\r\n❯ Alpha  first\r\n") {
		t.Errorf("single render with filter: %q", got)
	}
	if !strings.HasSuffix(got, "↑/↓ move · type to filter · enter confirm\r\n") {
		t.Errorf("single render hint: %q", got)
	}

	// Erase goes back exactly as many lines as were drawn.
	out.Reset()
	u.erase(5)
	if out.String() != "\033[5A\r\033[J" {
		t.Errorf("erase(5) = %q", out.String())
	}
	out.Reset()
	u.erase(0)
	if out.String() != "\r\033[J" {
		t.Errorf("erase(0) = %q", out.String())
	}
}

func TestRenderScrolls(t *testing.T) {
	var opts []Option
	for _, n := range strings.Split("a b c d e f g h i j k l m", " ") {
		opts = append(opts, Option{Value: n, Label: n})
	}
	var out bytes.Buffer
	u := &UI{Out: &out, TTY: true}
	s := newSelector("Many", opts, false, nil)
	lines := s.render(u)
	// label + 10 rows + "… 3 more" + hint
	if lines != 13 || !strings.Contains(out.String(), "… 3 more") || strings.Contains(out.String(), "  k\r\n") {
		t.Fatalf("first page: %d lines, %q", lines, out.String())
	}
	out.Reset()
	s.move(-1) // wraps to the last option, which must scroll into view
	lines = s.render(u)
	got := out.String()
	if !strings.Contains(got, "❯ m\r\n") || strings.Contains(got, "  a\r\n") {
		t.Fatalf("scrolled page: %q", got)
	}
	// The hidden rows are now above, and the frame says so instead of
	// claiming three more below.
	if !strings.Contains(got, "… 3 above\r\n") || strings.Contains(got, "more") || lines != 13 {
		t.Fatalf("scrolled page hides rows above: %d lines, %q", lines, got)
	}
	out.Reset()
	s.move(-1) // one up from the end: three above, then rows c..l, none below
	s.render(u)
	got = out.String()
	if !strings.Contains(got, "… 3 above") || strings.Contains(got, "more") || !strings.Contains(got, "❯ l\r\n") {
		t.Fatalf("second scrolled page: %q", got)
	}
}

// A filter that removes rows above the cursor must pull the window back so
// every remaining match is drawn: here "ma" (a prefix match, first in the
// list) must not vanish above a window left over from the unfiltered list.
func TestRenderClampsWindowAfterFilter(t *testing.T) {
	var opts []Option
	for _, n := range strings.Split("ma b c d e f g h i j k l am", " ") {
		opts = append(opts, Option{Value: n, Label: n})
	}
	var out bytes.Buffer
	u := &UI{Out: &out, TTY: true}
	s := newSelector("Many", opts, false, nil)
	s.render(u)
	s.move(-1) // cursor on "am", window scrolled to the end
	s.render(u)
	s.filter = "m"
	s.refilter()
	out.Reset()
	lines := s.render(u)
	got := out.String()
	want := "? Many m\r\n  ma\r\n❯ am\r\n↑/↓ move · type to filter · enter confirm\r\n"
	if got != want || lines != 4 {
		t.Fatalf("filtered frame (%d lines):\n%q\nwant:\n%q", lines, got, want)
	}
}
