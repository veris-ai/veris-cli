package main

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/veris-ai/veris-proxy/internal/api"
	"github.com/veris-ai/veris-proxy/internal/cfg"
	"github.com/veris-ai/veris-proxy/internal/cli"
	"github.com/veris-ai/veris-proxy/internal/twin"
	"github.com/veris-ai/veris-proxy/internal/ui"
)

// session is what every command in the tree starts from: the resolved
// configuration (which profile, key, plane, environment and sandbox, each
// with the layer that answered), a UI on stderr already told about --quiet
// and --yes, and the context the tree handed the command.
type session struct {
	ctx *cli.Context
	ui  *ui.UI        // stderr; Quiet/AssumeYes from the globals; Color/TTY detected
	res *cfg.Resolved // profile, api base/key with sources, project, local, env, sandbox
	ver string        // main.version

	// cwd is where the project walk started, for "searched up from".
	cwd string
	// warnedIgnore is whether the "twin.local.yaml is not ignored" warning
	// has been printed: once per command is enough, however many writes.
	warnedIgnore bool
}

// stdin is what the session's UI reads prompts and --key-stdin from. A test
// that drives a command end to end through cli.Execute replaces it.
var stdin io.Reader = os.Stdin

// newSessionHook, when set, sees every session newSession builds before it
// is returned. Tests use it to force a TTY or to inspect what a command
// resolved; the binary never sets it.
var newSessionHook func(*session)

// newSession resolves configuration for a command. envFlag and sandboxFlag
// are the command's own --env and --sandbox values, "" when absent; the
// globals (--profile, --api-base, --quiet, --yes) come from ctx. It never
// fails for a missing key -- a command that needs one calls s.client() --
// but it does fail for a file that will not parse or a --profile that does
// not exist, as cfg.Resolve does.
func newSession(ctx *cli.Context, envFlag, sandboxFlag string) (*session, error) {
	g := ctx.Globals
	if g == nil {
		g = &cli.Globals{}
	}
	u := ui.New(ctx.Stderr, stdin)
	u.Quiet, u.AssumeYes = g.Quiet, g.Yes
	// Getwd can fail only when the directory was removed under us; the walk
	// then starts from "" (the working directory, as Resolve reads it) and
	// the message names what we know.
	cwd, _ := os.Getwd()
	res, err := cfg.Resolve(cfg.Inputs{
		ProfileFlag: g.Profile,
		APIBaseFlag: g.APIBase,
		EnvFlag:     envFlag,
		SandboxFlag: sandboxFlag,
		Cwd:         cwd,
	})
	if err != nil {
		return nil, err
	}
	s := &session{ctx: ctx, ui: u, res: res, ver: version, cwd: cwd}
	if newSessionHook != nil {
		newSessionHook(s)
	}
	return s, nil
}

// client returns a client for the resolved plane, or the not-logged-in
// error (printed, exit 1) when nothing supplied a key:
//
//	✗ Not logged in for profile 'dev' (no API key)
//	→ Next: veris login --profile dev
func (s *session) client() (*api.Client, error) {
	if s.res.APIKey == "" {
		return nil, notLoggedIn(s.ui, s.res.ProfileName, "")
	}
	return s.plane(), nil
}

// plane returns a client for the resolved plane with whatever key there is,
// possibly none: for the routes that need no credential (healthz, the device
// pairing), and for login itself, which is here because it has no key yet.
func (s *session) plane() *api.Client {
	c := api.New(s.res.APIBase, s.res.APIKey)
	c.UserAgent = "veris/" + s.ver
	return c
}

// twin returns a client for one service's control_url.
func (s *session) twin(controlURL string) *twin.Client {
	return twin.New(controlURL)
}

// consoleURL is the profile's console_url without a trailing slash, or ""
// when the profile never learned one. It is used only for → links, through
// studioLink, which prints nothing for "".
func (s *session) consoleURL() string {
	return strings.TrimRight(s.res.Profile.ConsoleURL, "/")
}

// fail is output.go's fail with this session's profile, so a 401 can name
// the login to redo. Commands use this one; the free function is for code
// that has a UI but no session.
func (s *session) fail(verb, noun string, err error) error {
	return failAs(s.ui, s.res.ProfileName, verb, noun, err)
}

// requireProject returns the project file or, when no directory up from the
// working directory has one, the printed error (exit 1):
//
//	✗ No .veris/twin.yaml found (searched up from /Users/victor/src/app)
//	→ Next: veris env create
func (s *session) requireProject() (*cfg.Project, error) {
	if s.res.Project != nil {
		return s.res.Project, nil
	}
	s.ui.Fail("No .veris/twin.yaml found (searched up from %s)", s.cwd)
	s.ui.Next("veris env create")
	return nil, printed(1)
}

