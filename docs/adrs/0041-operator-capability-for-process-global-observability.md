---
id: 41
slug: operator-capability-for-process-global-observability
title: Process-global observability mutations require an explicit operator capability
status: Accepted
date: 2026-08-12
supersedes: []
reverses: []
relates_to: [34, 40]
change: 51
---

## Context

Fuse serves multiple authenticated tenants from one process. Change #51 adds live observability
controls, including logging configuration reload and reopening a shared file sink after
host/container-managed log rotation. Those operations mutate process-global state: one request
can otherwise change the telemetry behavior or output destination for every tenant running in
that process.

An ordinary authenticated tenant identity establishes ownership for tenant and loop resources,
but it is not authority to administer shared host-level runtime state. Treating possession of any
valid tenant credential as sufficient for a global observability mutation would let one tenant
alter another tenant's diagnostics and interfere with operator-managed telemetry.

## Decision

Process-global observability mutations require an explicit operator capability that is distinct
from ordinary authenticated tenant identity. Logging reload and shared file-sink reopen are
operator-only controls and must verify that capability before they change global state.

Tenant principals may manage only observability overrides that are explicitly authorized for
their own tenant or loop scope. Those requests must remain ownership-checked and must not provide
a path to alter process-wide logging configuration, shared sinks, or other tenants' telemetry.

## Consequences

- A tenant credential alone cannot reconfigure or disrupt process-wide telemetry for every
  tenant; deployment owners retain control over global logging and rotation operations.
- Control surfaces must carry and enforce a separate operator authorization decision for global
  mutations, rather than inferring it from successful tenant authentication.
- Tenant- and loop-scoped overrides remain available for local debugging and signal control when
  the caller is authorized for that scope.
- Implementations and tests must distinguish global mutations from scoped overrides and prove
  that tenant authentication cannot escalate into operator actions.
