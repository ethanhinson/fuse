---
id: 52
slug: tool-identity-propagation
title: Tool/resource identity propagation — per-call RFC 8693 token exchange to downstream MCP/APIs
status: implemented
priority: medium
type: feat
created: 2026-08-11
updated: 2026-08-11
depends_on: [48, 49]
related: [43, 47, 50, 51]
discovered_from: [47, 49]
adrs: [36]
spec: docs/superpowers/specs/2026-08-11-tool-identity-propagation-design.md
plan: docs/superpowers/plans/2026-08-11-tool-identity-propagation-plan.md
results: docs/results/2026-08-11-tool-identity-propagation-results.md
trivial: false
auto_groomable:
branch: feat/tool-identity-propagation
pr: https://github.com/ethanhinson/fuse/pull/55
blocked_by:
claimed_at: 2026-08-11T20:29:13Z
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-11-tool-identity-propagation-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-11-tool-identity-propagation-design.md) |
| Plan | [2026-08-11-tool-identity-propagation-plan.md](https://github.com/ethanhinson/fuse/blob/feat/tool-identity-propagation/docs/superpowers/plans/2026-08-11-tool-identity-propagation-plan.md) |
| Results | [2026-08-11-tool-identity-propagation-results.md](https://github.com/ethanhinson/fuse/blob/feat/tool-identity-propagation/docs/results/2026-08-11-tool-identity-propagation-results.md) |
| PR | [#55](https://github.com/ethanhinson/fuse/pull/55) |
| ADRs | [ADR-0036](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0036-tool-authz-delegated-downstream-rfc8693-egress-seam.md) |
<!-- docket:artifacts:end -->

## Why

fuse is becoming a hostable multi-tenant agent-loop runtime. Change 0049 designs **loop
authz** (Plane 1): who may start/send/observe/replay/administer *loops*, tenant=org +
owner=user, fuse-owned and coarse. But fuse also decides what **tools** exist and can
generate tools on the fly, and when a loop calls a tool that reaches an **external
system** — an MCP server, an HTTP API, a database with row-level security — fuse must
**not** try to model that system's permissions. It should carry the **loop-initiator's
identity** to the downstream and let the downstream enforce its own authorization ("call
MCP with the identity of who started the loop, and they tell us yes/no"). This is
**Plane 2: tool/resource identity propagation**, and it is the architectural crux of
fuse-as-a-service — 0049's loop-authz seams should be shaped to feed it, which is why it
is being designed first (discovered_from 0047's dogfood + 0049's brainstorm).

**The concrete gap (found dogfooding 0047):** fuse's MCP HTTP client
(`internal/mcp/http_client.go`) sends a **single static per-server bearer token for ALL
callers** (config `TokenFile`, oauth2). Every loop, regardless of who started it, presents
the *same* credential downstream — there is no per-loop/per-principal identity. For a
single-tenant local tool that is fine; for a deployed multi-tenant service it is both a
correctness gap and, per the current MCP spec, a **prohibited** pattern (token passthrough
+ missing audience binding = the named confused-deputy vulnerability).

## What changes

Design settled during grooming (full design in the linked spec). An
**identity-propagation egress seam** on every tool/MCP call exchanges the authenticated
loop-initiator's identity for a **short-lived, audience-bound, downscoped** downstream
credential, so the downstream — not fuse — adjudicates:

- **Per-call token exchange (RFC 8693).** At the tool/MCP egress, exchange
  `subject_token` (loop initiator) + `actor_token` (fuse) at an authorization server for a
  new token scoped to the specific target. Use **delegation** semantics (user in `sub`,
  fuse in an `act` claim) so the downstream can distinguish owner from intermediary and
  audit the chain — not impersonation.
- **MCP-spec compliance (rev 2025-11-25).** MCP server = OAuth 2.1 resource server that
  validates audience-bound tokens and enforces its own authz (401/403). fuse must satisfy
  the spec's hard rules: **no token passthrough**, **audience binding via RFC 8707
  Resource Indicators** (`resource` param), and never forward a wrong-audience token. This
  reworks the current single-static-bearer MCP client into a per-caller, per-target model.
- **Credential broker, not a honeypot (CB4A pattern).** fuse holds **no long-lived
  downstream secrets**; a broker mints just-in-time, narrowly-scoped, auditable credentials
  per call and **separates the policy decision point from the credential-dispensing point**
  (validated by HashiCorp Vault's AI-agent OBO pattern and Solo agentgateway's built-in
  RFC 8693 STS). Per-tenant credential isolation; automatic rotation.
- **Root-of-trust constraint (hard).** The principal identity AND every downstream scope
  MUST originate from the authenticated loop-start context and **NEVER** from anything the
  model emits, discovers, or expands — otherwise the same prompt-injection that corrupts a
  call corrupts its scope. No model-derived scopes, ever.
- **Per-call mediation (identity is necessary, not sufficient).** Tools validate
  *credentials*, not *intent*. Enforcement is **per tool call**, not once at loop start;
  "tool exists + schema-valid args" is NOT authorization (complete mediation, Saltzer &
  Schroeder). Intent-binding / per-call mediation remains required on top of propagation.
- **Hosts map their authz in.** The seam lets a consuming system supply the identity /
  credential mapping for its own downstreams (its AS, its scopes, its RLS), rather than
  fuse modeling every downstream role.
- **No secrets in the event stream.** Tokens/credentials must never leak into the 0043/0047
  event stream, model context, logs, or the durable store — a specific redaction constraint
  given fuse persists and replays events.

## Out of scope

- **Loop authz (Plane 1)** — that is change 0049 (authn seam, coarse loop actions,
  tenant/owner, liveness lease, PolicyProvider). 0052 consumes 0049's authenticated
  principal; it does not redo loop-level authz.
- **The networked transport** — change 0048. 0052 rides whatever binding 0048 ships.
- **The client SDK** — change 0050.
- **Modeling downstream systems' permissions** — explicitly rejected; fuse propagates
  identity, downstreams decide.

## Open questions

- **Authorization server dependency.** Does fuse require an external OAuth AS/STS to perform
  RFC 8693 exchange, ship a minimal built-in STS (like agentgateway), or make the STS a
  pluggable seam? What is the local/dev story with no AS?
- **Where the initiator's downstream identity comes from.** The loop-start JWT (0049) proves
  identity to fuse; how does that become a `subject_token` the *downstream's* AS trusts —
  same issuer, token exchange across issuers, or a per-downstream identity mapping the host
  supplies?
- **Delegation vs impersonation per downstream.** Default to delegation (`act` claim); are
  there downstreams that only accept impersonation, and how is that configured per target?
- **Broker placement.** In-process seam vs a separate broker service; how per-tenant
  credential isolation and audit are realized; rotation policy.
- **MCP client rework scope.** How much of `internal/mcp` changes — audience binding, the
  `resource` param, dropping the static per-server bearer, and the fallback for MCP servers
  that are NOT OAuth resource servers (many today are unauthenticated or static-token).
- **On-the-fly-generated tools.** How a dynamically generated tool declares its downstream
  audience/scope so the egress seam can mint the right credential without model input.
- **Per-call mediation mechanism.** What intent-binding looks like concretely (allowlists,
  ceilings, human-approval hooks) and how it composes with fuse's existing approval func.
- **Redaction.** Exact mechanism to keep credentials out of the event stream / logs / model
  context, given fuse's persist-and-replay design.

## Reconcile log

### 2026-08-11 — reconciled against `origin/main` @ 59dbd31

**Scope-adjustable reconcile (not a re-brainstorm).** The design intent — a per-call
identity-propagation egress seam, RFC 8693 delegation exchange behind a pluggable
`TokenExchanger`, the tiered OAuth/static-fallback MCP rework, per-call mediation on the
existing approval gate, and credential redaction from the event stream — is fully intact.
Both deps (#48 networked binding, #49 auth/multi-tenancy) merged to `main` exactly as the
spec anticipated, so several open questions now have concrete, code-grounded answers. No
obsolescence; no fundamental invalidation; no escalation warranted. Auto-capture disabled
this repo (nothing minted).

**Current-reality findings folded in (verified on `origin/main` @ 59dbd31):**

- **Principal source is settled (`internal/loopauth`).** #49/ADR-0034 shipped
  `loopauth.Principal{Tenant event.TenantID, Subject string}` behind a pluggable `Verifier`
  seam, resolved at the Connect edge by `authInterceptor` and read via
  `loopconnect.PrincipalFrom(ctx) (Principal, bool)`. **Key constraint the spec must
  absorb:** the `Principal` carries **identity only, no credential/token** — so the "where
  the initiator's downstream identity comes from" open question is real. For this first PR
  the `subject_token` is derived from the authenticated `Principal` (its `Subject`/`Tenant`)
  via the built-in STS; cross-issuer/external-AS exchange rides the `TokenExchanger` seam's
  external impl, not this PR.

- **The concrete gap is confirmed and located.** `internal/mcp/manager.go:dial()` resolves a
  single token once via `GetAccessToken(srv.Name, srv.URL, srv.Auth)` and bakes it into
  `httpClient.bearerToken` (`internal/mcp/http_client.go`), reused for every caller and every
  call. The connection is per-server (established at server-add), **not** per tool call — so
  per-caller identity requires attaching the credential **per call** (per-request header
  injection at the egress seam), not per connection. This shapes the MCP rework scope: the
  egress seam sits between the tool-call site and the transport `call()`, injecting a
  freshly-minted, audience-bound token per call rather than the static per-server bearer.

- **Approval gate is the mediation point (D5), signature known.**
  `permissions.PermissionGate.Execute(ctx, name, args)` wraps the registry and calls
  `ApprovalFunc(ctx, ApprovalRequest{ToolName, Args, Preview})` per tool call
  (`internal/permissions/gate.go`), invoked from `internal/agent/loop.go`'s tool loop. D5's
  target/scope ceilings extend THIS gate (sourced from the loop-start root of trust), not a
  parallel PDP.

- **Redaction (D6) is structural, not a scrubber.** `tool.call`/`tool.result` events
  (`event.ToolCallPayload.Args` / `ToolResultPayload.Result`, emitted in
  `internal/agent/loop.go` with **no redaction today**) carry model-supplied args/results —
  credentials never flow through them because the minted token lives only in the outbound
  transport header, never in tool args. The build-time verification is a structural assertion
  that no minted credential reaches the event payloads / durable store / logs, plus keeping
  the token out of any error string surfaced back into the loop.

- **Identity threading is the hard cross-cutting seam.** `Principal` is on the Connect-edge
  ctx, but `internal/runtime` is deliberately policy-free (ADR-0030) and does not thread
  identity down to `internal/mcp`. The egress seam therefore carries the propagated identity
  as a **request-context value** threaded from loop-start (the `BuildAgent` factory in
  `cmd/fuse/loop_server.go`) to the MCP `Execute`, mirroring the existing
  `permissions.WithUserMessages(ctx, …)` context-carry precedent — never reconstructed from
  model output (honors the root-of-trust constraint).

- **Composition-root pattern to mirror.** The `TokenExchanger` (and the credential broker
  front) wire at the `cmd/fuse` composition root with the same two-implementation discipline
  as the store/verifier seams (built-in minimal STS as the zero-config default; external-AS
  exchanger behind config), keeping `internal/runtime` free of the seam.

**Scope note for the plan (large change, one PR).** D1–D7 is a broad surface. The plan lands
the coherent architectural spine end-to-end: the egress seam (D1) + pluggable `TokenExchanger`
with the built-in minimal STS (D2) + tiered OAuth/explicit-static MCP rework injecting the
credential per call with `resource`/audience binding (D3) + the broker-shaped no-long-lived-
secret posture and per-tenant isolation (D4) + per-call mediation ceilings on the existing
approval gate (D5) + the redaction constraint + tests (D6) + the ADR (D7). The external-AS
exchanger and a separate out-of-process broker service stay behind their seams as follow-ons,
consistent with the spec's "seam is fixed, placement is open" framing — not new scope cuts,
the spec already scopes them as pluggable.
