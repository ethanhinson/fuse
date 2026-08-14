---
slug: runtime-deps-field-overwrites-builder-injection
hook: "Injecting a collaborator inside the agent builder is not enough when the runtime also carries it as a Deps field — the runtime re-sets it on the built agent, so a seam test that calls Deps.BuildAgent directly proves nothing about the real StartLoop path"
topics: [go, observability, dependency-injection, runtime, testing]
changes: [61]
created: 2026-08-14
updated: 2026-08-14
promotion_state: candidate
---

## Apply

When you thread a collaborator (an observer, a logger, a store) into agents by adding a
parameter to the builder and calling `a.SetX(...)`, first grep the runtime for a `Deps` field
of the same type. If one exists, the runtime is the **later writer**: it assigns the field onto
the agent after `BuildAgent` returns, so a builder-side injection is silently reverted the
moment the runtime drives the loop. Fix both — pass the collaborator into `runtime.Deps` at
every binding site *and* into the builder.

The test shape matters as much as the fix. A test that calls `Deps.BuildAgent` directly
observes only the builder's injection and stays green while production is reset to the no-op
default. Keep both shapes deliberately: the seam test for the builder contract, and a test that
goes through `runtime.New`/`StartLoop` for the field the runtime actually honors.

Generalization: the last writer on a shared object wins, and a per-instance injection point is
not authoritative just because it is the one you edited. Enumerate every writer of the field
before believing an injection landed.

## War story

- 2026-08-14 (#61, PR #59) — Wiring observability into `fuse shell`, one-shot `fuse <task>`, and
  the research probe threaded an `observe.Observer` through `buildAgentCore` /
  `buildAgentWithRendererAndTrace` and called `a.SetObserver(observer)`. Every local root agent
  still emitted nothing: `internal/runtime/inproc.go` (`:306`, `:596`) overwrites the agent's
  observer from `runtime.Deps.Observer`, which the local bindings left unset, so the runtime
  reset each agent to `NoopObserver{}` as soon as it drove the loop. The binding tests bypassed
  `runtime.New`/`StartLoop` and could not see the overwrite they existed to guard. Fixed by
  publishing `Observer: observer` into all three `runtime.Deps` and adding StartLoop-level
  coverage alongside the retained seam tests.
