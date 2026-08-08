<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0026 — Workflow composition — chain, fan-out, and conditional routing](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0026-agent-workflow-composition.md)**
<!-- docket:backlink:end -->

# Implementation plan — Pipeline composition (change 0026)

Deterministic DAG execution over the subagent runtime: a new `internal/pipeline/`
package (types + YAML/JSON parser + Tarjan-validated DAG + readiness-driven parallel
engine + fixed-operator conditional routing + per-step `on_error` + bounded runtime
synthesis), plus a `pipeline_run` built-in tool wired into all three cloned child
builders, plus synthesis caps in `internal/config`.

Spec: `docs/superpowers/specs/2026-08-08-agent-workflow-composition-design.md` (read
from the metadata branch). Reconcile verified **no drift** — all substrate APIs
(`agent.Blackboard`, `agent.SpawnOpts.Expects`, `agent.Spawner`/`ChildBuilder`,
`config.WorkflowConfig`/`PoolConfig`, the three `cmd/fuse` wiring sites, the
gateway-seam e2e harness) are present exactly as the spec assumes.

## Method

TDD throughout (the build role runs `subagent-driven-development`: focused failing
test → implement → per-task review → commit). Each task below states its **tests
first**, then the implementation, then the acceptance bar. The `internal/pipeline`
engine is exercised deterministically with a **fake spawner** (a `ChildBuilder`
returning scripted results) — no real LLM. The `cmd/fuse` wiring and the synthesis
path are proven end-to-end at the **`LLM_GATEWAY_URL` scripted-double seam**
(`verify-tool-loop-at-gateway-seam` learning) because the tool is wired in
`cmd/fuse`, which no unit test reaches. TUI evidence is captured via
`internal/tui/harness_test.go`'s `captureFrame` (`FinalModel().View()` +
`termenv.TrueColor`, `FUSE_SCREENSHOT_DIR`-gated, freeze best-effort —
`teatest-final-frame-via-finalmodel-view` learning).

**Import direction (spec decision 1):** `internal/pipeline` MAY import
`internal/agent` and `internal/tools`; the reverse must never hold — the scheduler
must not learn about pipelines. `pipeline_run` lives in `internal/tools` (like
`spawn_agent`) and takes the pipeline engine via a narrow function seam so
`internal/tools` does not import `internal/pipeline` either; the `cmd/fuse` wiring
composes them.

## Task 1 — `Pipeline`/`Step`/`Condition` types + YAML/JSON parser

**Tests first** (`internal/pipeline/parse_test.go`):
- YAML → `Pipeline` round-trips every field (`name`, `workflow`, steps with
  `worker`, `prompt`, `inputs`, `outputs`, `depends_on`, `fanout`, `expects`,
  `on_error`, `conditions`, `default`).
- JSON → `Pipeline` yields an identical value (YAML/JSON parity).
- `on_error` string forms parse: `fail`, `skip`, `retry(3)` → an `ErrorPolicy`
  value; `retry(0)`/malformed → parse error naming the step.
- `expects` survives as `json.RawMessage`/decoded doc, unmodified.
- The YAML free-text-scalar trap (`yaml-plain-scalar-colon-space` learning): a
  `prompt:` containing `": "` must parse — cover a prompt with an embedded colon.

