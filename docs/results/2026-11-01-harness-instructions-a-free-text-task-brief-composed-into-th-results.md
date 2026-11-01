<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0112 — Harness instructions: a free-text task brief composed into the system prompt](../changes/active/0112-harness-instructions-a-free-text-task-brief-composed-into-th.md)**
<!-- docket:backlink:end -->

# Harness instructions: results

**Human action:** Optional only. Automated tests cover the feature end to end. A reviewer may want to try a quick manual run of the interactive shell, because no test drives it.

## Outcome

`HarnessSpec.instructions` (proto field 14, `max_len` 16384, textarea widget) and a top-level `instructions:` config key now add a free-text task brief to the main agent's system prompt. The order is `system_prefix`, then the persona prompt, then the instructions, then the existing extra blocks. Subagents and workflow workers never get the brief. Empty or whitespace-only instructions leave every prompt byte-for-byte unchanged, which goldens recorded before the change prove. The value carries over in both directions in harnessconfig and is part of the spec digest. The docs (README, sdk/README) and an example spec (`examples/harness/release-notes.json`) were added.

## Human actions and testing

- **Optional:** check the interactive shell by hand. The TUI build closure in `cmd/fuse/shell.go` passes `cfg.Instructions`, but no test can drive it headless.
  - Setup: a fuse config pointing at a cheap gateway model (not an Anthropic model), with a top-level `instructions: "Always answer in exactly one sentence."`.
  - Steps: run `fuse` in shell mode and ask a multi-part question.
  - Expected: the answer follows the brief. If `/models` or tracing shows the system prompt, it contains the brief after the persona text.
  - Cleanup: remove the `instructions:` key.

## Verification performed

- Build gate `make test` passed at 7707c89 (driven by the docket gate). `make lint` (`go vet`) and `make test-race` both exited 0 at the same head. `make sdk-ts-test` exited 0 at eb501b9. The only later commit changes Go config code, which those TS tests do not cover.
- `make proto` was regenerated, and a second run left no diff.
- The repo has no vulncheck target and `govulncheck` is not installed. Per the run constraints it was not installed, so no vulncheck ran.
- Every root and child `buildAgentCore` call site was classified by grep: 5 root and 4 child. Mutation tests showed each tested site fails when its wiring is broken.
- Review: deep tier. It returned 1 minor finding: whitespace-only instructions cleared an inherited brief. This was fixed in 7707c89 with regression tests.
- WARNING: `superpowers:writing-plans` was unavailable in this session, so the plan writer authored the plan itself.

## Known issues and follow-ups

- **Digest and whitespace-only instructions (confirmed, low impact).** A spec whose instructions are only whitespace gets a different digest from one with no instructions, although both now compose the same prompt. The only effect is that two specs that behave the same have different digests. There is no workaround and none is needed. Suggested next action: normalize the value before computing the digest if digest equality matters to callers. Since a human would need to decide this, it is left as a note.
