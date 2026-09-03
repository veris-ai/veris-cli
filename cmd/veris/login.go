package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/cfg"
	"github.com/veris-ai/veris-cli/internal/cli"
	"github.com/veris-ai/veris-cli/internal/ui"
)

// loginCommands is the identity group: login, logout, whoami and profile.
func loginCommands() []*cli.Command {
	return []*cli.Command{loginCommand(), logoutCommand(), whoamiCommand(), profileCommand()}
}

// The knobs of the polling loop, variables so a test can run the fifteen
// minute flow in milliseconds and see the URL reprinted without waiting a
// minute for it. The binary never changes them.
var (
	// pollSleep waits between device-token polls; an error means the wait
	// was interrupted (the context is done) and the flow stops.
	pollSleep = sleepFor
	// urlReprintEvery is how often a non-terminal run repeats the URL line,
	// since a CI log has no browser and the line scrolls away.
	urlReprintEvery = time.Minute
	// openBrowser opens a URL on the desktop; failures are silent because
	// the URL is on the screen anyway.
	openBrowser = openInBrowser
)

// maxPollFailures is how many consecutive transport failures the poll
// tolerates before giving up. The poll is a read by contract, so retrying
// it is safe; twelve at the server's interval is about a minute of outage.
const maxPollFailures = 12

// keyPrefixLen is how much of a key GET /v1/api-keys shows (the vsk_ marker
// plus eight characters, keys.DISPLAY_PREFIX_LENGTH), which is what a key in
// hand is matched against to learn its name.
const keyPrefixLen = 12

func sleepFor(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// bareSession is a session for the commands that manage profiles rather
// than use one: login, which may be creating the very profile it names, and
// the profile verbs. cfg.Resolve refuses a --profile the file does not have,
// which is right for every command that needs a key and wrong for these, so
// the profile and plane are chosen here by the same precedence without the
// refusal, and no key is carried: nothing these commands call needs one.
func bareSession(ctx *cli.Context) (*session, error) {
	g := ctx.Globals
	if g == nil {
		g = &cli.Globals{}
	}
	u := ui.New(ctx.Stderr, stdin)
	u.Quiet, u.AssumeYes = g.Quiet, g.Yes
	global, err := cfg.LoadGlobal()
	if err != nil {
		return nil, err
	}
	res := &cfg.Resolved{Global: global}
	switch {
	case g.Profile != "":
		res.ProfileName, res.ProfileSource = g.Profile, cfg.SourceFlag
	case os.Getenv(cfg.EnvProfile) != "":
		res.ProfileName, res.ProfileSource = os.Getenv(cfg.EnvProfile), cfg.SourceEnv
	case global.ActiveProfile != "":
		res.ProfileName, res.ProfileSource = global.ActiveProfile, cfg.SourceProfile
	default:
		res.ProfileName, res.ProfileSource = "default", cfg.SourceDefault
	}
	res.Profile = global.Profiles[res.ProfileName]
	switch {
	case g.APIBase != "":
		res.APIBase, res.APIBaseSource = g.APIBase, cfg.SourceFlag
	case os.Getenv(cfg.EnvAPIBase) != "":
		res.APIBase, res.APIBaseSource = os.Getenv(cfg.EnvAPIBase), cfg.SourceEnv
	case res.Profile.APIBase != "":
		res.APIBase, res.APIBaseSource = res.Profile.APIBase, cfg.SourceProfile
	default:
		res.APIBase, res.APIBaseSource = cfg.DefaultAPIBase, cfg.SourceDefault
	}
	res.APIBase = strings.TrimRight(res.APIBase, "/")
	cwd, _ := os.Getwd()
	s := &session{ctx: ctx, ui: u, res: res, ver: version, cwd: cwd}
	if newSessionHook != nil {
		newSessionHook(s)
	}
	return s, nil
}

// --- login -------------------------------------------------------------------

type loginOptions struct {
	consoleURL string
	noBrowser  bool
	keyStdin   bool
	name       string
}

func loginCommand() *cli.Command {
	var o loginOptions
	return &cli.Command{
		Name:    "login",
		Summary: "Pair this machine with a Veris control plane",
		Usage:   "veris login [KEY] [--profile NAME] [--api-base URL] [--console-url URL] [--no-browser] [--key-stdin]",
		Help: `Prints a pairing code and the console URL where a signed-in person
approves it for one of their organisations; the next poll receives a fresh
API key, saved to ~/.veris/twin.yaml under the profile (mode 0600) and made
active. One profile is one plane, one key, one organisation: log in again
with --profile NAME and --api-base URL for another.

--key-stdin reads an existing key from stdin instead (for CI, where nobody
can approve a pairing); the key is verified against /v1/me before it is
saved. A key given as KEY works too but lands in shell history.`,
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&o.consoleURL, "console-url", "", "console `URL` for → links, when the plane's own answer is wrong")
			fs.BoolVar(&o.noBrowser, "no-browser", false, "print the URL without opening it")
			fs.BoolVar(&o.keyStdin, "key-stdin", false, "read an API key from stdin instead of pairing")
			fs.StringVar(&o.name, "name", "", "how this device appears in the console's key list (default: veris on <machine>)")
		},
		Run: func(ctx *cli.Context, args []string) error {
			return runLogin(ctx, o, args)
		},
	}
}

