---
id: 105
slug: 'pin-spec-mcp-server-dials-to-the-addresses-checked-at-startl'
title: 'Pin spec MCP server dials to the addresses checked at StartLoop'
status: 'killed'
priority: 'medium'
type: 'fix'
deliverable: 'pr'
created: '2026-11-01'
updated: '2026-11-07'
depends_on: []
stacked_on:
related: [101]
discovered_from: [101]
adrs: []
spec:
plan:
results:
auto_groomable:
hold:
branch_prefix:
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
<!-- docket:artifacts:end -->

## Why

The private-network deny resolves a spec MCP host once at StartLoop, but the MCP client re-resolves at dial time for the loop's whole life, so a TTL-0 DNS rebind can steer the server's egress to a private address after the check passed.

## What changes

- Re-check every resolved address at dial time in the MCP client's dialer for spec-supplied servers (or pin the dialer to the addresses checked at StartLoop).
- Refuse a dial whose address fails the private-network check, with a tool error.
- Test with a resolver that returns a public then a private address.

## Out of scope

- Operator-configured MCP servers.
- Built-in web tools egress.

## Why killed

Out of scope after the 2026-11-06 refocus: fuse is a coding harness run as one container per task, with no shared server. Guards spec-defined MCP servers; harness specs were removed.
