# agency-sim — a fun end-to-end harness/model eval

One prompt, a whole company. A founder agent gets a charter and must: write a
strategy, author agent profiles for a C-suite + ICs, **hire models by interviewing
them** (cheapest-first, promotion only on a failed screen), then dispatch the hired
team to ship a real product — a cooking-brand website with a working blog — plus a
demo script, branding, and marketing collateral. All inside one fuse loop, under a
spawn budget, with Claude aliases banned and token cost as an explicit pressure.

It exercises, in one run: multi-model `spawn_agent` fan-out, per-spawn system
prompts, the blackboard, file tools, web_search, the tool-call loop detector,
provider-error recovery, and the founder model's planning/delegation/honesty. Which
is to say: it's a load test that grades the harness and the models at the same time.

- **Prompt:** [PROMPT.md](PROMPT.md) (charter below the `---`; header explains wiring)
- **Grading:** [RUBRIC.md](RUBRIC.md) — 8 dimensions × 0–5, plus recorded harness metrics
- **Run it:** `./run.sh [founder-model]` — defaults to `glm`; refuses claude aliases

The founder model is the main variable to sweep over time. Keep the rubric stable;
note any prompt changes in the log below (a prompt change starts a new comparability
window).

## Prompt log

- **v1** (2026-08-17): initial charter, as run ad-hoc.
- **v2** (2026-08-17, current): added the demo-honesty rule ("the demo may describe
  ONLY what the shipped site actually contains") after the v1 run's demo script
  hallucinated videos/animations. Dimension 7 existed in v1 grading regardless.

## Results

| Date | Founder | Prompt | Overall | Notes |
|------|---------|--------|---------|-------|
| 2026-08-17 | glm | v1 | **B+ (3.7)** | Clean process + cost floor; demo script fabricated features; per-page style forks. [GRADE.md](results/2026-08-17-glm/GRADE.md) |
