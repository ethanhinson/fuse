package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/ethanhinson/fuse/internal/permissions"
)

// stdinIsTerminal reports whether stdin is an interactive terminal. A human is
// reachable only when it is; a piped stdin (CI, `fuse "task" < file`, a shell
// pipeline) reads as false, driving the deny-by-default fallback.
func stdinIsTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// gateApprovalProvenance is the process-wide answer to "who answers approval
// prompts in this binding" stamped on LayerHuman permission.decision events
// (audit provenance). A fuse process is exactly one binding, so this is static
// per process: each entry point that wires a policy stand-in (loop-serve's
// AlwaysApprove, --approve-all, piped-stdin deny fallback, headless mcp-server)
// downgrades it from the human default before gates are built.
var gateApprovalProvenance = permissions.DecidedByHuman

// markPolicyApproval records that this binding answers approvals by policy,
// not by a reachable human.
func markPolicyApproval() { gateApprovalProvenance = permissions.DecidedByPolicy }

// oneShotApprovalFunc builds the approval fallback for the non-TUI entry points
// (one-shot `fuse "<task>"` and its children). It replaces the removed
// blanket-auto-approve bypass so a configured `ask`/prompt verdict is honored at
// every entry point rather than silently auto-approved:
//
//   - approveAll ⇒ the explicit --approve-all opt-in: auto-approve every call
//     (equivalent to mode: off for this run — the documented footgun).
//   - isTTY ⇒ a human is reachable: prompt y/N/a on the terminal, honoring
//     session_allow via the "a" (always) answer.
//   - otherwise (piped stdin / CI) ⇒ deny-by-default with a structured error.
//
// in/out are injected for testability; production passes os.Stdin / os.Stderr.
func oneShotApprovalFunc(approveAll, isTTY bool, in io.Reader, out io.Writer) permissions.ApprovalFunc {
	if approveAll {
		return autoApprove
	}
	if isTTY {
		return terminalApproval(in, out)
	}
	return denyNonInteractive
}

// autoApprove auto-approves every call without granting a session cache entry.
// It is reachable only via the explicit --approve-all flag; it deliberately
// restores the old approve-all posture for scripted, non-interactive use.
func autoApprove(_ context.Context, _ permissions.ApprovalRequest) (bool, bool, error) {
	return true, false, nil
}

// denyNonInteractive is the deny-by-default fallback for a non-interactive
// (piped stdin / CI) one-shot run: no human can answer the prompt, so a call
// that reaches the human-approval path is denied with a structured, actionable
// error rather than silently approved.
func denyNonInteractive(_ context.Context, req permissions.ApprovalRequest) (bool, bool, error) {
	return false, false, fmt.Errorf(
		"tool call requires approval but this run is non-interactive (piped stdin / CI); "+
			"pass --approve-all to auto-approve, or run in an interactive terminal / `fuse shell`: %s",
		req.Preview)
}

// terminalApproval returns an ApprovalFunc that surfaces the pending call's
// preview line and reads a y/N/a decision from in. "y" approves once, "a"
// (always) approves and grants session_allow so the gate caches it, anything
// else (including EOF) denies. A cancelled context returns its error.
func terminalApproval(in io.Reader, out io.Writer) permissions.ApprovalFunc {
	reader := bufio.NewReader(in)
	return func(ctx context.Context, req permissions.ApprovalRequest) (bool, bool, error) {
		if err := ctx.Err(); err != nil {
			return false, false, err
		}
		fmt.Fprintf(out, "\nApprove tool call?\n  %s\n[y]es / [N]o / [a]lways for this session: ", req.Preview)
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			// EOF or read error with nothing typed ⇒ deny (fail closed).
			fmt.Fprintln(out, "no")
			return false, false, nil
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return true, false, nil
		case "a", "always":
			return true, true, nil
		default:
			return false, false, nil
		}
	}
}
