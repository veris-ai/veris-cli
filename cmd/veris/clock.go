package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/cli"
)

// sandboxClockCommand is `veris sandbox clock`: read the one virtual clock
// every twin in a sandbox shares, and `set` to move it. The clock is one
// sandbox-wide row behind GET/PATCH …/sandboxes/{id}/clock, so there is no
// --service here: a frozen stripe is a frozen postgres.
func sandboxClockCommand() *cli.Command {
	var getID string
	var o clockSetOptions
	return &cli.Command{
		Name:    "clock",
		Summary: "The sandbox's shared virtual clock: get, set",
		Usage:   "veris sandbox clock [--id ID] [--json]",
		Help: "Without a verb, the clock as the sandbox reads it: `live (+7d)` advances with real time plus the\n" +
			"offset; `frozen at 2026-03-01T09:00:00Z` holds every twin at that second, and pauses outbound\n" +
			"webhook delivery until the clock is live again. `set` moves it.",
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&getID, "id", "", "sandbox id (default: this folder's)")
		},
		Run: func(ctx *cli.Context, args []string) error {
			if err := noPositionals(ctx, args); err != nil {
				return err
			}
			return clockGet(ctx, getID)
		},
		Sub: []*cli.Command{
			{
				Name:    "set",
				Summary: "Freeze the clock, offset it, or set it live",
				Usage:   "veris sandbox clock set (--freeze-at RFC3339|UNIX | --offset ±DURATION | --live) [--id ID] [--yes] [--json]",
				Help: "One of the three: --freeze-at holds every twin at that instant (RFC 3339, or a bare Unix second);\n" +
					"--offset sets the clock live at real time plus the offset (Go durations such as 90m or 36h, plus\n" +
					"d and w for days and weeks: +7d, -2w, 1d12h); --live returns it to real time with no offset.\n" +
					"Moving time backwards is allowed; the server's warning about data now dated in the future is\n" +
					"printed as a ! line. Freezing pauses outbound webhook delivery until `set --live`.",
				Flags: func(fs *flag.FlagSet) {
					fs.StringVar(&o.id, "id", "", "sandbox id (default: this folder's)")
					fs.StringVar(&o.freezeAt, "freeze-at", "", "hold the clock at this instant (RFC 3339 or a Unix second)")
					fs.StringVar(&o.offset, "offset", "", "run live at real time plus this (+7d, -36h, 1w)")
					fs.BoolVar(&o.live, "live", false, "run at real time with no offset")
				},
				Run: func(ctx *cli.Context, args []string) error {
					if err := noPositionals(ctx, args); err != nil {
						return err
					}
					return clockSet(ctx, o)
				},
			},
		},
	}
}

// clockSetOptions are set's flags; exactly one of freezeAt, offset and live
// must be given.
type clockSetOptions struct {
	id       string
	freezeAt string
	offset   string
	live     bool
}

// openSandboxClock is how both clock verbs start: the session, a client and
// the sandbox from GET /v1/sandboxes/{id}, which is where the environment
// id the clock route needs comes from.
func openSandboxClock(ctx *cli.Context, idFlag string) (*session, *api.Client, *api.Sandbox, error) {
	s, err := newSession(ctx, "", idFlag)
	if err != nil {
		return nil, nil, nil, err
	}
	id, err := s.requireSandbox()
	if err != nil {
		return nil, nil, nil, err
	}
	c, err := s.client()
	if err != nil {
		return nil, nil, nil, err
	}
	sb, err := c.GetSandbox(context.Background(), id)
	if err != nil {
		return nil, nil, nil, s.fail("read", "sandbox "+id, err)
	}
	return s, c, sb, nil
}

