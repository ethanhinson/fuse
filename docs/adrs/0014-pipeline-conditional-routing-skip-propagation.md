---
id: 14
slug: pipeline-conditional-routing-skip-propagation
title: Pipeline conditional-routing execution semantics (skip-propagation join)
status: Accepted
date: 2026-08-08
supersedes: []
reverses: []
relates_to: []
change: 26
---

## Context

Change 0026 (agent-workflow-composition / pipeline composition) adds a deterministic
DAG pipeline engine (`internal/pipeline`) that runs steps readiness-first over the
subagent spawn seam: a step becomes schedulable once its `depends_on` predecessors
have completed. On top of that readiness model, steps may declare structured
`conditions` — each `{if: {key, op, value}, goto: <step>}` — plus a `default`
target, expressing conditional routing ("if scan clean → synth, else → repair").

The open question was how routing interacts with a readiness-driven engine. Two
outcomes are irreconcilable: either `goto`/`default` genuinely *steer execution*
(some steps run, others do not, based on a router's outcome), or they are validated
and advertised in the tool/synthesizer schema but do nothing at runtime while the
plain `depends_on` DAG executes unchanged. The change's own title promises real
conditional branching, so the semantics had to be pinned down precisely — including
what happens at a join, and what happens when a condition cannot be evaluated
(type mismatch, missing key, invalid regex).

## Decision

**Routing affects execution — it is NOT inert.** A step that declares
`conditions`/`default` acts as a *router*: on its success, exactly one target is
chosen — the first matching condition's `goto`, else `default` — and that target is
*released* for scheduling while the router's other routing targets are *skipped*.

- A **branch-gated** step runs only when released. It is skipped once every router
  that targets it has decided without choosing it. A skipped step produces no
  outputs, so it **propagates the skip** to its `depends_on` downstream: a skipped
  step's dependents skip too (branch-not-taken carries its whole downstream).
- **Non-routed** steps keep pure `depends_on` readiness scheduling. A plain DAG with
  no `conditions` anywhere is therefore unaffected by this feature.
- **Routing is TOTAL.** A type mismatch, a missing `key`, or an invalid regex makes
  the condition simply *false* — never an error, never a panic. The operator set is
  fixed (no expression language), so evaluation is closed and deterministic.
- **v1 join semantics are deliberately conservative:** a step is skipped if **ANY**
  of its `depends_on` was skipped. This is the "branch-not-taken carries its whole
  downstream" rule stated at the join.

## Consequences

**Enables** the real conditional branching the change's title promises — e.g.
"if scan clean → synth, else → repair" — with deterministic, total, panic-free
routing and **no expression-language dependency** (a fixed operator set only, so
nothing to parse, sandbox, or attack). A plain DAG is a strict subset: adding no
conditions changes nothing.

**Costs / given up:** the conservative *ANY-skipped ⇒ skip* join means a diamond
that re-joins after a branch is **not expressible in v1** — a documented limitation.
A future "join-after-branch" that runs when ANY (rather than all) dependency
*completed* would need richer join semantics; it is explicitly **deferred** and
carried as a follow-up if a real need appears.

**Alternative rejected:** leaving `goto`/`default` validated-but-inert — advertised
in the tool/synthesizer schema yet doing nothing at runtime. Rejected as dishonest:
the model would be instructed to emit routing that never fires, making the schema a
lie the synthesizer would faithfully reproduce.
