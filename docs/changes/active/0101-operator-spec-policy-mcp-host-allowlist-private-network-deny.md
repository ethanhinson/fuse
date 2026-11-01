---
id: 101
slug: 'operator-spec-policy-mcp-host-allowlist-private-network-deny'
title: 'Operator spec policy: MCP host allowlist, private-network deny, registry-only models'
status: 'in-progress'
priority: 'high'
type: 'feat'
deliverable: 'pr'
created: '2026-11-01'
updated: '2026-11-01'
depends_on: []
stacked_on:
related: []
discovered_from: []
adrs: []
spec: 'docs/superpowers/specs/2026-11-01-operator-spec-policy-mcp-host-allowlist-private-network-deny-design.md'
plan:
results:
auto_groomable:
hold:
branch_prefix:
branch: 'feat/operator-spec-policy-mcp-host-allowlist-private-network-deny'
pr:
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-11-01-operator-spec-policy-mcp-host-allowlist-private-network-deny-design.md](../../superpowers/specs/2026-11-01-operator-spec-policy-mcp-host-allowlist-private-network-deny-design.md) |
<!-- docket:artifacts:end -->

## Why

ADR-0061 made the operator config a tighten-only floor, but two client-settable surfaces still widen it. A spec's `mcp_servers` replace the operator list and may point at any URL (`internal/harnessconfig/harnessconfig.go:190-211`), so a client of a networked server can make it open connections to loopback, link-local or RFC-1918 addresses on the server's own network, and `research.custom.url` is the same hole. A spec's `models.entries[*].id` may name any gateway model id, and the request is paid for by the operator's gateway credential. The `web_fetch` tool already has an SSRF floor (`internal/permissions/fetchhost.go`); MCP and research egress have none.

## What changes

- `LoopServerConfig.SpecPolicy` (`loop_server.spec_policy`): `mcp_hosts []string` (hostname patterns, `*.example.com` allowed; empty = any public host), `deny_private_networks bool` (default true: refuses literal IPs and hostnames resolving to loopback, link-local, RFC-1918, unique-local, and the `.local`/`.internal`/`localhost` names), `allow_http_hosts []string` (hosts where `http://` is accepted; otherwise `https` only), `models: registry-only | any` (default `any`).
- `harnessresolve.Resolve` applies the policy to `mcp_servers[i].url` and `research.custom.url` with `RuleOperatorOnly` violations on the field path, and to `models.entries{alias}.id` under `registry-only` (the id must equal the gateway id of an advertised operator registry model). Checks are static (no DNS) except `deny_private_networks`, which resolves the hostname once with a 2-second timeout and refuses on any private answer; a resolution failure is a violation.
- `ValidateHarness` reports the same violations so a client sees the refusal before `StartLoop`.
- `GetCapabilitiesResponse` gains `SpecPolicy {mcp_hosts, deny_private_networks, models_registry_only}` so a client can show the rule.
- Operator-configured MCP servers in the trusted config are exempt (they are the operator's own).
- ADR recording: spec policy is an operator-only floor; private networks denied by default; registry-only models.

## Out of scope

routing `web_fetch` through the same policy (it has its own floor); per-tenant policies; DNS rebinding protection beyond one-time resolution (file as later); changing the default for `models` away from `any`.

## Reconcile log

### 2026-11-01

Unchanged against origin/main 3567c07; code paths (harnessresolve.Resolve, config.LoopServerConfig, GetCapabilitiesResponse) match the spec.
