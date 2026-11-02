<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0114 — spawn_agent picks a role: role enum with descriptions, role brief composed into the child prompt](../changes/archive/2026-11-02-0114-spawn-agent-picks-a-role-role-enum-with-descriptions-role-br.md)**
<!-- docket:backlink:end -->

# Results: spawn_agent picks a role

**Human action:** Optional only. The full suite is green and a live glm run chose a role on its own, but one live-check item (the child's exact tool list) was not confirmed from the trace; an optional re-run below covers it.

## Outcome

`spawn_agent` now offers a `role` parameter when roles are defined. It is an enum of role names, and its description lists one `name — description` line per role. Outside a workflow every role is offered; inside one, the workflow's listed roles are offered (or every role when it lists none).

When a parent spawns with a role:

- the role's instructions go into the child's system prompt in the slot the harness instructions take for the root (persona, then role brief, then any extra);
- a non-empty role tool list becomes the child's whole tool set plus the blackboard tools, and the call's `tools` may only narrow it; an empty list inherits the parent's tools;
- the model is the call's model, else the role's model, else the parent's;
- the role name appears on the agent tree node and snapshot, the `spawn.start` event (omitted when empty) and the spawn span.

These are refused before any node or quota is spent: `role` with `system_prompt`, `role` with `worker`, an unknown role, a role the target workflow does not offer, and a `tools` list that widens the role.

With no roles defined, the `spawn_agent` schema, the system prompts and the `spawn.start` JSON are byte-identical to before (pinned by new golden tests). When roles exist, one sentence ("Prefer a named role when one fits the task.") follows the spawn block.

There are four child-builder closures, not five as the spec counted: `shell.go:333` is only a wiring comment (now corrected to "2 of 4"). The parity test has five rows; the fifth runs the loop server with roles from a harness spec. A guard fails if a new builder site appears without a row.

## Human actions and testing

- **Optional — confirm a role child's tool list in a live run.** Prerequisites: the local gateway at `http://localhost:4000` and a build from the feature branch (`make build`). Steps: copy `~/.fuse/config.yml` into a temporary HOME, append a `roles:` block with `reviewer` (tools `[read_file, grep]`) and `writer`, then run `HOME=<tmp> ./fuse --model glm --trace <tmp>/trace.jsonl "Use a helper agent to review internal/tools/spawn_agent.go for correctness bugs and report what it finds."`. Expected: the child request whose system text contains your reviewer instructions lists exactly `read_file`, `grep` and the five `blackboard_*` tools. Cleanup: delete the temporary HOME. Never use an Anthropic model for this.

## Verification performed

- Full suite (`make test`) green through the build gate at the final head; `go vet ./...` clean.
- Live check with `glm` through the local gateway, using a temporary HOME: the parent called `spawn_agent` with `"role":"reviewer"`; the reviewer instructions appeared in the child's prompt; the `reviewer — Reviews Go source…` description line was in the tool schema; blackboard tools were present in the run. The trace was deleted before the child's exact tool list was extracted, so item (d) is covered by the parity test only.
- Review: deep tier. One important finding (a role with a workflow was checked against the parent's offered set instead of that workflow's) and three minor findings, all fixed in-branch.

## Known issues and follow-ups

- **The root still sees every role when it starts a workflow.** When a parent outside any workflow spawns with both `role` and `workflow`, its schema lists every role, but the runtime now accepts only roles that workflow offers. A model that picks an unlisted role gets a clear refusal naming the available roles and can retry. Confirmed, by design of this change; no action needed unless models trip on it in practice.
- **Task 4 was routed one tier lower than the plan suggested.** The plan labelled the workflow-scope task premium; it was built at standard. The deep review covered that code, and its one important finding there was fixed.
