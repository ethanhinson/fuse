---
id: 60
slug: 'harnessspec-proto-is-the-portable-harness-definition-client'
title: 'HarnessSpec proto is the portable harness definition - client-settable surface only, public packages, protovalidate shape plus Go semantic validation'
status: 'Accepted'
date: '2026-10-30'
supersedes: []
reverses: []
relates_to: [6, 33]
change: 84
---

## Context

A fuse harness (models, skills, MCP servers, permission posture, agent, workflow and throughput brakes, context management, research settings) was defined only by internal/config's YAML loader. Clients of the runtime other than the local CLI - the Go SDK, the TS SDK, a remote caller of the networked binding - had no portable, versioned, IDL-defined way to build, serialize or validate one. config.Config also mixes that client-settable surface with operator-only trusted settings that ADR-0006 keeps away from lower-trust sources (loop_server credentials and issuers, tool_identity signing keys, observability, host paths, host processes). Validation rules lived in internal/config and in cmd/fuse (package main), neither importable from another Go module, and reported only the first problem as a flat error string with no field location.

## Decision

1. The protobuf package fuse.harness.v1 (proto/fuse/harness/v1) defines HarnessSpec, the portable harness definition, generated with the existing buf setup into committed Go and TS stubs like fuse.loop.v1 (ADR-0033).
2. HarnessSpec covers only the client-settable surface of config.Config: models, skills as name references (SkillRef), remote MCP servers (http, sse, streamable-http), permissions and auto mode, orchestrator, agents, workflows, throughput, context, pipeline caps, max_turns, max_tokens, research. Operator-only surfaces are absent from the type so a spec cannot express them: gateway, loop_server, tool_identity, observability, skill_paths, projects, permissions.auto.write_roots, stdio MCP servers (command, env) and auth.token_file, and sandbox substrate selection. Secret-bearing fields hold only secret NAMES (client_secret_ref, HeaderValue.secret_ref), resolved by the runtime. Any new config key must be classified client-settable or operator-only when added, and defaults to operator-only (absent) - the same fail-safe rule ADR-0006 applies to permission keys.
3. Field options (fuse.harness.v1.ui / value_ui) carry presentation metadata for clients that render a spec as a form: label, help, widget, a secret_ref marker and a tighten_only marker (the ADR-0006 tighten-only axes). They carry no validation semantics.
4. Validation is two layers. Shape rules are protovalidate annotations in the proto (buf.build/bufbuild/protovalidate), including a few CEL rules (unique MCP server names, the reserved model alias default, no literal value for credential-named headers, no literal using the mapping's reserved secret-ref: prefix), evaluated from the same descriptors in Go and TS, so both reject the same input with the same field paths and rule ids. Semantic rules stay in internal/config (Config.Problems) and internal/configcheck (model-alias resolution) and are reached by mapping the spec into config.Config - never duplicated. The validator reports every violation with a spec field path; a field flagged by both layers is reported once, with the shape violation.
5. internal/config remains the authority for semantics: the spec-to-config mapping renders the spec as the loader's YAML and resolves it through the loader's own trusted merge (config.FromYAML).
6. Generated Go stubs live at github.com/ethanhinson/fuse/harness/v1 and the validator library at github.com/ethanhinson/fuse/harness - public paths - because a client in another Go module must import them. The config mapping stays internal (internal/harnessconfig) because its signatures name config.Config.
7. HarnessService.ValidateHarness is defined and a Go handler (harness.Service) is provided, but fuse mounts it on no binding until a later change needs it.

## Consequences

Enables: one schema is the single source of a harness definition for every language; any client can build, serialize and validate a spec offline with the runtime's own rules; a form renderer can be driven entirely from descriptors; the ADR-0006 trust boundary is enforced by the type for anything built from a spec. The CLI and YAML loader are unchanged (Validate keeps its first-problem contract and exact text).
Costs: the spec must be kept in step with config.Config by hand (a descriptor test fails if an operator-only key appears, and round-trip tests catch mapping drift, but a new client-settable config key is not added to the spec automatically); the spec inherits the loader's 0-means-unset convention, so a numeric cannot be set to an explicit 0 where the loader cannot either; protovalidate adds a buf dependency (buf.lock), a Go dependency (buf.build/go/protovalidate with cel-go) and an npm dependency (@bufbuild/protovalidate); some range rules exist in both the proto and internal/config (the shape copy is what lets TS reject them), deduplicated at report time.
Given up: stdio MCP servers and host-path settings cannot come from a spec; an operator configures them on the runtime host.

## Alternatives considered

- JSON Schema generated from config.Config: no shared IDL with the existing Connect wire, no generated types, and validation would be a third rule source.
- Exposing config.Config itself (moving internal/config public): would publish operator-only fields and the loader's YAML quirks as API, the opposite of encoding the trust boundary.
- Re-implementing the semantic rules in protovalidate CEL only: duplicates internal/config's rules and lets the CLI and spec validators drift; cross-field and registry-dependent rules (model aliases) do not fit CEL.
- Putting the stubs under internal/ like fuse.loop.v1: unimportable from another Go module, which defeats the purpose.
- Mounting HarnessService on loop-serve-net now: no consumer yet; deferred to the change that makes the runtime accept a spec.
