package cfg

import (
	"path/filepath"
	"strings"
	"testing"
)

// fixture is one arrangement of the three files plus the process inputs.
// Every field is optional so a case names only what it exercises.
type fixture struct {
	global  string            // ~/.veris/twin.yaml body; "" means no file
	project string            // .veris/twin.yaml body; "" means no project
	local   string            // .veris/twin.local.yaml body; "" means none
	env     map[string]string // environment variables
	in      Inputs            // flags; Cwd and Getenv are filled in
}

func (f fixture) resolve(t *testing.T) (*Resolved, error) {
	t.Helper()
	tempHome(t)
	if f.global != "" {
		writeFile(t, GlobalPath(), f.global)
	}
	cwd := t.TempDir()
	if f.project != "" {
		project(t, cwd, f.project)
		if f.local != "" {
			writeFile(t, filepath.Join(cwd, ".veris", "twin.local.yaml"), f.local)
		}
	}
	in := f.in
	in.Cwd = cwd
	in.Getenv = func(k string) string { return f.env[k] }
	return Resolve(in)
}

func mustResolve(t *testing.T, f fixture) *Resolved {
	t.Helper()
	r, err := f.resolve(t)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

const globalTwo = `active_profile: work
profiles:
  default:
    api_base: https://default.plane/
    api_key: vsk_default
    default_environment: from-default-profile
  work:
    api_base: https://work.plane
    api_key: vsk_work
    default_environment: from-work-profile
  staging:
    api_key: vsk_staging
`

const projectTwo = `version: 1
project: p
default: dev
environments:
  dev: {id: id_dev, ttl_minutes: 30}
  ci: {id: id_ci, profile: staging}
`

func TestResolveEnvironmentPrecedence(t *testing.T) {
	all := fixture{
		global:  globalTwo,
		project: projectTwo,
		local:   "use: ci\n",
		env:     map[string]string{"VERIS_ENV": "from-env", "VERIS_ENVIRONMENT_ID": "id_bare"},
		in:      Inputs{EnvFlag: "from-flag"},
	}
	cases := []struct {
		name    string
		f       fixture
		want    string
		source  Source
		wantEnv bool
	}{
		{"flag beats all", all, "from-flag", SourceFlag, false},
		{"VERIS_ENV beats the files", func() fixture { f := all; f.in = Inputs{}; return f }(),
			"from-env", SourceEnv, false},
		{"local use beats project default", fixture{global: globalTwo, project: projectTwo, local: "use: ci\n",
			env: map[string]string{"VERIS_ENVIRONMENT_ID": "id_bare"}}, "ci", SourceLocal, true},
		{"project default beats the profile", fixture{global: globalTwo, project: projectTwo,
			env: map[string]string{"VERIS_ENVIRONMENT_ID": "id_bare"}}, "dev", SourceProject, true},
		{"profile default_environment when the project has no default", fixture{global: globalTwo,
			project: "version: 1\nenvironments: {dev: {id: id_dev}}\n",
			env:     map[string]string{"VERIS_ENVIRONMENT_ID": "id_bare"}}, "from-work-profile", SourceProfile, false},
		{"profile default_environment without a project", fixture{global: globalTwo,
			env: map[string]string{"VERIS_ENVIRONMENT_ID": "id_bare"}}, "from-work-profile", SourceProfile, false},
		{"the active profile's default, not another's", fixture{global: globalTwo,
			in: Inputs{ProfileFlag: "default"}}, "from-default-profile", SourceProfile, false},
		{"VERIS_ENVIRONMENT_ID as bare id last", fixture{global: "profiles: {default: {api_key: k}}\n",
			env: map[string]string{"VERIS_ENVIRONMENT_ID": "id_bare"}}, "id_bare", SourceEnv, false},
		{"none", fixture{}, "", "", false},
		{"a bare id that the project knows sets Env", fixture{global: globalTwo, project: projectTwo,
			in: Inputs{EnvFlag: "id_ci"}}, "id_ci", SourceFlag, true},
		{"a name the project does not know leaves Env nil", fixture{project: projectTwo,
			in: Inputs{EnvFlag: "prod"}}, "prod", SourceFlag, false},
		{"an empty local use falls through", fixture{project: projectTwo, local: "sandbox: {id: sb}\n"},
			"dev", SourceProject, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := mustResolve(t, tc.f)
			if r.EnvName != tc.want || r.EnvSource != tc.source {
				t.Errorf("env = %q from %q; want %q from %q", r.EnvName, r.EnvSource, tc.want, tc.source)
			}
			if (r.Env != nil) != tc.wantEnv {
				t.Errorf("Env = %+v; want present=%v", r.Env, tc.wantEnv)
			}
			if r.Env != nil && r.Env.ID != "id_"+strings.TrimPrefix(tc.want, "id_") {
				t.Errorf("Env.ID = %q for %q", r.Env.ID, tc.want)
			}
		})
	}
}

