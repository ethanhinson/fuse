<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0096 — Unparseable shell commands are denied when no human approver is bound](../changes/archive/2026-10-31-0096-unparseable-shell-commands-are-denied-when-no-human-approver.md)**
<!-- docket:backlink:end -->

# Unparseable shell commands are denied when no human approver is bound - Results

**Human action:** No action is required to ship. One optional walkthrough is offered, and a reviewer should decide whether to accept the unbounded command echo described under Known issues.

## Outcome

Some shell commands cannot be broken into pieces by the permission parser, for example `sudo ...`, `npx ...`, `xargs ...`, a path-qualified binary such as `/bin/rm`, an `env` prefix with non-inert assignments, or `bash script.sh`. Before this change, when no person was present to answer (hosted loop servers, the headless MCP server, the research probe, piped-stdin one-shot runs), a stand-in approver said yes, so the most dangerous shapes ran while their plain forms were denied.

Now, in both auto and smart mode, such a command is denied at the rules layer with the reason `unparseable command denied; no approver bound: <command>` and `decided_by: policy`. With a person bound it still prompts; in smart mode that prompt now reports `layer: parse` with an empty reason. Protective rules (`auto.deny`, `auto.ask`, `always_prompt`) now match the raw command text in auto mode, as smart mode already did. In smart mode a first-token `auto_approve` entry such as `bash:sudo` still admits the command, which is the existing documented escape hatch; auto mode has none. ADR-0071 records the decision. The README is updated in the permissions, loop-server and research-probe sections.

The build departed from the spec in three small, documented ways: the unparseable parity rows live in their own table (the shared table's witness cannot approve them in auto mode), no parse memo was added, and three existing tests that encoded the old behavior now bind a human approver with no assertion weakened.

## Human actions and testing

### Optional - see the deny in a hosted loop

Automated tests already cover this end to end with a scripted model; this is only a manual look. Build fuse from this branch and start a loop server in auto mode with a model that issues `/usr/bin/touch ran` through the bash tool, in an empty temp directory.

1. Run the command through the loop and watch the `permission.decision` events.
   Expected: one `deny` at layer `rules` with `decided_by: policy` and the reason beginning `unparseable command denied; no approver bound: `, and no file named `ran` in the directory.

Cleanup: delete the temp directory.

## Verification performed

Full suite (`make test`) green at the final head via the build gate. Mutation probes confirmed the new tests fail when the policy deny branches are removed. All model traffic was scripted fakes. Whole-branch review (deep tier): 2 findings; the minor README gap was fixed in-branch, the important one is recorded below.

## Known issues and follow-ups

- The new deny reasons repeat the full raw command with no length cap or redaction. This shows up when a refused command is very long or carries credentials in its arguments (for example a URL with userinfo): the text goes into the permission decision event and back to the model. Parsed-command denies already echo the command but are capped at 10000 bytes; the over-length refusal shape is not. The spec mandates the `<prefix> + <command>` form, so this was left as designed. Confirmed by review. Suggested next action: bound and redact the echoed command the way the decision event's command preview does. Backlog: Related to #97 (checked 5).
- The hint appended to rules-layer denials describes the command as catastrophic, which is inexact for this deny and wrong in smart mode. Confirmed. Suggested next action: make the hint reason-aware. Backlog: Related to #97 (checked 5).