func runLogin(ctx *cli.Context, o loginOptions, args []string) error {
	if len(args) > 1 {
		return &cli.UsageError{Msg: "login takes at most one KEY"}
	}
	if len(args) == 1 && o.keyStdin {
		return &cli.UsageError{Msg: "pass the key either as KEY or with --key-stdin, not both"}
	}
	s, err := bareSession(ctx)
	if err != nil {
		return err
	}
	// A key already in the profile is replaced, not revoked: device keys
	// never expire, and once the file forgets one nothing local can revoke
	// it. Said before the flow, while there is still time to log out first.
	if old := s.res.Global.Profiles[s.res.ProfileName]; old.APIKey != "" {
		s.ui.Warn("Profile '%s' already holds key %s; it stays valid on the server after this login. Revoke it first with veris logout --profile %s, or later on the console",
			s.res.ProfileName, ui.MaskKey(old.APIKey), s.res.ProfileName)
	}
	if len(args) == 1 {
		return s.loginWithKey(o, args[0], true)
	}
	if o.keyStdin {
		return s.loginWithKey(o, "", false)
	}
	return s.loginDevice(o)
}

// loginDevice is the RFC 8628 device grant: mint a pairing, show the code,
// poll until a person approves it on the console, save the key it redeems.
func (s *session) loginDevice(o loginOptions) error {
	u, name, base := s.ui, s.res.ProfileName, s.res.APIBase
	if isLocalPlane(base) {
		u.Warn("A local plane usually has nobody who can approve a pairing (approval needs a login session); use --key-stdin")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// No key on the device routes: the device is here because it has none,
	// and a stale one from a previous login has no business on the wire.
	plane := api.New(base, "")
	plane.UserAgent = "veris/" + s.ver
	client := o.name
	if client == "" {
		client = clientName()
	}
	code, err := plane.DeviceCode(ctx, client)
	if err != nil {
		if api.IsStatus(err, 404) {
			u.Fail("This control plane has no device login (%s/v1/device/code answered 404).", base)
			u.Next("veris login --key-stdin --profile " + name)
			return printed(1)
		}
		// A 429 is a full pairing table; the mint is unauthenticated, so
		// looping on it would be the abuse the cap exists for.
		return s.fail("start", "login", err)
	}
	console := strings.TrimRight(o.consoleURL, "/")
	if console == "" {
		console = origin(code.VerificationURL)
	}

	u.Info("Pairing this machine with Veris")
	u.Info("  Profile  %s", name)
	u.Info("  API      %s", base)
	u.Info("  Client   %s", client)
	u.Info("")
	// The URL and the code ARE the login; they print under --quiet too, so
	// they go to Out directly rather than through Info.
	printURL := func() { fmt.Fprintf(u.Out, "  Open   %s\n", code.VerificationURLComplete) }
	printURL()
	fmt.Fprintf(u.Out, "  Code   %s\n", code.UserCode)
	if u.TTY && !o.noBrowser {
		if openBrowser(code.VerificationURLComplete) == nil {
			u.Info("  (opened in your browser — pass --no-browser to skip)")
		}
	}
	u.Info("")

	tok, err := s.pollDevice(ctx, plane, code, printURL)
	if err != nil {
		return err
	}

	// The key is written the instant it arrives, before anything else is
	// asked of the control plane: the token route answers exactly once, and
	// a hiccup on the /v1/me that follows must never lose it.
	p := s.res.Profile
	p.APIBase, p.APIKey, p.ConsoleURL = base, tok.APIKey, console
	p.KeyID, p.KeyName, p.OrganizationID = tok.KeyID, tok.KeyName, tok.OrganizationID
	switched, err := s.saveLogin(p)
	if err != nil {
		return err
	}
	keyed := api.New(base, tok.APIKey)
	keyed.UserAgent = plane.UserAgent
	org := "organisation " + tok.OrganizationID
	if me, err := keyed.Me(ctx); err != nil {
		u.Warn("Could not read the organisation's name (%v); the key is saved regardless", err)
	} else {
		org = orgLabel(me)
		// The name is decoration for profile list; the key is already on
		// disk, so a failure to add it is a warning, not a failed login.
		if p.OrganizationName = orgName(me); p.OrganizationName != "" {
			if _, err := s.saveLogin(p); err != nil {
				u.Warn("Could not record the organisation's name (%v); the key is saved regardless", err)
			}
		}
	}
	u.Success("Approved for %s", org)
	u.Success("Logged in as key '%s' (%s, %s)", tok.KeyName, tok.KeyID, ui.MaskKey(tok.APIKey))
	s.reportSaved(switched)
	return nil
}

// pollDevice polls the token route until the pairing is redeemed or settled.
// Every wait is interval+1 s: the server re-arms slow_down on any poll under
// its interval, and a second of slack keeps clock skew from earning one.
func (s *session) pollDevice(ctx context.Context, plane *api.Client, code *api.DeviceCodeResponse, reprint func()) (*api.DeviceTokenResponse, error) {
	u := s.ui
	interval := code.Interval
	if interval <= 0 {
		interval = 5
	}
	ttl := time.Duration(code.ExpiresIn) * time.Second
	deadline := time.Now().Add(ttl)
	label := func() string {
		return fmt.Sprintf("Waiting for approval in the browser · expires in %s · polling every %d s",
			mmss(time.Until(deadline)), interval)
	}
	sp := u.Spinner(label())
	defer func() { sp.Stop() }()
	// say prints a line between spinner frames: the spinner is stopped, the
	// line printed, and a fresh spinner started (which, off a terminal, also
	// prints the new label -- a log then shows the new cadence).
	say := func(print func()) {
		sp.Stop()
		print()
		sp = u.Spinner(label())
	}
	stopped := func() (*api.DeviceTokenResponse, error) {
		sp.Stop()
		fmt.Fprintf(u.Out, "Stopped waiting. Nothing was saved; code %s expires on its own.\n", code.UserCode)
		return nil, printed(1)
	}
	expired := func() (*api.DeviceTokenResponse, error) {
		sp.Stop()
		u.Fail("Pairing %s expired after %s without approval. Run veris login again.", code.UserCode, mmss(ttl))
		return nil, printed(1)
	}

	lastURL := time.Now()
	failures := 0
	for {
		if err := s.waitPoll(ctx, time.Duration(interval+1)*time.Second, sp, label); err != nil {
			return stopped()
		}
		if !time.Now().Before(deadline) {
			return expired()
		}
		if !u.TTY && time.Since(lastURL) >= urlReprintEvery {
			reprint()
			lastURL = time.Now()
		}
		tok, err := plane.DeviceToken(ctx, code.DeviceCode)
		if err == nil {
			return tok, nil
		}
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Status >= 500 {
			// Nothing answered, or something in front of the control plane
			// did: a read, so retried, but not forever.
			if ctx.Err() != nil {
				return stopped()
			}
			failures++
			if failures >= maxPollFailures {
				sp.Stop()
				u.Fail("Could not reach the control plane after %d attempts (%v). Nothing was saved; code %s expires on its own.",
					failures, err, code.UserCode)
				return nil, printed(1)
			}
			say(func() {
				u.Warn("Could not reach the control plane; retrying in %d s (%d of %d)", interval, failures, maxPollFailures)
			})
			continue
		}
		failures = 0
		switch ae.Code {
		case "authorization_pending":
		case "slow_down":
			// Permanent for this flow: a fast poll re-arms it rather than
			// being forgiven, so the interval only ever grows.
			interval += 5
			say(func() {
				u.Warn("The control plane asked us to slow down; polling every %d s now.", interval)
			})
		case "access_denied":
			sp.Stop()
			u.Fail("Pairing %s was denied on the console. Nothing was saved.", code.UserCode)
			return nil, printed(1)
		case "expired_token":
			return expired()
		case "invalid_grant":
			sp.Stop()
			u.Fail("The control plane no longer recognises this pairing (invalid_grant). Run veris login again.")
			return nil, printed(1)
		default:
			sp.Stop()
			return nil, s.fail("poll", "the pairing", err)
		}
	}
}

// waitPoll sleeps d before the next poll. On a terminal it does so a second
// at a time so the countdown ticks; anywhere else the spinner printed its
// label once and a line per second would be a log full of noise.
func (s *session) waitPoll(ctx context.Context, d time.Duration, sp *ui.Spinner, label func() string) error {
	if !s.ui.TTY {
		return pollSleep(ctx, d)
	}
	for d > 0 {
		step := min(d, time.Second)
		if err := pollSleep(ctx, step); err != nil {
			return err
		}
		d -= step
		sp.Update(label())
	}
	return nil
}

// loginWithKey saves a key the user already has, after the control plane
// has confirmed it works. fromArg is the positional form, which is kept for
// parity with the Python CLI and warned about, since a shell keeps history.
func (s *session) loginWithKey(o loginOptions, key string, fromArg bool) error {
	u, name, base := s.ui, s.res.ProfileName, s.res.APIBase
	if fromArg {
		u.Warn("a key on the command line lands in shell history; prefer --key-stdin")
	} else {
		line, err := bufio.NewReader(u.In).ReadString('\n')
		if err != nil && line == "" {
			u.Fail("No key on stdin")
			u.Next("printf '%s' \"$VERIS_API_KEY\" | veris login --key-stdin --profile " + name)
			return printed(1)
		}
		key = line
	}
	key = strings.TrimSpace(key)
	if key == "" {
		u.Fail("The key is empty")
		return printed(1)
	}
	ctx := context.Background()
	c := api.New(base, key)
	c.UserAgent = "veris/" + s.ver
	me, err := c.Me(ctx)
	if err != nil {
		if api.IsStatus(err, 401) {
			u.Fail("The control plane at %s rejected this key: %v. Nothing was saved.", base, err)
			return printed(1)
		}
		return s.fail("verify", "the key", err)
	}
	p := s.res.Profile
	p.APIBase, p.APIKey, p.OrganizationID = base, key, me.OrganizationID
	p.OrganizationName = orgName(me)
	p.KeyID, p.KeyName = "", ""
	if o.consoleURL != "" {
		p.ConsoleURL = strings.TrimRight(o.consoleURL, "/")
	}
	if k := keyRecord(ctx, c, me, key); k != nil {
		p.KeyID, p.KeyName = k.ID, k.Name
	}
	switched, err := s.saveLogin(p)
	if err != nil {
		return err
	}
	if p.KeyName != "" {
		u.Success("Logged in as key '%s' (%s, %s) on %s", p.KeyName, p.KeyID, ui.MaskKey(key), orgLabel(me))
	} else {
		u.Success("Logged in with key %s on %s", ui.MaskKey(key), orgLabel(me))
	}
	s.reportSaved(switched)
	return nil
}

// saveLogin stores p under the session's profile name and makes it active,
// reporting whether the active profile changed by that.
func (s *session) saveLogin(p cfg.Profile) (switched bool, err error) {
	g, name := s.res.Global, s.res.ProfileName
	prev := g.ActiveProfile
	if prev == "" {
		prev = "default"
	}
	if g.Profiles == nil {
		g.Profiles = map[string]cfg.Profile{}
	}
	g.Profiles[name] = p
	g.ActiveProfile = name
	if err := g.Save(); err != nil {
		return false, err
	}
	s.res.Profile = p
	return prev != name, nil
}

// reportSaved prints the tail every successful login shares.
func (s *session) reportSaved(switched bool) {
	u, name := s.ui, s.res.ProfileName
	u.Success("API key saved to %s (profile '%s', mode 0600)", s.res.Global.Path, name)
	if switched {
		u.Success("Active profile switched to '%s'", name)
	}
	studioLink(u, s.consoleURL(), "overview")
	u.Next("veris env create")
}

// keyRecord is the list row for the key c authenticates with: /v1/me's own
// key field when the control plane sends one, else the GET /v1/api-keys
// row whose prefix matches. nil when neither names it; the name is a
// nicety, so a plane that lists nothing for this credential (an operator
// secret, an older deployment) is not an error.
func keyRecord(ctx context.Context, c *api.Client, me *api.Me, key string) *api.APIKey {
	if me != nil && me.Key != nil {
		return me.Key
	}
	if len(key) < keyPrefixLen {
		return nil
	}
	keys, err := c.ListAPIKeys(ctx)
	if err != nil {
		return nil
	}
	prefix := key[:keyPrefixLen]
	for i := range keys {
		if keys[i].KeyPrefix == prefix {
			return &keys[i]
		}
	}
	return nil
}

// orgLabel is "Acme (org_…)" for the organisation a credential acts as, or
// the id alone when /v1/me's rows do not carry it.
func orgLabel(me *api.Me) string {
	if o := meOrg(me); o != nil && o.Name != "" {
		return fmt.Sprintf("%s (%s)", o.Name, o.ID)
	}
	if me.OrganizationID != "" {
		return "organisation " + me.OrganizationID
	}
	return "no organisation"
}

// meOrg is the /v1/me row for the organisation the credential acts as; a
// key has exactly one, a session may have several and the id says which.
func meOrg(me *api.Me) *api.Organization {
	for i := range me.Organizations {
		if me.Organizations[i].ID == me.OrganizationID {
			return &me.Organizations[i]
		}
	}
	if me.OrganizationID == "" && len(me.Organizations) == 1 {
		return &me.Organizations[0]
	}
	return nil
}

// clientName is what the approver sees asked: the binary and the machine,
// within the 120 characters the control plane accepts.
func clientName() string {
	host := machineName()
	name := "veris"
	if host != "" {
		name += " on " + host
	}
	if r := []rune(name); len(r) > 120 {
		name = string(r[:120])
	}
	return name
}

// machineName is the machine as a person would name it: macOS's computer
// name when it has one (os.Hostname there is often "Mac.localdomain"), else
// the hostname with its domain stripped.
func machineName() string {
	if runtime.GOOS == "darwin" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, "scutil", "--get", "ComputerName").Output(); err == nil {
			if n := strings.TrimSpace(string(out)); n != "" {
				return n
			}
		}
	}
	host, _ := os.Hostname()
	if i := strings.IndexByte(host, '.'); i > 0 {
		host = host[:i]
	}
	return host
}

