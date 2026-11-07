package main

import (
	"fmt"
	"io"
	"os"

	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// newSandboxService resolves the bash substrate for this process, ONCE.
//
// # Why once, and why here
//
// The off-switch is a file — <root>/.fuse/sandbox.local.yml — and the agent
// running inside the sandbox can write files. Resolving containment per bash
// call would let a model author its own escape on one call and use it on the
// next. So every entry point calls this exactly once, at startup, before any
// model has run, and hands the resulting frozen *sandbox.Service to the tool
// registry. Nothing downstream can re-read config, re-select a handler, or
// reach the filesystem (see sandbox.Service).
//
// root is the process working directory, which is the same trusted-local root
// internal/config already resolves .fuse.local.yml against. It is read here,
// at startup, rather than at Exec time — a tool's working_dir argument never
// reaches this decision.
//
// A refused selection and an uncontained one are both said out loud on warnw. The
// Service is still returned when selection refused: it is fail-CLOSED and refuses
// every command, which is the correct posture.
func newSandboxService(warnw io.Writer) *sandbox.Service {
	root, err := os.Getwd()
	if err != nil {
		// LoadConfig treats an empty root as "no file was consulted" and
		// returns the contained default plus a warning — the safe direction.
		root = ""
	}

	svc, warns, serr := sandbox.NewServiceFromRoot(root)
	for _, w := range warns {
		fmt.Fprintf(warnw, "warning: %s\n", w.Error())
	}
	if serr != nil {
		fmt.Fprintf(warnw, "sandbox: substrate unavailable (%v); the bash tool will refuse to run commands\n", serr)
		return nil
	}
	if refusal := svc.SelectionRefusal(); refusal != nil {
		fmt.Fprintf(warnw, "sandbox: substrate unavailable (%v); the bash tool will refuse to run commands\n", refusal)
	}
	if !svc.Contained() && svc.Available() {
		fmt.Fprintf(warnw, "sandbox: UNCONTAINED — %s/.fuse/sandbox.local.yml authorizes running bash commands directly on this host\n", root)
	}
	return svc
}
