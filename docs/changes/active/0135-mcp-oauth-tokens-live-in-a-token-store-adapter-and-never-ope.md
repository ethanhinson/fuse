---
id: 135
slug: 'mcp-oauth-tokens-live-in-a-token-store-adapter-and-never-ope'
title: 'MCP OAuth tokens live in a token store adapter and never open a browser on a server'
status: 'proposed'
priority: 'high'
type: 'fix'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-03'
depends_on: []
stacked_on:
related: [89, 105]
discovered_from: []
adrs: []
spec: 'docs/superpowers/specs/2026-11-03-mcp-oauth-tokens-live-in-a-token-store-adapter-and-never-ope-design.md'
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
| Artifact | Link |
|---|---|
| Spec | [2026-11-03-mcp-oauth-tokens-live-in-a-token-store-adapter-and-never-ope-design.md](../../superpowers/specs/2026-11-03-mcp-oauth-tokens-live-in-a-token-store-adapter-and-never-ope-design.md) |
<!-- docket:artifacts:end -->

## Why

Operator-configured OAuth2 MCP servers keep their tokens in a file under the replica's home directory and, when the token is missing, run an interactive browser flow synchronously inside loop start and resume. On a fleet that means per-replica tokens, refresh-token races between replicas, and a two-minute hang followed by the server being skipped whenever a loop lands on a replica without the file.

## What changes

A `TokenStore` interface with the file implementation and a Postgres implementation (encrypted at rest with the operator key), a single-flight refresh across replicas, a headless mode where a missing token is an immediate, explicit error, and an operator command to seed tokens. Conformance suite; a two-process refresh race test.

## Out of scope

OAuth for spec-defined MCP servers (still refused); dynamic client registration changes beyond storing the result in the store.