// origin is the scheme and host of a URL, for learning the console from the
// verification_url the pairing carries; "" when it does not parse.
func origin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// isLocalPlane reports whether base is a loopback address, where nobody
// with a browser session is likely to be waiting to approve a pairing.
func isLocalPlane(base string) bool {
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// openInBrowser hands a URL to the desktop. Nothing waits on the command:
// some openers block until the browser exits.
func openInBrowser(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// mmss renders a remaining duration as the countdown shows it, rounded up
// so 14:52.3 reads 14:53 rather than a second the user does not have.
func mmss(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	secs := int((d + time.Second - 1) / time.Second)
	return fmt.Sprintf("%02d:%02d", secs/60, secs%60)
}

// --- logout ------------------------------------------------------------------

func logoutCommand() *cli.Command {
	var keepKey bool
	return &cli.Command{
		Name:    "logout",
		Summary: "Revoke this machine's key and forget it",
		Usage:   "veris logout [--profile NAME] [--keep-key] [--yes]",
		Help: `Revokes the profile's key on the control plane (a key may revoke itself;
every replica stops honouring it within 30 s) and removes the credentials
from ~/.veris/twin.yaml. The plane and console URL stay, so a later login
lands on the same profile. --keep-key skips the revoke for a key that is
shared with CI.`,
		Flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&keepKey, "keep-key", false, "forget the key locally without revoking it")
		},
		Run: func(ctx *cli.Context, args []string) error {
			if len(args) > 0 {
				return &cli.UsageError{Msg: "logout takes no arguments; pick the login with --profile"}
			}
			return runLogout(ctx, keepKey)
		},
	}
}

