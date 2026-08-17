---
slug: spec-asserted-error-code-verify-emitter-first
hook: "When a spec keys client/UI behavior on a SPECIFIC error code or terminal condition (\"on failed_precondition show paused\"), verify the emitting code actually produces it BEFORE building the affordance — enumerate the emitter's real error returns. If the code turns out to be unemitted-but-documented (an ADR lists it as possible), keep the handling branch and relabel it defensive rather than deleting it: deletion silently reroutes a future emission into the wrong arm."
topics: [spec-drift, error-handling, review, defensive-code, build-loop]
changes: [62]
created: 2026-08-17
updated: 2026-08-17
promotion_state: retained
promoted_to:
---

## Apply

Specs written at design time freely assert *which* error a server surfaces for a given condition —
and the assertion can be pure fiction. Before building an affordance keyed on a named error code,
open the emitting code and enumerate its actual error returns. In #62 the spec's "session paused"
UI was keyed on a terminal `failed_precondition` that `internal/loopconnect/observe.go` never
emits (a reap is a *clean* stream end: channel closes, handler returns nil, the SDK reconnects and
transparently resumes) — the whole affordance was unreachable, and review caught it only by
independently verifying the emitter.

Disposition matters on the way out too. If the asserted code is documented as *possible* (ADR-0037
lists `failed_precondition` in the SDK's terminal set) but currently unemitted, do NOT delete the
handling branch — relabel it defensive with a comment naming why. Deleting it means a future server
change that starts emitting the code routes into the generic fallback arm and silently destroys
the state the branch existed to protect.

## War story

- 2026-08-17 (#62, PR #73): D2's "session paused — reload to resume" UI was spec'd against a
  `failed_precondition` the server has no code path to emit; a worker enumerated observe.go's four
  error returns to prove it. Outcome D2 cared about (reap ≠ data loss) held anyway — via clean
  stream end + transparent Resume — so the README was rewritten to the real behavior and the dead
  branch was kept-and-relabelled as defensive per ADR-0037.
