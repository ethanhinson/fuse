---
id: 59
slug: mcp-full-binding-wiring-e2e-tested
title: Wire MCP into every loop binding and prove it end-to-end — no more features on untestable paths
status: proposed
priority: critical
type: feat
created: 2026-08-11
updated: 2026-08-11
depends_on: [52]
related: [48, 49, 55, 57, 58]
discovered_from: [52]
adrs: []
spec:
plan:
results:
trivial: false
auto_groomable:
branch:
pr:
blocked_by:
reconciled: false
---

## Why

MCP tool execution is attached to **exactly one** loop binding: the interactive **shell** (`cmd/fuse/shell.go` → `tui.NewMCPProvider` → `mcp.NewManager`, with `mcp.WithCredentialSource` wired). Every other binding is blind to MCP:

- **`loop-server` (`cmd/fuse/loop_server.go`)** — the networked, multi-tenant, fuse-as-a-service binding — **never calls `mcp.NewManager`**. A hosted loop cannot invoke an MCP tool at all.
- **one-shot / probe (`cmd/fuse/run.go`)** — no MCP.
- **`fuse mcps` (list/tools/logs)** — attaches a manager for *introspection only*, with **no** credential source (no tool execution).

The consequence is the thing we can no longer accept: **change #52's per-call identity propagation (RFC 8693 delegation, per-tenant tokens, complete-mediation) has never been exercised end-to-end through the binding it exists for.** #52's multi-tenant acceptance was proven one layer down — at the egress seam (real Broker + STS minting distinct-per-principal tokens) — because a loop-server → MCP → downstream path **does not exist in the codebase**. We shipped the engine and bench-tested it; it was never dropped into the car that is the product.

This is a **critical** correctness-and-process gap. We keep stacking service features (identity #52, HTTP-tool identity #57, sessions #54, SDK #56) on top of a runtime whose primary binding cannot actually run the tool subsystem those features secure. **We will not add further features whose real-world scenario cannot be tested through a deployed binding.** This change closes that gap and makes the end-to-end path the permanent, CI-enforced proof.

## What changes (proposal altitude — design in brainstorm)

Wire MCP tool execution into **every** loop binding that should have it — first-class on the **loop-server** — and prove the full identity-propagation flow end-to-end against a **real MCP server** in CI. The deliverable is "MCP works, with #52 identity, through the actual service binding, and a test kills it if it regresses."

Likely scope to settle in design:

- **Loop-server MCP attach.** Give `buildLoopServerRuntimeDeps` (binding #2) an MCP manager + `WithCredentialSource`, honoring the loop-start principal (per-loop, per-tenant) rather than the shell's single local `DefaultTenant`. This is where #52's multi-tenant STS finally has a real principal to mint for — it likely retires the `TODO(#52-followup)` single-tenant shim.
- **Principal threading over the wire.** The authenticated loop-start identity (#49, arriving over #48/#55's transport) must reach `MCPTool.Execute`'s `CredentialFor` call on the loop-server path — the context carrier exists (`toolidentity.WithPrincipal`); wire it from the binding, not a seeded local principal.
- **One-shot / probe policy.** Decide whether one-shot attaches MCP (with what identity) or is explicitly, loudly MCP-less.
- **Shared attach helper.** Factor the shell's attach-with-credential-source into one helper all bindings use, so a binding cannot silently ship without it again.
- **End-to-end acceptance as a permanent CI lane.** Two loops started by *different* principals, on the **loop-server**, each calling the **same real MCP server** (a scripted test server, not a mock of our own client), present **distinct, audience-bound, delegated tokens** (user in `sub`, fuse in `act`); the server adjudicates (403 for the unauthorized principal surfaces as a distinguishable tool error); wrong-audience is rejected; the complete-mediation gate denies an undeclared target through the loop-server path; and no credential leaks into the event stream / durable store / logs. This is the #52 verification checklist, finally run **through the binding** and pinned in CI. Loud on toolchain absence; scripted `LLM_GATEWAY_URL` double, never Claude/Anthropic.

## Out of scope

- **Extending identity to non-MCP tools** — `web_fetch`/`web_search` is #57; `bash` containment is #58. This change is specifically MCP-through-every-binding.
- **New MCP protocol features / transports** — this is wiring + proof of what exists, not new MCP surface.
- **Re-designing #52's token exchange** — settled; this change consumes the seam and drives it through the real bindings.

## Note

Filed 2026-08-11 immediately after merging #52 (PR #55), at the human's explicit direction: **stop adding features we cannot test with real-world scenarios.** #52 is sound for its declared scope, but its multi-tenant path was only proven at the seam because no binding wires MCP end-to-end. Marked **critical** and treated as a gate on further service-feature work. Needs a brainstorm to settle the loop-server attach + principal-threading design and the CI harness before it is build-ready — but it should jump the queue.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