func runLogout(ctx *cli.Context, keepKey bool) error {
	s, err := newSession(ctx, "", "")
	if err != nil {
		return err
	}
	u, name, g := s.ui, s.res.ProfileName, s.res.Global
	// The file's own record, not the resolved one: VERIS_API_KEY blanks the
	// resolved profile's key, and logout is about what the file holds.
	p, ok := g.Profiles[name]
	if !ok || p.APIKey == "" {
		u.Warn("Profile '%s' holds no API key; nothing to log out of", name)
		return nil
	}
	// The key is revoked on the plane it belongs to, whatever --api-base
	// says today.
	base := p.APIBase
	if base == "" {
		base = s.res.APIBase
	}
	c := api.New(base, p.APIKey)
	c.UserAgent = "veris/" + s.ver
	bg := context.Background()

	keyID, keyName := p.KeyID, p.KeyName
	revoke := !keepKey
	org := ""
	if revoke {
		me, err := c.Me(bg)
		switch {
		case api.IsStatus(err, 401):
			// Already dead on the server: revoked elsewhere or rotated away.
			// Nothing to revoke, and the local copy is still worth removing.
			u.Warn("The control plane no longer accepts this key (%v); nothing to revoke", err)
			revoke = false
		case err != nil:
			return s.fail("reach", "the control plane", err)
		default:
			// The question names the organisation as a person knows it;
			// the id alone only when /v1/me carries no name.
			if org = orgName(me); org == "" {
				org = me.OrganizationID
			}
			if keyID == "" {
				if k := keyRecord(bg, c, me, p.APIKey); k != nil {
					keyID, keyName = k.ID, k.Name
				}
			}
			if keyID == "" {
				u.Warn("Cannot tell which key profile '%s' holds (no key_id stored, no prefix match on /v1/api-keys); it is not revoked. Revoke it on the console.", name)
				revoke = false
			}
		}
	}

	which := ui.MaskKey(p.APIKey)
	if keyName != "" {
		which = "'" + keyName + "'"
	}
	var question string
	switch {
	case revoke && org != "":
		question = fmt.Sprintf("Revoke key %s (%s) on %s and forget it locally?", which, shortID(keyID), org)
	case revoke:
		question = fmt.Sprintf("Revoke key %s (%s) and forget it locally?", which, shortID(keyID))
	default:
		question = fmt.Sprintf("Forget the credentials of profile '%s' locally (key %s stays valid)?", name, ui.MaskKey(p.APIKey))
	}
	if err := confirm(u, question); err != nil {
		return err
	}

	if revoke {
		_, err := c.RevokeAPIKey(bg, keyID)
		switch {
		case err == nil:
			u.Success("Key revoked (takes effect within 30 s on every replica)")
		case api.IsStatus(err, 404), api.IsStatus(err, 401):
			u.Warn("Key %s was already gone", shortID(keyID))
		default:
			// Nothing is stripped over a failed revoke: the user asked for
			// the key to die, and forgetting it now would leave it alive
			// with no local way to find it again.
			return s.fail("revoke", "key "+shortID(keyID), err)
		}
	}
	p.APIKey, p.KeyID, p.KeyName, p.OrganizationID, p.OrganizationName = "", "", "", "", ""
	g.Profiles[name] = p
	if err := g.Save(); err != nil {
		return err
	}
	u.Success("Removed the credentials of profile '%s' from %s", name, g.Path)
	return nil
}