func TestResolveEnvIsACopy(t *testing.T) {
	r := mustResolve(t, fixture{project: projectTwo})
	r.Env.TTLMinutes = 99
	if r.Project.Environments["dev"].TTLMinutes != 30 {
		t.Error("Env aliases the project map")
	}
}

func TestResolveProfilePrecedence(t *testing.T) {
	cases := []struct {
		name   string
		f      fixture
		want   string
		source Source
		key    string
	}{
		{"flag beats VERIS_PROFILE", fixture{global: globalTwo, project: projectTwo, local: "use: ci\n",
			env: map[string]string{"VERIS_PROFILE": "staging"}, in: Inputs{ProfileFlag: "default"}},
			"default", SourceFlag, "vsk_default"},
		{"VERIS_PROFILE beats the environment's profile", fixture{global: globalTwo, project: projectTwo,
			local: "use: ci\n", env: map[string]string{"VERIS_PROFILE": "default"}},
			"default", SourceEnv, "vsk_default"},
		{"the environment's profile beats active_profile", fixture{global: globalTwo, project: projectTwo,
			local: "use: ci\n"}, "staging", SourceProject, "vsk_staging"},
		{"an environment without profile falls to active_profile", fixture{global: globalTwo,
			project: projectTwo}, "work", SourceProfile, "vsk_work"},
		{"active_profile without a project", fixture{global: globalTwo}, "work", SourceProfile, "vsk_work"},
		{"default when nothing names one", fixture{global: "profiles: {default: {api_key: vsk_default}}\n"},
			"default", SourceDefault, "vsk_default"},
		{"default with no file at all", fixture{}, "default", SourceDefault, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := mustResolve(t, tc.f)
			if r.ProfileName != tc.want || r.ProfileSource != tc.source {
				t.Errorf("profile = %q from %q; want %q from %q", r.ProfileName, r.ProfileSource, tc.want, tc.source)
			}
			if r.APIKey != tc.key {
				t.Errorf("key = %q, want %q", r.APIKey, tc.key)
			}
		})
	}
}

func TestResolveProfileNotFound(t *testing.T) {
	cases := []struct {
		name string
		f    fixture
		want string
	}{
		{"named by flag", fixture{global: globalTwo, in: Inputs{ProfileFlag: "dev"}},
			`profile "dev" not found; run veris login --profile dev`},
		{"named by VERIS_PROFILE", fixture{global: globalTwo, env: map[string]string{"VERIS_PROFILE": "nope"}},
			`profile "nope" not found; run veris login --profile nope`},
		{"named by the environment", fixture{global: globalTwo,
			project: "version: 1\ndefault: x\nenvironments: {x: {profile: gone}}\n"},
			`profile "gone" not found; run veris login --profile gone`},
		{"named by active_profile", fixture{global: "active_profile: old\nprofiles: {default: {api_key: k}}\n"},
			`profile "old" not found; run veris login --profile old`},
		{"default when the file exists without it", fixture{global: "profiles: {work: {api_key: k}}\n"},
			`profile "default" not found; run veris login --profile default`},
		{"named by flag with no file", fixture{in: Inputs{ProfileFlag: "work"}},
			`profile "work" not found; run veris login --profile work`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.f.resolve(t)
			if err == nil || err.Error() != tc.want {
				t.Errorf("err = %v\nwant %s", err, tc.want)
			}
		})
	}
}

