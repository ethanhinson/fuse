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
// It settles TWO things, and the second is equally security-bearing: it is
// where the off-switch config is read from, AND it is the working tree the
// container substrate bind-mounts at /workspace. A model's working_dir selects
// a subdirectory of it and can never replace it (sandbox.WithTrustedRoot).
// When os.Getwd fails there is no trusted root, so nothing is mounted and any
// working_dir is refused — degraded, but never a mount the model chose.
//
// hosted declares the ADR-0034 posture: true when this binary executes
// workloads on behalf of REMOTE principals (the loop servers), false for the
// operator's own local CLI. It comes from how the binary was launched and from
// nowhere else — never from config, an environment variable, a wire field, or
// model output. Under the hosted posture the off-switch is structurally inert.
//
// The Warnings are the operator's only signal that their config file did not do
// what they thought, so they are logged unconditionally. Selection of the HOST
// substrate is logged too: uncontained execution should never be quiet.
//
// A construction error yields a NIL Service, which is fail-CLOSED — the bash
// tool then refuses every call. It is never an invitation to run uncontained;
// note that a missing container runtime is deliberately NOT an error here, it
// surfaces as a refusal at Acquire.
func newSandboxService(hosted bool, warnw io.Writer) *sandbox.Service {
	root, err := os.Getwd()
	if err != nil {
		// LoadConfig treats an empty root as "no file was consulted" and
		// returns the contained default plus a warning — the safe direction.
		root = ""
	}

	svc, warns, serr := sandbox.NewServiceFromRoot(root, sandbox.WithHostedPosture(hosted))
	for _, w := range warns {
		fmt.Fprintf(warnw, "warning: %s\n", w.Error())
	}
	if serr != nil {
		fmt.Fprintf(warnw, "sandbox: substrate unavailable (%v); the bash tool will refuse to run commands\n", serr)
		return nil
	}
	if svc != nil && !svc.Contained() && svc.Available() {
		fmt.Fprintf(warnw, "sandbox: UNCONTAINED — %s/.fuse/sandbox.local.yml authorizes running bash commands directly on this host\n", root)
	}
	return svc
}
