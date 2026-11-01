<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0101 — Operator spec policy: MCP host allowlist, private-network deny, registry-only models](../changes/archive/2026-11-01-0101-operator-spec-policy-mcp-host-allowlist-private-network-deny.md)**
<!-- docket:backlink:end -->

# Operator spec policy results

**Human action:** Optional: set `loop_server.spec_policy` on any shared server; no action needed to merge.

## Outcome

Added `loop_server.spec_policy` (trusted config only): `mcp_hosts`, `deny_private_networks` (default true), `allow_http_hosts`, `models: any|registry-only`. `harnessresolve.Resolve` applies it to `mcp_servers[*].url`, `research.custom.url` and spec model ids with `fuse.operator_only` violations at the field path; `GetCapabilities` advertises `SpecPolicy` (proto field 7, stubs regenerated, SDK mapped). Operator MCP servers are exempt. ADR-0073 records the decision. Docs: `docs/deploying.md`.

## Verification performed

`make test` green at the final head via the gate driver. Standard-tier review: two important findings. Registry-only now enforces the same registry set GetCapabilities advertises (fixed, with a test); the untrusted-config drop is now tested. DNS resolution in tests is stubbed; no live traffic was used.

## Known issues and follow-ups

- `ValidateHarness` (the public, non-server-aware `harness.Service`) does not apply the policy, although the spec asked for it. Clients see the refusal at `StartLoop` only. Practical impact: no pre-flight refusal for these fields. Confirmed. Suggested next action: add a server-aware validate path. No existing change checked against the backlog for this item.
- Private-network resolution happens once at validation, so DNS rebinding is not covered (declared out of scope).
