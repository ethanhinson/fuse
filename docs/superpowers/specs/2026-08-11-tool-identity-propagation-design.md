<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0052 — Tool/resource identity propagation — per-call RFC 8693 token exchange to downstream MCP/APIs](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/archive/2026-08-11-0052-tool-identity-propagation.md)**
<!-- docket:backlink:end -->

# Spec 0052 — Tool/resource identity propagation: per-call RFC 8693 token exchange to downstream MCP/APIs

## Problem

fuse is becoming a hostable multi-tenant agent-loop runtime. Change 0049 designs **loop
authz (Plane 1)**: who may start/send/observe/replay/administer *loops*, fuse-owned and
coarse (tenant=org, owner=user, cross-tenant admin). But fuse also decides what **tools**
exist and can generate tools on the fly, and when a loop calls a tool that reaches an
**external system** — an MCP server, an HTTP API, a database with row-level security —
fuse must **not** model that system's permissions. It must carry the **loop-initiator's
identity** to the downstream and let the downstream enforce *its own* authorization. This
is **Plane 2**, and it is the architectural crux of fuse-as-a-service; it is designed
before 0049 so 0049's identity seams are shaped to feed it cleanly.

**The concrete gap (found dogfooding 0047).** fuse's MCP HTTP client
(`internal/mcp/http_client.go`) sends a **single static per-server bearer token for ALL
callers** (config `TokenFile`, oauth2). Every loop, regardless of who started it, presents
the *same* credential downstream — no per-loop/per-principal identity. For a single-tenant
local tool that is acceptable; for a deployed multi-tenant service it is both a correctness
gap and, per the current MCP spec, a **prohibited** pattern (token passthrough + missing
audience binding = the named confused-deputy vulnerability).

## Research basis (settled inputs, do not re-litigate at build)

A deep-research pass (adversarially verified, 3-0 votes) established the standards-grounded
direction; the load-bearing, build-relevant conclusions:

- **RFC 8693 OAuth 2.0 Token Exchange** is the canonical mechanism to convert an
  intermediary-held identity into a downstream credential per call: present a
  `subject_token` (the party on whose behalf the request is made) at the AS token endpoint
  with `grant_type=urn:ietf:params:oauth:grant-type:token-exchange`, receive a token whose
  subject inherits that identity, scoped via `audience`/`scope`/`resource`.
- **Delegation, not impersonation.** Issue tokens with the loop initiator in `sub` and fuse
  in an **`act` claim**, so the downstream distinguishes resource owner from intermediary
  and can audit the chain. Impersonation (same `sub`, no actor) is not the default.
- **MCP spec rev 2025-11-25** formalizes fuse's intuition: a protected MCP server is an
  **OAuth 2.1 resource server** that validates access tokens and enforces its own authz
  (401 invalid / 403 insufficient_scope); the MCP client is an OAuth 2.1 client acting on
  behalf of a resource owner. It **prohibits** three things fuse's current client would
  tempt: (1) **no token passthrough** — an MCP server calling upstream acts as a separate
  client and must not forward the token it received; (2) **audience binding via RFC 8707
  Resource Indicators** — clients send `resource` identifying the target; servers reject
  wrong-audience tokens; (3) forwarding a wrong-audience token is the confused-deputy vuln.
