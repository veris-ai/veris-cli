// Package cfg is where the veris CLI keeps what it was told and decides which
// telling wins.
//
// Three files carry that state, each owned by a different scope:
//
//   - ~/.veris/twin.yaml is the user's: profiles, one per control plane login,
//     and which of them is active. It holds API keys, so it is 0600.
//   - .veris/twin.yaml is the project's: named environments and the default
//     one, committed so every checkout shares them.
//   - .veris/twin.local.yaml is this checkout's: the environment chosen here,
//     the sandbox last created, the baselines promoted. It is gitignored,
//     0600, and never written where no project file exists.
//
// Resolve reads all three together with the flags and environment variables
// and answers the five questions every command asks -- which profile, key,
// API base, environment and sandbox -- naming the layer each answer came from.
// Layers never merge: a value comes from exactly one place, so a surprising
// answer can always be traced to the line that produced it.
package cfg

import (
	"fmt"
	"os"
	"path/filepath"
)

// DefaultAPIBase is the control plane the CLI talks to unless a flag, the
// environment or a profile says otherwise.
const DefaultAPIBase = "https://svc.api.veris.ai"

// Environment variables Resolve honours. VERIS_API_BASE, VERIS_API_KEY,
// VERIS_SANDBOX_ID and VERIS_ENVIRONMENT_ID are the engine's own names, kept
// so a shell set up for veris-proxy needs no second spelling.
const (
	EnvProfile       = "VERIS_PROFILE"
	EnvAPIBase       = "VERIS_API_BASE"
	EnvAPIKey        = "VERIS_API_KEY"
	EnvEnv           = "VERIS_ENV"
	EnvEnvironmentID = "VERIS_ENVIRONMENT_ID"
	EnvSandboxID     = "VERIS_SANDBOX_ID"
)

// File names. The project and local files sit side by side under .veris/.
const (
	globalFileName  = "twin.yaml"
	projectDirName  = ".veris"
	projectFileName = "twin.yaml"
	localFileName   = "twin.local.yaml"
)

// Source names the layer a resolved value came from, so a command can print
// "environment dev (from .veris/twin.local.yaml)" rather than leave the user
// to guess which of five places to edit.
type Source string

const (
	SourceFlag    Source = "flag"
	SourceEnv     Source = "env"
	SourceLocal   Source = "local"
	SourceProject Source = "project"
	SourceProfile Source = "profile"
	SourceDefault Source = "default"
)

// writeAtomic replaces path with body in one rename, so an interrupted write
// can never leave a half-parsed file behind for the next command to trip on.
// The temp file is created beside the target because rename is atomic only
// within a filesystem. mode is set explicitly rather than inherited: CreateTemp
// makes 0600, which is right for a key file and wrong for a committed one.
func writeAtomic(path string, body []byte, mode, dirMode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// fileExists reports whether path names a regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// unreadable is the one error shape for a file that exists but does not
// parse: it names the path, because "yaml: line 3" alone sends the user
// hunting through three candidates.
func unreadable(path string, err error) error {
	return fmt.Errorf("%s is unreadable: %w", path, err)
}
