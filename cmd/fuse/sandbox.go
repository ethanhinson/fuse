package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// newSandboxService resolves the bash substrate for this process, ONCE.
//
// # Why once, and why here
//
// The off-switch is a file — ~/.fuse/sandbox.local.yml — and the agent running
// inside the sandbox can write files. Resolving containment per bash
// call would let a model author its own escape on one call and use it on the
// next. So every entry point calls this exactly once, at startup, before any
// model has run, and hands the resulting frozen *sandbox.Service to the tool
// registry. Nothing downstream can re-read config, re-select a handler, or
// reach the filesystem (see sandbox.Service).
//
// The file is read from the home directory and never from the working directory,
// because a cloned repository could otherwise ship it and disable containment. The
// working directory is only what a contained command sees. Both are resolved here,
// at startup, rather than at Exec time — a tool's working_dir argument never
// reaches this decision.
//
// A refused selection and an uncontained one are both said out loud on warnw. The
// Service is still returned when selection refused: it is fail-CLOSED and refuses
// every command, which is the correct posture.
func newSandboxService(warnw io.Writer) *sandbox.Service {
	root, err := os.Getwd()
	if err != nil {
		root = ""
	}
	// An unresolved home leaves configRoot empty, and LoadConfig then returns the
	// contained default plus a warning: the safe direction.
	configRoot, err := os.UserHomeDir()
	if err != nil {
		configRoot = ""
	}
	warnRepoSandboxFile(root, configRoot, warnw)

	svc, warns, serr := sandbox.NewServiceFromConfigRoot(configRoot, root)
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
		fmt.Fprintf(warnw, "sandbox: UNCONTAINED — ~/.fuse/sandbox.local.yml authorizes running bash commands directly on this host\n")
	}
	return svc
}

// warnRepoSandboxFile says when a working tree carries a sandbox file that is being
// ignored, since the person running fuse may expect it to apply and a hostile
// repository's attempt is worth seeing.
func warnRepoSandboxFile(root, configRoot string, warnw io.Writer) {
	if root == "" || root == configRoot {
		return
	}
	path := filepath.Join(root, ".fuse", "sandbox.local.yml")
	if _, err := os.Stat(path); err == nil {
		fmt.Fprintf(warnw, "warning: ignoring %s; sandbox settings are only read from ~/.fuse/sandbox.local.yml\n", path)
	}
}