- **Broker, not honeypot (CB4A pattern, IETF draft; HashiCorp Vault OBO; Solo
  agentgateway's built-in RFC 8693 STS).** fuse should hold **no long-lived downstream
  secrets**; a broker mints just-in-time, narrowly-scoped, auditable credentials per call
  and **separates the Policy Decision Point from the Credential Dispensing Point**.
- **Root of trust outside the model (hard constraint).** The principal identity AND every
  downstream scope MUST originate from the authenticated loop-start context and NEVER from
  anything the model emits, discovers, or expands — else the injection that corrupts a call
  corrupts its scope.
- **Identity propagation is necessary but NOT sufficient.** Tools validate *credentials*,
  not *intent*; enforcement is **per tool call** ("tool exists + schema-valid args" is not
  authorization — complete mediation, Saltzer & Schroeder). Per-call intent mediation
  remains required on top of propagation.

## Design

### 1. The identity-propagation egress seam (D1)

Introduce an **egress seam** invoked on every outbound tool/MCP call that may reach an
external system. Conceptually:

```
CredentialFor(ctx, principal Principal, target Target) (Credential, error)
```

- `principal` is the **authenticated loop initiator** from 0049 (identity + tenant + owner),
  threaded from loop-start context — never reconstructed from model output.
- `target` names the downstream (MCP server / API), including its **audience/resource
  identifier** and required scopes, declared by the tool definition (D5), not the model.
- `Credential` is a short-lived, audience-bound token (or an explicit static fallback, D3).

The seam is the single choke point where identity becomes a downstream credential; it is
where the RFC 8693 exchange (D2), the broker (D4), and redaction (D6) attach.

### 2. Token exchange via a pluggable STS seam (D2)

Define a **`TokenExchanger` seam** performing the RFC 8693 exchange:
`subject_token` = the loop initiator's identity, `actor_token` = fuse, requesting an
**audience-bound, downscoped** token for `target` using **delegation** (yielding an `act`
claim). Two provided implementations, selected at the composition root (the same
seam-with-two-implementations discipline as 0047's store):

- **Minimal built-in STS** for local/dev and single-issuer cases (mints delegation tokens
  fuse's own downstreams trust). Keeps a zero-config local story.
- **External-AS exchanger** that delegates the exchange to a configured OAuth AS/STS
  (Keycloak/Auth0/Vault/agentgateway) for production — the recommended posture where an AS
  already owns identity.

The exchanger is a seam so a host can plug its own; fuse does not hard-depend on one AS.

### 3. Tiered downstream auth: OAuth path + explicit static fallback (D3)

Not every MCP server is an OAuth resource server today (many are unauthenticated or
static-token). 0052 is **tiered**, per-server explicit config:

- **OAuth-capable target** → full RFC 8693 path (D2): audience-bound delegated token, the
  downstream enforces.
- **Legacy/static target** → an **explicit, per-server static credential** may be
  configured. It is never a silent default, and the loop-initiator identity is **never
  carried into** a static credential (a static cred is fuse-as-client, not delegation) —
  so a legacy server can still be reached without falsely claiming per-user identity. The
  config makes the weaker posture visible and auditable.

This is the pragmatic bridge while the MCP ecosystem adopts the 2025-11-25 auth model,
without blocking existing servers.

### 4. Credential broker — no long-lived secrets in fuse (D4)

The seam is fronted by a **broker** that mints JIT, narrowly-scoped, auditable credentials
per call and **separates the policy decision from credential dispensing** (CB4A):

- fuse holds no long-lived downstream secrets; credentials are short-lived and rotated.
- **Per-tenant credential isolation** — a tenant's exchange/credential material is never
  reachable by another tenant (the same app-enforced boundary as 0047's store keying).
- Every mint is **audited** with the `(principal, tenant, owner, target, scope, act-chain)`
  so a cross-process operation is traceable — pairing with 0047's `node_id`/trace hooks and
  0051's observability.

Whether the broker is an in-process component or a separate service is an open question
(below); the **seam** is fixed regardless.

### 5. Per-call mediation via fuse's existing approval gate (D5)

Identity propagation is necessary but not sufficient. 0052 makes the **per-call mediation
point fuse's existing `ApprovalFunc`/permissions gate** (already invoked per tool call),
**extended** — not a parallel mechanism — to enforce, from the **loop-start root of trust**:

- **Scope/target ceilings and allowlists** per principal/tenant (what downstreams and scopes
  a loop may ever reach), sourced from config/0049 grants, never from model output.
- **Tool→target declaration.** A tool (including an on-the-fly-generated one) declares its
  downstream **audience + required scope** as part of its definition, so the egress seam
  mints the right credential **without model input**. A tool that reaches an undeclared
  target is denied.
- **Human-approval hooks** compose as today (the gate already supports approve/deny),
  giving an intent-binding surface over credentialed calls.

This closes the confused-deputy gap that identity alone leaves open: the gate is the
complete-mediation checkpoint on every access to authority.

### 6. Credential redaction from the event stream (D6)

fuse **persists and replays** events (0043/0047). Tokens/credentials must **never** enter
the event stream, the durable store, logs, or model context. 0052 adds a **redaction
constraint** at the seam: credentials live only in the outbound transport call; the emitted
`tool.call`/`tool.result` events carry the *fact* of the call and its target/scope, never
the secret. This is a named build-time verification, not an afterthought.

### 7. ADR bookkeeping

A new ADR records the decision: **tool/resource authz is delegated to downstreams via
per-call RFC 8693 delegation (act-claim) token exchange at a pluggable egress seam, with a
credential broker and per-call mediation on fuse's approval gate**, superseding the
implicit "static per-server bearer" model of the current MCP client. Recorded at build via
`docket-adr` (like 0047's ADR-0031). It should explicitly state the MCP-spec constraints it
honors (no passthrough, audience binding) so the implementation cannot silently regress
them.

## Verification

- **Per-caller identity reaches the downstream.** Two loops started by *different*
  principals calling the same OAuth-capable MCP target present **distinct**, audience-bound,
  delegated tokens (user in `sub`, fuse in `act`) — not one shared bearer. A test asserts
  the tokens differ by principal and carry the `act` chain.
- **Downstream adjudicates, fuse does not.** A target that rejects a principal (403
  insufficient_scope) results in a distinguishable tool error surfaced to the loop; fuse
  makes no allow/deny authz decision of its own for the resource.
- **Audience binding enforced.** A token minted for target A is rejected by target B
  (RFC 8707); fuse never forwards a wrong-audience or passthrough token (assert no
  passthrough on the MCP-server-calls-upstream path).
- **No model-derived scope.** A test proves the principal identity and the target
  audience/scope come from loop-start context + tool declaration, and that model output
  cannot expand scope or redirect the target (attempt an injected over-broad scope; assert
  it is ignored/denied).
- **Per-call mediation blocks undeclared targets.** A tool call to a target not on the
  principal's allowlist is denied at the approval gate even though the tool exists and args
  are schema-valid (complete-mediation test).
- **Legacy fallback is explicit and identity-free.** A statically-configured legacy MCP
  server is reachable, does NOT carry initiator identity, and is flagged in config/audit as
  the weaker tier (no silent static default).
- **No credential leakage.** Assert that no emitted event, durable-store record, log line,
  or model-context message ever contains a token/credential (redaction test over the 0043
  event stream and the 0047 durable store).
- **Broker holds no long-lived secret.** Structural assertion that fuse's process/config
  carries no long-lived downstream credential for OAuth targets — only the exchange seam +
  short-lived minted tokens.
- **ADR recorded** for the identity-propagation seam and the MCP-spec constraints honored.

Any live-model verification uses a **non-Anthropic** local gateway (project policy); the
load-bearing checks here are auth/token-flow and mediation, not model quality, so live-model
turns are minimal.

## Dependencies

- **Depends on 0049** (loop authz) — 0052 consumes 0049's authenticated `Principal`
  (identity + tenant + owner) as the `subject_token` source; it does not redo loop authz.
- **Depends on 0048** (networked binding) — the authenticated loop-start context 0052 threads
  to the egress seam arrives over 0048's transport; 0052 rides whatever binding 0048 ships.
- **Related:** 0043/0047 (event stream + durable store — redaction + audit hooks), 0050
  (client SDK — how clients supply identity), 0051 (observability — the audit/trace surface).
- **Reworks** `internal/mcp` (drop the single static per-server bearer as the default;
  add audience binding / `resource`, the tiered fallback, and the egress seam hook).

## Open questions (resolve at build reconcile)

- **AS dependency & local/dev story.** Exact shape of the built-in minimal STS vs the
  external-AS exchanger; the no-AS local path; which AS products are first-class.
- **Cross-issuer identity.** How the loop-start JWT (0049) becomes a `subject_token` the
  *downstream's* AS trusts — same issuer, token-exchange across issuers, or a host-supplied
  per-downstream identity mapping.
- **Delegation vs impersonation per target.** Default delegation (`act`); which downstreams
  require impersonation and how that's configured per target.
- **Broker placement.** In-process component vs separate service; concrete per-tenant
  credential isolation, audit, and rotation mechanics.
- **MCP client rework scope.** Precisely how much of `internal/mcp` changes; behavior for
  MCP servers that are neither OAuth nor intended for static creds.
- **On-the-fly-generated tools.** How a generated tool declares its downstream
  audience/scope so the egress seam mints the right credential with no model input.
- **Per-call mediation surface.** Exact allowlist/ceiling/intent-binding vocabulary on the
  extended approval gate and how it composes with existing human-approval flows.
- **Redaction mechanism.** The concrete redaction implementation across event stream, durable
  store, logs, and model context.

## Assumptions

Judgment calls made in place of asking (build reconcile may adjust):

- **Delegation is the default** (`act` claim), impersonation the per-target exception —
  follows the research's audit-chain recommendation.
- **The egress seam is the single choke point** for identity→credential; on-the-fly tools
  and static tools both route through it (static via the explicit fallback tier).
- **Per-call mediation reuses the existing approval gate** rather than a new PDP — chosen for
  composition with fuse's current per-tool-call permissions, extended with
  target/scope/ceiling checks sourced from the loop-start root of trust.
- **`tenant_id` isolation extends to credential material** the same app-enforced way 0047
  keys the store — not a backend-specific isolation feature.
