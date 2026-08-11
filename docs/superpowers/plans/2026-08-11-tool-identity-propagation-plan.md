<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0052 — Tool/resource identity propagation — per-call RFC 8693 token exchange to downstream MCP/APIs](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0052-tool-identity-propagation.md)**
<!-- docket:backlink:end -->

# Implementation plan — Tool/resource identity propagation (change #52)

**Spec:** `docs/superpowers/specs/2026-08-11-tool-identity-propagation-design.md` (on the docket branch)
**Branch:** `feat/tool-identity-propagation` cut from `origin/main` @ 59dbd31
**Deps merged:** #48 networked binding (Connect/protobuf `fuse.loop.v1`), #49 auth/multi-tenancy
(`internal/loopauth.Principal{Tenant,Subject}` + `Verifier`, `internal/loopconnect.PrincipalFrom(ctx)`).

> Authored by docket-implement-next's plan step. **NOTE: the configured plan skill
> (`superpowers:writing-plans`) is not installed on this machine**, so per docket's Skill-layer
> missing-skill rule the plan role degraded to `auto` and this file was authored directly. The build
> and review roles (`superpowers:subagent-driven-development`, `superpowers:requesting-code-review`)
> are likewise absent in this environment and degrade the same way — see the PR body. The build is
> executed inline on the feature branch by the running agent, TDD, and reviewed whole-branch before
> the PR opens.

## What this PR delivers (and what it defers)

This is a large surface (spec D1–D7). This PR lands the **architectural spine end-to-end** so a
per-caller, audience-bound credential reaches a downstream MCP server on every tool call, with the
seams shaped for richer implementations:

- **D1** the egress `CredentialSource` seam (single choke point identity→credential).
- **D2** a pluggable `TokenExchanger` (RFC 8693 delegation shape) with a **built-in minimal STS**
  as the zero-config default; the external-AS exchanger stays behind the seam (follow-on).
- **D3** tiered per-server downstream auth — OAuth-exchange tier + an **explicit, identity-free
  static-credential fallback** tier — reworking `internal/mcp` to inject the credential **per call**
  (per-request header) with **RFC 8707 `resource`/audience binding**, dropping the single static
  per-server bearer as the default.
- **D4** the broker-shaped posture: fuse holds no long-lived downstream secret for OAuth targets;
  short-lived minted tokens; **per-tenant credential isolation** and a per-mint audit line.
- **D5** per-call mediation on the **existing approval gate** — target/scope **ceilings + allowlist**
  sourced from the loop-start root of trust, denying undeclared targets even for schema-valid args.
- **D6** the **redaction constraint** — credentials live only in the outbound transport call, never
  in `tool.call`/`tool.result` events, the durable store, logs, or model context; a named test.
- **D7** the ADR recording the decision + the MCP-spec constraints honored.

**Deferred behind the fixed seams** (spec says "seam is fixed, placement is open" — not new cuts):
the external-AS `TokenExchanger` impl (Keycloak/Auth0/Vault), a separate out-of-process broker
service, per-target impersonation mode, and cross-issuer token exchange. Each rides the seam this PR
establishes; the built-in STS + delegation default is the shippable slice.

## Guardrails (hold across every task)

- **Root of trust outside the model (HARD).** The `Principal` AND every downstream target/scope
  MUST originate from the authenticated loop-start context (`loopauth.Principal`) and a tool's own
  declaration — **NEVER** from anything the model emits, discovers, or expands. No model-derived
  scope or target, ever. A test proves an injected over-broad scope / redirected target is
  ignored/denied.
- **Policy-free runtime seam (ADR-0030).** `internal/runtime` imports **no** auth package and **no**
  `cmd/fuse`. The propagated identity travels as a **request-context value** threaded from the
  loop-start factory (`cmd/fuse/loop_server.go` `BuildAgent`) through to the MCP `Execute` — mirroring
  the existing `permissions.WithUserMessages(ctx, …)` context-carry precedent — never as a runtime
  import. The new seam types live in an **auth-light package** the edge + composition root import,
  never `internal/runtime` (learning: `break-import-cycle-with-agent-free-subpackage`).
