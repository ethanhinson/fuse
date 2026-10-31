<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0094 — Approval provenance is per gate, not per process](../changes/active/0094-approval-provenance-is-per-gate-not-per-process.md)**
<!-- docket:backlink:end -->

# Approval provenance is per gate, not per process - Results

**Human action:** No action is required before merge. One optional walkthrough is offered; one follow-up (provenance across the CLI-model relay) is reported for triage.

## Outcome

Who answered a gate's approval prompts was bound per gate, but the "human or policy" label was a single process-wide variable. Child agents built with the always-approve stand-in inside an attended run, the research probe, and the shell orchestrator's deny stand-in were all recorded as decided by a human, and the probe's stand-in approved explicit ask rules and smart-mode catastrophic commands.

Now provenance travels with the approver. `permissions.Approver` carries the answer function and who it is (`Human(...)` or `Policy(...)`); `AlwaysApprove` is a policy value; `permissions.New` takes provenance only from the approver and panics on an unlabelled or unknown one; `WithApprovalProvenance`, `gateApprovalProvenance` and `markPolicyApproval` are gone. Every builder, the one-shot approver, the HITL client, the shell, the orchestrator stand-in, both loop servers, the mcp-server and the research probe were migrated. Observable behavior change: in `fuse research-probe`, an explicit ask rule (`auto.ask`, `always_prompt`) and, in smart mode, a catastrophic command are now denied at the rules layer with `decided_by: policy`; orchestrator stand-in denials now record `policy`. ADR-0070 supersedes ADR-0066 and records the per-approver rule. README wording was updated.

Departures from the spec: `New` also rejects an unknown `DecidedBy` value (not only empty); the "default human posture" half of `TestHumanDecisionCarriesProvenance` was rewritten since there is no default any more; the orchestrator approver and builder were extracted into named values so the binding test exercises production wiring; the end-to-end test starts the loop with an empty harness spec so the production resolver runs and the child node is captured.

## Human actions and testing

### Optional - Probe denies an explicit ask rule

Automated tests cover this; a hand check confirms the operator-facing effect. Prerequisite: a built `fuse` and a config with `permissions: {mode: auto, auto: {ask: ["bash:touch*"]}}`.

1. Run a `fuse research-probe` whose task makes the model run `touch sentinel`.
   Expected: the command does not run, the tool result says `matched ask rule bash:touch*; no approver bound`, and the `permission.decision` event shows `deny`, layer `rules`, `decided_by: policy`.
2. Move the pattern to `auto_approve` and rerun.
   Expected: the command runs.

Cleanup: remove `sentinel`.

## Verification performed

Full suite (`make test`) green at the final branch head via the build gate; `make lint` and gofmt clean. Every new binding test was mutation-checked (approver flipped to human, test fails, reverted). Tests use scripted fakes and httptest gateways only; no live model traffic. Whole-branch review (deep tier): 3 findings; one fixed in-branch (orchestrator test now drives the real builder), one important finding and one minor finding recorded below. Backlog check against proposed/deferred changes was run.

## Known issues and follow-ups

- **Provenance is lost across the CLI-model HITL relay.** When a `cli/` model is used under a policy approver, the gate relays prompts to a `fuse mcp-server` subprocess whose HITL socket client labels itself human, so an ask match behind that hop can still be approved and recorded as human. Confirmed by review; pre-existing, now documented in ADR-0070. Workaround: avoid explicit ask rules with `cli/` models in headless bindings. Suggested next action: carry the approver's provenance across the relay (for example an env var next to the socket). Backlog: No existing change fits (checked 5); Related to #96 (same area, but it concerns unparseable commands, not the relay). A human captures it with `docket change create`.
- **Policy denials keep the reason text "tool call denied by user".** Orchestrator and piped-stdin denials now record `decided_by: policy` but the reason still names a user. Suspected cosmetic/audit-wording issue; no behavior impact. Suggested next action: use a neutral reason for policy approvers and update the one pinned assertion. Backlog: No existing change fits (checked 5).
