package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/twin"
	"github.com/veris-ai/veris-cli/internal/ui"
)

// printedError is an error whose message is already on stderr, in the ✗
// grammar of internal/ui, and that carries only an exit status out. main's
// exitStatusTo returns the code without printing: the "veris: …" line every
// other error earns would say the same thing a second time, in a different
// voice. fail, notLoggedIn and the session's require* helpers all return one;
// a command that has printed its own ✗ line returns printed(1) itself.
//
// It is distinct from run.go's exitCode on purpose: that one is the child's
// status passed through, and a reader (or a test) should be able to tell "the
// command under test exited 1" from "veris reported a failure and exited 1".
type printedError struct{ code int }

func (e printedError) Error() string {
	return fmt.Sprintf("exit status %d (already reported on stderr)", e.code)
}

// printed returns the error for a failure whose message is already printed.
// The code is 1 for every failure and 4 for an indeterminate outcome.
func printed(code int) error { return printedError{code: code} }

// errDeclined is what confirm returns when the user answered no. It is a
// plain error -- "veris: declined" on stderr, exit 1 -- so a script sees the
// prompt was refused rather than a silent 0 that looks like the action ran.
var errDeclined = errors.New("declined")

// fail reports err in the doc's grammar and returns printed(1):
//
//	✗ Failed to add to stripe: [422]
//	  customers[0].email: must be a string
//	  unknown table 'customer'
//
// A control-plane or twin error with Reasons puts each on an indented line of
// its own; any other error is one line, "Failed to <verb> <noun>: <err>", so
// an *api.Error reads "[404] environment k3j2… not found". A 401 from the
// control plane is not a "Failed to": the key was refused, so the line is the
// not-logged-in one and the next step is veris login. Without a session the
// profile is unknown and that line names none; prefer (*session).fail, which
// knows it.
//
// Errors that are already someone's message pass through unchanged: a
// printedError (printed once already), ui.ErrInterrupted (the user left; main
// exits quietly) and a *ui.NoTTYError (its text names the flag to pass).
func fail(u *ui.UI, verb, noun string, err error) error {
	return failAs(u, "", verb, noun, err)
}

func failAs(u *ui.UI, profile, verb, noun string, err error) error {
	var already printedError
	var noTTY *ui.NoTTYError
	if errors.As(err, &already) || errors.Is(err, ui.ErrInterrupted) || errors.As(err, &noTTY) {
		return err
	}
	status, reasons := describe(err)
	if status == http.StatusUnauthorized {
		var ae *api.Error
		if errors.As(err, &ae) {
			return notLoggedIn(u, profile, ae.Error())
		}
	}
	if len(reasons) > 0 {
		u.Fail("Failed to %s %s: [%d]", verb, noun, status)
		// Written directly rather than through Detail: Quiet suppresses
		// Detail, and the reasons are the failure, not commentary on it.
		for _, r := range reasons {
			fmt.Fprintf(u.Out, "  %s\n", r)
		}
		return printed(1)
	}
	u.Fail("Failed to %s %s: %v", verb, noun, err)
	return printed(1)
}

// describe is the status and reasons of an *api.Error or *twin.Error; zero
// and nil for anything else.
func describe(err error) (int, []string) {
	var ae *api.Error
	if errors.As(err, &ae) {
		return ae.Status, ae.Reasons
	}
	var te *twin.Error
	if errors.As(err, &te) {
		return te.Status, te.Reasons
	}
	return 0, nil
}

// notLoggedIn prints the one message every path without a usable key ends
// in, and returns printed(1):
//
//	✗ Not logged in for profile 'dev' (no API key)
//	→ Next: veris login --profile dev
//
// detail replaces "(no API key)" when a key was presented and refused, as
// "[401] invalid or missing API key" is for a revoked one. An empty profile
// (no session to name one) drops the "for profile" clause and the --profile.
func notLoggedIn(u *ui.UI, profile, detail string) error {
	suffix := " (no API key)"
	if detail != "" {
		suffix = ": " + detail
	}
	if profile == "" {
		u.Fail("Not logged in%s", suffix)
		u.Next("veris login")
		return printed(1)
	}
	u.Fail("Not logged in for profile '%s'%s", profile, suffix)
	u.Next("veris login --profile " + profile)
	return printed(1)
}

// printJSON writes v as indented JSON to w, which under --json is stdout;
// progress and marks stay on stderr so `veris env list --json | jq` works.
// A json.RawMessage (a body passed through as the server sent it) is
// re-indented, not re-encoded. HTML escaping is off: a URL with & in it
// must survive `jq -r`.
func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// confirm asks question with a default of No, honours --yes (the answer is
// echoed so the transcript still shows what was agreed to), and returns
// errDeclined on no. Off a TTY without --yes the error is ui's NoTTYError
// naming --yes. Destructive commands call it before any server delete.
func confirm(u *ui.UI, question string) error {
	ok, err := u.Confirm(question, false, "--yes")
	if err != nil {
		return err
	}
	if !ok {
		return errDeclined
	}
	return nil
}

// shortID renders a 25-char id as its first 8 characters and an ellipsis, for
// tables; ✓ lines and --json carry the full id, since a short one cannot be
// pasted into a later command. An id that short already is returned as is.
func shortID(id string) string {
	r := []rune(id)
	if len(r) <= 8 {
		return id
	}
	return string(r[:8]) + "…"
}

// looksLikeID reports whether s has the shape of a control-plane id: the
// server mints 25 characters of lowercase letters and digits. It tells a
// pasted id from an environment name the project file does not know.
func looksLikeID(s string) bool {
	if len(s) != 25 {
		return false
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// studioLink prints "→ <console>/<path…>", a place on the console for what a
// command just touched: studioLink(u, s.consoleURL(), "environments", id).
// An empty console (a profile that never learned one) prints nothing, since
// a link nobody can follow is noise.
func studioLink(u *ui.UI, console string, path ...string) {
	if console == "" {
		return
	}
	u.Link(strings.TrimRight(console, "/") + "/" + strings.Join(path, "/"))
}
