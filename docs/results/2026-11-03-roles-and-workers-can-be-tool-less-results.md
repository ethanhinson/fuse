<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0116 — Tool-less loops, roles and workers](../changes/archive/2026-11-03-0116-roles-and-workers-can-be-tool-less.md)**
<!-- docket:backlink:end -->

# Tool-less loops, roles and workers — results

**Human action:** Not required to merge. One optional walkthrough is listed below, and two follow-ups need human triage.

## Outcome

A loop, role or worker can now be declared tool-less with `no_tools: true`, in a harness spec (`HarnessSpec`, `Role`, `Worker`) or in config (`Config`, `RoleConfig`, `WorkerConfig`).

- A tool-less loop's model call carries no `tools` key. That means no platform tools, no spec MCP tools (the servers are not started), and no internal tools: no `segment_read`, codeindex, blackboard, `spawn_agent` or `pipeline_run`.
- A tool-less child gets an empty registry, blackboard included, at every child-builder site and from pipeline steps.
- Narrow-only holds. A spawn-time `tools` request on a tool-less role or worker is refused, and a `model` override is still honoured.
- Tool-less prompts use a persona line saying the model has no tools.
- `GetCapabilitiesResponse.spec_features` advertises `no_tools`, and both SDKs expose it.
- Validation rejects `no_tools` set beside a non-empty `tools`. `Worker.tools` is no longer a shape rule, so a worker may set `no_tools` instead.
- The first request of an ordinary spec is byte-identical to before; a golden captured before any change pins it.

Review fixes added three refusals:

- `no_tools` with a `cli/` model is refused. The CLI subprocess always has fuse's tools.
- `no_tools` with `orchestrate_loop_id` is refused with InvalidArgument.
- A tool-less child given `expects` is offered exactly `return_result`. This is documented in the proto comments as the one exception, because it is the result channel.

## Human actions and testing

- **Optional — start a tool-less loop against a local loop server.**
  - **Prerequisites:** a built `fuse` and a non-Anthropic gateway model alias.
  - **Steps:** start the loop server. Call `StartLoop` with a spec that sets `no_tools: true` and `max_turns: 1`.
  - **Expected:** the model answers in plain text, the gateway request log shows no `tools` field, and `GetCapabilities` lists `no_tools` under `spec_features`.
  - **Cleanup:** stop the server.
  - Automated tests already cover this at the gateway seam (`TestLoopServerRunsAToolLessSpec`).

## Verification performed

- The full suite (`make test`) passed after the build, and again after the review fixes.
- Each task's focused tests went RED then GREEN. Where a task only added tests, mutation probes showed the guards turn red.
- Deep-tier review returned 3 important and 3 minor findings, with no blockers. All six were fixed in-branch.
- Backlog check ran over 17 proposed or deferred changes.

## Known issues and follow-ups

- **A role combined with `expects` is refused.** Confirmed, and it predates this change.
  - **When:** any spawn or pipeline step that names a role and also sets `expects`.
  - **What happens:** it fails with "a role supplies the system prompt; drop system_prompt or the role". The cause is that the Spawner writes the `return_result` hint into the system prompt, and `checkRole` then rejects it.
  - **Impact:** roles cannot return structured results.
  - **Workaround:** use a worker instead.
  - **Next:** fix how the hint composes with a role's prompt. No existing change fits (checked 17); a human could capture it as a new change related to the merged roles work.
- **A tool-less child given `expects` loses its tool-less persona line.** Confirmed.
  - **Why:** with `expects` set, the system prompt is non-empty, so every child builder takes the `buildChildAgent` branch, which skips persona composition. The `return_result` hint also says "use your other tools first".
  - **Impact:** the wording is misleading; the tool set stays correct.
  - **Next:** a small change to `SpawnOpts` so the builders can tell a hint-only prompt from an explicit `system_prompt`. No existing change fits (checked 17).
