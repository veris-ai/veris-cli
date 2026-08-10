//go:build unix

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
	"syscall"
)

// isolateProcessGroup puts the child in its own process group so a signal can
// be delivered to the whole tree. A test runner spawns compilers, servers and
// workers of its own; signalling only the runner leaves them running and the
// proxy waiting on a pipe that never closes.
func Isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// RunAsUID makes the child exec as uid, keeping its own process group.
//
// For a helper started while the parent is still root: the child needs none of
// that privilege, and keeping it for the life of the run is the difference
// between a proxy that dropped and one that only looks like it did.
func RunAsUID(cmd *exec.Cmd, uid int) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Credential = &syscall.Credential{
		Uid: uint32(uid), Gid: uint32(uid),
	}
}

// Terminate signals the child's whole process group. The negative pid is
// what makes it a group signal.
func Terminate(cmd *exec.Cmd, sig os.Signal) {
	if cmd.Process == nil {
		return
	}
	s, ok := sig.(syscall.Signal)
	if !ok {
		s = syscall.SIGTERM
	}
	if err := syscall.Kill(-cmd.Process.Pid, s); err != nil {
		// The group may already be gone, or setpgid may have failed; fall back
		// to the process itself rather than giving up on teardown.
		_ = cmd.Process.Signal(sig)
	}
}

// waitStatus turns the child's outcome into an exit code. A child killed by a
// signal reports -1 through ExitCode, so the shell convention of 128+signal is
// reconstructed from the raw wait status.
func WaitStatus(cmd *exec.Cmd, err error) (int, error) {
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
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal()), nil
	}
	return 1, nil
}