// requireEnv returns the resolved environment: its name, the id to send to
// the control plane, and its project configuration (nil when the name is a
// bare id from VERIS_ENVIRONMENT_ID, a profile's default_environment or a
// pasted --env, for which the project file has no entry). When nothing
// chose one and the project file lists environments, a TTY gets a picker;
// otherwise the printed error (exit 1):
//
//	✗ No environment selected
//	→ Next: veris env use NAME, or pass --env
//
// A name the project file does not know and that is not shaped like an id
// is a typo, not an id, and is refused with the names the file does have.
func (s *session) requireEnv() (name, id string, conf *cfg.EnvConfig, err error) {
	if s.res.EnvName != "" {
		if s.res.Env != nil {
			return s.envFrom(s.res.EnvName, s.res.Env)
		}
		if s.res.Project != nil && len(s.res.Project.Environments) > 0 && !looksLikeID(s.res.EnvName) {
			s.ui.Fail("No environment '%s' in %s (have: %s)",
				s.res.EnvName, s.res.Project.Path, strings.Join(s.envNames(), ", "))
			s.ui.Next("veris env list")
			return "", "", nil, printed(1)
		}
		return s.res.EnvName, s.res.EnvName, nil, nil
	}
	if s.res.Project != nil && s.ui.TTY && len(s.res.Project.Environments) > 0 {
		var opts []ui.Option
		for _, n := range s.envNames() {
			e := s.res.Project.Environments[n]
			detail := shortID(e.ID)
			if e.ID == "" {
				detail = "(no id)"
			}
			opts = append(opts, ui.Option{Value: n, Label: n, Detail: detail})
		}
		opt, err := s.ui.Select("Select an environment:", opts, "--env")
		if err != nil {
			return "", "", nil, err
		}
		e := s.res.Project.Environments[opt.Value]
		// An answer at the prompt is as explicit as a flag, and the rest of
		// the command reads the choice from the same place a flag lands.
		s.res.EnvName, s.res.Env, s.res.EnvSource = opt.Value, &e, cfg.SourceFlag
		return s.envFrom(opt.Value, &e)
	}
	s.ui.Fail("No environment selected")
	s.ui.Next("veris env use NAME, or pass --env")
	return "", "", nil, printed(1)
}

// envFrom is requireEnv's answer for a project environment; one that was
// edited to have no id cannot be sent anywhere and says so.
func (s *session) envFrom(name string, e *cfg.EnvConfig) (string, string, *cfg.EnvConfig, error) {
	if e.ID == "" {
		s.ui.Fail("Environment '%s' in %s has no id", name, s.res.Project.Path)
		s.ui.Next("veris env create " + name + " --from ID --force")
		return "", "", nil, printed(1)
	}
	return name, e.ID, e, nil
}

// envNames is the project's environment names, sorted so a list or a picker
// reads the same however the file was written.
func (s *session) envNames() []string {
	names := make([]string, 0, len(s.res.Project.Environments))
	for n := range s.res.Project.Environments {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// requireSandbox returns the current sandbox id -- --sandbox, then
// VERIS_SANDBOX_ID, then the local file's pointer -- or the printed error
// (exit 1):
//
//	✗ No sandbox for this folder
//	→ Next: veris up
func (s *session) requireSandbox() (string, error) {
	if s.res.SandboxID != "" {
		return s.res.SandboxID, nil
	}
	s.ui.Fail("No sandbox for this folder")
	s.ui.Next("veris up")
	return "", printed(1)
}

// rememberSandbox writes sb as the local file's sandbox pointer, so run,
// status and down find it without being told, and makes it the session's
// current sandbox unless a flag or VERIS_SANDBOX_ID already named one.
// Without a project file there is no local file to write, which is said
// once as a warning rather than failing a sandbox that already exists.
func (s *session) rememberSandbox(sb *api.Sandbox) error {
	if s.res.Local == nil {
		s.ui.Warn("no .veris/twin.yaml here, so sandbox %s is not remembered for this folder; "+
			"pass --sandbox %s or set VERIS_SANDBOX_ID", sb.ID, sb.ID)
		return nil
	}
	s.res.Local.Sandbox = &cfg.SandboxRef{
		ID:            sb.ID,
		EnvironmentID: sb.EnvironmentID,
		CreatedAt:     stamp(sb.CreatedAt),
		ExpiresAt:     stamp(sb.ExpiresAt),
	}
	if err := s.saveLocal(); err != nil {
		return err
	}
	if s.res.SandboxSource == "" || s.res.SandboxSource == cfg.SourceLocal {
		s.res.SandboxID, s.res.SandboxSource = sb.ID, cfg.SourceLocal
	}
	return nil
}

// forgetSandbox clears the local file's sandbox pointer, after a delete or
// a 404 that says it is already gone. Nothing to forget is not an error.
func (s *session) forgetSandbox() error {
	if s.res.Local == nil || s.res.Local.Sandbox == nil {
		return nil
	}
	s.res.Local.Sandbox = nil
	if s.res.SandboxSource == cfg.SourceLocal {
		s.res.SandboxID, s.res.SandboxSource = "", ""
	}
	return s.saveLocal()
}

// saveLocal writes the local file and warns, once per command, when git
// would commit it: the file holds sandbox ids, which are capabilities.
func (s *session) saveLocal() error {
	ignored, err := s.res.Local.Save()
	if err != nil {
		return err
	}
	if !ignored && !s.warnedIgnore {
		s.warnedIgnore = true
		s.ui.Warn("%s is not ignored by git; add it to .gitignore, it holds sandbox ids",
			relPath(s.cwd, s.res.Local.Path))
	}
	return nil
}

// relPath is path relative to dir when that is shorter to read, else path.
func relPath(dir, path string) string {
	if rel, err := filepath.Rel(dir, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

// stamp renders an API instant for the local file, "" for an unset one.
func stamp(t api.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
