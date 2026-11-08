package tools

import (
	"context"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// A shell killed by a signal says so, instead of "exit status -1" and nothing.
func TestBashReportsASignalDeath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX signals")
	}
	res := hostBash(t).Execute(context.Background(), `{"command":"echo before; kill -TERM $$"}`)
	if !res.IsError {
		t.Fatalf("a killed shell must be an error: %q", res.Output)
	}
	if !strings.Contains(res.Output, "before") || !strings.Contains(res.Output, "killed by signal 15") {
		t.Errorf("output %q", res.Output)
	}
	if strings.Contains(res.Output, "exit status -1") || strings.Contains(res.Output, "own shell") {
		t.Errorf("output %q", res.Output)
	}
}

// pkill -f matches the shell running it. Live, the model re-ran such a command
// three times on "exit status -1"; the result now names the likely cause.
func TestBashExplainsAPkillThatKilledItsOwnShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX signals")
	}
	// Stand in for pkill: the shell's own command line holds the -f pattern, and
	// the shell dies of SIGTERM, exactly as it does when pkill matches it.
	res := hostBash(t).Execute(context.Background(), `{"command":": pkill -f 'http.server 8080'; kill -TERM $$"}`)
	if !strings.Contains(res.Output, "killed by signal 15") || !strings.Contains(res.Output, "killed its own shell") {
		t.Errorf("output %q", res.Output)
	}
}

func TestExitMessage(t *testing.T) {
	for _, c := range []struct {
		cmd  string
		out  sandbox.Output
		want []string
		not  []string
	}{
		{"false", sandbox.Output{ExitCode: 1}, []string{"error: exit status 1"}, []string{"signal"}},
		{"make", sandbox.Output{ExitCode: 2}, []string{"exit status 2"}, []string{"signal"}},
		{"sleep 9", sandbox.Output{ExitCode: -1, Signal: syscall.SIGKILL}, []string{"killed by signal 9"}, []string{"own shell"}},
		{"python3 x.py", sandbox.Output{ExitCode: 137}, []string{"exit status 137", "killed by signal 9"}, []string{"own shell"}},
		{"pkill -f 'http.server'", sandbox.Output{ExitCode: -1, Signal: syscall.SIGTERM}, []string{"killed by signal 15", "own shell"}, nil},
		{"pkill -9 -f web-server.py; sleep 1", sandbox.Output{ExitCode: 143}, []string{"exit status 143", "own shell"}, nil},
		{"kill $(pgrep -af server)", sandbox.Output{ExitCode: -1, Signal: syscall.SIGTERM}, []string{"own shell"}, nil},
		{"pkill -x python3", sandbox.Output{ExitCode: -1, Signal: syscall.SIGTERM}, []string{"killed by signal 15"}, []string{"own shell"}},
		{"pkill -f server", sandbox.Output{ExitCode: 1}, []string{"exit status 1"}, []string{"own shell", "signal"}},
	} {
		got := exitMessage(c.cmd, c.out)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%q %+v: %q lacks %q", c.cmd, c.out, got, w)
			}
		}
		for _, n := range c.not {
			if strings.Contains(got, n) {
				t.Errorf("%q %+v: %q has %q", c.cmd, c.out, got, n)
			}
		}
	}
}
