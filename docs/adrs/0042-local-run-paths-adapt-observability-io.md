---
id: 42
slug: local-run-paths-adapt-observability-io
title: Local run paths adapt observability I/O to the terminal, not the server posture
status: Accepted
date: 2026-08-14
supersedes: []
reverses: []
relates_to: [40]
change: 61
---

## Context

Change #0051 built the observability stack and wired it into exactly one entry point, the
`loop-serve-net` Connect server. Change #0061 extended the same `newObservability` service to the
three LOCAL entry points — `fuse shell` (interactive TUI), one-shot `fuse <task>`, and the research
probe — via a shared `setupLocalObservability` helper in `cmd/fuse/shell.go`.

Two properties of the server posture do not survive the move to a local process, and both were
discovered in review rather than at design time:

1. `newObservability(ctx, cfg, w)` uses its writer argument as the structured-log sink whenever
   `observability.logging.enabled` is true and `logging.output` is not `file` (observability.go,
   `var sink io.Writer = stdout`). `fuse shell` hands that same stdout to bubbletea
   (`tea.NewProgram(..., tea.WithOutput(stdout))`). Passing stdout through would stream JSONL into
   the alt-screen the TUI is painting, corrupting the display for the whole session. The server has
   no TUI and so cannot hit this.

2. The metrics scrape endpoint supports `metrics.access: authenticated`, which wraps the handler in
   `authenticatedHTTP(verifier, h)`. A local entry point has no operator-auth verifier and passes
   nil, and `authenticateHTTP` returns `ErrInvalidToken` unconditionally for a nil verifier. Binding
   anyway produced a listener that succeeded at startup, printed no warning, and 401'd every scrape
   forever. `authenticated` is the value used throughout the repo's existing config examples, so
   this was the likely misconfiguration rather than an exotic one.

## Decision

A local entry point owns its terminal I/O and has no operator-auth identity, so observability adapts
to the local process rather than mirroring the server:

- The shell passes `stderr` as the observability log sink, never the writer bubbletea owns.
  `setupLocalObservability`'s first writer parameter is named `logSink` and documented as "must not
  be a writer another component owns exclusively."
- When `metrics.bind` is set and `metrics.access` is `authenticated` while no verifier is available,
  the local entry point SKIPS `startMetricsEndpoint` entirely and warns on stderr, rather than
  binding an unserviceable listener. Local runs that want a scrape endpoint must set
  `access: public` (acceptable because the bind is loopback).
- Both follow the change's existing settled rule that observability never breaks the primary tool:
  these are warn-and-continue paths, never startup failures. `loop-serve-net` is unchanged — it
  supplies a real verifier and legitimately uses `authenticated`.

## Consequences

- Enables telemetry on the local run paths without the shell's display being corrupted and without a
  silently-dead metrics endpoint.
- Costs: `access` semantics now differ by entry point, which a reader of the config schema alone
  would not predict — this ADR is that signpost. A local operator who wants authenticated local
  metrics has no path short of supplying a verifier, which is deliberately deferred (shell metrics
  auth is an explicit non-goal of #0061).
- `config.Validate` still accepts `access: authenticated` globally; the refusal is at the local
  caller, not in validation, so a config shared between a server and a local run stays valid for
  both.
