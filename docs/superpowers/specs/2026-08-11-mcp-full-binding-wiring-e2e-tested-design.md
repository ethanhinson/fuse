<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0059 — Wire MCP into every loop binding and prove it end-to-end — no more features on untestable paths](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0059-mcp-full-binding-wiring-e2e-tested.md)**
<!-- docket:backlink:end -->

# Spec 0059 — Wire MCP into every loop binding and prove it end-to-end

## Problem

MCP tool execution is attached to **exactly one** loop binding: the interactive shell
(`cmd/fuse/shell.go` → `tui.NewMCPProvider` → `mcp.NewManager`, with
`mcp.WithCredentialSource` wired from `buildToolIdentitySource`). Every other binding is
blind to MCP:

- **`loop-server` / `loop-serve-net` (`cmd/fuse/loop_server.go`, `loop_serve_net.go`)** —
  the networked, multi-tenant, fuse-as-a-service binding (#2/#3) — `buildLoopServerRuntimeDeps`
  **never constructs an `mcp.NewManager`** and never registers MCP tools into the per-loop
  tool registry (`cloneServerToolRegistry`). A hosted loop cannot invoke an MCP tool at all.
- **one-shot / probe (`cmd/fuse/run.go`)** — no MCP.
- **`fuse mcps` (list/tools/logs)** — attaches a manager for *introspection only*, with **no**
  credential source (no tool execution). Out of scope here (it is not a loop binding).

MCP tools appearing to every loop is table stakes for an agent runtime — a binding that
cannot run them is a defect, not a policy choice.

The consequence we can no longer accept: **change #52's per-call identity propagation
(RFC 8693 delegation, per-tenant tokens, complete-mediation) has never been exercised
end-to-end through the binding it exists for.** #52 built the whole machine —
`internal/toolidentity` (`CredentialSource` seam, `Principal`, delegation via a built-in
HS256 STS, a credential `Broker`) and a `TargetMediator` on `internal/permissions` — but,
per ADR-0036's own "Costs/limits," wired it into **the single-user shell path only**, minting
for one hard-coded identity (`event.DefaultTenant`, via `localPrincipal`). #52's multi-tenant
acceptance was therefore proven **one layer down** — at the egress seam (real Broker + STS
minting distinct-per-principal tokens) — because a *loop-server → MCP → downstream* path does
not exist in the codebase. We shipped the engine and bench-tested it; it was never dropped
into the car that is the product.

This is a **critical** correctness-and-process gap and a **gate on further service-feature
work** (#54 sessions, #56 SDK viability, #57 HTTP-tool identity). It is filed at the human's
explicit direction: *stop adding features we cannot test with real-world scenarios.*

## Settled decisions (do not re-litigate at build)

Grooming settled these against the merged #52 code on `origin/main`:

1. **MCP attaches to every loop binding**, routed through **one shared attach helper** — no
   binding may silently ship MCP-less again. This is the structural spine.
2. **The `DefaultTenant` shim retires on the loop-server path by construction** — the manager
   there mints for the **real authenticated loop-start principal** (from #49's verifier), not
   `DefaultTenant`. On the shell and one-shot paths the shim *stays* (those are local
   single-user CLI paths with no authenticated user) — it survives only as the *value each of
   those bindings supplies to the shared helper*, not as a special path.
3. **One-shot mints for the same local `localPrincipal(cfg)` → `DefaultTenant` the shell uses**,
   via the shared helper. **No new CLI identity flag** — a `--tenant`/`--principal` surface is an
   explicit non-goal (YAGNI; loop-server already provides real per-principal identity, which is
   the honest place to exercise multi-identity).
4. **The permanent CI lane asserts the full #52 verification checklist through the loop-server
   binding** (see Acceptance). Not a subset.

## Design

### 1. Shared attach helper (the spine)

Factor the shell's attach-with-credential-source into a single helper every loop binding calls.
Today `shell.go` does, inline:

```go
mcpOpts := []mcp.ManagerOption{}
if src, reason := buildToolIdentitySource(cfg); src != nil {
    mcpOpts = append(mcpOpts, mcp.WithCredentialSource(src))
}
// ... logToolIdentityPosture(...) ...
mcpProv, err := tui.NewMCPProvider(config.Path(), cfg, toolReg, mcpOpts...)
```

Introduce a helper in `cmd/fuse` (composition root) that yields the resolved
`[]mcp.ManagerOption` (credential source when configured) **plus** the target mediator option
and the posture log, so a binding cannot wire the manager without also wiring complete
mediation. Shape (names to firm up at build):

```go
// mcpAttach resolves the identity-propagation options every binding must apply when it
// constructs an mcp.NewManager: the CredentialSource egress seam (when configured) and the
// posture log. Returns the options plus the TargetMediator permissions.Option so the caller
// wires complete mediation on the same gate. The single choke point — no binding attaches
// MCP without it.
func mcpAttach(cfg config.Config, w io.Writer) (mcpOpts []mcp.ManagerOption, mediator permissions.Option)
```

- Shell (`shell.go`) is refactored onto this helper (behavior-preserving; the existing shell
  acceptance still passes).
- One-shot (`run.go`) calls it and constructs a manager, seeding `localPrincipal(cfg)`.
- Loop-server (`buildLoopServerRuntimeDeps`) calls it and constructs a **per-loop** manager
  seeded with the loop's real principal (§2).

The mediator is already built by `buildTargetMediator(cfg)`; the helper co-locates the two so
they cannot drift apart per binding.

### 2. Loop-server MCP attach + real principal threading (the crux)

Two sub-parts, both on the loop-server path:

**(a) Attach a manager per loop.** `buildLoopServerRuntimeDeps` builds `NewToolRegistry` and
`BuildAgent` closures per loop (change 0046 — N isolated loops, no shared tree/store/registry).
MCP tools must be registered into **each loop's own** tool registry so tools are loop-local and
never cross-loop. Construct the `mcp.Manager` inside the per-loop wiring (via `mcpAttach`'s
options) and register its tools into that loop's registry, honoring the same subset/gating rules
the existing per-loop tool wiring uses. The manager lifecycle (start/stop) binds to the loop's
lifecycle.

**(b) Thread the real principal to the egress.** #52's carrier exists:
`toolidentity.WithPrincipal(ctx, p)` stashes the `loopauth.Principal`; `MCPTool.Execute` reads
it via `toolidentity.PrincipalFrom(ctx)` and mints a per-call credential. Today **only the shell**
calls `WithPrincipal` (`internal/tui/shell_model.go:1107`), seeding the static `localPrincipal`.
On the loop-server path the authenticated principal already exists — `buildLoopVerifier`
(`loop_serve_net.go`) maps each bearer token → `loopauth.Principal{Tenant, Subject}` at the
Connect edge (#49) — but it is **never threaded into the tool-call context**. Wire it: the
loop's initiator principal (captured at `StartLoop` from the authenticated request) must reach
the per-turn tool-execution context via `toolidentity.WithPrincipal`, so `MCPTool.Execute` mints
for the *real* user (user in `sub`, fuse in `act`), per tenant.

The exact seam where `WithPrincipal` is applied on the runtime path is the primary design detail
to firm up at build — candidates: at loop construction (stamp the principal onto a context the
per-turn run derives from) or per-turn in the runtime dispatch, mirroring where the shell does it.
Requirement: the principal that reaches `Execute` is the loop's authenticated initiator, not a
seeded local default, and two concurrent loops started by different principals never cross.

**Retiring the shim.** With (b) in place, the loop-server no longer mints for `DefaultTenant`.
`buildToolIdentitySource`'s single-tenant STS keying (`event.DefaultTenant` signing key) must be
generalized so the STS can mint for the actual per-loop tenant — per-tenant signing-key isolation
already exists in the Broker/STS design (#52), so this is wiring the real tenant through, not a new
mechanism. The shell/one-shot keep supplying `DefaultTenant` as their principal; nothing regresses
for them.

### 3. One-shot / probe policy

`run.go` attaches MCP via `mcpAttach`, seeding `localPrincipal(cfg)` (→ `DefaultTenant`), exactly
mirroring the shell's local-identity model. No new flag. If a genuine local multi-identity need
appears later, it is a follow-up — loop-server is the multi-tenant path.

### 4. `fuse mcps` — unchanged

The introspection commands stay execution-less (no credential source). Out of scope.

## Acceptance — permanent CI lane (the full #52 checklist, through the binding)

A new end-to-end test lane, run against a **real scripted MCP server** (a stand-up test server
speaking the MCP wire — NOT a mock of our own client), driving the **loop-server binding**. It
asserts the complete #52 verification checklist that was previously only provable at the seam:

1. **Two loops, two principals, one server.** Two loops started on the loop-server by *different*
   authenticated principals (distinct tenant/subject via `buildLoopVerifier` tokens), each calling
   the **same** real MCP server.
2. **Distinct, audience-bound, delegated tokens.** Each loop's call presents a *different* token:
   the initiator in `sub`, fuse in the `act` claim, audience-bound (RFC 8707 `resource`) to the
   server's declared target. The two tokens are provably distinct per principal.
3. **Downstream adjudication.** The scripted server adjudicates by identity: it returns 403 for the
   unauthorized principal, which surfaces to the loop as a **distinguishable tool error** (not a
   generic failure), and success for the authorized one.
4. **Wrong-audience rejected.** A token minted for the wrong audience is rejected by the server.
5. **Complete-mediation denial through the binding.** A tool reaching a target **not on the
   principal's allowlist / not declared in config** is denied terminally by the `TargetMediator`
   on the loop-server path, independent of permission mode (loop-server is `AlwaysApprove`) — proving
   mediation is not bypassed by the auto-approve binding policy.
6. **No credential leak.** No minted token or downstream credential appears in the **event stream**,
   the **durable store** (0047 fsstore/pgstore), or **logs** — reusing #52's redaction guard,
   asserted on the loop-server path.

**Harness discipline** (grounded in existing learnings):

- **Real backend for the acceptance.** The scripted MCP server is a *real* server the fuse client
  talks to over the wire — not a mock of our client — so the lane proves the *system*, not just that
  the wire serializes (`smoke-over-fake-backend-proves-wire-not-system`).
- **Loud on toolchain absence.** If the lane cannot run (missing toolchain / server binary), it fails
  loudly or is a **loud skip** — never a silent `t.Skip` that lets a green suite hide that the path
  never ran (`smoke-over-fake-backend-proves-wire-not-system`).
- **Scripted LLM double, never Claude/Anthropic.** Any model turn in the lane runs against a scripted
  `LLM_GATEWAY_URL` double, never live Claude/Anthropic (`verify-tool-loop-at-gateway-seam`; project
  rule: verification traffic uses cheap gateway doubles).
- **Concurrency is exercised.** Because two loops run under one process (0046) with per-loop managers,
  the lane drives them concurrently so a cross-loop principal/credential bleed or a shared-state race
  is observable, not latent (`race-invisible-to-race-detector-without-concurrent-test`).

This checklist, run **through the loop-server binding** and pinned in CI, is the deliverable: MCP
works, with #52 identity, through the actual service binding, and a test kills it if it regresses.

## Out of scope

- **Extending identity to non-MCP tools** — `web_fetch`/`web_search` is #57; `bash` containment is
  #58. This change is specifically MCP-through-every-binding.
- **New MCP protocol features / transports** — wiring + proof of what exists, not new MCP surface.
- **Re-designing #52's token exchange** — settled (ADR-0036); this change consumes the seam and
  drives it through the real bindings.
- **A one-shot CLI identity flag** (`--tenant`/`--principal`) — explicit non-goal (§3).
- **`fuse mcps` gaining tool execution** — stays introspection-only.

## Open questions for build-time reconcile

- Exact seam for applying `toolidentity.WithPrincipal` on the runtime/loop-server path (loop
  construction vs per-turn dispatch) — mirror the shell's `shell_model.go` placement, adapted to the
  loop-server's per-loop, principal-per-loop reality.
- Generalizing the STS signing-key map from `DefaultTenant`-only to per-loop tenant — confirm the
  #52 Broker/STS per-tenant isolation is reached, not re-implemented.
- Whether the real scripted MCP server for the lane is fuse's own MCP server (dogfood, preferred where
  it can express the identity/403 scenarios) or a purpose-built scripted server.

## ADRs

Likely **no new ADR** — this change *consumes* ADR-0036 (delegated tool authz at the egress seam)
and ADR-0034 (edge-enforced tenancy) and drives them through the bindings; it makes no new
architectural decision. If the principal-threading seam or the shared-helper boundary turns out to
encode a durable decision at build, record it then. Cites: ADR-0036, ADR-0034, ADR-0028/0029.
