<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0101 — Operator spec policy: MCP host allowlist, private-network deny, registry-only models](../../changes/archive/2026-11-01-0101-operator-spec-policy-mcp-host-allowlist-private-network-deny.md)**
<!-- docket:backlink:end -->

# Operator spec policy: MCP host allowlist, private-network deny, registry-only models

- Type: feat
- Priority: high

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

## Acceptance criteria

- Default config: a spec with `mcp_servers[0].url: http://127.0.0.1:8080` or `https://10.0.0.5/` or `https://metadata.internal/` is refused by `ValidateHarness` and `StartLoop` with a violation at `mcp_servers[0].url` and `RuleOperatorOnly`; `https://mcp.example.com/` passes (test resolver stubbed to a public address).
- `mcp_hosts: ["*.example.com"]`: `https://api.example.com/` passes, `https://example.org/` is refused; `research.custom.url` follows the same rule at its own path.
- `allow_http_hosts: [localhost]` plus `deny_private_networks: false` accepts `http://localhost:9000/` (the local dev case) and still refuses `http://169.254.169.254/`.
- `models: registry-only` with registry `{fast: {id: gw/fast}}`: a spec entry `{id: gw/fast, max_tokens: 100}` passes; `{id: gw/other}` is refused at `models.entries{x}.id`; with `models: any` both pass (existing tests unchanged).
- `GetCapabilities` returns the configured policy; a client-visible doc section describes each key.
- Operator-configured `mcp_servers` with a private URL still start (exemption test). `make test` green.
