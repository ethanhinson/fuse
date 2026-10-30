---
id: 85
slug: 'start-a-loop-from-a-harnessspec'
title: 'Start a loop from a HarnessSpec'
status: 'in-progress'
priority: 'high'
type: 'feat'
deliverable: 'pr'
created: '2026-10-30'
updated: '2026-10-30'
depends_on: []
stacked_on:
related: [84]
discovered_from: []
adrs: [6, 60]
spec: 'docs/superpowers/specs/2026-10-30-start-a-loop-from-a-harnessspec-design.md'
plan:
results:
auto_groomable:
hold:
branch_prefix:
branch: 'feat/start-a-loop-from-a-harnessspec'
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-10-30-start-a-loop-from-a-harnessspec-design.md](../../superpowers/specs/2026-10-30-start-a-loop-from-a-harnessspec-design.md) |
| ADRs | [ADR-0006](../../adrs/0006-fuse-local-yml-tighten-only-trust-boundary.md), [ADR-0060](../../adrs/0060-harnessspec-proto-is-the-portable-harness-definition-client.md) |
<!-- docket:artifacts:end -->

## Why

Change 0084 (ADR-0060) made fuse.harness.v1 HarnessSpec the portable, validated definition of a harness, but nothing runs one. Every loop a fuse server starts is built from the one process-wide config the operator loaded at startup, so on one networked fuse server shared by many tenants and clients, every client gets the same models, skills, connectors and brakes, and cannot start a loop with its own harness. 0084 also left SkillRef carried but unused.

## What changes

- StartLoop carries an optional HarnessSpec on every binding: Connect (fuse.loop.v1 StartLoopRequest.harness), stdio loop.start, in-process runtime.LoopConfig, the Go SDK and the TS SDK. Absent = byte-identical behavior.
- The server's trusted config is the floor: the spec is layered over it through the loader's own merge, tighten_only fields may only tighten the operator's value (the existing ADR-0006 rule), operator-only surfaces always come from the server.
- The spec is validated before anything is created; an invalid spec returns InvalidArgument with structured field-path violations in the error details.
- The loop's agent, subagents, workflows and pools are built from the resolved per-loop config through a per-loop factory on the runtime seam; the CLI/TUI paths are unchanged.
- The spec and its content digest are recorded in the loop's durable, tenant-scoped event stream; resume on any replica rebuilds the same harness.
- SkillRef names resolve against the server's installed skills (unknown = violation) and scope the loop's skill set.
- secret_ref fields resolve through a SecretResolver; one operator-configured env/file allowlist implementation ships, deny by default, values never enter events, logs or errors.

## Out of scope

- Fetching specs by reference or revision id from a remote registry.
- stdio MCP servers in specs.
- Per-tenant model gateways.
- Mounting HarnessService on loop-serve-net.
