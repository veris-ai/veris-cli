package ui

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMaskKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"vsk_mi4pa0uo1234567890", "vsk_mi4pa0uo…"},
		{"vsk_mi4pa0uo1", "vsk_mi4pa0uo…"}, // 13: just over the long threshold
		{"vsk_mi4pa0uo", "vsk_…"},          // exactly 12 is the short form
		{"vsk_a", "vsk_…"},                 // 5 is the shortest short form
		{"vsk_", "…"},                      // 4 shows nothing
		{"", "…"},
		{"abc", "…"},
	}
	for _, c := range cases {
		if got := MaskKey(c.in); got != c.want {
			t.Errorf("MaskKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMarksPlain(t *testing.T) {
	cases := []struct {
		name string
		call func(u *UI)
		want string
	}{
		{"success", func(u *UI) { u.Success("built %s", "x") }, "✓ built x\n"},
		{"fail", func(u *UI) { u.Fail("no %d", 1) }, "✗ no 1\n"},
		{"warn", func(u *UI) { u.Warn("hm") }, "! hm\n"},
		{"link", func(u *UI) { u.Link("https://studio.veris.ai/e/1") }, "→ https://studio.veris.ai/e/1\n"},
		{"next", func(u *UI) { u.Next("veris sandbox create") }, "→ Next: veris sandbox create\n"},
		{"info", func(u *UI) { u.Info("plain %v", true) }, "plain true\n"},
		{"detail", func(u *UI) { u.Detail("under") }, "  under\n"},
	}
	for _, c := range cases {
		var out bytes.Buffer
		u := &UI{Out: &out}
		c.call(u)
		if out.String() != c.want {
			t.Errorf("%s: got %q, want %q", c.name, out.String(), c.want)
		}
	}
}

func TestMarksColor(t *testing.T) {
	cases := []struct {
		name string
		call func(u *UI)
		want string
	}{
		{"success", func(u *UI) { u.Success("ok") }, "\033[32m✓\033[0m ok\n"},
		{"fail", func(u *UI) { u.Fail("bad") }, "\033[31m✗\033[0m bad\n"},
		{"warn", func(u *UI) { u.Warn("hm") }, "\033[33m!\033[0m hm\n"},
		{"link", func(u *UI) { u.Link("u") }, "\033[2m→ u\033[0m\n"},
		{"next", func(u *UI) { u.Next("c") }, "→ Next: c\n"},
	}
	for _, c := range cases {
		var out bytes.Buffer
		u := &UI{Out: &out, Color: true}
		c.call(u)
		if out.String() != c.want {
			t.Errorf("%s: got %q, want %q", c.name, out.String(), c.want)
		}
	}
}

func TestColorDetection(t *testing.T) {
	env := func(vars map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
	}
	cases := []struct {
		name     string
		terminal bool
		vars     map[string]string
		want     bool
	}{
		{"not a terminal", false, map[string]string{"TERM": "xterm"}, false},
		{"terminal", true, map[string]string{"TERM": "xterm-256color"}, true},
		{"no TERM at all", true, map[string]string{}, true},
		{"NO_COLOR empty still counts", true, map[string]string{"TERM": "xterm", "NO_COLOR": ""}, false},
		{"NO_COLOR=1", true, map[string]string{"TERM": "xterm", "NO_COLOR": "1"}, false},
		{"TERM=dumb", true, map[string]string{"TERM": "dumb"}, false},
	}
	for _, c := range cases {
		if got := colorEnabled(c.terminal, env(c.vars)); got != c.want {
			t.Errorf("%s: colorEnabled = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestNewOffTerminal(t *testing.T) {
	// Buffers and nil are not files: neither detection may panic, and both
	// answers are "no".
	var out bytes.Buffer
	for _, in := range []interface{ Read([]byte) (int, error) }{strings.NewReader(""), nil} {
		u := New(&out, in)
		if u.TTY || u.Color {
			t.Errorf("New(buffer, %T): TTY=%v Color=%v, want both false", in, u.TTY, u.Color)
		}
		if u.Out != &out {
			t.Error("New did not keep Out")
		}
	}
}

func TestQuietSuppression(t *testing.T) {
	var out bytes.Buffer
	u := &UI{Out: &out, Quiet: true}
	u.Success("s")
	u.Info("i")
	u.Detail("d")
	u.Link("l")
	u.Next("n")
	if out.Len() != 0 {
		t.Fatalf("quiet UI printed %q", out.String())
	}
	u.Fail("f")
	u.Warn("w")
	if got := out.String(); got != "✗ f\n! w\n" {
		t.Fatalf("quiet UI: Fail and Warn printed %q", got)
	}
}

func TestTable(t *testing.T) {
	var out bytes.Buffer
	u := &UI{Out: &out}
	u.Table([]string{"NAME", "ID", "STATUS"}, [][]string{
		{"dev", "k3j2v0d8p1q7", "ready"},
		{"staging-long", "x", "provisioning"},
		{"ci", "abc", ""},
	})
	want := "" +
		"NAME          ID            STATUS\n" +
		"dev           k3j2v0d8p1q7  ready\n" +
		"staging-long  x             provisioning\n" +
		"ci            abc           \n"
	if out.String() != want {
		t.Fatalf("Table:\n%s\nwant:\n%s", out.String(), want)
	}

	out.Reset()
	u.Table(nil, [][]string{{"a", "b"}})
	if out.String() != "a  b\n" {
		t.Fatalf("Table without header: %q", out.String())
	}
}

func TestSpinnerOffTerminal(t *testing.T) {
	var out bytes.Buffer
	u := &UI{Out: &out}
	s := u.Spinner("creating sandbox")
	s.Update("creating sandbox") // unchanged: not repeated
	s.Update("waiting for ready")
	s.Stop()
	s.Stop()
	if got := out.String(); got != "creating sandbox\nwaiting for ready\n" {
		t.Fatalf("non-TTY spinner printed %q", got)
	}
}

func TestSpinnerQuiet(t *testing.T) {
	var out bytes.Buffer
	u := &UI{Out: &out, Quiet: true, TTY: true}
	s := u.Spinner("x")
	s.Update("y")
	s.Stop()
	if out.Len() != 0 {
		t.Fatalf("quiet spinner printed %q", out.String())
	}
}

// syncBuffer is a bytes.Buffer the spinner goroutine and the test can share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestSpinnerOnTerminal(t *testing.T) {
	old := spinnerInterval
	spinnerInterval = time.Millisecond
	defer func() { spinnerInterval = old }()

	out := &syncBuffer{}
	u := &UI{Out: out, TTY: true}
	s := u.Spinner("first")
	time.Sleep(20 * time.Millisecond)
	s.Update("second")
	time.Sleep(20 * time.Millisecond)
	s.Stop()
	s.Stop() // idempotent
	got := out.String()
	if !strings.Contains(got, "\r\033[K"+spinnerFrames[0]+" first") {
		t.Errorf("no first frame in %q", got)
	}
	if !strings.Contains(got, " second") {
		t.Errorf("label update not drawn in %q", got)
	}
	if !strings.HasSuffix(got, "\r\033[K") {
		t.Errorf("Stop did not clear the line: %q", got)
	}
	// Nothing is written after Stop returns.
	time.Sleep(5 * time.Millisecond)
	if out.String() != got {
		t.Error("spinner wrote after Stop")
	}
}
