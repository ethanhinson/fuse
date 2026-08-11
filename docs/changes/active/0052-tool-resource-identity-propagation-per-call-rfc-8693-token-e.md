---
id: 52
slug: tool-resource-identity-propagation-per-call-rfc-8693-token-e
title: Tool/resource identity propagation — per-call RFC 8693 token exchange to downstream MCP/APIs
status: proposed
priority: medium
type: feat
created: 2026-08-11
updated: 2026-08-11
depends_on: []
related: []
discovered_from: [49]
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

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
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

To be designed during grooming. The research-backed leading direction: an
**identity-propagation egress seam** on every tool/MCP call that exchanges the
authenticated loop-initiator's identity for a **short-lived, audience-bound, downscoped**
downstream credential, so the downstream — not fuse — adjudicates. Sketch:

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