// --- whoami ------------------------------------------------------------------

func whoamiCommand() *cli.Command {
	return &cli.Command{
		Name:    "whoami",
		Summary: "Which key, organisation and plane a command would use",
		Usage:   "veris whoami [--json]",
		Help: `GET /v1/me with the credential every other command would send, naming
where it came from: VERIS_API_KEY or a profile. The first thing a broken
CI job runs. --json prints the /v1/me body.`,
		Run: func(ctx *cli.Context, args []string) error {
			if len(args) > 0 {
				return &cli.UsageError{Msg: "whoami takes no arguments"}
			}
			return runWhoami(ctx)
		},
	}
}

func runWhoami(ctx *cli.Context) error {
	s, err := newSession(ctx, "", "")
	if err != nil {
		return err
	}
	c, err := s.client()
	if err != nil {
		return err
	}
	bg := context.Background()
	if ctx.Globals != nil && ctx.Globals.JSON {
		// The body as sent, not re-encoded through api.Me: stdout carries
		// what the plane said, fields this binary does not know included.
		raw, err := c.MeRaw(bg)
		if err != nil {
			return s.fail("read", "/v1/me", err)
		}
		return printJSON(ctx.Stdout, raw)
	}
	me, err := c.Me(bg)
	if err != nil {
		return s.fail("read", "/v1/me", err)
	}
	u := s.ui
	source := "profile '" + s.res.ProfileName + "'"
	switch s.res.APIKeySource {
	case cfg.SourceEnv:
		source = "env " + cfg.EnvAPIKey
	case cfg.SourceFlag:
		source = "--api-key"
	}
	u.Info("Profile  %s → %s", source, s.res.APIBase)

	keyLine := ui.MaskKey(s.res.APIKey)
	if id, name, status := s.keyIdentity(bg, c, me); name != "" {
		keyLine += fmt.Sprintf(" · '%s' (%s) · %s", name, id, status)
	} else {
		keyLine += " · (unknown key)"
	}
	u.Info("Key      %s", keyLine)

	orgLine := "—"
	if o := meOrg(me); o != nil {
		orgLine = fmt.Sprintf("%s (%s) · %s", o.Name, o.ID, o.Kind)
	} else if me.OrganizationID != "" {
		orgLine = me.OrganizationID
	}
	u.Info("Org      %s", orgLine)
	if console := s.consoleURL(); console != "" {
		u.Info("Studio   %s", console)
	}
	return nil
}

