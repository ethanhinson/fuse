---
id: 59
slug: mcp-full-binding-wiring-e2e-tested
title: Wire MCP into every loop binding and prove it end-to-end — no more features on untestable paths
status: done
priority: critical
type: feat
created: 2026-08-11
updated: 2026-08-11
depends_on: [52]
related: [48, 49, 55, 57, 58, 60]
discovered_from: [52]
adrs: [36]
spec: docs/superpowers/specs/2026-08-11-mcp-full-binding-wiring-e2e-tested-design.md
plan: docs/superpowers/plans/2026-08-11-mcp-full-binding-wiring-e2e-tested-plan.md
results: docs/results/2026-08-11-mcp-full-binding-wiring-e2e-tested-results.md
trivial: false
auto_groomable:
branch: feat/mcp-full-binding-wiring-e2e-tested
claimed_at: 
pr: https://github.com/ethanhinson/fuse/pull/56
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-11-mcp-full-binding-wiring-e2e-tested-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-11-mcp-full-binding-wiring-e2e-tested-design.md) |
| Plan | [2026-08-11-mcp-full-binding-wiring-e2e-tested-plan.md](https://github.com/ethanhinson/fuse/blob/main/docs/superpowers/plans/2026-08-11-mcp-full-binding-wiring-e2e-tested-plan.md) |
| Results | [2026-08-11-mcp-full-binding-wiring-e2e-tested-results.md](https://github.com/ethanhinson/fuse/blob/main/docs/results/2026-08-11-mcp-full-binding-wiring-e2e-tested-results.md) |
| PR | [#56](https://github.com/ethanhinson/fuse/pull/56) |
| ADRs | [ADR-0036](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0036-tool-authz-delegated-downstream-rfc8693-egress-seam.md) |
<!-- docket:artifacts:end -->

## Why

MCP tool execution is attached to **exactly one** loop binding: the interactive **shell** (`cmd/fuse/shell.go` → `tui.NewMCPProvider` → `mcp.NewManager`, with `mcp.WithCredentialSource` wired). Every other binding is blind to MCP:

- **`loop-server` (`cmd/fuse/loop_server.go`)** — the networked, multi-tenant, fuse-as-a-service binding — **never calls `mcp.NewManager`**. A hosted loop cannot invoke an MCP tool at all.
- **one-shot / probe (`cmd/fuse/run.go`)** — no MCP.
- **`fuse mcps` (list/tools/logs)** — attaches a manager for *introspection only*, with **no** credential source (no tool execution).

The consequence is the thing we can no longer accept: **change #52's per-call identity propagation (RFC 8693 delegation, per-tenant tokens, complete-mediation) has never been exercised end-to-end through the binding it exists for.** #52's multi-tenant acceptance was proven one layer down — at the egress seam (real Broker + STS minting distinct-per-principal tokens) — because a loop-server → MCP → downstream path **does not exist in the codebase**. We shipped the engine and bench-tested it; it was never dropped into the car that is the product.

This is a **critical** correctness-and-process gap. We keep stacking service features (identity #52, HTTP-tool identity #57, sessions #54, SDK #56) on top of a runtime whose primary binding cannot actually run the tool subsystem those features secure. **We will not add further features whose real-world scenario cannot be tested through a deployed binding.** This change closes that gap and makes the end-to-end path the permanent, CI-enforced proof.

## What changes

Wire MCP tool execution into **every** loop binding — first-class on the **loop-server** — through **one shared attach helper**, and prove the full identity-propagation flow end-to-end against a **real MCP server** in a permanent CI lane. The deliverable is "MCP works, with #52 identity, through the actual service binding, and a test kills it if it regresses." Design settled in the linked spec; the load-bearing decisions:

- **One shared attach helper** in the composition root yields the credential-source options **and** the complete-mediation option together, so no binding can wire an MCP manager without also wiring #52's egress seam and `TargetMediator`. Shell is refactored onto it (behavior-preserving); one-shot and loop-server adopt it.
- **Loop-server MCP attach + real principal.** `buildLoopServerRuntimeDeps` (binding #2/#3) constructs a **per-loop** MCP manager (tools registered into each loop's own registry — no cross-loop bleed) and threads the **real authenticated loop-start principal** (from #49's verifier) to `MCPTool.Execute` via the existing `toolidentity.WithPrincipal` carrier. This retires the `DefaultTenant` single-tenant shim on the loop-server path by construction — #52's per-tenant STS finally mints for a real principal.
- **One-shot** attaches MCP via the shared helper, seeding the same local `localPrincipal` → `DefaultTenant` the shell uses. **No new CLI identity flag** (explicit non-goal — loop-server is the multi-tenant path).
- **Shell / one-shot keep `DefaultTenant`** — they are local single-user CLI paths; the shim survives only as the value they supply to the helper, not a special path.
- **A real, stateful Wander rentals MCP server** as the lane's backend — grounded in Wander's own domain (`examples/concierge-demo`), not a contrived fixture. It owns a **per-principal favorites store** and a **mutating** `favorite_listing` write (plus `search_rentals` / `list_favorites`), so the acceptance verifies identity on a real *write* path — the downstream acts on *the caller's* data, isolated per tenant — not just a read-only 403. Real MCP wire + real per-principal token adjudication + real per-principal state; only the listing *data* is canned (behind a seam) to keep the lane hermetic. The live-Tavily backend + demo wiring is follow-up **#60**.
- **Permanent CI acceptance lane** — the **full** #52 verification checklist, run **through the loop-server binding**: two loops started by *different* principals, each calling the **same real rentals MCP server**, present **distinct, audience-bound, delegated tokens** (user in `sub`, fuse in `act`); the server adjudicates (403 for the unauthorized principal surfaces as a distinguishable tool error); **A's `favorite_listing` write is invisible to B's `list_favorites`** (per-principal write isolation — the confused-deputy property, on a mutating path); wrong-audience is rejected; complete-mediation denies an undeclared target through the loop-server path (even under `AlwaysApprove`); and no credential leaks into the event stream / durable store / logs. Loops driven concurrently; loud on toolchain absence; scripted `LLM_GATEWAY_URL` double, never Claude/Anthropic.

## Out of scope

- **Extending identity to non-MCP tools** — `web_fetch`/`web_search` is #57; `bash` containment is #58. This change is specifically MCP-through-every-binding.
- **New MCP protocol features / transports** — this is wiring + proof of what exists, not new MCP surface.
- **Re-designing #52's token exchange** — settled; this change consumes the seam and drives it through the real bindings.

## Note

Filed 2026-08-11 immediately after merging #52 (PR #55), at the human's explicit direction: **stop adding features we cannot test with real-world scenarios.** #52 is sound for its declared scope, but its multi-tenant path was only proven at the seam because no binding wires MCP end-to-end. Marked **critical** and treated as a gate on further service-feature work. Groomed 2026-08-11 (spec linked); build-ready and jumps the queue.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->

### 2026-08-11 — reconcile (docket-implement-next)

Verified the spec against the **current integration tip** `origin/main` (`fab812c`, PR #55 / #52 merged). The dependency `#52` is `done`; ADR-0036 (the egress seam this change consumes) is `Accepted`; cited ADR-0034/0028/0029 present. **No scope change, no obsolescence, no new constraints** — the spec is accurate as written and NOT invalidated.

Load-bearing spec claims re-confirmed against `origin/main` (feature branch cuts from here):

- `cmd/fuse/shell.go:77-84` — the inline MCP-attach block (`mcpOpts` / `buildToolIdentitySource` / `mcp.WithCredentialSource` / `logToolIdentityPosture` / `tui.NewMCPProvider`) is exactly as the spec quotes; shell also does `WithToolPrincipal(localPrincipal(cfg))` (`shell.go:265`). This is the block to factor into the shared `mcpAttach` helper.
- `cmd/fuse/loop_server.go` — `buildLoopServerRuntimeDeps` (:86) builds per-loop `NewToolRegistry`/`BuildAgent` closures and `cloneServerToolRegistry` (:225) with **zero MCP references** — the exact gap this change closes. Change 0046's per-loop isolation (fresh tree/store/registry per loop) is present as the spec assumes.
- `cmd/fuse/loop_serve_net.go` — `buildLoopVerifier` (:48) maps bearer token → `loopauth.Principal{Tenant, Subject}` at the Connect edge (dev token → `event.DefaultTenant`); the authenticated principal exists but is never threaded into the tool-call context, as the spec states.
- `cmd/fuse/run.go` — 0 MCP references (one-shot is MCP-blind, per spec §3).
- `cmd/fuse/tool_identity.go` — `buildTargetMediator` (:78), `buildToolIdentitySource` (:121), `localPrincipal` (:214 → `loopauth.Principal{Tenant: event.DefaultTenant}`) all present; the STS signing-key map is keyed **single-tenant on `event.DefaultTenant`** (:173) — precisely the shim §2 generalizes to per-loop tenant.
- `internal/toolidentity/` (`context.go` carrier `WithPrincipal`/`PrincipalFrom`, `broker.go`, `exchange.go`, `seam.go`) and `internal/loopauth/verifier.go` all present.

Follow-up already captured: the live (Tavily-style) rentals data backend + Wander demo wiring is out of scope here and is filed as **#60** (`related`) — no new stub minted (auto-capture disabled; and it already exists).

Reconcile caution recorded for the build: a broad code-verification sweep initially read the **local checked-out worktree** (HEAD `22480d5`, which predates the #52 merge) and wrongly reported the whole `toolidentity`/`loopauth`/`loop_serve_net` surface as absent. It is present on `origin/main`. Build agents MUST resolve code claims against `origin/main`, not the ambient local `main` — the local `main` here is behind origin.

### 2026-08-11 — reconcile (docket-implement-next)

Reconciled against current `origin/main` (tip `fab812c`, the merge of PR #55 = change #52
tool-identity-propagation), cited/recent ADRs, and the `related` changes. **Verdict: build-ready
as specified, no scope change.** The spec was groomed against merged #52 on `origin/main` and
remains accurate to it.

- **#52 infrastructure confirmed present on the branch base (`origin/main`)** — the whole engine the
  spec assumes exists and matches the spec's quotes:
  - `internal/toolidentity/` (`seam.go` CredentialSource, `context.go` `WithPrincipal`/`PrincipalFrom`,
    `exchange.go` built-in HS256 STS, `broker.go` credential Broker).
  - `internal/loopauth/verifier.go` (#49 `Verifier` → `loopauth.Principal{Tenant, Subject}`).
  - `cmd/fuse/loop_serve_net.go` (`buildLoopVerifier`, the Connect auth edge) and
    `cmd/fuse/loop_server.go` (`buildLoopServerRuntimeDeps`, per-loop isolation from #46 —
    still MCP-blind, exactly the gap this change closes).
  - `cmd/fuse/shell.go:77-84` — the inline `mcpOpts` + `buildToolIdentitySource(cfg)` +
    `mcp.WithCredentialSource` + `logToolIdentityPosture` block the shared attach helper refactors.
  - `permissions` `TargetMediator` seam (#52 Task 5) and complete-mediation wiring (round-2 fix).
- **Note on tooling:** an initial code-scan read the repo's *stale working-tree files* (a `main`
  checkout predating the #52/#55 merge, so `toolidentity/` was absent on disk) and wrongly reported
  the infrastructure missing. Re-verified directly against `origin/main` via `git show`/`git ls-tree` —
  everything is present. The feature branch cuts from `origin/main`, so the base is correct.
- **`related` movement, no impact on scope:** #50 client-sdk (PR #54) merged; #56
  sdk-viability-hardening is now build-ready behind #59 in the queue; #60 already captures the
  live-Tavily backend follow-up the spec defers. No new constraint folds in.
- **ADRs:** consumes ADR-0036 (delegated tool authz egress seam) + ADR-0034 (edge-enforced tenancy);
  ADR-0036's own "Costs/limits" names the shell-only wiring + single-tenant `DefaultTenant` STS with a
  follow-up TODO — precisely this change. Likely no new ADR (spec §ADRs); will record at build only if
  the principal-threading seam or the shared-helper boundary encodes a durable decision.
- Auto-capture disabled (`AUTO_CAPTURE_ENABLED=false`) — no stubs minted; nothing material surfaced
  beyond what #60 already captures.
