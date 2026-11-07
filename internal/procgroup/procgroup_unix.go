//go:build !windows

// Package procgroup puts a child process in its own process group so the child
// and everything it forks can be killed as one. Killing only the direct child
// leaves a compound shell command's sleep, or an npx-launched MCP server, running
// with nothing left to stop it.
package procgroup

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// Configure makes c the leader of a new process group. It must be called before
// c starts.
func Configure(c *exec.Cmd) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.Setpgid = true
}

// Kill sends SIGKILL to the whole group led by pid. Only a pid from a child
// started with Configure, and not yet reaped, names a group this process owns.
//
// os.ErrProcessDone covers a group that is already gone. EPERM counts as gone
// too because Darwin reports it when only zombies are left, and that is the
// answer exec.Cmd.Cancel needs to treat the kill as having nothing to do.
func Kill(pid int) error {
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) {
		return os.ErrProcessDone
	}
	return err
}
