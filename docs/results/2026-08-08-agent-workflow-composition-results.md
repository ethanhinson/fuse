<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0026 — Workflow composition — chain, fan-out, and conditional routing](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0026-agent-workflow-composition.md)**
<!-- docket:backlink:end -->

# Pipeline composition — results

Change: #0026 · Branch: feat/agent-workflow-composition · PR: <set at PR open> · Plan: docs/superpowers/plans/2026-08-08-agent-workflow-composition-plan.md · ADRs: ADR-0014

## Verify (human)

Automated tests cover the parser (YAML/JSON parity, `on_error` forms, colon-in-prompt),
the Tarjan validator + caps, total condition routing over every operator, the
readiness-driven parallel engine (chain/diamond concurrency, `hits/*` fanout glob,
`on_error` modes, `expects` structured-value landing + mismatch degradation, real
conditional routing incl. skip-propagation), the bounded synthesizer (re-ask loop +
fail-loud), config caps (tighten-only), the `pipeline_run` tool (mode inference), wiring
at all three cloned child builders, gateway-seam e2e (authored + synthesized), the
slot-yield-under-tight-cap deadlock regression, and a TUI screenshot e2e. Full
`go test ./... -race` is green.

Surfaces worth a live look at the merge gate:

- [ ] **Live synthesized pipeline against a real gateway.** From a `fuse shell`, call
      `pipeline_run` with a `{goal: "..."}` and a small model. Confirm the trace shows the
      synthesis call first (its request carries the `Pipeline` schema as `expects`), then
      the step spawns in dependency order, `pipeline.plan` holds the generated DAG, and
      `pipeline.status` is written. A `{definition: <yaml>}` call must skip synthesis and
      spawn directly.
- [ ] **Conditional routing branches.** Author a pipeline whose first step writes a
      status key and declares `conditions` + `default`; confirm the taken branch runs and
      the non-taken branch (and its downstream) is skipped (outputs absent).
- [ ] **Slot pressure.** Invoke `pipeline_run` from within a spawned child under a tight
      `max_concurrent` (`.fuse.local.yml`); confirm the pipeline completes (no deadlock) —
      the calling agent yields its slot around the run.

## TUI screenshot evidence

A `teatest` screenshot e2e (`internal/tui/pipeline_tui_e2e_test.go`,
`TestTUI_PipelineRunRoundTrip`) drives `pipeline_run` through the live `ShellModel` and
captures the settled transcript via `captureFrame` (env-gated `FUSE_SCREENSHOT_DIR`,
`FinalModel().View()` + forced `termenv.TrueColor`, `freeze` PNG best-effort). The frame
shows the `pipeline_run` call, the `pipeline research-chain: completed` result node, and
the final reply — visual confirmation the feature renders end-to-end. Regenerate with:
`FUSE_SCREENSHOT_DIR=/tmp/shots go test ./internal/tui/ -run TestTUI_PipelineRunRoundTrip`.

## Findings & deviations

- **Conditional routing execution semantics → ADR-0014.** Routing is wired into
  execution (not inert): a router releases its chosen target and skips the others; a skip
  propagates to `depends_on` downstream; routing is total (bad condition = false, never an
  error). v1 join semantics are conservative — a step is skipped if ANY `depends_on` was
  skipped, so a diamond that re-joins after a branch is not expressible in v1 (documented
  in the engine `Run` doc comment; a richer join is a future change if needed). This was
  the top review finding (routing was initially computed-but-discarded) and is recorded in
  ADR-0014.
- **`pipeline_run` yields the calling agent's scheduler slot (fix).** Review caught that
  running a pipeline from a depth≥1 child would hold that child's pool slot while the
  pipeline's step spawns queued for the same pool — the `slot-cap-yield-while-blocked-on-children`
  deadlock. Fixed by wrapping `pipeline.Run`/`Synthesize` in `sched.YieldSlot`/`UnyieldSlot`
  at the tool seam (matching `spawn_agent`), with a deadlock regression test under a tight cap.
- **`on_error: fail` now cancels in-flight siblings (fix).** A failing step derives a
  cancellable run ctx and cancels it so sibling spawns stop early rather than running to
  completion with discarded results.
- **`name` param is v1-unsupported (deviation).** `pipeline_run` advertises `name` for a
  future registered-pipeline lookup, but there is no pipeline registry yet — a `name`-only
  call returns a clear error directing the caller to `definition` or `goal`.
- **`confirm` gate is a no-op headless (deviation).** `confirm` is parsed and plumbed, but
  headless (no interactive confirmer) synthesis proceeds regardless, so an autonomous
  caller is never blocked (spec open-question resolved to false-default / proceed-headless).
- **Skill-layer degrade.** The plan/build/review/finish role skills
  (`superpowers:*`) were not installed on the build machine; per the docket skill-layer
  missing-skill rule the run degraded each to `auto` (inline authoring / subagent-driven
  build + review) — noted in the PR body.

## Follow-ups (not filed — auto-capture disabled this run)

- Skill-frontmatter `pipelines:` block (a skill body replaced by a declared DAG) — the
  spec's first deferral (`discovered_from: 26`).
- TUI step sub-nodes (per-step glyphs / elapsed under a pipeline root) — the spec's second
  deferral (`discovered_from: 26`).
- Richer join-after-branch semantics for conditional routing (ADR-0014 consequence).
