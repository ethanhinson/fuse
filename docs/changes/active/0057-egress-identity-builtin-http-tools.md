---
id: 57
slug: egress-identity-builtin-http-tools
title: Egress identity for built-in HTTP tools — route web_fetch/web_search through the #52 credential seam
status: proposed
priority: medium
type: feat
created: 2026-08-11
updated: 2026-08-11
depends_on: [52]
related: [49, 55]
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

Change #52 built the identity-propagation **egress seam** (`toolidentity.Broker.CredentialFor(ctx, principal, target)`) and the per-call RFC 8693 delegation exchange, but wired it into exactly **one** production call site: `MCPTool.Execute` (`internal/mcp/tool.go`). Every other tool that reaches an external system still presents **no per-principal identity** downstream.

Concretely, in a deployed multi-tenant service today: two different tenants both calling `web_fetch` (or `web_search`) present **whatever credentials the fuse process holds**, with no loop-initiator identity in `sub`/`act`. If a fetch target is an authenticated endpoint, either everyone shares one process credential — the exact confused-deputy pattern #52 killed for MCP — or the target is unauthenticated. `web_fetch` does have a real SSRF/host floor (`internal/permissions/fetchhost.go`) that denies loopback/RFC-1918/link-local and honors deny/blocklist globs, but that is **invoke-time authorization** (should this call happen?), not **downstream authn** (on whose behalf?). The identity story is silently MCP-only.

`#52`'s seam was deliberately designed as the single choke point so that *any* egressing tool routes through it — the built-in HTTP tools are additional call sites on an existing seam, not new architecture. This change closes that gap so "fuse propagates the loop initiator's identity to external systems" is true for the built-in HTTP tools, not just MCP.

## What changes (proposal altitude — design in brainstorm)

Route the built-in HTTP-egressing tools — **`web_fetch` and `web_search`** — through the same `toolidentity` egress seam #52 established, so each carries a per-principal, audience-bound credential (or the explicit identity-free static/anonymous tier) rather than an ambient process credential.

Likely scope to settle in design:

- **Target declaration for a non-MCP tool.** MCP tools declare a downstream `Target` (audience + required scope) statically; `web_fetch` takes an arbitrary model-supplied URL. Settle how a fetch target's audience/`resource` is derived **without trusting model output** for scope (e.g. per-host declared targets from config, an anonymous tier for undeclared hosts, and how that composes with the existing SSRF/host floor). The root-of-trust constraint from #52 (scope never from model output) holds.
- **`web_search` credentialing.** `web_search` hits a search-provider API — likely a static provider credential (identity-free tier) rather than delegated per-user identity, made explicit and auditable per #52's D3 tier model, not a silent process default.
- **Composition-root wiring.** Extend the `tool_identity` wiring (`cmd/fuse/tool_identity.go`) to hand the `CredentialSource` to the built-in HTTP tools the same way `mcp.WithCredentialSource` does, and seed the principal on the run context for these tools.
- **Redaction + audit parity.** The #52 redaction (credentials never enter the event stream / durable store / logs) and the per-mint audit record must cover these tools too.

## Out of scope

- **`bash` containment** — a fundamentally different problem (no declared target; the model gets a shell). Tracked separately as change #58.
- **`codeindex` / subprocess tools** — local subprocess, no downstream authz surface; not egress in the #52 sense.
- **The external-AS exchanger / cross-issuer exchange / broker-as-separate-service** — deferred behind #52's fixed seams; this change consumes whatever exchanger is wired, it does not add new exchange topology.
- **Loop-server → MCP end-to-end wiring** — the other #52 follow-up (MCP is not attached to the loop-server binding); orthogonal to built-in-tool coverage.

## Note

Filed 2026-08-11 while reviewing #52 (PR #55) before merge. #52's seam is sound and spec-complete for its declared scope (MCP was the priority gap — the standards-prohibited static-bearer pattern); this change extends the same seam to the built-in HTTP tools so the deployed identity posture is not silently MCP-only. Needs a brainstorm to settle non-MCP target declaration and the `web_search` tier before it is build-ready.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
