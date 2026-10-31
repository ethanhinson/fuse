---
id: 63
slug: 'max-turns-is-a-per-exchange-cap-in-interactive-loops'
title: 'max_turns is a per-exchange cap in interactive loops'
status: 'Accepted'
date: '2026-10-31'
supersedes: []
reverses: []
relates_to: []
change: 90
---

## Context

Interactive (persistent conversational) loops run many user exchanges over one loop. Previously max_turns was forced to 0 for them, which silently dropped a configured spec value, while treating it as a whole-run cap would end the conversation once the cumulative turn count was reached. Separately, token usage was accumulated only by the TUI renderer, so session and pool token ceilings did not trip in other hosts.

## Decision

In interactive loops, max_turns caps the turns spent answering one user message, not the whole run. When the cap is hit the loop emits loop.parked with reason "max_turns" and parks instead of returning ErrMaxTurns; the next Send gets a fresh allowance. Unset stays uncapped: a binding's headless backstop is dropped for interactive loops, while an explicitly configured cap is kept. Non-interactive loops are unchanged and still return ErrMaxTurns on a whole-run cap. Token usage accounting moves from the TUI renderer into the Agent via SetUsageSink, wired to the tree node, so session and pool token ceilings trip in every host.

## Consequences

A configured max_turns is honored for interactive loops without ending the session; a runaway exchange parks and waits for the user. Hosts no longer need to reimplement usage accounting for budgets to apply. Interactive and non-interactive loops now interpret max_turns differently, which callers must keep in mind.

## Alternatives considered

Whole-run cap for interactive loops: rejected because it ends the session after a cumulative turn count. Keep forcing max_turns to 0: rejected because it silently drops the spec value.
