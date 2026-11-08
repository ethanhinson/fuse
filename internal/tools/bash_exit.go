package tools

import (
	"fmt"
	"regexp"
	"syscall"

	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// selfMatchingKill finds pkill/pgrep with -f, which matches every process whose
// full command line contains the pattern. The shell running the command has
// that pattern in its own command line, so it matches itself.
var selfMatchingKill = regexp.MustCompile(`\b(?:pkill|pgrep)\b(?:\s+-\S+)*?\s+-[a-zA-Z]*f\b`)

const selfKillNote = "The command probably killed its own shell: pkill -f (or pgrep -f) matches any process whose command line contains the pattern, " +
	"and the shell running this command contains it too. Match more narrowly — pkill -x NAME, a PID file, " +
	"or a bracketed pattern such as pkill -f '[h]ttp.server' that matches the process but not the text of this command."

// exitMessage says how a command that ran ended badly. A shell killed by a
// signal used to read "exit status -1" with no output, which told the model
// nothing, and live it re-ran the same self-killing command up to three times.
func exitMessage(command string, out sandbox.Output) string {
	sig := out.Signal
	var msg string
	switch {
	case sig != 0:
		msg = fmt.Sprintf("error: killed by signal %d (%s)", int(sig), sig)
	case out.ExitCode > 128 && out.ExitCode <= 128+31:
		// Shells and container execs report a process killed by signal N as 128+N.
		sig = syscall.Signal(out.ExitCode - 128)
		msg = fmt.Sprintf("error: exit status %d (usually: killed by signal %d (%s))", out.ExitCode, int(sig), sig)
	default:
		return fmt.Sprintf("error: exit status %d", out.ExitCode)
	}
	if selfMatchingKill.MatchString(command) {
		msg += "\n" + selfKillNote
	}
	return msg
}
