<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0059 — Wire MCP into every loop binding and prove it end-to-end — no more features on untestable paths](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0059-mcp-full-binding-wiring-e2e-tested.md)**
<!-- docket:backlink:end -->

# Implementation plan — Wire MCP into every loop binding and prove it end-to-end (change #59)

**Spec:** `docs/superpowers/specs/2026-08-11-mcp-full-binding-wiring-e2e-tested-design.md` (on the docket branch)
**Branch:** `feat/mcp-full-binding-wiring-e2e-tested` cut from `origin/main` @ fab812c (PR #55 / #52 merged)
**Deps merged:** #52 tool-identity-propagation (`internal/toolidentity` egress seam + built-in STS + Broker + `TargetMediator`, ADR-0036), #49 auth/multi-tenancy (`internal/loopauth.Principal{Tenant,Subject}` + `Verifier`, `internal/loopconnect.PrincipalFrom(ctx)`), #46 multi-loop host (per-loop isolated tree/store/registry).

> Authored by docket-implement-next's plan step. **NOTE: the configured plan skill
> (`superpowers:writing-plans`) is not installed on this machine**, so per docket's Skill-layer
> missing-skill rule the plan role degraded to `auto` and this file was authored directly. The build
> and review roles (`superpowers:subagent-driven-development`, `superpowers:requesting-code-review`)
> are likewise absent in this environment and degrade the same way — see the PR body. The build is
> executed inline on the feature branch by the running agent, TDD, and reviewed whole-branch before
> the PR opens.

## What this PR delivers

MCP tool execution attached to **every** loop binding — first-class on the loop-server — through one
shared attach helper, with the **real authenticated principal** threaded to the egress on the
loop-server path (retiring the `DefaultTenant` shim there by construction), and a **permanent CI
acceptance lane** that runs the full #52 verification checklist *through the loop-server binding*
against a real, stateful Wander rentals MCP server. Deliverable: "MCP works, with #52 identity,
through the actual service binding, and a test kills it if it regresses."

The infrastructure #52 built is present and correct on `origin/main`; this change is **wiring + proof**,
not new identity mechanism. Confirmed seam map (file:line against the branch base):

- `cmd/fuse/shell.go:76-88` — inline MCP attach (`mcpOpts` / `buildToolIdentitySource` / `mcp.WithCredentialSource` / `logToolIdentityPosture` / `tui.NewMCPProvider`); `shell.go:265` — `WithToolPrincipal(localPrincipal(cfg))`.
- `cmd/fuse/tool_identity.go` — `buildToolIdentitySource` (121-193, STS keyed **`event.DefaultTenant` only**, with an explicit multi-tenant TODO at 163-168), `buildTargetMediator` (78-90, exists but **not wired into the shell approve gate**), `localPrincipal` (214-220), `newConfigTargetMediator`.
- `internal/toolidentity/context.go:26,36` — `WithPrincipal(ctx, loopauth.Principal)` / `PrincipalFrom(ctx)`.
- `internal/mcp/manager.go` — `NewManager(servers, reg, opts...)` (139), `ManagerOption` (125), `WithCredentialSource` (127); source stamped onto each MCPTool in `Add()` (182-184); `Close()` lifecycle.
- `internal/mcp/tool.go:74-170` — `MCPTool.Execute` reads `PrincipalFrom(ctx)`, mints via `source.CredentialFor`, threads `WithCallAuth`; nil source ⇒ pre-#52 static path.
- `cmd/fuse/loop_server.go:86-219` — `buildLoopServerRuntimeDeps`; per-loop registry cloned via `cloneServerToolRegistry(toolReg)` (225); per-loop `BuildAgent` closure (118-217); **no MCP wiring**.
- `cmd/fuse/loop_serve_net.go:48-65` — `buildLoopVerifier`; `internal/loopconnect/auth.go` interceptor stashes `Principal`, `handler.go:99` reads it via `PrincipalFrom`; **not threaded into the per-turn agent ctx**.
- `internal/mcp/egress_test.go` — `capturingSSEServer` (real httptest MCP server) + `TestEgress_TwoPrincipalsDistinctTokens` — the seam-level prior art the lane elevates to the binding level.
- Multi-site child-registry clone sites (learning `patch-every-cloned-child-builder`): `shell.go`, `loop_server.go` (×2), `runtime_binding.go`, `research_probe.go`.

## Guardrails (hold across every task)

- **Root of trust outside the model (HARD, ADR-0036).** The `Principal` and every target audience/scope
  originate from the authenticated loop-start context and the tool declaration — never from model output.
  On the loop-server path the principal is the one `buildLoopVerifier` resolved at the Connect edge, not a seeded local default.
- **Policy-free runtime seam (ADR-0030).** `internal/runtime` imports no auth package and no `cmd/fuse`.
  The principal travels as a **context value** threaded from the composition-root loop factory
  (`cmd/fuse/loop_server.go` `BuildAgent`) into `MCPTool.Execute` via `toolidentity.WithPrincipal` —
  mirroring the shell's `WithToolPrincipal` precedent — never a runtime import.
- **Per-loop isolation is total (learning `deglobalize-holder-also-per-instance-the-shared-graph`).**
  The per-loop MCP `Manager` and its registered tools are constructed **inside** the per-loop factory,
  registered into **that loop's own** cloned registry, and lifecycle-bound to that loop — never a shared
  manager. Two concurrent loops by different principals must never share a manager, a registry, or a credential.
- **Per-tenant credential isolation (learning `cache-over-tenant-scoped-source-reassert-key-on-hit`).**
  The STS/broker key is the full compound key incl. `Tenant`; a hit re-asserts the tenant and falls
  through on mismatch. Generalizing the STS from `DefaultTenant`-only reaches #52's existing per-tenant
  key isolation — wiring the real tenant through, not a new mechanism.
- **No credential leakage (ADR-0036 D6).** The minted token lives only on the outbound transport header;
  never in `tool.call`/`tool.result` events, the durable store (0047 fs/pg), logs, model context, or error
  strings. Re-assert #52's redaction guard on the loop-server path.
- **Complete mediation not bypassed (ADR-0036 D5).** `TargetMediator` denies an undeclared target
  terminally, independent of permission mode — proven on the loop-server path even though it runs `AlwaysApprove`.
- **Real server for acceptance; loud on absence (learning `smoke-over-fake-backend-proves-wire-not-system`).**
  The rentals MCP server is a real server fuse's client talks to over the wire; only its listing *data*
  is canned behind a seam. If the lane can't run, it fails loud or loud-skips — never a silent `t.Skip`.
- **Concurrency exercised (learning `race-invisible-to-race-detector-without-concurrent-test`).**
  The two loops are driven concurrently under one process so a cross-loop principal/credential bleed is
  observable under `-race`, not latent.
- **Scripted LLM double, never Claude/Anthropic (learning `verify-tool-loop-at-gateway-seam`;
  project rule).** Any model turn runs against a scripted `LLM_GATEWAY_URL` double (as
  `loop_serve_net_twoinstance_test.go` already does), never live Claude/Anthropic.
- **TDD.** Each task: a failing test first, minimal code to pass, then refactor. Full suite green at the end.

## Tasks

### Task 1 — Shared `mcpAttach` helper (the spine), shell refactored onto it

**Goal:** one composition-root helper yields the MCP manager options **and** the complete-mediation
option together, so no binding can wire a manager without also wiring the egress seam + `TargetMediator`.

- **Test first:** a `cmd/fuse` unit test asserting `mcpAttach(cfg, w)` returns (a) `mcp.WithCredentialSource`
  options exactly when `buildToolIdentitySource(cfg)` yields a source, (b) the `buildTargetMediator(cfg)`
  `permissions.Option` when any server is identity-propagating, and (c) writes the same posture log the
  shell writes today. Table-test the identity-propagating and non-propagating configs.
- **Implement:** add `func mcpAttach(cfg config.Config, w io.Writer) (mcpOpts []mcp.ManagerOption, mediator permissions.Option)`
  in `cmd/fuse` (near `tool_identity.go`). Move the shell's inline attach block (shell.go:76-88) into it;
  co-locate `buildTargetMediator` so the two cannot drift.
- **Refactor shell onto it (behavior-preserving):** `shell.go` calls `mcpAttach`, applies the returned
  options to `tui.NewMCPProvider`, and applies the mediator `permissions.Option` to the shell's approve
  gate (this **also fixes** the current gap where `buildTargetMediator` was built but never wired into the
  shell gate — assert the mediator now gates the shell path). The existing shell MCP acceptance still passes.
- **Guardrail check:** `patch-every-cloned-child-builder` — enumerate the clone sites by grep now; the
  helper is the single attach path each binding will call in later tasks.

### Task 2 — Generalize the built-in STS to per-loop tenant

**Goal:** retire the `DefaultTenant`-only STS keying so the STS can mint for the real per-loop tenant,
reaching #52's existing per-tenant signing-key isolation (not a new mechanism).

- **Test first:** a `toolidentity`/`cmd/fuse` test that the STS mints a valid, tenant-distinct delegation
  token for a non-`_default` tenant, and that two different tenants get keys that do not cross (a token
  minted for tenant A does not verify as tenant B). Assert the `DefaultTenant` path is unchanged (shell/one-shot regression guard).
- **Implement:** generalize `buildToolIdentitySource` (tool_identity.go:158-175) so `TenantKeys` is built for
  every tenant the config/loop-verifier knows (derive per-tenant keys from the configured signing material via
  the #52 per-tenant derivation, keeping `DefaultTenant` as one entry). Remove the single-tenant TODO (163-168).
- **Guardrail check:** compound-key isolation — a cache/map over minted material re-asserts `Tenant` on hit.

### Task 3 — Thread the real principal into the loop-server per-turn context

**Goal:** the authenticated loop-start principal (`buildLoopVerifier` → `loopauth.Principal`) reaches
`MCPTool.Execute` via `toolidentity.WithPrincipal`, so the egress mints for the real user (user in `sub`,
fuse in `act`), per tenant — never a seeded default.

- **Test first:** a `cmd/fuse` test driving the loop-server BuildAgent path: an MCP tool call inside a loop
  started by principal P observes `PrincipalFrom(ctx) == P` at `Execute` (use a fake `CredentialSource` that
  captures the principal it was asked to mint for). A second loop started by principal Q observes Q — never P.
- **Implement:** capture the initiator principal at loop-start on the loop-server path
  (`loop_serve_net.go`/`loopconnect` handler already has it via `PrincipalFrom(ctx)` at `handler.go:99`) and
  thread it into the per-turn agent context the loop-server's `BuildAgent` closure derives from — stamping
  `toolidentity.WithPrincipal(ctx, p)` at the same seam the shell uses (`WithToolPrincipal`), adapted to the
  per-loop, principal-per-loop reality. Respect ADR-0030: the principal is a context value carried from the
  composition root, not a runtime import. **Open-question resolution (spec):** apply at loop construction
  (stamp the principal onto the context the per-turn run derives from) so it holds for every turn without
  per-turn re-plumbing; confirm two concurrent loops never cross by driving them concurrently.

### Task 4 — Attach a per-loop MCP manager on the loop-server (the crux)

**Goal:** `buildLoopServerRuntimeDeps` constructs a **per-loop** `mcp.Manager` via `mcpAttach`, registers
its tools into **that loop's own** cloned registry, and binds the manager lifecycle to the loop.

- **Test first:** a `cmd/fuse` test that a loop started on the loop-server can list + invoke an MCP tool
  (against a fake/stub MCP server), and that two concurrent loops each get their **own** manager/registry
  (a tool registered for loop A is not visible to loop B; no shared manager) — driven concurrently, `-race`.
- **Implement:** inside the per-loop `NewToolRegistry`/`BuildAgent` factory (loop_server.go:114/225), after
  `cloneServerToolRegistry(toolReg)`, construct `mcp.NewManager(cfg.MCPServers, loopToolReg, mcpOpts...)` with
  the options from `mcpAttach` (Task 1), register tools into the loop-local registry, apply the mediator
  `permissions.Option` to that loop's gate, and register `Manager.Close()` on the loop's teardown. No shared
  state across loops (guardrail: `deglobalize-holder-also-per-instance-the-shared-graph`).
- **Retire the shim by construction:** with Task 3, the loop-server no longer mints for `DefaultTenant`.

### Task 5 — One-shot / probe attaches MCP via the shared helper

**Goal:** `run.go` one-shot attaches MCP through `mcpAttach`, seeding `localPrincipal(cfg)` (→ `DefaultTenant`),
mirroring the shell's local-identity model. **No new CLI identity flag** (explicit non-goal).

- **Test first:** a `cmd/fuse` test that the one-shot path constructs an MCP manager and can invoke an MCP
  tool, seeding `localPrincipal(cfg)`; assert no `--tenant`/`--principal` flag was added.
- **Implement:** at the one-shot entry (`run.go` / `runtime_binding.go` BuildAgent closure) call `mcpAttach`,
  construct the manager into the one-shot registry, apply the mediator, and stamp `WithToolPrincipal(localPrincipal(cfg))`.
  `fuse mcps` stays introspection-only — unchanged.
- **Guardrail check:** `patch-every-cloned-child-builder` — confirm every child-registry clone site that
  should carry MCP now does, and `research_probe.go` intentionally does/does-not per its role (document the choice).

### Task 6 — The real Wander rentals MCP server + canned backend + per-principal favorites store

**Goal:** a genuinely real MCP server (real wire + handshake, real per-principal token adjudication, real
per-principal mutable state) with a canned listing-data backend behind a seam, co-located with the acceptance
harness (importable by Wander for #60).

- **Test first (drives the server's own contract):** unit tests for the server: `search_rentals(query)` returns
  canned listings; `favorite_listing(id)` writes into the **calling principal's** favorites keyed by the
  token's `sub`/tenant (idempotent per principal); `list_favorites()` returns only the caller's set; an
  unauthorized principal gets a 403 that surfaces as a distinguishable tool error; a wrong-audience token is
  rejected. The store is an in-memory per-principal map, reset per run; the isolation key is the **token
  identity**, never a client-supplied arg.
- **Implement:** the server (reuse `internal/mcp` server prior art / the egress_test `capturingSSEServer`
  shape as a starting point) with a `data source` seam: default canned/deterministic in-repo listings (no
  network, no key). The live Tavily backend is **#60**, out of scope. Place it under the acceptance harness /
  a demo package Wander can import.
- **Guardrail check:** `smoke-over-fake-backend-proves-wire-not-system` — the server's wire + token
  adjudication + per-principal store are real; only listing data is canned.

### Task 7 — Permanent CI acceptance lane (the full #52 checklist, through the loop-server binding)

**Goal:** one pinned CI lane asserting the whole checklist end-to-end through the loop-server binding.

- **Test (the lane):** build on `loop_serve_net_twoinstance_test.go` / `loop_serve_net_auth_test.go` (two
  loops, two principals, one process) + a scripted `LLM_GATEWAY_URL` double that scripts each loop's turn to
  call the rentals MCP tools. Assert, driven **concurrently** under `-race`:
  1. Two loops by different authenticated principals call the **same** rentals server.
  2. Each call presents a **distinct, audience-bound, delegated** token (initiator in `sub`, fuse in `act`,
     RFC 8707 `resource`); the two tokens are provably distinct per principal.
  3. Downstream adjudication: listings for the authorized principal, **403 for the unauthorized** one,
     surfacing as a **distinguishable tool error**.
  4. **Per-principal write isolation:** A `favorite_listing(X)` → X in A's favorites; B `list_favorites()`
     returns B's set **without** X. A's write invisible to B (the confused-deputy property on a mutating path).
  5. Wrong-audience token rejected by the server.
  6. Complete-mediation: an undeclared target denied terminally through the loop-server path even under `AlwaysApprove`.
  7. No credential leak into the event stream, the durable store (0047 fs/pg), or logs.
- **Loud on absence:** if the lane can't run (toolchain/server absent) it fails loud or loud-skips — never silent.
- **CI wiring:** ensure the lane runs in the repo's CI (a permanent lane, not opt-in), documented so a
  regression kills it.

### Task 8 — ADR (only if a durable decision emerges) + whole-branch review + docs

- **ADR:** per spec §ADRs, **likely none** — this change consumes ADR-0036/0034 and drives them through the
  bindings. If the principal-threading seam (Task 3) or the shared-helper boundary (Task 1) encodes a durable
  decision at build, record it via the `docket-adr` path and cite it in `adrs:`. Otherwise cite ADR-0036/0034 only.
- **Review:** whole-branch review before the PR opens (per docket step 6).
- **Docs:** brief note in the MCP/loop-server docs that MCP is now attached on every binding via `mcpAttach`,
  and that the loop-server carries the real per-principal identity.

## Test strategy summary

- Unit: `mcpAttach` composition (T1), STS per-tenant keying (T2), principal threading capture (T3), per-loop
  manager isolation (T4), one-shot attach (T5), rentals server contract + isolation (T6).
- Acceptance (permanent CI lane, `-race`, concurrent, scripted LLM double): the full 7-point #52 checklist
  through the loop-server binding against the real rentals server (T7).
- Regression: shell + one-shot `DefaultTenant` paths unchanged; `fuse mcps` still execution-less; #52
  egress-seam tests still green.

## Out of scope (per spec)

Non-MCP tool identity (#57 web_fetch/web_search, #58 bash); new MCP protocol/transports; re-designing #52's
token exchange; a one-shot CLI identity flag; `fuse mcps` gaining execution; the live Tavily backend + Wander
demo wiring (**#60**).