// clockGet prints the clock:
//
//	Clock  frozen at 2026-03-01T09:00:00Z
//	! Deliveries paused while the clock is frozen
func clockGet(ctx *cli.Context, idFlag string) error {
	s, c, sb, err := openSandboxClock(ctx, idFlag)
	if err != nil {
		return err
	}
	clock, err := c.GetSandboxClock(context.Background(), sb.EnvironmentID, sb.ID)
	if err != nil {
		return s.fail("read", "clock of sandbox "+sb.ID, err)
	}
	if s.ctx.Globals.JSON {
		return printJSON(s.ctx.Stdout, clock)
	}
	s.ui.Info("Clock  %s", clockLabel(clock))
	if clock.Mode == api.ClockModeFrozen {
		s.ui.Warn("Deliveries paused while the clock is frozen")
	}
	return nil
}

// clockSet is one PATCH from the flag given, after the doc's confirmation,
// and prints what the platform says the clock now is. The clock is read
// first so the ✓ line can say what it was, and so entering frozen (as
// opposed to moving an already frozen clock) is told apart.
func clockSet(ctx *cli.Context, o clockSetOptions) error {
	s, c, sb, err := openSandboxClock(ctx, o.id)
	if err != nil {
		return err
	}
	req, err := clockRequest(o)
	if err != nil {
		s.ui.Fail("%v", err)
		return printed(1)
	}
	bg := context.Background()
	before, err := c.GetSandboxClock(bg, sb.EnvironmentID, sb.ID)
	if err != nil {
		return s.fail("read", "clock of sandbox "+sb.ID, err)
	}
	if err := confirm(s.ui, fmt.Sprintf("%s of sandbox %s (now %s)?", clockVerb(req), sb.ID, clockLabel(before))); err != nil {
		return err
	}
	res, err := c.SetSandboxClock(bg, sb.EnvironmentID, sb.ID, req)
	if err != nil {
		return s.fail("set", "clock of sandbox "+sb.ID, err)
	}
	after := &res.Clock
	if after.Mode == api.ClockModeFrozen && before.Mode != api.ClockModeFrozen {
		s.ui.Warn("The clock is now frozen: outbound webhook delivery is paused until veris sandbox clock set --live")
	}
	for _, w := range res.Warnings {
		s.ui.Warn("Server: %s", w)
	}
	s.ui.Success("Clock %s (was %s)", clockLabel(after), clockWas(before))
	if s.ctx.Globals.JSON {
		return printJSON(s.ctx.Stdout, res)
	}
	return nil
}

// clockRequest turns set's flags into the PATCH body, refusing none or
// several of them. --offset sets the mode live as well: an offset on a
// frozen clock changes nothing the code under test can see.
func clockRequest(o clockSetOptions) (api.SetSandboxClockRequest, error) {
	given := 0
	for _, on := range []bool{o.freezeAt != "", o.offset != "", o.live} {
		if on {
			given++
		}
	}
	if given != 1 {
		return api.SetSandboxClockRequest{}, errors.New("sandbox clock set takes exactly one of --freeze-at, --offset or --live")
	}
	live, frozen := api.ClockModeLive, api.ClockModeFrozen
	switch {
	case o.freezeAt != "":
		at, err := parseFreezeAt(o.freezeAt)
		if err != nil {
			return api.SetSandboxClockRequest{}, err
		}
		return api.SetSandboxClockRequest{Mode: &frozen, FrozenTime: &at}, nil
	case o.offset != "":
		d, err := parseOffset(o.offset)
		if err != nil {
			return api.SetSandboxClockRequest{}, err
		}
		secs := int64(d / time.Second)
		return api.SetSandboxClockRequest{Mode: &live, OffsetSeconds: &secs, ClearFrozenTime: true}, nil
	}
	zero := int64(0)
	return api.SetSandboxClockRequest{Mode: &live, OffsetSeconds: &zero, ClearFrozenTime: true}, nil
}

// clockVerb is the confirmation's first words for a request.
func clockVerb(req api.SetSandboxClockRequest) string {
	switch {
	case req.FrozenTime != nil:
		return "Freeze the clock at " + unixStamp(*req.FrozenTime)
	case req.OffsetSeconds != nil && *req.OffsetSeconds != 0:
		return "Set the clock live at real time " + formatOffset(*req.OffsetSeconds)
	}
	return "Set the clock live at real time"
}

