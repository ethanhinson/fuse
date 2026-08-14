---
slug: patch-every-cloned-child-builder
hook: "cmd/fuse wires child-agent tool registries in three cloned builders (main.go one-shot run(), shell.go, research_probe.go) — a fix to child tool wiring must land in all three; enumerate sites by grep at fix time, never from a prior scoping list"
topics: [go, refactoring, subagents, agents]
changes: [34, 61]
created: 2026-08-06
updated: 2026-08-14
promotion_state: candidate
---

## Apply

When a fix targets behavior that lives in duplicated/cloned wiring, derive the site list by
grepping for the pattern itself at fix time (e.g. the `spawn_agent` re-registration call) —
never from a plan, reconcile log, or any earlier enumeration. A prior list freezes its own
blind spots: it looks authoritative precisely because someone already did the scoping, and
review of the diff alone cannot see a site the diff never touched.

In `cmd/fuse` specifically, child-agent tool registries are built in three clones — `main.go`'s
one-shot `run()` builder, `shell.go`, and `research_probe.go`. Any change to child tool
registration, spawn wiring, or depth/quota guards must be verified against all three (and the
count re-checked by grep, in case a fourth has appeared).

## War story

- 2026-08-06 (#34, PR #20) — The "honor a `tools` subset that omits `spawn_agent`" fix was
  scoped by the reconcile log to `shell.go` + `research_probe.go` and initially shipped that
  way; `main.go`'s one-shot `run()` builder — the default production path for `fuse "<task>"`
  — still unconditionally re-registered `spawn_agent`, so a parent could not withhold spawning
  from a child on that path (spec Acceptance 3 violated). Caught in review; fixed by applying
  the same `childNode.Depth >= agent.MaxDepth || !shouldWireChildSpawn(opts.Tools)` guard as
  the other two sites.
- 2026-08-14 (#61, PR #59) — The same three-clone shape re-appeared for observability wiring:
  the observer had to be constructed once and published into `runtime.Deps` at all three local
  entry points (`shell`, one-shot `run()`, research probe), and the teardown block was
  triplicated verbatim across them before review collapsed it into a shared
  `setupLocalObservability` helper. Confirms the rule beyond child tool registration: *any*
  cross-cutting collaborator in `cmd/fuse` must be enumerated by grep across the clones, and the
  duplication is worth factoring out at the moment you touch all three.
