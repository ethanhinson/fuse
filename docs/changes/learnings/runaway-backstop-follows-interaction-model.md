---
name: runaway-backstop-follows-interaction-model
title: A runaway backstop calibrated for one interaction model becomes a bug when the interaction model changes — and budget posture is not approval posture
promotion_state: candidate
changes: [38]
created: 2026-08-06
updated: 2026-08-06
topics: [architecture, agents, safety, ux]
---

A safety limit tuned for how users interact today silently misfires when a new mode changes that. The agent loop's 25-turn cap was fine while every step needed human approval — 25 was generous. The moment auto mode enabled long unattended runs, the same cap became the thing that killed the first real long run ("agent: max turns reached"). 0038 retired the interactive cap (unlimited in the shell, generous headless backstop, doom-loop detection by *shape* rather than count). The review also caught that **budget posture and approval posture are independent axes**: `fuse "task" --approve-all` on a TTY resolved unlimited turns AND an auto-approving loop hook — a doom loop auto-continued forever. Fix: `--approve-all` takes the headless *budget* posture (backstop + abort on loop) even though it's the *approval* footgun.

**Why:** Limits encode assumptions about the interaction they guard. Survey of peers confirmed the direction (Claude Code has no interactive cap; Cline shipped one and removed it; OpenCode detects loops by repetition, not count) — a blunt counter punishes the productive runs it can't distinguish from stuck ones.

**How to apply:** When adding a new interaction mode (autonomous, headless, batch), re-derive every existing limit against it — don't inherit the old default. Keep *budget* (how long may it run) and *approval* (may it act unattended) as separate resolved axes; a flag that sets one must not silently set the other. Prefer loop-*detection* (N identical calls) over turn-*counting* for catching stuck agents. Related: [[live-control-reads-state-at-decision-point]].
