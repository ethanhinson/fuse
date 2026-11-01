---
id: 73
slug: 'operator-only-spec-policy-floor-for-client-harness-egress-an'
title: 'Operator-only spec policy floor for client harness egress and models'
status: 'Accepted'
date: '2026-11-01'
supersedes: []
reverses: []
relates_to: [61]
change: 101
---

## Context

Client-submitted harness specs can name MCP server URLs, research.custom endpoints, and models. Without an operator-controlled floor, a client spec could point the runtime at plaintext endpoints, arbitrary hosts, or private-network addresses (SSRF), or select models the operator does not want exposed. ADR-0061 established that client inputs may only tighten, never loosen, operator floors.

## Decision

Client harness specs are held to an operator-only egress/model floor configured as loop_server.spec_policy. MCP and research.custom URLs must use https unless the host is listed in allow_http_hosts; when mcp_hosts is set they must match it; and private-network destinations are denied by default, checked with one DNS resolution at validation time (2s timeout, resolution failure = refusal). This is a validation-time check, not DNS-rebinding protection. Models may be restricted to registry gateway ids (registry-only mode; default allows any). Operator-configured MCP servers are exempt. The policy is trusted-config only (never settable by clients) and is advertised to clients via GetCapabilities SpecPolicy.

## Consequences

Clients get a predictable, discoverable floor and early refusals instead of runtime failures; hostnames resolving to internal addresses are caught at validation. Costs: a DNS lookup on spec validation adds latency and makes validation depend on resolver availability (failures refuse). Not a defense against DNS rebinding between validation and connection; that remains out of scope.

## Alternatives considered

Static-only URL checks without DNS resolution: rejected because they miss public hostnames that resolve to private/internal addresses. Extending web_fetch's existing egress floor to MCP: rejected because MCP and research endpoints have different trust and configuration needs (operator-exempt servers, host allowlists, http exceptions) and coupling them would conflate two policies.
