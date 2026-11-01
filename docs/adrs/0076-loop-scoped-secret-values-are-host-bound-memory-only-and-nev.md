---
id: 76
slug: 'loop-scoped-secret-values-are-host-bound-memory-only-and-nev'
title: 'Loop-scoped secret values are host-bound, memory-only, and never persisted'
status: 'Accepted'
date: '2026-11-01'
supersedes: []
reverses: []
relates_to: [75, 73, 61]
change: 102
---

## Context

Change 0102 lets a StartLoop caller supply secret values for the loop's harness spec. Operator-configured loop_server.secrets already exist (ADR-0073..0075, ADR-0061 context); client-supplied values add a new trust boundary: a value must never reach a host its sender did not intend, must never be recorded durably (loop config, events, logs), and must not let a client override or redirect an operator secret.

## Decision

Loop-scoped secret values on StartLoopRequest.secrets (field 7, LoopSecret{name,value,hosts}) are a transport-level, memory-only channel.

- Host-bound: a value resolves only for a destination whose canonical hostname (lower case, trailing dot stripped; one shared canonicalizer) is in its required hosts; empty hosts is a violation.
- Precedence: operator loop_server.secrets entries win; the client entry is tried only when the operator refuses.
- No source mixing: a name never mixes sources across refs in one spec (overlay substitutes by name, so mixing would send an operator value to a client-chosen host or vice versa); such a name is refused at every ref.
- Authorization and limits: sending secrets requires the secrets:inline scope (unrestricted principals pass), checked right after loop:start and before limits and tenant admission; limits are 32 entries and 16 KiB per value, rejected with InvalidArgument, never echoing a value.
- Transport: values travel via the stdlib-only leaf package internal/loopsecret on the resolution context, not persisted in LoopConfig. The runtime clears LoopConfig.Secrets right after resolution so Deps.LoopContext and the loop never hold them; loopsecret.Secret redacts itself in every fmt/slog/JSON rendering; loop.harness records names only.

## Consequences

- A resumed loop has no values, so a spec relying on a loop-scoped secret fails re-resolution (fail closed).
- Secrets sent without a harness spec are dropped.
- GetCapabilities does not list loop-scoped names.
- Values cannot leak through persisted config, events, or logs; the cost is that resumption requires the client to restart rather than continue with secrets.

## Alternatives considered

- Persisting encrypted values: rejected; adds key management and durable exposure for a per-run credential.
- A pluggable remote SecretResolver: deferred to later work.
- Adding values to the harness spec: rejected; the spec is recorded durably.
- Loop-scoped precedence over operator entries: rejected; lets a client shadow operator credentials.
- Unbound (hostless) values: rejected; a value could be sent to any destination.