**Implement** (`internal/pipeline/pipeline.go`, `parse.go`): the `Pipeline`,
`Step`, `Condition`, `ErrorPolicy` types from the spec's data model; `Parse([]byte)
(*Pipeline, error)` accepting YAML or JSON (detect/try both). No validation yet
(Task 2) beyond what parsing forces.

**Acceptance:** every field round-trips from both YAML and JSON; `on_error` forms
parse; malformed `on_error` errors with the step name; colon-in-prompt parses.

## Task 2 — Validator: uniqueness, reference resolution, operators, Tarjan cycle detection, caps

**Tests first** (`internal/pipeline/validate_test.go`):
- Duplicate step name → error naming the dupe.
- Dangling `depends_on` / `goto` / `default` (references a non-existent step) →
  error naming the offending step + missing target.
- Invalid `op` (not in `exists|eq|ne|gt|lt|contains|matches`) → error.
- **Cycle detection (Tarjan):** a 2-node cycle, a 3-node cycle, and a self-loop
  each rejected; a valid diamond DAG accepted.
- `goto` edges participate in cycle detection alongside `depends_on` (a routing
  cycle is still a cycle).
- `Caps` enforcement (synthesis path): over-`max_steps`, over-`max_fanout` (per
  step), over-`max_depth`, and missing pool binding each rejected with a clear
  message; a within-caps pipeline passes. `Caps{}` (all zero) = no cap enforced
  (authored path uses layers 1–2 with an empty/relaxed cap set — decision 6).

**Implement** (`internal/pipeline/validate.go`): `Validate(*Pipeline, Caps) error`
running (a) step-name uniqueness, (b) reference resolution over `depends_on`,
`goto`, `default`, (c) operator validity, (d) Tarjan SCC cycle detection over the
combined dependency+routing edge set, (e) the `Caps` checks (skipped when a cap
field is 0). Error messages name the offending step(s).

**Acceptance:** all invalid shapes rejected with named-step diagnostics; valid DAGs
(chain, diamond, fan-out) pass; caps reject over-limit synthesized DAGs and are
inert at zero.

## Task 3 — Condition evaluation over the blackboard (fixed operator set, total routing)

**Tests first** (`internal/pipeline/condition_test.go`), evaluating each operator
against a real `agent.Blackboard` (or a small store fake):
- `exists` true/false by key presence.
- `eq`/`ne` on strings, numbers, bools (JSON-value equality).
- `gt`/`lt` numeric; a **non-numeric side → false, never an error** (both orders).
- `contains` — string substring AND array membership.
- `matches` — regex match; an invalid regex → false, not an error.
- **Order + default:** first matching condition wins; `default` taken when none
  match; with no `default`, falls through to normal `depends_on` successor
  ordering. An `op`/type mismatch is a false condition (routing is total, never
  errors).

**Implement** (`internal/pipeline/condition.go`): `evalCondition(Condition, *agent.Blackboard) bool`
and a `route(step, bb) (nextStepName string, ok bool)` helper. Reads keys via
`Blackboard.Get`. Coercions per the spec (§3): numeric coercion for gt/lt,
substring/membership for contains, regexp for matches. No panics on any input.

**Acceptance:** every operator behaves per spec including all type-mismatch →
false paths; ordering and default resolution correct.

## Task 4 — Engine: readiness-driven parallel execution, blackboard I/O, `on_error`, fan-out

**Tests first** (`internal/pipeline/engine_test.go`), driving the engine with a
**fake `ChildBuilder`** (scripted per-step results; no real LLM) wired through a
real `agent.Spawner` + `agent.AgentTree` + `agent.Blackboard`:
- **Readiness ordering:** a chain A→B→C runs in order; a diamond A→{B,C}→D runs B
  and C concurrently after A, D after both (assert observed spawn order / a
  concurrency-witness counter).
- **Blackboard I/O:** a step's `inputs` keys are injected into its prompt
  (template substitution — `{{key}}`); its result is written to each `outputs`
  key; a downstream step reads them. `hits/*` glob round-trip: `fanout: N` writes
  N glob-namespaced keys, a downstream `inputs: [hits/*]` collects all N (via
  `Blackboard.Keys("hits/*")`).
- **Fan-out:** `fanout: 3` ⇒ exactly 3 spawns for that step.
- **`on_error`:** `fail` stops the pipeline and sets `pipeline.status`=failed with
  the offending step; `skip` records the failure, leaves outputs absent, continues
  (a downstream `op: exists` sees it absent); `retry(2)` re-spawns up to twice
  before falling through to fail.
- **Deadlock guard (`slot-cap-yield-while-blocked-on-children`):** a pipeline
  whose steps saturate the pool while the engine's driver goroutine waits must not
  deadlock — the engine must not itself hold a scheduler slot while blocking on
  step spawns (the engine drives spawns; it is not itself a pool member, but the
  regression test fills the pool with fan-out steps that each spawn and asserts
  completion). Confirm the engine yields/does not charge a slot for the
  coordinator.
- **`expects` integration:** a step with an `expects` schema lands the validated
  structured value on the output key (via `AgentHandle.Result()`); a schema
  **mismatch degrades** (raw text on the key + note) and is **not** a step error
  (`on_error` untouched).
- **Terminal status:** `Run` returns a terminal `Status` and writes
  `pipeline.status` to the blackboard.

**Implement** (`internal/pipeline/engine.go`): `Run(ctx, *Pipeline, *agent.Spawner,
*agent.Blackboard) (Status, error)`. Topological, readiness-driven: repeatedly find
steps whose `depends_on` are satisfied and not yet run, launch each ready step as a
goroutine (one `Spawner.Spawn` call per instance, `fanout` ⇒ N), collect results,
write `outputs`, evaluate `conditions` to route, honor `on_error`. Prompt inputs
resolved from blackboard keys (glob-expanded) and substituted before spawn.
`expects` threaded into `SpawnOpts.Expects`; structured value taken via
`AgentHandle.Result()` (falling back to `Wait().Result` text on
`ErrNoStructuredResult`). Writes `pipeline.status`.

**Acceptance:** all engine behaviors above green under `-race`; the deadlock
regression completes; structured/mismatch/`on_error`/fan-out/routing all correct.

## Task 5 — Synthesizer: bounded, self-correcting DAG generation (decision 5+6)

**Tests first** (`internal/pipeline/synthesize_test.go`), with a **scripted
synthesis reply** (a fake spawner/handle returning a canned structured `Pipeline`
value, since synthesis is itself a spawn declaring the `Pipeline` schema as its 0024
`expects`):
- A well-formed reply → a `Pipeline` that parses+validates and is returned.
- A cyclic/dangling/over-cap reply → the re-ask loop fires, feeding the validation
  error back; a subsequent well-formed reply succeeds.
- The loop **fails loudly** after `max_attempts` (returns a synthesis-failure
  error, never an invalid DAG).
- Caps enforced on the synthesized DAG (reuses Task 2 `Validate(_, Caps)`): each of
  over-steps/over-fanout/over-depth/missing-pool rejects (then re-asks).

**Implement** (`internal/pipeline/synthesize.go`): `Synthesize(ctx, goal string,
ctxInfo SynthContext, sp *agent.Spawner, caps Caps) (*Pipeline, error)`. Runs the
synthesis spawn (system prompt = goal + `Pipeline` schema + available
workers/tools; `SpawnOpts.Expects` = the `Pipeline` JSON schema), then
`Parse`→`Validate(_, caps)`; on rejection, re-spawn with the error appended, up to
`caps`-derived / config `max_attempts`. Never returns an invalid or over-cap DAG.

**Acceptance:** valid reply runs; invalid replies drive the bounded re-ask; loop
fails loudly at the cap; synthesized-DAG caps enforced.

## Task 6 — Config: `pipeline.synthesis.*` caps (tighten-only via `.fuse.local.yml`)

**Tests first** (`internal/config/*_test.go`, alongside existing loader tests):
- Defaults present after `Default()` (conservative: e.g. `max_steps`,
  `max_fanout`, `max_depth`, `max_attempts`).
- YAML unmarshal of a `pipeline: { synthesis: {...} }` block.
- **Tighten-only** (ADR-0006): a `.fuse.local.yml` lowering a cap on an existing
  value is honored; a **loosening** value is ignored + warned (mirror the existing
  `Workflows` `tightenOnly` pattern in `internal/config/loader.go`).

**Implement:** add `PipelineConfig{ Synthesis PipelineSynthesisConfig }` and
`PipelineSynthesisConfig{ MaxSteps, MaxFanout, MaxDepth, MaxAttempts int }` to
`internal/config/schema.go`; wire into `Config`, the `rawConfig` mirror, `Default()`
(conservative defaults), and the tighten-only merge in `loader.go`. Map to
`pipeline.Caps` at the `cmd/fuse` call site.

**Acceptance:** defaults; block parses; loosening rejected + warned; tightening
honored.

## Task 7 — `pipeline_run` built-in tool (mode inferred from params)

**Tests first** (`internal/tools/pipeline_run_test.go`):
- Schema: `Name()`=`pipeline_run`, `Parameters()` advertises `definition`, `name`,
  `goal`, `confirm`.
- Mode inference: exactly one of `{definition|name}` (authored) or `{goal}`
  (synthesized) required; zero or both → an error `Result` (IsError).
- Authored `{definition}` → parses+validates+runs via the injected engine seam,
  returns terminal status text; writes `pipeline.status`.
- Synthesized `{goal}` → calls the injected synthesize seam, writes the generated
  DAG to `pipeline.plan` + a labeled trace entry, then runs; `confirm: true` gates
  (in a headless/autonomous run `confirm` defaults to **false** so an autonomous
  caller is never blocked — spec open-question resolved to false-default).

**Implement** (`internal/tools/pipeline_run.go`): a `Tool` carrying two injected
function seams — `runFn(ctx, *pipeline.Pipeline) (pipeline.Status, error)` and
`synthFn(ctx, goal string) (*pipeline.Pipeline, error)` — plus a blackboard-store
seam for `pipeline.plan`/`pipeline.status`. This keeps `internal/tools` free of an
`internal/pipeline` import (the `cmd/fuse` wiring supplies closures binding the real
engine/synthesizer). `Execute` infers mode, runs, returns `Result`.

**Acceptance:** schema advertised; mode inference incl. error cases; authored +
synthesized paths drive the injected seams; `pipeline.plan`/`pipeline.status`
written; `confirm` default false.

## Task 8 — Wire `pipeline_run` into all three cloned child builders

Per `patch-every-cloned-child-builder`: **re-derive the site list by grep at build
time** (`grep -rn NewSpawnAgentToolWithBudget cmd/fuse`), do not trust this list.
Currently three: `cmd/fuse/main.go` (one-shot `run()`), `cmd/fuse/shell.go`,
`cmd/fuse/research_probe.go` — each registers `spawn_agent` at root and inside the
child builder closure.

**Tests first** (`cmd/fuse/pipeline_wiring_test.go`, mirroring
`blackboard_wiring_test.go`): assert `pipeline_run` is registered on the root
registry AND on a child's registry at each entry point, and that an explicit
`tools`-subset excluding it withholds it (spec open-question: likely always-wired
honoring an explicit exclusion, like the blackboard tool).

**Implement:** at each of the three sites, construct the `pipeline_run` tool with
closures binding the real `pipeline.Run`/`pipeline.Synthesize` (using that site's
`Spawner`, blackboard, and the `pipeline.Caps` from config), register it on the
root registry, and register it inside the child-builder closure (alongside the
existing `wireChildBlackboard`/`spawn_agent` re-registration). Add a shared helper
if the closure is identical across sites (analogous to `wireRootBlackboard`).

**Acceptance:** wiring test green at all three sites; `go build ./...` clean; the
grep site-count re-verified in the task's review note.

## Task 9 — Gateway-seam e2e (real binary, scripted `LLM_GATEWAY_URL`)

Per `verify-tool-loop-at-gateway-seam`. **Tests first**
(`cmd/fuse/pipeline_gateway_e2e_test.go`, mirroring
`structured_delegation_e2e_test.go`):
- **Authored:** a `pipeline_run({definition: <chain yaml>})` call drives execution;
  the scripted gateway log shows the step spawns in dependency order (no synthesis
  call).
- **Synthesized:** a `pipeline_run({goal})` call drives **synthesis first** (the
  gateway sees a request whose `expects`/schema is the `Pipeline` schema), then the
  step spawns in dependency order; the generated DAG appears on `pipeline.plan`.
- Discriminate turns at the seam by inspecting each request's `tools[]` / body
  (helpers like `sdReqHasSpawnAgent`).

**Acceptance:** both e2e flows green against the scripted double; the request log
confirms synthesis-then-execution ordering for the goal path and direct execution
for the definition path.

## Task 10 — TUI screenshot evidence

**Tests** (`internal/tui/pipeline_tui_e2e_test.go`, mirroring
`blackboard_tui_e2e_test.go`): drive a `pipeline_run` through the live `ShellModel`
against a scripted gateway so the pipeline's step spawns render as agent nodes in
the transcript, then `captureFrame(t, tm, "pipeline-run")` (and, for the synthesized
path, `captureFrame(t, tm, "pipeline-synthesized-plan")` after the `pipeline.plan`
surfaces). Screenshots write to `FUSE_SCREENSHOT_DIR` (`.ansi`/`.txt` always, `.png`
when `freeze` is on PATH).

**Acceptance:** the test is green and, when run with `FUSE_SCREENSHOT_DIR` set,
emits an `.ansi`/`.txt` (and `.png` if freeze present) frame showing the pipeline's
steps rendered in the TUI. This is the visual-confirmation evidence for the change
(captured and viewed by the implementer, per the run's evidence requirement).

## Final gate

- `go build ./...` clean; `go test ./... -race` green.
- `go vet ./...` clean; `gofmt` clean.
- Re-run the `patch-every-cloned-child-builder` grep once more; confirm no fourth
  wiring site appeared during the build.
- Capture and **view** the TUI screenshot(s) to confirm the feature renders.

## Notes / deferrals (already in spec, `discovered_from: 26`)

- Skill-frontmatter `pipelines:` block — follow-up stub.
- TUI step sub-nodes (per-step glyphs/elapsed under a pipeline root) — follow-up
  stub; v1 relies on the existing per-`spawn_agent` node each step produces.
