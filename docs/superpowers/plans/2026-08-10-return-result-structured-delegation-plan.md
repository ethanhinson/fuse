<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0042 — Fix structured-delegation (expects) vs tool-calling collision via a return_result tool](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0042-return-result-structured-delegation.md)**
<!-- docket:backlink:end -->

# Plan — Fix structured-delegation (`expects`) vs. tool-calling collision via a `return_result` tool

> Change: 0042 · Spec: `docs/superpowers/specs/0042-return-result-structured-delegation.md`
>
> **Skill-degrade notice:** the resolved plan skill `superpowers:writing-plans` is not
> invocable in this environment, so per docket-convention's *Skill layer* missing-skill rule
> this plan was authored by the implementer directly (the `auto` plan fallback). Same for the
> build/review/finish roles downstream — each degrades to its auto fallback and is noted in the
> PR body.

## Goal

Move the structured `expects` result off the message channel and onto the tool channel so a
child that also calls working tools (e.g. `write_file`) no longer crams the result object into a
tool call's arguments. When a spawn carries `Expects`, synthesize a per-child `return_result`
tool (parameters schema = the `expects` schema), stop injecting the "final message = only JSON"
directive, treat a `return_result` call as terminal, and source `SpawnDone.Structured` from the
captured, validated call — with a lenient final-message fallback for back-compat.

## Architecture decision (from reconcile)

The `agent.ChildBuilder` seam returns only `(string, error)` and is cloned across THREE cmd
sites (`cmd/fuse/main.go`, `cmd/fuse/shell.go`, `cmd/fuse/research_probe.go` — re-grep at build
time per the `patch-every-cloned-child-builder` learning). Therefore the entire mechanism lives
INSIDE the `agent` package and is driven off `opts.Expects` at the choke point; the cmd-site
child builders stay untouched.

Data-flow shape:

- The `Agent` gains an optional per-run `expects` schema + a captured-result holder (a small
  struct behind a setter, e.g. `SetExpects(schema map[string]any) *returnResultCapture`).
