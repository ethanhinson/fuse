---
name: gate-as-tool-executor
title: Implement middleware gates as ToolExecutor adapters, not Agent loop patches
promotion_state: retained
changes: [3]
created: 2026-08-04
updated: 2026-08-04
topics: [architecture, interfaces, middleware, testing]
---

Implement a permission/rate-limit/audit gate by making it satisfy the same `ToolExecutor` interface as the registry it wraps. The agent loop calls `gate.Execute()` identically to `registry.Execute()` — no agent struct changes, no loop modification, no new field. The gate delegates to the inner registry after policy resolution.

**Why:** The alternative (adding a `gate` field to `Agent` and branching inside `Run()`) couples policy to the loop, forces every `Agent` to carry gate wiring even in tests, and splits the concern across two types. An adapter keeps the gate independently testable (inject a stub registry) and composable (stack gates). Discovered during change 0003 (HITL permission gate).

**How to apply:** Any future cross-cutting tool concern (audit log, rate limiting, quota, sandboxing) follows the same shape: implement the `ToolExecutor` interface, wrap the inner registry, and inject at construction time. Thread any new constructor parameters (e.g. an `ApprovalFunc`) through `AgentBuilder` so tests receive the same type signature as production callers — stubs just ignore the extra param.