- **Per-tenant credential isolation (ADR-0031 discipline).** Any cache/map over minted credentials
  or exchange material is keyed by the **full compound key** including `Tenant`; a hit re-asserts the
  tenant and falls through on mismatch — a mismatch is a miss, never a wrong answer (learning:
  `cache-over-tenant-scoped-source-reassert-key-on-hit`). Wired per host instance, not a process
  global (learning: `deglobalize-holder-also-per-instance-the-shared-graph`).
- **No credential leakage (D6).** The minted token is set on the outbound `*http.Request` header
  only. It is never placed in tool args, `tool.result` output, an error string surfaced to the loop,
  a log line, or the event payloads. Structural test over the 0043 event stream.
- **MCP-spec compliance (rev 2025-11-25).** No token passthrough (the exchange mints a NEW token for
  the specific target; fuse never forwards a token it received); audience binding via the `resource`
  param; never forward a wrong-audience token.
- **TDD.** Each task writes a failing test first, then the minimal code to pass, then refactors.
  `go build ./... && go test ./...` green (with `-race` on the touched packages) at every task
  boundary.
- **Live verification uses a scripted `LLM_GATEWAY_URL` double — never Claude/Anthropic** (project
  policy). The load-bearing checks here are auth/token-flow + mediation, not model quality; drive the
  real backend for at least one acceptance (learning: `smoke-over-fake-backend-proves-wire-not-system`,
  `verify-tool-loop-at-gateway-seam`).
- **MCP HTTP retry safety.** Any retry on the reworked egress path resets `req.Body` from
  `req.GetBody()` before each attempt (learning: `rewind-request-body-on-manual-retry`) — relevant if
  a per-call token refresh triggers a re-POST.
- **cmd/fuse cloned wiring.** Wiring that touches child-agent tool registries or the composition root
  is enumerated by grep at build time across all clone sites (learning:
  `patch-every-cloned-child-builder`), never from this plan's list alone.

---

## Task 0 — Package skeleton + the identity-propagation seam types (D1)

**New package:** `internal/toolidentity` (auth-light: imports only `internal/loopauth` for `Principal`
and stdlib — importable by the Connect edge and `cmd/fuse`, NEVER by `internal/runtime`).

- Define the value types:
  - `type Target struct { Name string; Audience string; Scopes []string; Tier Tier }` — the
    downstream descriptor a **tool declares** (D5): its `Audience` (RFC 8707 resource id) and required
    `Scopes`. `Tier` ∈ {`TierOAuth`, `TierStatic`}.
  - `type Credential struct { Scheme string; Token string; carriesIdentity bool }` — a resolved,
    short-lived credential (or an explicit static one). `Token` is unexported-adjacent: only the
    transport injector reads it; `String()`/`GoString()` MUST redact (return `Credential{…redacted…}`)
    so a stray `%v`/log never prints it.
  - `type CredentialSource interface { CredentialFor(ctx context.Context, p loopauth.Principal, t Target) (Credential, error) }`
    — the single egress choke point.
- **Tests first:** `Credential.String()`/`GoString()` never contain the token (redaction unit test);
  `Target` zero value is `TierOAuth` with empty audience → a helper `(Target).Valid()` reports
  "undeclared target" for an empty audience (drives D5 deny).

**Done when:** package compiles; redaction + validity tests green.

## Task 1 — The `TokenExchanger` seam + built-in minimal STS (D2)

**Files:** `internal/toolidentity/exchange.go`, `..._test.go`