// clockLabel is the clock in the doc's words: "frozen at 2026-03-01T09:00:00Z",
// "live (+7d)", or plain "live" when nothing is added to real time.
func clockLabel(c *api.SandboxClock) string {
	if c.Mode == api.ClockModeFrozen && c.FrozenTime != nil {
		return "frozen at " + unixStamp(*c.FrozenTime)
	}
	if c.OffsetSeconds == 0 {
		return "live"
	}
	return "live (" + formatOffset(c.OffsetSeconds) + ")"
}

// clockWas is what the clock read before a change: a frozen clock by its
// hold time, a live one with the virtual instant it had reached, so the ✓
// line shows how far time moved.
func clockWas(c *api.SandboxClock) string {
	if c.Mode == api.ClockModeFrozen && c.FrozenTime != nil {
		return clockLabel(c)
	}
	return clockLabel(c) + ", " + unixStamp(time.Now().Unix()+c.OffsetSeconds)
}

// unixStamp renders a Unix second as RFC 3339 UTC, the form --freeze-at
// takes, so what is printed can be pasted back.
func unixStamp(sec int64) string {
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

// parseFreezeAt reads --freeze-at as RFC 3339 or as a bare Unix second.
func parseFreezeAt(v string) (int64, error) {
	v = strings.TrimSpace(v)
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("--freeze-at: '%s' is before 1970", v)
		}
		return n, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return 0, fmt.Errorf("--freeze-at: '%s' is not RFC 3339 (2026-03-01T09:00:00Z) or a Unix second", v)
	}
	return t.Unix(), nil
}

// parseOffset reads --offset: an optional sign, then Go duration units
// (h, m, s, …) plus d for days and w for weeks, in any order: "+7d",
// "-36h", "1w2d", "1d12h30m". "0" is a plain zero.
func parseOffset(v string) (time.Duration, error) {
	raw := strings.TrimSpace(v)
	bad := func() (time.Duration, error) {
		return 0, fmt.Errorf("--offset: '%s' is not a duration (try +7d, -36h or 1w)", v)
	}
	sign := time.Duration(1)
	switch {
	case strings.HasPrefix(raw, "+"):
		raw = raw[1:]
	case strings.HasPrefix(raw, "-"):
		raw, sign = raw[1:], -1
	}
	if raw == "" {
		return bad()
	}
	if raw == "0" {
		return 0, nil
	}
	var total time.Duration
	for raw != "" {
		i := 0
		for i < len(raw) && (raw[i] >= '0' && raw[i] <= '9' || raw[i] == '.') {
			i++
		}
		j := i
		for j < len(raw) && (raw[j] < '0' || raw[j] > '9') && raw[j] != '.' {
			j++
		}
		if i == 0 || j == i {
			return bad()
		}
		num, unit := raw[:i], raw[i:j]
		raw = raw[j:]
		switch unit {
		case "d", "w":
			f, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return bad()
			}
			days := 1.0
			if unit == "w" {
				days = 7
			}
			total += time.Duration(f * days * float64(24*time.Hour))
		default:
			d, err := time.ParseDuration(num + unit)
			if err != nil {
				return bad()
			}
			total += d
		}
	}
	return sign * total, nil
}

// formatOffset renders seconds as a signed duration in the largest whole
// units: "+7d", "-36h", "+1d12h30m", "+45s".
func formatOffset(secs int64) string {
	sign := "+"
	if secs < 0 {
		sign, secs = "-", -secs
	}
	if secs == 0 {
		return "+0s"
	}
	var b strings.Builder
	b.WriteString(sign)
	units := []struct {
		name string
		size int64
	}{{"d", 86400}, {"h", 3600}, {"m", 60}, {"s", 1}}
	for _, u := range units {
		if n := secs / u.size; n > 0 {
			fmt.Fprintf(&b, "%d%s", n, u.name)
			secs -= n * u.size
		}
	}
	return b.String()
}
