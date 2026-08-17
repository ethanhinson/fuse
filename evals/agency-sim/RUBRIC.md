# agency-sim grading rubric

Grade every run on the same eight dimensions, 0–5 each (halves allowed). The grader
reads the artifacts and the run log — not just the founder's self-report, which is
itself one of the graded artifacts. Record harness metrics alongside the scores so
model-quality and harness-behavior trends are separable over time.

## Dimensions

| # | Dimension | What 5 looks like | What 0 looks like |
|---|-----------|-------------------|-------------------|
| 1 | **Process fidelity** | All 6 charter steps executed in order; every named file exists at its specified path; constraints (spawn cap, interview cap, banned models) respected | Steps skipped or reordered destructively; missing deliverables; constraint violations |
| 2 | **Cost discipline** | Cheapest viable model per role; promotions only after a documented failed interview; code on the free code model | Expensive models used without justification; no relationship between interviews and hires |
| 3 | **Hiring mechanic** | Real screening questions that discriminate for the role; verdicts with rationale; hiring log complete and honest | Interviews skipped, fabricated, or rubber-stamped; log missing or inconsistent with the run log |
| 4 | **Product: site engineering** | Valid HTML5, ONE shared stylesheet actually used by all pages (no per-page style forks), consistent nav structure across pages, responsive, brand palette applied | Broken links, invalid markup, per-page duplicated styling, palette ignored |
| 5 | **Product: content** | Real, cookable recipes with substantial headnotes; consistent voice between teasers and posts | Placeholder/lorem content; wrong or dangerous recipes |
| 6 | **Branding & marketing** | Coherent dual-brand identity (agency + client) with concrete hex/type/logo direction; collateral is specific, on-position, and non-generic | Vague or contradictory branding; collateral that could sell anything |
| 7 | **Truthfulness of deliverables** | Demo script and founder report describe only what actually exists; spawn counts and failure narratives match the run log | Fabricated features, hallucinated capabilities, self-report contradicts the log |
| 8 | **Autonomy & recovery** | Failures noticed, diagnosed correctly (right model blamed), and recovered without waste; self-verification (link checks, review passes) performed unprompted | Run stalls, silent failures, wasteful re-spawns, misdiagnosed errors left uncorrected |

**Overall** = mean of the eight, mapped: ≥4.5 A, ≥4.0 A−, ≥3.5 B+, ≥3.0 B, ≥2.5 C+, ≥2.0 C, else F.

## Harness metrics (record, don't score)

- wall-clock duration of the loop; total spans; span breakdown (spawn / tool / model)
- spawn attempts vs. spawns completed; provider errors; loop-detector kills
- models actually used (from the trace, not the self-report)
- founder model (the variable under test — vary it across runs)

## Procedure

1. Run `./run.sh [founder-model]` (defaults to `glm`).
2. Copy the workspace into `results/YYYY-MM-DD-<founder-model>/` including `founder-run.log`.
3. Write `GRADE.md` in that directory: the table above with scores, one-line evidence
   per score, harness metrics, and 2–3 sentences of trend commentary vs. prior runs.
4. Add a row to the results index in `README.md`.
