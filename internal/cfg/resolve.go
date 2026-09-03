package cfg

import (
	"fmt"
	"os"
	"strings"
)

// Inputs is everything Resolve consults besides the files: the flags as
// parsed, where the command runs, and how it reads the environment. Getenv
// is injectable so a test can lay out a whole precedence matrix without
// touching the process environment.
type Inputs struct {
	ProfileFlag string // --profile
	APIBaseFlag string // --api-base
	APIKeyFlag  string // --api-key
	EnvFlag     string // --env
	SandboxFlag string // --sandbox
	// Cwd is where the project walk starts; empty means the working directory.
	Cwd string
	// Getenv reads one environment variable; nil means os.Getenv.
	Getenv func(string) string
}

// Resolved is the answer to the five questions, each with the layer that
// answered it. A Source is empty when nothing answered: no key, no
// environment, no sandbox.
type Resolved struct {
	ProfileName   string
	Profile       Profile
	ProfileSource Source

	APIBase       string
	APIBaseSource Source

	// APIKey is the key to send. APIKeySource is "env" when VERIS_API_KEY won;
	// then Profile.APIKey is blank as well, so nothing downstream can fall
	// back to a key the user asked not to use.
	APIKey       string
	APIKeySource Source

	// The files as loaded. Project is nil when no directory up from Cwd has
	// one, and Local is nil with it: the local file has no home without a
	// project file beside it.
	Global  *Global
	Project *Project
	Local   *Local

	// EnvName is the environment as named: a project environment's name, or a
	// bare id from VERIS_ENVIRONMENT_ID or a flag. Env is that name's entry in
	// the project file, nil when it has none.
	EnvName   string
	Env       *EnvConfig
	EnvSource Source

	SandboxID     string
	SandboxSource Source
}

// Resolve reads the files and applies the precedence, which for every value
// is "the most explicit source that says anything":
//
//	environment  --env → VERIS_ENV → local use → project default →
//	             profile default_environment → VERIS_ENVIRONMENT_ID → none
//	profile      --profile → VERIS_PROFILE → the environment's profile →
//	             active_profile → "default"
//	api key      --api-key → VERIS_API_KEY → profile
//	api base     --api-base → VERIS_API_BASE → profile → DefaultAPIBase
//	sandbox      --sandbox → VERIS_SANDBOX_ID → local sandbox.id → none
//
// Layers never merge: a local file that names an environment hides the
// project's default entirely, rather than filling in around it.
//
// The environment step consults the profile and the profile step consults the
// environment, so the environment is resolved first against the profile the
// flag, VERIS_PROFILE or active_profile picks; only then can the environment's
// own profile: line redirect the final choice.
func Resolve(in Inputs) (*Resolved, error) {
	getenv := in.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}

	global, err := LoadGlobal()
	if err != nil {
		return nil, err
	}
	project, err := FindProject(in.Cwd)
	if err != nil {
		return nil, err
	}
	var local *Local
	if project != nil {
		local, err = LoadLocal(project.LocalPath())
		if err != nil {
			return nil, err
		}
	}
	r := &Resolved{Global: global, Project: project, Local: local}

	// The profile as chosen without the environment's say, for the one step
	// of the environment chain that reads the profile file.
	preName, _ := profileName(in, getenv, global, nil)
	pre := global.Profiles[preName]

	switch {
	case in.EnvFlag != "":
		r.EnvName, r.EnvSource = in.EnvFlag, SourceFlag
	case getenv(EnvEnv) != "":
		r.EnvName, r.EnvSource = getenv(EnvEnv), SourceEnv
	case local != nil && local.Use != "":
		r.EnvName, r.EnvSource = local.Use, SourceLocal
	case project != nil && project.Default != "":
		r.EnvName, r.EnvSource = project.Default, SourceProject
	case pre.DefaultEnvironment != "":
		r.EnvName, r.EnvSource = pre.DefaultEnvironment, SourceProfile
	case getenv(EnvEnvironmentID) != "":
		r.EnvName, r.EnvSource = getenv(EnvEnvironmentID), SourceEnv
	}
	if project != nil && r.EnvName != "" {
		r.Env = project.lookup(r.EnvName)
	}

	r.ProfileName, r.ProfileSource = profileName(in, getenv, global, r.Env)
	p, ok := global.Profiles[r.ProfileName]
	if !ok {
		// A machine that has never logged in has no file and no profile, and
		// that is fine for every command that needs no key -- they say so
		// themselves. Any other missing profile was named by someone.
		if r.ProfileName != "default" || fileExists(global.Path) {
			return nil, fmt.Errorf("profile %q not found; run veris login --profile %s",
				r.ProfileName, r.ProfileName)
		}
	}
	r.Profile = p

	switch {
	case in.APIKeyFlag != "":
		r.APIKey, r.APIKeySource = in.APIKeyFlag, SourceFlag
		r.Profile.APIKey = ""
	case getenv(EnvAPIKey) != "":
		r.APIKey, r.APIKeySource = getenv(EnvAPIKey), SourceEnv
		r.Profile.APIKey = ""
	case p.APIKey != "":
		r.APIKey, r.APIKeySource = p.APIKey, SourceProfile
	}

	switch {
	case in.APIBaseFlag != "":
		r.APIBase, r.APIBaseSource = in.APIBaseFlag, SourceFlag
	case getenv(EnvAPIBase) != "":
		r.APIBase, r.APIBaseSource = getenv(EnvAPIBase), SourceEnv
	case p.APIBase != "":
		r.APIBase, r.APIBaseSource = p.APIBase, SourceProfile
	default:
		r.APIBase, r.APIBaseSource = DefaultAPIBase, SourceDefault
	}
	r.APIBase = strings.TrimRight(r.APIBase, "/")

	switch {
	case in.SandboxFlag != "":
		r.SandboxID, r.SandboxSource = in.SandboxFlag, SourceFlag
	case getenv(EnvSandboxID) != "":
		r.SandboxID, r.SandboxSource = getenv(EnvSandboxID), SourceEnv
	case local != nil && local.Sandbox != nil && local.Sandbox.ID != "":
		r.SandboxID, r.SandboxSource = local.Sandbox.ID, SourceLocal
	}
	return r, nil
}

// profileName is the profile chain. env is the resolved environment, or nil
// while the environment is still being chosen.
func profileName(in Inputs, getenv func(string) string, global *Global, env *EnvConfig) (string, Source) {
	switch {
	case in.ProfileFlag != "":
		return in.ProfileFlag, SourceFlag
	case getenv(EnvProfile) != "":
		return getenv(EnvProfile), SourceEnv
	case env != nil && env.Profile != "":
		return env.Profile, SourceProject
	case global.ActiveProfile != "":
		return global.ActiveProfile, SourceProfile
	}
	return "default", SourceDefault
}

// lookup finds an environment by its name, or failing that by its id, so
// `--env <id>` pasted from the console lands on the same entry as its name.
// A copy is returned: Resolved must not alias the map the file was read into.
func (p *Project) lookup(name string) *EnvConfig {
	if e, ok := p.Environments[name]; ok {
		return &e
	}
	for _, e := range p.Environments {
		if e.ID != "" && e.ID == name {
			return &e
		}
	}
	return nil
}
