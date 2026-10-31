<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0098 — Permission deny reasons bound and redact the command they echo](../changes/archive/2026-10-31-0098-permission-deny-reasons-bound-and-redact-the-command-they-ec.md)**
<!-- docket:backlink:end -->

# Permission deny reasons bound and redact the command they echo - Results

**Human action:** No human action is required before merge. One optional walkthrough is listed below; the release note should mention the approve-all restoration and the new relay variable.

## Outcome

Permission deny and ask reasons that quote a command used to repeat it in full, unbounded and with any URL credentials intact, into durable decision events and the tool result sent to the model. They now go through one helper shared with the structured command preview: credentials in URLs become `***@`, text is trimmed and cut at 200 bytes on a rune boundary with an ellipsis. Short, credential-free commands produce byte-identical reasons.

Other delivered behavior:
- A deny from a policy approver now reads "tool call denied by policy approver"; human denies still read "tool call denied by user". `decided_by` is unchanged.
- The rules-layer deny hint now says the call is blocked by the permission rules instead of calling every rules deny catastrophic. Em dashes were removed from the other hints.
- `--approve-all` with a `cli/` model now reaches the tool subprocess: the parent passes `FUSE_HITL_MODE=off`, which `fuse mcp-server` honors only when `FUSE_HITL_SOCKET` is also set, so the subprocess runs with permissions off for that call.
- The classifier deny reason now bounds the command it echoes (beyond the spec), and a stale inherited `FUSE_HITL_MODE` is stripped before the adapter sets its own.
- No ADR was recorded: this applies the existing preview rule to the prose reason, makes the approver wording agree with `decided_by`, and makes the existing "moot, since that run is mode off" statement true across the relay.

Departure from the spec: the heuristic-deny test row could not be built because the heuristic layer never returns a deny; that call site is still converted and covered by a source guard plus an unparseable-command `auto.deny` row.

## Human actions and testing

### Optional - see approve-all across the relay

Confirms the one change that crosses a process boundary. Automated tests cover it with scripted fakes, so this is a manual sanity check only.

Prerequisites: a `cli/` model configured and an `auto.ask` rule such as `bash:touch*` in your user config.

1. Run `fuse` with `--approve-all` and a prompt that makes the model run `touch ran` in a scratch directory.
   Expected: the file `ran` is created with no prompt and no rules-layer deny.
2. Run the same without `--approve-all`.
   Expected: the call is asked or denied per your rules.

Cleanup: delete the scratch directory.

## Verification performed

- Full suite (`make test`) green at the final head via the gate driver.
- Whole-branch review (standard tier): 3 findings; 2 fixed in-branch (rune-boundary cut, honest wording of the relay trust claim), 1 recorded below.
- Hosted end-to-end cases on the production loop-server wiring (over-length unparseable command bounded; credentialed URL redacted in the reason and in the tool message) pass, and fail under a mutation that disables the helper.

## Known issues and follow-ups

### Relayed mode-off does not follow a mid-turn tightening

When a user switches from mode off to a stricter mode in the middle of a turn, later model calls in that same turn still start a tool subprocess at mode off, because the adapter captures the mode when the agent is built. The parent gate tightens immediately. The spec accepted this lag; the practical impact is limited to the remainder of one turn. Confirmed. Workaround: none needed beyond waiting for the next turn. Suggested next action: read the live session mode in `subprocessEnv` on each call. No existing change fits (checked 4); a human can capture it with `docket change create`.

### FUSE_HITL_MODE=off trusts the server's process environment

The mcp-server checks only that `FUSE_HITL_SOCKET` is non-empty, so whoever controls the server's environment can set both variables and turn its rules layer off. The code comment and README say this plainly and name it as a loosening path next to the Trust boundary paragraph. Confirmed; low impact since that party usually controls the command line too. Suggested next action: have the server dial the relay socket before honoring the value. No existing change fits (checked 4).