- The loop (`Agent.Run`) injects the synthesized `return_result` tool schema into the turn's
  `schemas` (alongside the registry's) when `expects` is set, and intercepts a `return_result`
  tool call in dispatch: validate args against the schema (reuse `validateAgainstSchema`), on
  valid → capture the value + emit a trivial tool result + terminate the run; on invalid →
  return the validation error as the tool result and let the child retry, bounded by a retry cap
  (N=2). On exhaustion, end the run with no captured value.
- `spawnLocal` (the choke point, `spawn.go:339`) installs the schema onto the child `Agent` — but
  since the `Agent` is built inside `buildChild`, the spawner cannot reach it directly. Instead
  the spawner passes the capture holder to the loop via the `Agent`'s own setter, which the loop
  path already owns. Concretely: keep the mechanism entirely loop-internal and have `spawnLocal`
  read the captured value back through a channel it owns. **Implementation note for the builder:**
  the cleanest seam is to have the child `Agent` created inside `buildChild` call `SetExpects`
  from `opts.Expects` — but to keep cmd sites untouched, do this in the `agent` package by
  having the loop derive `expects` from a field the spawner sets on the `Agent` before `Run`.
  Since the spawner does not construct the `Agent`, the actual wiring is: the loop reads
  `opts`-equivalent state that the child builder already threads (the child builder passes
  `opts.SystemPrompt`/`opts.Task` into the agent). **Resolve the exact seam in Task 2** — either
  (a) a new `Agent.SetExpects` the child builders call (requires touching all 3 cmd sites — the
  learning's cost), or (b) a spawner-owned capture that the loop writes via a context value /
  a holder passed through `opts`. Prefer (b): thread an unexported capture pointer through a new
  unexported field on `SpawnOpts` set by `spawnLocal`, read by the loop when the child builder
  forwards `opts` to `Agent` construction — verify at build time which forwarding already exists.

> The plan intentionally leaves the precise seam to Task 2's spike because it depends on how much
> of `opts` each child builder forwards into `agent.New`. The invariant that MUST hold: **cmd
> sites stay untouched OR all three change together**, and the captured value reaches
> `spawnLocal`'s result assembly.

## Tasks (TDD — write the test first for each)

### Task 1 — Synthesize the `return_result` tool schema (unit)

- **Test:** `schemavalidate_test.go` (or a new `return_result_test.go`): a helper
  `returnResultSchema(expects map[string]any) map[string]any` (or a `returnResultTool` value)
  produces a tool whose `Parameters()` deep-equals the `expects` schema and whose name is
  `return_result`, with a non-empty description naming "return_result" and "once".
- **Impl:** add the synthesis helper beside `augmentPromptWithSchema` in `schemavalidate.go`
  (spec open-question: home is schemavalidate.go — chosen). Description text per spec D1.

### Task 2 — Agent-side expects state + capture seam (unit)

- **Test:** construct an `Agent` with the scripted completer; set expects via the chosen seam;
  assert the loop offers a `return_result` schema in the request `tools[]` (extend
  `scriptedCompleter` to record the last request's `Tools`), and does NOT (regression) when
  expects is unset.
- **Impl:** add the `expects` schema field + capture holder to `Agent`; inject the synthesized
  schema into `schemas` in `Run` when set. Resolve the spawner→agent seam (see architecture
  note); wire `spawnLocal` to install expects + read the capture.

### Task 3 — No message-channel directive when return_result installed (unit)

- **Test:** with expects set (return_result path), the child's effective system prompt does NOT
  contain "final message MUST be a single JSON object"; it DOES contain a short hint naming
  `return_result`. Update `spawn.go:339-340` so `augmentPromptWithSchema` is not called on this
  path; inject the non-contradictory hint instead.
- **Impl:** replace the unconditional `augmentPromptWithSchema` call at `spawn.go:339` with the
  return_result hint; keep `augmentPromptWithSchema` for the fallback-only mode (D5).

### Task 4 — Terminal handling: valid return_result ends the run (loop)

- **Test:** scripted child emits a `return_result` call with conforming args → `Run` returns,
  the captured value is set, and the spawn's `SpawnDone.Structured` (via `Result()`) equals it.
  Assert the loop does NOT require a further assistant message.
- **Impl:** in `executeTools`/`Run`, detect `return_result`, validate, capture, emit trivial tool
  result ("result recorded"), signal terminal, end the loop.

### Task 5 — Self-repair loop: invalid then valid (loop)

- **Test:** first `return_result` call non-conforming → tool result carries the validation error
  string → next `return_result` conforms → success. Assert bounded retries; spawn never
  hard-fails.
- **Impl:** on invalid args return `validateAgainstSchema`'s error as the tool result, increment a
  retry counter, continue the loop.

### Task 6 — Exhaustion: cap reached → no structured result (loop)

- **Test:** persistent non-conforming `return_result` calls hit N=2 → `Run` ends, `Result()`
  returns `ErrNoStructuredResult`, spawn completes (no error). 
- **Impl:** past the cap, stop treating further return_result as retryable; end with no capture.
  Confirm the doom-loop detector interaction (identical calls) does not abort first — the
  return_result retries must be distinguishable / bounded independently, OR arrange the test so
  args differ each attempt; document the interaction.

### Task 7 — Regression (the reported bug): write_file + return_result coexist (loop) ⭐

- **Test:** scripted child with expects set emits, in one turn or across turns, a
  `write_file{path,content}` call (well-formed, `path` present, `content` a real file body) AND a
  separate `return_result({...})` call. Assert: (a) the `write_file` args are a well-formed object
  with a non-empty `path` and `content` that is NOT the structured result object; (b) the
  structured object arrives via `return_result` → `Structured`; (c) the structured object is NOT
  crammed into `write_file.content`. This is the direct guard for the production failure.
- **Impl:** none beyond Tasks 1-6; this test proves the collision is gone. Register a real
  `write_file` (or a recording stub) in the child registry so the call is dispatched.

### Task 8 — Back-compat: final-message fallback (spawn) ⭐ must keep existing tests green

- **Test:** a `ChildBuilder` that returns conforming final-message text and never calls
  `return_result` still yields `Structured` via the lenient fallback (this is exactly the
  existing `spawn_expects_test.go` shape — those tests MUST stay green unchanged).
- **Impl:** in `spawnLocal` result assembly (`spawn.go:362-404`): prefer the captured
  `return_result` value; if none was captured AND `runErr == nil`, fall back to
  `validateAgainstSchema(schema, result)` exactly as today. Keep the match/mismatch notes and the
  `schema_mismatch` tree event for the fallback path.

### Task 9 — Pipeline path unchanged (integration)

- **Test:** an authored pipeline step with `expects` gets a structured result via the same path.
  Add/confirm a pipeline test (or assert `spawnOnce` composes) — pipelines set `opts.Expects` via
  `engine.go:355-357` and read `h.Result()`; no pipeline code changes. Verify `synthesize.go:26`
  structured branch still composes.
- **Impl:** none expected; verify by test + `go test ./internal/pipeline/...`.

### Task 10 — spawn_agent param description (unit/docs)

- **Impl:** update `spawn_agent.go:142-148` `expects` description to mention the child returns via
  `return_result` (helps the parent model reason); no arg-shape change. Add/adjust a small
  assertion if one pins the description.

### Task 11 — Full-suite gate + ADR question

- Run `go build ./...`, `go test ./...`, and `go test -race ./internal/agent/... ./internal/pipeline/...`.
- Re-grep the three child-builder sites; confirm none needed changes (or all three changed).
- ADR: the mechanism supersedes the ADR-0012-companion "final-message directive" design. Decide
  during review whether to mint a NEW ADR ("structured delegation returns via a synthesized
  `return_result` tool") or append an `## Update` to the 0024-lineage ADR — dispatch the
  `docket-adr` subagent (Step 6) for the chosen record.

## Verification beyond unit tests

Per the `verify-tool-loop-at-gateway-seam` learning: unit/loop tests exercise the real `Run`
loop at the Completer seam (the correct level here, since the mechanism is entirely in-package).
A full gateway-seam run (scripted `LLM_GATEWAY_URL` logging each request's `tools[]`, confirming
`return_result` is offered to an expects-child and absent otherwise) is the recommended
belt-and-suspenders check; note it in results as a follow-up if not run in-loop.

## Out of scope (from spec)

Provider-native `response_format`/`json_schema`; the agent-extractor two-phase pattern;
`Result()`/`ErrNoStructuredResult` contract changes; auto-mode's missing-`path` prompt.

## Risks / open questions to resolve at build

- **Retry cap N**: proposed 2, not configurable initially (spec open question).
- **Keep the lenient fallback permanently**: yes (D5) — it is what keeps `spawn_expects_test.go`
  green and makes the change strictly additive; not behind a flag for now.
- **Doom-loop detector vs. return_result retries**: ensure bounded retries don't trip / aren't
  masked by the existing loop detector (Task 6).
- **Exact spawner→agent capture seam** (architecture note): resolve in Task 2 without churning
  cmd sites if possible; if all three must change, apply identically (the learning).
