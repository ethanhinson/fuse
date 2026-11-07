package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// runStopGrace is how long a cancelled run gets to tear down before the process
// exits regardless. Cancellation reaches well-behaved tools at once and teardown is
// itself bounded, so this catches only a tool that ignores its context. Nobody may
// be there to send a second signal, and a run past its max_run_duration has to end.
// It exceeds bash's own drain, so a container kill in progress gets to finish.
const runStopGrace = 15 * time.Second

// exitCodeLimit is a run that reached max_run_duration: the ordinary failure code,
// as for max_turns and max_run_tokens.
const exitCodeLimit = 1

// runStop owns the two ways a one-shot run ends early: a signal and the wall-clock
// limit. Both cancel ctx, and both print why at the moment it happens, so the
// reason is there even if teardown then takes a while.
type runStop struct {
	ctx      context.Context
	finished chan struct{}
	once     sync.Once
	exited   chan struct{}
	code     int // read after exited closes
}

// startRunStop installs the signal handler and starts the limit's clock. limit 0
// means no limit. finish must be called to release the handler.
//
// SIGHUP is here because of process groups. Bash commands and MCP servers now run
// in their own groups, so a terminal closing no longer signals them directly, and
// this process is what has to stop them.
func startRunStop(limit time.Duration, stderr io.Writer) *runStop {
	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	s := &runStop{ctx: ctx, finished: make(chan struct{}), exited: make(chan struct{})}

	go func() {
		defer close(s.exited)
		defer cancel()
		defer signal.Stop(sigs)
		var limitC, graceC <-chan time.Time
		if limit > 0 {
			t := time.NewTimer(limit)
			defer t.Stop()
			limitC = t.C
		}
		beginStop := func(code int) {
			s.code = code
			cancel()
			graceC = time.After(runStopGrace)
		}
		for {
			select {
			case <-s.finished:
				return
			case sig := <-sigs:
				code := signalExitCode(sig)
				if graceC != nil {
					// Already stopping and someone is insisting. This skips teardown,
					// which is the point: it is the way out when teardown is stuck.
					fmt.Fprintf(stderr, "fuse: received %s while stopping, exiting now\n", signalName(sig))
					os.Exit(code)
				}
				fmt.Fprintf(stderr, "fuse: received %s, stopping the run (send it again to exit immediately)\n", signalName(sig))
				beginStop(code)
			case <-limitC:
				limitC = nil
				if graceC == nil { // a signal already stopped the run; it keeps its code
					fmt.Fprintf(stderr, "fuse: run stopped: reached max_run_duration (%s)\n", limit)
					beginStop(exitCodeLimit)
				}
			case <-graceC:
				fmt.Fprintf(stderr, "fuse: teardown did not finish within %s, exiting\n", runStopGrace)
				os.Exit(s.code)
			}
		}
	}()
	return s
}

// finish releases the signal handler and reports the exit code the stop requires,
// or 0 if the run was not stopped. It is idempotent.
func (s *runStop) finish() int {
	s.once.Do(func() { close(s.finished) })
	<-s.exited
	return s.code
}

// signalExitCode is the shell convention of 128+N, which a supervisor reads as
// "stopped by this signal" even though the process exited by itself.
func signalExitCode(sig os.Signal) int {
	if n, ok := sig.(syscall.Signal); ok {
		return 128 + int(n)
	}
	return 1
}

func signalName(sig os.Signal) string {
	switch sig {
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGHUP:
		return "SIGHUP"
	}
	return sig.String()
}
