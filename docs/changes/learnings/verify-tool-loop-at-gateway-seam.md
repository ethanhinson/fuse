---
name: verify-tool-loop-at-gateway-seam
slug: verify-tool-loop-at-gateway-seam
title: Verify agent-loop changes at the model-facing gateway seam — a scripted local gateway drives the real binary where faked-seam harnesses cannot reach
hook: "when a change alters what the model sees or does per turn (tool schemas, budgets, strips, caps), verify with the real binary against a scripted LLM_GATEWAY_URL double that logs each request's tools[] — the TUI harness fakes the Completer seam and never exercises the cmd/fuse wiring"
promotion_state: candidate
changes: [33]
created: 2026-08-06
updated: 2026-08-06
topics: [verification, agents, tui, testing]
---

## Apply

The adapter accepts buffered-JSON responses by design ("a test double"), so a ~100-line
scripted HTTP server pointed at by `LLM_GATEWAY_URL` turns the *shipped* binary into a
deterministic end-to-end rig: script the tool_calls per turn, log the `tools[]` array of
every incoming request (the exact model-facing surface), and drive the TUI in tmux with
small config overrides (`.fuse.local.yml`, e.g. `max_concurrent: 1`, `max_spawns: 2`) to
make caps reachable. Use this whenever the change alters what the model is offered or how
the loop reacts — the teatest harness fakes the agent at the Completer seam, so it proves
keystroke→transcript wiring but structurally cannot reach spawn wiring, budgets, strips,
or anything `cmd/fuse` composes around the real agent. The gateway can also *force* a tool
call the model was never offered — the direct probe for advisory-strip-vs-backstop layers.
Two capture disciplines: dump the FULL pane before claiming any UI element is absent
(popups compose mid-screen; a `head -16` of a 40-row pane reads as "missing"), and treat UI
state keyed on a resource-creation event as suspect when a guard can reject the creation
before the event ever fires.

## War story

- 2026-08-06 (#33, PR #19) — Unit tests were green, but live gateway-seam verification found
  a real bug: a budget-backstop-rejected `spawn_agent` left its transcript block frozen at
  `● Running · 0s` forever — rejection fires before node creation, so the node lifecycle
  that settles the block never ran and the error result was deliberately skipped. Fixed in
  the same PR (`e47bd27`). The same session also produced a false blocker the same technique
  retracted: "approval popup invisible from the agents tab" was an artifact of capturing only
  the top 16 rows of a 40-row pane; full-pane capture showed the popup composing correctly at
  rows 31–37. One live rig, one real bug found, one fabricated bug avoided.
