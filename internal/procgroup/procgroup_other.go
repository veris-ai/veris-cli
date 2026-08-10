//go:build !unix

// Package procgroup runs a child process as its own group, so stopping it
// stops everything it started.
//
// Two callers need the same three operations -- the command under test and the
// callback tunnel -- and the failure when it is skipped is identical in both:
// killing the parent leaves the children running, and the pipe they inherited
// never closes, so the wait blocks until a timeout rather than returning.
package procgroup

import (
	"errors"
	"os"
	"os/exec"
)

// Windows has no process groups in the POSIX sense and os.Process.Signal does
// not implement os.Interrupt there, so the child is killed outright rather
// than asked to stop. Descendants it spawned are not reached; a Job Object
// would be needed for that.
func Isolate(*exec.Cmd) {}

// RunAsUID is a no-op: Windows has no uid to drop to.
func RunAsUID(*exec.Cmd, int) {}

func Terminate(cmd *exec.Cmd, _ os.Signal) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func WaitStatus(_ *exec.Cmd, err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return 0, err
	}
	if code := ee.ExitCode(); code >= 0 {
		return code, nil
	}
	return 1, nil
}
