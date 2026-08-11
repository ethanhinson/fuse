---
id: 36
slug: tool-authz-delegated-downstream-rfc8693-egress-seam
title: Tool/resource authz delegated to downstreams via per-call RFC 8693 delegation token exchange at a pluggable egress seam
status: Accepted
date: 2026-08-11
supersedes: []
reverses: []
relates_to: [30, 31, 34]
change: 52
---

## Context

fuse is a hostable multi-tenant agent-loop runtime. Loop authz (change #49, ADR-0034) established WHO may drive a loop — a bearer token resolves to a `loopauth.Principal{Tenant, Subject}` at the Connect edge. But when a loop calls a tool that reaches an EXTERNAL system (an MCP server, an HTTP API, a DB with row-level security), fuse must not model that system's permissions.

Before #52, fuse's MCP HTTP client (`internal/mcp/http_client.go`) resolved a SINGLE static per-server bearer token once at dial time (`GetAccessToken` → `httpClient.bearerToken`) and presented it for EVERY caller and every call — no per-loop/per-principal identity. For a deployed multi-tenant service that is both a correctness gap and, per MCP spec rev 2025-11-25, a prohibited pattern (token passthrough + missing audience binding = the confused-deputy vulnerability).

## Decision

Delegate tool/resource authorization to the downstream by carrying the loop initiator's identity per call, via an identity-propagation EGRESS SEAM. Concretely:

- A `CredentialSource` seam (`internal/toolidentity`) is the single choke point turning an authenticated `Principal` + a config-declared `Target` (audience/scopes/tier) into a short-lived downstream `Credential`, invoked per tool call at the MCP egress.
- Token exchange is RFC 8693 DELEGATION (not impersonation): the loop initiator in `sub`, fuse in an `act` claim, audience-bound (RFC 8707 `resource`) and scope-downscoped. A pluggable `TokenExchanger` seam backs it; a built-in minimal HS256 STS is the zero-config default (keeping a local story), with per-tenant signing-key isolation; an external-AS exchanger slots in behind the same seam.
- A credential `Broker` fronts the seam: fuse holds no long-lived downstream secret for OAuth targets (short-lived minted tokens), per-tenant isolation, one token-free audit record per mint. Downstream auth is TIERED per server: an OAuth-exchange tier (per-call delegation) and an EXPLICIT static-credential fallback tier that carries NO initiator identity (never a silent default) so legacy MCP servers still work.
- Per-call complete mediation extends fuse's EXISTING approval gate (`internal/permissions`) with a `TargetMediator` seam: a tool reaching a target not on the principal's allowlist / beyond its scope ceiling is denied even for schema-valid args, terminally and independent of permission mode (Saltzer & Schroeder complete mediation). Ceilings/targets come from the loop-start root of trust and config, NEVER from model output.
- Root-of-trust constraint (hard): the `Principal` AND every target audience/scope originate from the authenticated loop-start context and the tool declaration — never from anything the model emits. The principal is threaded via an unforgeable, unexported context key.
- Redaction (D6): the minted credential lives ONLY in the outbound transport header — never in `tool.call`/`tool.result` events, the durable store, logs, model context, or error strings. Credential formatting redacts the token.

This supersedes the implicit "single static per-server bearer" model of the pre-#52 MCP client, and explicitly honors the MCP-spec constraints: no token passthrough, audience binding via RFC 8707, never forward a wrong-audience token.

## Consequences

**Enables:** per-caller identity reaches downstreams so THEY adjudicate (fuse makes no authz decision of its own for the resource); a deployed multi-tenant service can carry each user's identity to MCP/APIs; richer exchangers (external AS, cross-issuer) and a separate broker service drop in behind the fixed seams.

**Costs / limits:** this PR wires the seam into the single-user shell path only (one-shot and loop-server bindings do not attach MCP tools today), so the multi-tenant loop acceptance is proven at the egress-seam level rather than through a loop-server MCP path that does not yet exist; the built-in STS is single-tenant on the CLI/shell path (`DefaultTenant` only) with an explicit follow-up TODO for multi-tenant loop-server wiring; external-AS exchange, per-target impersonation, and cross-issuer exchange remain behind their seams as follow-ons.

**Relates to:** ADR-0034 (loop authz / `Principal` this consumes), ADR-0030 (the policy-free runtime seam this preserves — identity travels as a ctx value from the composition root, never a runtime import), and ADR-0031 (durable event store + loop registry; app-enforced tenancy extended here to credential material).