func TestResolveAPIKeyPrecedence(t *testing.T) {
	cases := []struct {
		name       string
		f          fixture
		want       string
		source     Source
		profileKey string
	}{
		{"flag beats VERIS_API_KEY", fixture{global: globalTwo,
			env: map[string]string{"VERIS_API_KEY": "vsk_env"}, in: Inputs{APIKeyFlag: "vsk_flag"}},
			"vsk_flag", SourceFlag, ""},
		{"VERIS_API_KEY beats the profile and hides its key", fixture{global: globalTwo,
			env: map[string]string{"VERIS_API_KEY": "vsk_env"}}, "vsk_env", SourceEnv, ""},
		{"profile", fixture{global: globalTwo}, "vsk_work", SourceProfile, "vsk_work"},
		{"none", fixture{global: "profiles: {default: {api_base: https://x}}\n"}, "", "", ""},
		{"none without a file", fixture{}, "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := mustResolve(t, tc.f)
			if r.APIKey != tc.want || r.APIKeySource != tc.source {
				t.Errorf("key = %q from %q; want %q from %q", r.APIKey, r.APIKeySource, tc.want, tc.source)
			}
			if r.Profile.APIKey != tc.profileKey {
				t.Errorf("Profile.APIKey = %q, want %q", r.Profile.APIKey, tc.profileKey)
			}
		})
	}
}

func TestResolveAPIKeyFromEnvStillLoadsProfile(t *testing.T) {
	r := mustResolve(t, fixture{global: globalTwo, env: map[string]string{"VERIS_API_KEY": "vsk_env"}})
	if r.APIBase != "https://work.plane" || r.APIBaseSource != SourceProfile {
		t.Errorf("api base = %q from %q; the profile's base must still apply", r.APIBase, r.APIBaseSource)
	}
}

func TestResolveAPIBasePrecedence(t *testing.T) {
	cases := []struct {
		name   string
		f      fixture
		want   string
		source Source
	}{
		{"flag beats VERIS_API_BASE", fixture{global: globalTwo,
			env: map[string]string{"VERIS_API_BASE": "https://env.plane"}, in: Inputs{APIBaseFlag: "https://flag.plane/"}},
			"https://flag.plane", SourceFlag},
		{"VERIS_API_BASE beats the profile", fixture{global: globalTwo,
			env: map[string]string{"VERIS_API_BASE": "https://env.plane/"}}, "https://env.plane", SourceEnv},
		{"profile, trailing slash trimmed", fixture{global: globalTwo, in: Inputs{ProfileFlag: "default"}},
			"https://default.plane", SourceProfile},
		{"profile without a base falls to the default", fixture{global: globalTwo, in: Inputs{ProfileFlag: "staging"}},
			DefaultAPIBase, SourceDefault},
		{"default without a file", fixture{}, DefaultAPIBase, SourceDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := mustResolve(t, tc.f)
			if r.APIBase != tc.want || r.APIBaseSource != tc.source {
				t.Errorf("base = %q from %q; want %q from %q", r.APIBase, r.APIBaseSource, tc.want, tc.source)
			}
		})
	}
}