- `type TokenExchanger interface { Exchange(ctx, req ExchangeRequest) (ExchangeResult, error) }`
  where `ExchangeRequest` carries `Subject` (the principal's `Subject`), `Tenant`, `Actor` ("fuse"),
  `Audience`, `Scopes` — the RFC 8693 delegation inputs. `ExchangeResult` carries the minted token +
  expiry + the `act` chain descriptor.
- `type BuiltinSTS struct{…}` — signs a short-lived delegation token (JWT) with the loop initiator in
  `sub`, fuse in an `act` claim, `aud` = target audience, `scope` = requested scopes, `exp` short.
  Symmetric-key signing from **trusted config** (composition root), keyed per tenant so tenant A's
  signer material is never reachable by tenant B.
- **Tests first:** (a) two different `Subject`s → tokens whose decoded `sub` differ and both carry the
  `act` claim = fuse; (b) `aud` equals the requested audience; (c) requested `scope` is echoed, and a
  scope NOT in the request is absent (no expansion); (d) the token is short-lived (`exp` within the
  configured TTL); (e) per-tenant signer isolation (a token minted for tenant A does not verify under
  tenant B's key). No Anthropic; pure crypto/JWT unit tests.

**Done when:** `BuiltinSTS` mints verifiable delegation tokens; the seam has a fake for downstream
tests; `go test -race ./internal/toolidentity/...` green.

## Task 2 — The broker `CredentialSource`: tiered resolution + isolation + audit (D3/D4)

**Files:** `internal/toolidentity/broker.go`, `..._test.go`

- `type Broker struct { exch TokenExchanger; static map[string]StaticCred; audit AuditSink }`
  implementing `CredentialSource`:
  - `TierOAuth` target → call `exch.Exchange` with `(principal, target.Audience, target.Scopes)`;
    return an audience-bound `Credential{carriesIdentity:true}`. **No caching in this PR** (correctness
    first; short-lived tokens minted per call) — but if a cache is added, it is keyed by
    `(tenant, subject, audience, scopeset)` and re-asserts on hit.
  - `TierStatic` target → return the explicitly-configured per-server static credential with
    `carriesIdentity:false`; the principal identity is **NEVER** carried into a static cred. Absent
    config for a static-tier target → a distinct `ErrNoStaticCredential` (never a silent fallback to
    OAuth or to another server's cred).
  - Every resolution emits one audit record `(principal.Subject, tenant, target.Name, target.Audience,
    scopes, tier, act-chain)` via `AuditSink` — and the audit record **never contains the token**.
- **Tests first:** (a) OAuth target → delegated token, identity carried, audit line present w/o token;
  (b) static target → static cred, identity NOT carried, flagged weaker tier in audit; (c) static
  target with no config → `ErrNoStaticCredential`; (d) fuse holds no long-lived secret for OAuth
  targets — structural assertion the `Broker` has no stored OAuth bearer, only the exchanger seam;
  (e) two tenants resolving the same audience get isolated results (no cross-tenant material reuse).

**Done when:** `Broker` satisfies `CredentialSource`; tiering + isolation + audit-without-token tests
green.

## Task 3 — Thread `Principal` from loop-start to the tool-call context

**Files:** `internal/toolidentity/context.go` (ctx carrier), `cmd/fuse/loop_server.go` (loop-start
factory), the tool-loop call path.

- Add `WithPrincipal(ctx, loopauth.Principal) context.Context` + `PrincipalFrom(ctx) (Principal, bool)`
  in `internal/toolidentity` (the auth-light carrier — distinct from `loopconnect`'s edge-only carrier;
  this one is readable by the MCP egress without importing the Connect edge).
- At the loop-start factory (`BuildAgent` in `cmd/fuse/loop_server.go`), capture the authenticated
  `Principal` (resolved at the Connect edge via `loopconnect.PrincipalFrom`) and thread it so the
  agent's per-tool-call `ctx` carries it via `toolidentity.WithPrincipal`. **Grep every BuildAgent /
  child-builder clone site** and thread consistently (learning: `patch-every-cloned-child-builder`).
  For the non-networked CLI paths (one-shot, shell) with no Connect edge, seed a single explicit
  local principal (the "`_default` tenant" analog #49 already uses) — never an empty/spoofable one.
- **Tests first:** a table test that the `Principal` placed at loop-start is the exact one
  `PrincipalFrom(ctx)` returns at the tool-execute site; and that model output on the context path
  cannot overwrite it (the carrier key is unexported; only `WithPrincipal` sets it).

**Done when:** identity reaches the tool-execute ctx from loop-start; carrier is model-inaccessible;
`go build ./...` green.

## Task 4 — MCP egress rework: per-call credential injection + audience binding (D3)

**Files:** `internal/mcp/http_client.go`, `internal/mcp/streamable_http_client.go`,
`internal/mcp/manager.go`, `internal/mcp/tool.go`, `internal/config/schema.go`.

- **Config:** extend `MCPServerConfig`/`MCPAuthConfig` with the per-server **tier + audience/resource**
  declaration: `Audience string`, `Scopes []string`, and an `auth.type` value that selects the
  identity-propagation OAuth tier vs the explicit `static` tier vs `none`. Keep back-compat: existing
  `bearer`/`oauth2` configs map to the static tier (explicit, identity-free) so no current server
  breaks — the single static per-server bearer stops being the *default* path but remains reachable as
  the named static tier.
- **Per-call injection:** change the HTTP/streamable clients so the `Authorization` header (and the
  `resource` audience param where the transport carries it) is set **per `call()`** from a
  `CredentialSource.CredentialFor(ctx, principal, target)` resolution, NOT from a token baked in at
  `newHTTPClient`. `MCPTool.Execute` reads `toolidentity.PrincipalFrom(ctx)` + the tool's declared
  `Target`, resolves the credential at the egress seam, and injects it for that one request. The
  connection stays per-server; only the credential becomes per-call.
- **No passthrough:** assert the client never forwards a token it received from any upstream; each
  outbound token is freshly minted for that target's audience.
- **Tests first (httptest.Server doubles, no Anthropic):**
  1. Two loops with different principals calling the same OAuth-tier target present **distinct**
     audience-bound delegated tokens carrying the `act` chain (server double captures the header).
  2. A token minted for target A is rejected by target B (audience binding); fuse never sends a
     wrong-audience token.
  3. Static-tier server is reachable, receives the static cred, and the header carries **no** initiator
     identity.
  4. `-race` test driving a call concurrently with anything mutating client state (learning:
     `race-invisible-to-race-detector-without-concurrent-test`).

**Done when:** per-caller distinct tokens reach the double; audience binding + no-passthrough +
static-identity-free assertions green; existing MCP tests still pass (back-compat).

## Task 5 — Per-call mediation on the approval gate: ceilings + allowlist (D5)

**Files:** `internal/permissions/gate.go` (extend, don't fork), the egress seam call site.

- Extend the per-tool-call resolve path so a tool that reaches a downstream **Target** is checked
  against a **principal/tenant scope+target ceiling/allowlist** sourced from config / #49 grants
  (the loop-start root of trust) — carried on ctx alongside the `Principal`, never from model output.
  A call to a target NOT on the allowlist, or requesting a scope beyond the ceiling, is **denied at the
  gate** even though the tool exists and args are schema-valid (complete mediation). This composes with
  the existing `ApprovalFunc` human-approval hook (approve/deny) — the ceiling check runs first; a human
  hook still applies on top for allowed-but-sensitive calls.
- A tool that reaches an **undeclared** target (empty `Audience`) is denied (from Task 0 `Valid()`).
- **Tests first:** (a) undeclared-target tool call denied though schema-valid; (b) allowlisted target
  approved; (c) a model-injected over-broad scope on the args is ignored — the effective scope is the
  tool's declaration ∩ the ceiling, never the model's; (d) the human approval hook still fires for an
  allowed sensitive call.

**Done when:** complete-mediation + no-model-scope tests green; the gate change is an extension (no
parallel PDP), existing gate tests pass.

## Task 6 — Redaction verification over the event stream (D6)

**Files:** `internal/agent/loop.go` (emission sites — assert, don't rewrite), a focused test package.

- Add a **structural test** that runs a real tool call through the egress seam against an httptest
  double and asserts that **no** emitted `tool.call`/`tool.result` event payload, no error string
  surfaced to the loop, and no captured log contains the minted token. Because the token lives only in
  the transport header (never in tool args/results), this is primarily a **regression guard** ensuring
  the design invariant can't silently regress. If any path is found leaking (e.g. an error wrapping the
  outbound request including headers), fix it minimally at that site.
- Assert the `Credential`/token type's `String()`/`GoString()` redaction (from Task 0) is what any
  accidental `%v` would hit.

**Done when:** the no-leak assertion passes over the event stream + logs; redaction guard green.

## Task 7 — Composition root wiring (D4 placement)

**Files:** `cmd/fuse/run.go`, `cmd/fuse/shell.go`, `cmd/fuse/loop_server.go` (+ any grep'd clone),
`internal/config/schema.go`.

- Wire the `TokenExchanger` (default `BuiltinSTS` from trusted config; external-AS impl behind config
  as a follow-on stub returning a clear "not configured" error), the `Broker` (`CredentialSource`), and
  the audit sink at the composition root — **per host instance**, mirroring the store/verifier seam
  discipline; nothing process-global (learning: `deglobalize-holder-also-per-instance-the-shared-graph`).
- Pass the `CredentialSource` into the MCP manager/egress path and the ceiling config into the gate.
- Zero-config local story: with no exchange config, the `BuiltinSTS` mints local delegation tokens for
  fuse's own OAuth-tier downstreams; servers configured as `static`/`none` behave as today. Loudly log
  the posture at startup (which tier each server resolved to).
- **Tests first:** a composition-root smoke that builds the full wiring for the loop-server path and a
  CLI path and asserts a tool call resolves a credential end-to-end; two concurrent host instances get
  isolated broker/exchanger state (no shared mutable global).

**Done when:** the binary wires the seam on every path; concurrent-instance isolation asserted;
`go build ./... && go test -race ./...` green.

## Task 8 — Acceptance: end-to-end per-caller identity to a real MCP double + ADR (D7)

**Files:** an integration test in `internal/mcp` or `cmd/fuse`; ADR via `docket-adr` at review.

- **Acceptance test (real binary path, scripted `LLM_GATEWAY_URL`, no Anthropic):** start two loops as
  two different principals over the Connect edge, each calling the same OAuth-tier MCP server double;
  assert the double received **two distinct** audience-bound delegated tokens (user in `sub`, fuse in
  `act`), that a 403 from the double surfaces as a distinguishable tool error (fuse made no authz
  decision of its own), and that no token appears in the loop's event stream.
- The **ADR (D7)** is recorded in Step 6 via the `docket-adr` role: "tool/resource authz delegated to
  downstreams via per-call RFC 8693 delegation (act-claim) token exchange at a pluggable egress seam,
  with a credential broker and per-call mediation on fuse's approval gate," explicitly stating the
  MCP-spec constraints honored (no passthrough, audience binding), superseding the implicit static
  per-server bearer model.

**Done when:** the acceptance test is green over the real backend double with a loud skip if its
toolchain is absent (learning: `smoke-over-fake-backend-proves-wire-not-system`); ADR number recorded
on the change's `adrs:`.

## Verification checklist (maps to spec ## Verification)

- [ ] Per-caller identity reaches the downstream — distinct delegated tokens per principal (Task 4/8).
- [ ] Downstream adjudicates — 403 surfaces as a tool error, fuse makes no authz call of its own (Task 8).
- [ ] Audience binding enforced — token for A rejected by B; no passthrough (Task 4).
- [ ] No model-derived scope/target — injection ignored/denied (Task 3/5).
- [ ] Per-call mediation blocks undeclared/over-scope targets (Task 5).
- [ ] Legacy static fallback explicit + identity-free + flagged (Task 2/4/7).
- [ ] No credential leakage into events/store/logs/model context (Task 0/6).
- [ ] Broker holds no long-lived secret for OAuth targets (Task 2).
- [ ] ADR recorded (Task 8 / Step 6).
- [ ] `go build ./... && go test -race ./...` green; live check via non-Anthropic gateway double.
