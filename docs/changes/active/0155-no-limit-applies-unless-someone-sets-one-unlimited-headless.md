---
id: 155
slug: 'no-limit-applies-unless-someone-sets-one-unlimited-headless'
title: 'No limit applies unless someone sets one: unlimited headless turns and no assumed context window'
status: 'in-progress'
priority: 'high'
type: 'fix'
deliverable: 'pr'
created: '2026-11-11'
updated: '2026-11-11'
depends_on: []
stacked_on:
related: [38, 27, 147]
discovered_from: []
adrs: [83]
spec: 'docs/superpowers/specs/2026-11-11-no-limit-applies-unless-someone-sets-one-unlimited-headless-design.md'
plan: 'docs/superpowers/plans/2026-11-10-no-limit-applies-unless-someone-sets-one-unlimited-headless.md'
results:
auto_groomable:
hold:
branch_prefix:
branch: 'fix/no-limit-applies-unless-someone-sets-one-unlimited-headless'
pr:
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-11-11-no-limit-applies-unless-someone-sets-one-unlimited-headless-design.md](../../superpowers/specs/2026-11-11-no-limit-applies-unless-someone-sets-one-unlimited-headless-design.md) |
| Plan | [2026-11-10-no-limit-applies-unless-someone-sets-one-unlimited-headless.md](../../superpowers/plans/2026-11-10-no-limit-applies-unless-someone-sets-one-unlimited-headless.md) |
| ADRs | [ADR-0083](../../adrs/0083-an-unconfigured-context-window-is-unknown-and-is-learned-fro.md) |
<!-- docket:artifacts:end -->

## Why

Two defaults cap a run that nobody asked to cap. A headless run with max_turns unset stops at 100 turns, and a model with no context_window is treated as a 128,000 token model. In an 11 hour unattended factory run on Claude models with much larger windows, reviewer runs died six times with "conversation context too large: ~135k tokens against a 128k window" because the window was never configured, and every caller has to write max_turns: 0 to get an unlimited headless run. A harness should not invent a limit. Explicit limits stay available for anyone who wants one.

## What changes

- Headless entry points (one-shot run, non-TTY, mcp-server, research-probe) run with no turn cap when max_turns is unset, the same as the interactive shell. An explicit max_turns still caps every context.
- The doom-loop detector remains the protection against a stuck headless run.
- A model with no configured context_window has an unknown window. fuse does not prune, summarize or abort against a guessed size.
- When the provider rejects a request for context length and the window is unknown, fuse learns a working window from the size of the rejected request, prunes and retries, and manages context against the learned window for the rest of the run.
- A configured context_window behaves exactly as today.
- The models editor and docs stop describing a 128k default.

## Out of scope

- Removing the opt-in settings max_turns, max_run_tokens, max_run_duration or max_spawns.
- Per-tenant limits for hosted loops.
- Asking a gateway or provider API for a model's context window.
- Changes to the pruning and summarization algorithms themselves when the window is known.

## Reconcile log

### 2026-11-11

Unchanged against main 1d57c6e. headlessTurnBackstop, SetMaxTurnsExplicit, DefaultContextWindow and the 128k TUI and doc text are all still present as the spec describes.