// keyIdentity names the key in use: /v1/me's key field when the plane sends
// it, else what the profile stored at login (only when the profile's key is
// the one in use -- an env-supplied key is a different key), else the
// /v1/api-keys row with the same prefix. A stored name carries no status,
// but the key answered /v1/me a moment ago, which is what "active" means.
func (s *session) keyIdentity(ctx context.Context, c *api.Client, me *api.Me) (id, name, status string) {
	if me.Key != nil {
		return me.Key.ID, me.Key.Name, me.Key.Status
	}
	if s.res.APIKeySource == cfg.SourceProfile && s.res.Profile.KeyName != "" {
		return s.res.Profile.KeyID, s.res.Profile.KeyName, "active"
	}
	if k := keyRecord(ctx, c, nil, s.res.APIKey); k != nil {
		return k.ID, k.Name, k.Status
	}
	return "", "", ""
}

// --- profile -----------------------------------------------------------------

func profileCommand() *cli.Command {
	return &cli.Command{
		Name:    "profile",
		Summary: "Logins, one per control plane",
		Help: `A profile is one login: a plane, a key and the organisation the key is
bound to. login creates or refreshes one; the active profile is what every
command uses unless --profile or VERIS_PROFILE names another.`,
		Sub: []*cli.Command{
			{
				Name:    "list",
				Summary: "List profiles",
				Usage:   "veris profile list [--json]",
				Run: func(ctx *cli.Context, args []string) error {
					if len(args) > 0 {
						return &cli.UsageError{Msg: "profile list takes no arguments"}
					}
					return runProfileList(ctx)
				},
			},
			{
				Name:    "get",
				Summary: "Show one profile, key masked",
				Usage:   "veris profile get [NAME] [--json]",
				Run: func(ctx *cli.Context, args []string) error {
					if len(args) > 1 {
						return &cli.UsageError{Msg: "profile get takes at most one NAME"}
					}
					return runProfileGet(ctx, args)
				},
			},
			{
				Name:    "use",
				Summary: "Make a profile the active one",
				Usage:   "veris profile use [NAME]",
				Run: func(ctx *cli.Context, args []string) error {
					if len(args) > 1 {
						return &cli.UsageError{Msg: "profile use takes at most one NAME"}
					}
					return runProfileUse(ctx, args)
				},
			},
			{
				Name:    "delete",
				Summary: "Delete a profile",
				Usage:   "veris profile delete NAME [--yes]",
				Run: func(ctx *cli.Context, args []string) error {
					if len(args) != 1 {
						return &cli.UsageError{Msg: "profile delete takes exactly one NAME"}
					}
					return runProfileDelete(ctx, args[0])
				},
			},
		},
	}
}

