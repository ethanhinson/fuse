//go:build windows

package procgroup

import (
	"os"
	"os/exec"
)

// Configure does nothing: Windows has no process groups in this sense. The file
// exists so the package keeps building there, where a kill reaches only the
// direct child.
func Configure(*exec.Cmd) {}

// Kill kills the direct child.
func Kill(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return os.ErrProcessDone
	}
	return p.Kill()
}
