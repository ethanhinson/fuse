---
id: 62
slug: 'getcapabilities-advertises-from-the-enforcement-code-and-car'
title: 'GetCapabilities advertises from the enforcement code and carries a wire tenant for spoof refusal'
status: 'Accepted'
date: '2026-10-31'
supersedes: []
reverses: []
relates_to: [60, 61]
change: 86
---

## Context

Clients starting loops from a HarnessSpec need to discover models, skills, tighten-only ceilings, secret refs and MCP auth kinds. The spec wanted an empty request with PermissionDenied "through the same loopauth path" as StartLoop, but the only PermissionDenied that path emits is the wire-tenant spoof check (loopconnect.Handler.effectiveTenant); issuer refusals are Unauthenticated.

## Decision

(1) GetCapabilitiesRequest carries `string tenant = 1` with StartLoopRequest.tenant semantics - when set it must match the authenticated principal or the call is PermissionDenied; filtering always uses the principal's tenant, never the field; the Go SDK fills it from Credentials.Tenant.

(2) Every part of the answer is derived from the same code StartLoop enforces with: ceilings from the (fuse.harness.v1.ui).tighten_only descriptor options read through the shared operatorLimit accessor; MCP auth kinds from one shared specAuthAllowed predicate; secret names through the same `available` rule ResolveSecret uses; models from the operator registry (excluding cli/ routes and advertise:false), with effective context window and effective max output tokens (entry value, else operator top-level max_tokens). A golden test drives StartLoop with specs built from the advertisement. Ceilings are strict: a spec value must be positive and strictly below the advertised value.

## Consequences

Advertisement cannot drift from enforcement; host-restricted secrets are listed (hosts still constrain delivery); undeclared MCP auth enum numbers are now refused as operator-only.

## Alternatives considered

Empty request (cannot produce PermissionDenied); hand-maintained ceiling table (drifts silently); querying gateways live for model metadata (out of scope, slow, costly).