// profileView is a profile as list and get print it, the key masked: the
// file is the one place the whole key lives.
type profileView struct {
	Name               string `json:"name"`
	Active             bool   `json:"active"`
	APIBase            string `json:"api_base"`
	ConsoleURL         string `json:"console_url,omitempty"`
	OrganizationID     string `json:"organization_id,omitempty"`
	OrganizationName   string `json:"organization_name,omitempty"`
	Key                string `json:"key,omitempty"`
	KeyID              string `json:"key_id,omitempty"`
	KeyName            string `json:"key_name,omitempty"`
	DefaultEnvironment string `json:"default_environment,omitempty"`
}

func viewProfile(name string, p cfg.Profile, active bool) profileView {
	v := profileView{
		Name: name, Active: active, APIBase: p.APIBase, ConsoleURL: p.ConsoleURL,
		OrganizationID: p.OrganizationID, OrganizationName: p.OrganizationName,
		KeyID: p.KeyID, KeyName: p.KeyName, DefaultEnvironment: p.DefaultEnvironment,
	}
	if v.APIBase == "" {
		v.APIBase = cfg.DefaultAPIBase
	}
	if p.APIKey != "" {
		v.Key = ui.MaskKey(p.APIKey)
	}
	return v
}

// profileNames is the file's profiles, sorted so the list reads the same
// however the file was written.
func profileNames(g *cfg.Global) []string {
	names := make([]string, 0, len(g.Profiles))
	for n := range g.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func runProfileList(ctx *cli.Context) error {
	s, err := bareSession(ctx)
	if err != nil {
		return err
	}
	u, g := s.ui, s.res.Global
	active, _, _ := g.Active()
	names := profileNames(g)
	if ctx.Globals != nil && ctx.Globals.JSON {
		views := make([]profileView, 0, len(names))
		for _, n := range names {
			views = append(views, viewProfile(n, g.Profiles[n], n == active))
		}
		return printJSON(ctx.Stdout, views)
	}
	if len(names) == 0 {
		u.Info("No profiles yet")
		u.Next("veris login")
		return nil
	}
	if u.Quiet {
		return nil
	}
	rows := make([][]string, 0, len(names))
	for _, n := range names {
		v := viewProfile(n, g.Profiles[n], n == active)
		// The mark shares the name's cell: a column of its own would be
		// padded to the header's width and drift away from the name.
		mark := "  "
		if v.Active {
			mark = "* "
		}
		// The organisation's name, as the transcript has it, is what tells
		// two logins apart at a glance; the id stands in when login never
		// learned one.
		rows = append(rows, []string{mark + n, v.APIBase, orDash(firstNonEmpty(v.OrganizationName, shortID(v.OrganizationID))), orDash(v.Key)})
	}
	u.Table([]string{"  Profile", "API", "Org", "Key"}, rows)
	return nil
}

// orDash is "—" for an empty table cell.
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// lookupProfile is the named profile or the printed error (exit 1).
func (s *session) lookupProfile(name string) (cfg.Profile, error) {
	p, ok := s.res.Global.Profiles[name]
	if !ok {
		s.ui.Fail("No profile '%s' in %s", name, s.res.Global.Path)
		s.ui.Next("veris login --profile " + name)
		return cfg.Profile{}, printed(1)
	}
	return p, nil
}

func runProfileGet(ctx *cli.Context, args []string) error {
	s, err := bareSession(ctx)
	if err != nil {
		return err
	}
	u, g := s.ui, s.res.Global
	name := s.res.ProfileName
	if len(args) == 1 {
		name = args[0]
	}
	p, err := s.lookupProfile(name)
	if err != nil {
		return err
	}
	active, _, _ := g.Active()
	v := viewProfile(name, p, name == active)
	if ctx.Globals != nil && ctx.Globals.JSON {
		return printJSON(ctx.Stdout, v)
	}
	u.Info("Profile   %s", name)
	if v.Active {
		u.Info("Active    yes")
	} else {
		u.Info("Active    no")
	}
	u.Info("API       %s", v.APIBase)
	if v.ConsoleURL != "" {
		u.Info("Console   %s", v.ConsoleURL)
	}
	if v.OrganizationID != "" {
		u.Info("Org       %s", v.OrganizationID)
	}
	u.Info("Key       %s", orDash(v.Key))
	if v.KeyName != "" {
		u.Info("Key name  %s", v.KeyName)
	}
	if v.KeyID != "" {
		u.Info("Key id    %s", v.KeyID)
	}
	if v.DefaultEnvironment != "" {
		u.Info("Default environment  %s", v.DefaultEnvironment)
	}
	return nil
}

func runProfileUse(ctx *cli.Context, args []string) error {
	s, err := bareSession(ctx)
	if err != nil {
		return err
	}
	u, g := s.ui, s.res.Global
	var name string
	if len(args) == 1 {
		name = args[0]
		if _, err := s.lookupProfile(name); err != nil {
			return err
		}
	} else {
		names := profileNames(g)
		if len(names) == 0 {
			u.Fail("No profiles in %s", g.Path)
			u.Next("veris login")
			return printed(1)
		}
		opts := make([]ui.Option, 0, len(names))
		for _, n := range names {
			opts = append(opts, ui.Option{Value: n, Label: n, Detail: viewProfile(n, g.Profiles[n], false).APIBase})
		}
		opt, err := u.Select("Select a profile:", opts, "a profile NAME")
		if err != nil {
			return err
		}
		name = opt.Value
	}
	g.ActiveProfile = name
	if err := g.Save(); err != nil {
		return err
	}
	u.Success("Active profile set to '%s'", name)
	return nil
}

func runProfileDelete(ctx *cli.Context, name string) error {
	s, err := bareSession(ctx)
	if err != nil {
		return err
	}
	u, g := s.ui, s.res.Global
	p, err := s.lookupProfile(name)
	if err != nil {
		return err
	}
	if active, _, _ := g.Active(); active == name {
		u.Fail("'%s' is the active profile; run veris profile use OTHER first", name)
		return printed(1)
	}
	question := fmt.Sprintf("Delete profile '%s'?", name)
	if p.APIKey != "" {
		question = fmt.Sprintf("Delete profile '%s'? Its key %s stays valid; veris logout --profile %s revokes it.",
			name, ui.MaskKey(p.APIKey), name)
	}
	if err := confirm(u, question); err != nil {
		return err
	}
	delete(g.Profiles, name)
	if err := g.Save(); err != nil {
		return err
	}
	u.Success("Deleted profile '%s' from %s", name, g.Path)
	return nil
}
