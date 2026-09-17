---
slug: closed-label-allowlist-silently-swallows-enum-growth
hook: "A metrics label-cardinality allowlist is a closed map keyed by enum value: widen the enum anywhere in the codebase and the new value collapses to `__overflow__` with no compile error and no test failure. The dashboards go empty while the events are plainly present in the stream — a silently broken deployment, not a build break."
topics: [observability, prometheus, metrics, enums, go, coupling, silent-failure]
changes: [75]
created: 2026-09-17
updated: 2026-09-17
promotion_state: candidate
promoted_to:
---

## Apply

Bounding metric label cardinality with an allowlist is correct — an unbounded label from user or
tenant input is how a Prometheus deployment dies. The defect is the **coupling shape**: the
allowlist is a map literal in the observability package, and the enum it mirrors lives in some
other package that no one editing it thinks about.

Change 0075 added a handler kind (`kubernetes`), a release cause (`orphan`) and a health reason
(`floor_unverified`). Each would have rendered as `__overflow__`. Nothing failed: the code compiles,
the events carry the right values end to end, and the only symptom is a dashboard panel that stays
at zero while the operator can see the events in the stream. That is strictly worse than a crash,
because it is discovered during an incident.

**How to apply.** Treat a closed allowlist as a **derived view of an enum**, and make the
derivation enforceable rather than remembered:

- Prefer generating the allowlist from the enum's own canonical value list, so widening the enum
  widens the allowlist by construction.
- If it must be hand-maintained, write one guard test per enum that enumerates the source-of-truth
  values and asserts every one is admitted — so growing the enum reddens the suite at the point of
  growth, not in production.
- Make that guard **generic** over the closed maps in the package, not one test per enum: the
  failure mode recurs on every future enum, and a per-enum test only protects the enums somebody
  already thought about.
- Where a value genuinely must overflow, assert that too, so `__overflow__` is an intended outcome
  somewhere rather than the default for anything unrecognised.
