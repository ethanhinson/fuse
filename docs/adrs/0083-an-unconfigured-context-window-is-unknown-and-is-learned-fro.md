---
id: 83
slug: 'an-unconfigured-context-window-is-unknown-and-is-learned-fro'
title: 'An unconfigured context window is unknown and is learned from provider length rejections'
status: 'Accepted'
date: '2026-11-11'
supersedes: []
reverses: []
relates_to: []
change: 155
---

## Context

fuse used to assume a 128k context window (DefaultContextWindow) for any model without context_window, which killed healthy long runs on larger-window models ("~135k tokens against a 128k window"). It also capped headless runs at 100 turns when max_turns was unset.

## Decision

- Unset max_turns is unlimited in every entry point; the doom-loop detector (and opt-in max_turns, max_run_tokens, max_run_duration) protects headless runs.
- ContextWindow == 0 means unknown: no proactive pruning, summarization trigger, output-cap clamp, or ErrContextTooLarge.
- On a provider context-length rejection with an unknown window, the loop learns a window and then prunes, summarizes and aborts against it with the normal 85%/95% thresholds. The learned size is prompt estimate + the output cap actually sent (providers like OpenAI-compatible/vLLM count prompt + max_tokens); it falls back to the prompt alone when the cap is at or below the 4096 floor or the same prompt is refused again (provider counts prompt only). With a learned window the output cap is clamped against 95% of it so the retry is strictly smaller. A learned window only ever goes down, which guarantees termination.
- The learned window is not written to config. It is per agent loop, except the interactive shell keeps it for the session per model alias (reset on /model switch), so each exchange does not pay a fresh rejection. spawn_agent children never share it. A configured context_window always wins and keeps the old single-retry behavior.
- A new event kind context.window.learned (event.KindContextWindowLearned) records each learn, rather than reusing context.summarize, whose payload feeds the resume fold.

## Consequences

First overflow costs one rejected request (two for prompt-only-counting providers with a cap above 4096). A stuck headless run making varied tool calls is no longer stopped at 100 turns. Operators should still set context_window when known.

## Alternatives considered

Keep a 128k default: invents a limit, the original bug. Query the gateway/provider for the window: out of scope, not universally available. Learn from prompt only: under-learns badly when a raised output cap caused the rejection. Keep learned window strictly per Run: the shell would pay a rejection on most exchanges after the first overflow. Reuse KindSummarize: corrupts resume.
