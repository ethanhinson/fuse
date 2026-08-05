---
id: 27
slug: context-summarization
title: Anchored context summarization at compression threshold
status: proposed
priority: high
type: feat
created: 2026-08-06
updated: 2026-08-06
depends_on: [12]
related: [12]
discovered_from: [12]
adrs: []
spec:
plan:
results:
trivial: false
auto_groomable:
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
<!-- docket:artifacts:end -->

## Why

fuse's context management (change 0012) implements two-tier pruning: at 85% of the context window, old tool results are replaced with stubs; if the provider still rejects, a hard prune retries with a quarter protection budget. This is effective but lossy — stubs discard the content of old tool results entirely. An **anchored summarization** pass at the compression threshold would replace groups of old tool results with an LLM-generated summary structured as Objective / Details / State / Next / Files. This preserves the semantic content of the pruned region at a fraction of the token cost (typically 5-10× compression), and the structured format lets the agent understand what happened without re-running tools. The design is already documented in `docs/designs/context-management.md` as Tier 2 — this change implements it.

## What changes

- **Summarization trigger** in the agent loop (`loop.go`): when context budget reaches 85% (same threshold as the existing stub pruning), instead of (or before) stubbing individual tool results, invoke a **summarization pass** over the candidate region (tool results older than the recency-protected tail).
- **Summarization prompt** using the ODSNF template from the design doc:
  ```
  Objective: what the agent was trying to do
  Details: key findings and intermediate results
  State: current progress and open items
  Next: planned next steps
  Files: files touched and their state
  ```
- **Candidate selection**: summarize tool-result spans in groups (by conversation turn or logical task boundary), not individually. The recency-protected tail (~40k newest tokens + last 2 turns) is never summarized.
- **Summarization model**: configurable via a new `context.summarization.model` config key (default: same as the main model, or a cheaper model if one is configured). The summarizer is itself a bounded agent call (max 2000 output tokens, 30s timeout).
- **Fallback**: if the summarization call fails (timeout, error, empty output), fall back to the existing stub-pruning behavior — summarization is additive, never regressive.
- **Segment store**: the pre-summarization transcript is saved as markdown alongside the session log (`~/.fuse/sessions/.../segments/`) for replay and debugging (see change 0030).
- **Config surface**: new `context.summarization` block in `internal/config/schema.go`:
  ```yaml
  context:
    summarization:
      enabled: true
      model: ""  # empty = use the main model
      threshold: 0.85  # context window fraction
      max_output: 2000
  ```

## Out of scope

- Continuous summarization (every N turns) — threshold-triggered only.
- Summarization of user/assistant messages — tool results only.
- Cross-session summarization — no persistent summary store across sessions.

## Research notes (input for the brainstorm)

The ODSNF template is adapted from Cline's summarization approach and Claude Code's context management. The key empirical finding from the research behind the design doc is that structured templates (Objective/Details/State/Next/Files) produce more actionable summaries than free-form compression — the agent can directly use the "State" and "Next" fields to decide what to do next. The summarizer call itself consumes context tokens (the candidate region + the summarization prompt), so it must run on the full context before pruning — sequence matters: summarize first, then prune the raw content, inject the summary at the boundary of the protected region. Compression ratios vary by content but the design doc estimates 5-10× for typical tool result blobs (read_file outputs, bash results). The biggest risk is that the summary loses information the model needs — the `Next` field mitigates this by explicitly tracking intent, and the segment store ensures the raw data is recoverable.