func TestResolveSandboxPrecedence(t *testing.T) {
	cases := []struct {
		name   string
		f      fixture
		want   string
		source Source
	}{
		{"flag beats VERIS_SANDBOX_ID", fixture{project: projectTwo, local: "sandbox: {id: sb_local}\n",
			env: map[string]string{"VERIS_SANDBOX_ID": "sb_env"}, in: Inputs{SandboxFlag: "sb_flag"}},
			"sb_flag", SourceFlag},
		{"VERIS_SANDBOX_ID beats the local file", fixture{project: projectTwo, local: "sandbox: {id: sb_local}\n",
			env: map[string]string{"VERIS_SANDBOX_ID": "sb_env"}}, "sb_env", SourceEnv},
		{"local sandbox.id", fixture{project: projectTwo, local: "sandbox: {id: sb_local}\n"}, "sb_local", SourceLocal},
		{"local file without a sandbox", fixture{global: globalTwo, project: projectTwo, local: "use: ci\n"}, "", ""},
		{"no project, no local", fixture{}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := mustResolve(t, tc.f)
			if r.SandboxID != tc.want || r.SandboxSource != tc.source {
				t.Errorf("sandbox = %q from %q; want %q from %q", r.SandboxID, r.SandboxSource, tc.want, tc.source)
			}
		})
	}
}

func TestResolveFilesCarried(t *testing.T) {
	r := mustResolve(t, fixture{global: globalTwo, project: projectTwo, local: "use: ci\n"})
	if r.Global == nil || r.Global.ActiveProfile != "work" {
		t.Errorf("Global = %+v", r.Global)
	}
	if r.Project == nil || r.Project.Project != "p" {
		t.Errorf("Project = %+v", r.Project)
	}
	if r.Local == nil || r.Local.Use != "ci" || r.Local.Path != r.Project.LocalPath() {
		t.Errorf("Local = %+v", r.Local)
	}

	bare := mustResolve(t, fixture{})
	if bare.Global == nil || bare.Global.Path != GlobalPath() {
		t.Errorf("Global without a file = %+v", bare.Global)
	}
	if bare.Project != nil || bare.Local != nil {
		t.Errorf("no project: Project = %+v, Local = %+v; want nil, nil", bare.Project, bare.Local)
	}
}

func TestResolveProjectFoundFromBelow(t *testing.T) {
	tempHome(t)
	writeFile(t, GlobalPath(), globalTwo)
	root := project(t, t.TempDir(), projectTwo)
	writeFile(t, filepath.Join(root, ".veris", "twin.local.yaml"), "use: ci\nsandbox: {id: sb_1}\n")
	deep := filepath.Join(root, "a", "b")
	writeFile(t, filepath.Join(deep, "keep"), "")
	r, err := Resolve(Inputs{Cwd: deep, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	if r.EnvName != "ci" || r.EnvSource != SourceLocal || r.SandboxID != "sb_1" {
		t.Errorf("from below: env %q from %q, sandbox %q", r.EnvName, r.EnvSource, r.SandboxID)
	}
}

func TestResolveNilGetenvReadsProcess(t *testing.T) {
	tempHome(t)
	t.Setenv("VERIS_SANDBOX_ID", "sb_proc")
	t.Setenv("VERIS_ENV", "")
	t.Setenv("VERIS_ENVIRONMENT_ID", "")
	t.Setenv("VERIS_PROFILE", "")
	t.Setenv("VERIS_API_KEY", "")
	t.Setenv("VERIS_API_BASE", "")
	r, err := Resolve(Inputs{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if r.SandboxID != "sb_proc" || r.SandboxSource != SourceEnv {
		t.Errorf("sandbox = %q from %q", r.SandboxID, r.SandboxSource)
	}
}

func TestResolveUnreadableFilesFail(t *testing.T) {
	cases := []struct {
		name string
		f    fixture
		path string
	}{
		{"global", fixture{global: "profiles: 1\n"}, "twin.yaml"},
		{"project", fixture{project: "environments: [x]\n"}, "twin.yaml"},
		{"local", fixture{project: projectTwo, local: "use: [x]\n"}, "twin.local.yaml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.f.resolve(t)
			if err == nil || !strings.Contains(err.Error(), tc.path+" is unreadable") {
				t.Errorf("err = %v", err)
			}
		})
	}
}
