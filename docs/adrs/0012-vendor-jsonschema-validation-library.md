---
id: 12
slug: vendor-jsonschema-validation-library
title: Vendor a JSON-Schema validation library for structured-delegation result validation
status: Accepted
date: 2026-08-08
supersedes: []
reverses: []
relates_to: []
change: 24
---

## Context

Change 0024 (structured delegation) adds an optional `expects` JSON Schema to spawn_agent: the parent declares the shape it wants a child agent's result to take, and on return the spawner validates the child's output against that schema. The design (spec 2026-08-08-structured-delegation-design.md, decision D2) called for FULL JSON-Schema validation — nested object types, arrays-of, enums, and type checks — not a shallow key-presence check, so that a "matched expected schema" signal to the parent model is trustworthy. Before this change, fuse's go.mod vendored no JSON-Schema library. Two options: hand-roll a shallow validator, or take a dependency on a real JSON-Schema engine.

## Decision

Vendor `github.com/santhosh-tekuri/jsonschema/v6` (pinned at v6.0.3; transitively pulls golang.org/x/text v0.31.0) and use it for the full-fidelity validation of a child agent's JSON output against the parent-supplied `expects` schema. The dependency is scoped to result validation in internal/agent/schemavalidate.go only. Validation is preceded by lenient JSON extraction (stripping markdown fences / surrounding prose) to avoid false negatives on well-formed data with bad wrapping. A validation failure NEVER fails the spawn — it degrades to raw text plus a model-facing note and a labeled tree event — so the dependency governs only the match/mismatch classification, not control flow. Note: jsonschema/v6's ErrorKind.LocalizedString panics on a nil message.Printer, so a fixed English printer is passed.

## Consequences

- Enables real schema fidelity (nested/array/enum/type mismatches are each caught), which the design's fidelity tests guard and which a hand-rolled shallow check could not provide.
- Adds an external dependency (and one transitive, golang.org/x/text) to a previously dependency-light module; version is pinned; the surface is isolated to one file.
- The library is the sole validator; future schema features (formats, $ref) come "for free" from the engine rather than needing hand-maintenance.
- Cost: a supply-chain surface and a small binary-size increase, accepted deliberately for validation fidelity per spec decision D2.
