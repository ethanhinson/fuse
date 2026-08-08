<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0024 — Structured delegation — expected result schemas for spawn_agent](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0024-structured-delegation.md)**
<!-- docket:backlink:end -->

# Plan — 0024 Structured delegation: expected result schemas for spawn_agent

> Spec: `docs/superpowers/specs/2026-08-08-structured-delegation-design.md` (on `docket` branch)
> Change: 0024-structured-delegation
> ADR: none yet (a vendored-dependency decision may be recorded at review — see Task 2)

> ⚠️ **Plan authored by the docket auto-fallback** (Skill-layer missing-skill rule):
> the resolved plan skill `superpowers:writing-plans` was not invokable in this
> environment, so `docket-implement-next` authored this plan file directly. The build
> step likewise degrades to the inline auto-fallback (SDD `superpowers:subagent-driven-development`
> unavailable). Method is the agent's; the artifact contract (an executed plan, TDD) is unchanged.

## Approach & context

Add an **optional `expects` JSON Schema** to a spawn. The parent declares the shape; the
spawner injects it into the child's system prompt (producer side) and, on return,
validates the child's output against the schema (result side). It is an **asymmetric hint,
not a constraint** — a mismatch NEVER fails the spawn: it degrades to free text, appends a
one-line note the parent model reads, populates `Structured` on match, and records a
labeled trace/tree event on miss.

Build order threads a single field (`Expects`) across the tool→agent seam, then validates
at the one result-assembly site, then wires the three `cmd/fuse` child-builder adapters,
then verifies end-to-end.

### Key existing seams (verified on `origin/main` @ 106b792, in this worktree during reconcile)

- **`internal/agent/spawn.go`**
  - `SpawnOpts` (`:40`): fields `Label, Task, SystemPrompt, Tools, ModelID, MaxTurns,
    MaxTokens, Worker`. **Add `Expects`.**
  - `SpawnDone` (`:54`): `{Result string; Err error}`. **Add `Structured any`.**
  - `AgentHandle` (`:61`): `{NodeID string; Done <-chan SpawnDone}`, `Wait() SpawnDone`
    (`:67`). **Add `Result() (any, error)`.** Note: `Wait()` reads the channel destructively;
    `Result()` must not double-drain. Cache the `SpawnDone` on first receive (add an
    unexported `once sync.Once` + stored `SpawnDone` to the handle, or have both read a
    memoized value) so `Wait()` and `Result()` are each callable and consistent.
  - Result assembly: goroutine calls `s.buildChild(...)` at `:250`, sends
    `doneCh <- SpawnDone{Result: result, Err: runErr}` at `:266`. **Validation + note +
    event happen here, between the buildChild return and the channel send**, so the note
    rides inside `Result` (flowing through the `SpawnFunc` adapter to the tool) and
    `Structured` rides on the same `SpawnDone`.
- **`internal/tools/spawn_agent.go`**
  - `SpawnRequest` (`:13`): add `Expects any` (JSON-Schema object, `map[string]any`).
  - `Parameters()` (`:107`): add an optional `expects` property (`type: object`,
    description noting the child is asked to conform).
  - `spawnAgentInput` (`:152`): add `Expects map[string]any \`json:"expects"\``.
  - `Execute()` (`:162`) threads `input.Expects` into `SpawnRequest`. The result suffix
    `result + t.budgetLine() + t.quotaWarning()` (`:184`) is UNCHANGED — the schema note
    is already inside `result` (assembled in spawn.go), composing before those suffixes.
- **`internal/agent/tree.go`**
  - `AgentNode.AddEvent(AgentEvent)` (`:114`); `AgentEvent{Kind EventKind; Name string;
    Payload map[string]any; TS time.Time}` (`:82`). `EventKind` enum has no mismatch kind
    — **reuse `KindError`** with `Name: "schema_mismatch"` and the first validation error in
    `Payload` (no new enum needed; a labeled `Name` is the discriminator).
- **`cmd/fuse` — three child-builder / `SpawnFunc` adapter sites** (learning
  `patch-every-cloned-child-builder`; re-grep `SpawnOpts{` and `req.Worker` at build time):
  `main.go` (`SpawnOpts{...}` ~`:204`, adapter returns `done.Result, done.Err` ~`:223`),
  `research_probe.go` (~`:138`/adapter), `shell.go` (~`:189`/adapter). Each adapter yields
  its slot around `handle.Wait()` (learning `slot-cap-yield-while-blocked-on-children`) —
  `Structured` survives because it lives on the `SpawnDone` that `Wait()` returns.
- **`go.mod`** (Go 1.26.5): no JSON-Schema library vendored. Vend one in Task 2.

### Learnings that gate this change

- **patch-every-cloned-child-builder** — thread `Expects` through ALL THREE `cmd/fuse`
  adapters; enumerate by grep at build time, not from this list.
- **verify-tool-loop-at-gateway-seam** — the match/mismatch behavior is verified with the
  real binary against a scripted gateway double; the TUI harness never exercises the
  `cmd/fuse` spawn wiring. Model: `cmd/fuse/blackboard_gateway_e2e_test.go`.
- **slot-cap-yield-while-blocked-on-children** — the adapter yields/unyields around
  `Wait()`; do not move validation onto the parent's slot-held path. Validation lives in
  the child goroutine (spawn.go), before the channel send — off the parent's slot entirely.

## Tasks (TDD — each: failing test first, then implement, then green + `go vet`)

### Task 1 — `Expects` on the seam types; nil ⇒ byte-identical behavior
- **Test** (`internal/agent/spawn_test.go`): a spawn with `SpawnOpts.Expects == nil`
  returns `SpawnDone{Result, Err}` with the child's raw text unchanged and `Structured == nil`
  (golden: no note appended). This pins the "nil ⇒ identical" invariant before any new code.
- **Implement**: add `Expects any` to `SpawnOpts`; add `Structured any` to `SpawnDone`; add
  `Expects any` to `tools.SpawnRequest`. No behavior yet when nil.
- **Green**: existing spawn/tool tests still pass; new nil-path test passes.

### Task 2 — Vendor a JSON-Schema validator + a pure validation helper
- **Decide + record** (candidate ADR at review): vendor
  `github.com/santhosh-tekuri/jsonschema/v6` (or v5 — pick the maintained line compatible
  with Go 1.26.5). Pin the version in `go.mod`; run `go mod tidy`.
- **Test** (new `internal/agent/schemavalidate_test.go` or in `spawn_test.go`): a small pure
  helper `validateAgainstSchema(schema map[string]any, raw string) (parsed any, err error)`:
  - lenient extraction: strips ```` ```json ```` fences / surrounding prose / whitespace and
    takes the outermost JSON object before unmarshalling;
  - **D2 fidelity cases** (each must be caught): nested-object mismatch, array-of-wrong-type,
    enum violation, top-level type mismatch;
  - match case: conforming JSON returns `parsed` non-nil, `err == nil`.
- **Implement** the helper in `internal/agent` (new file, e.g. `schemavalidate.go`). Keep it
  pure (no tree/opts deps) so it is unit-testable in isolation.

### Task 3 — Producer side: inject the schema into the child's system prompt
- **Test**: with `Expects != nil`, the child's effective `SystemPrompt` carries the directive
  ("your final message MUST be a single JSON object conforming to this JSON Schema: <schema>;
  output only the JSON, no fences/commentary"). Assert against the prompt the child builder
  receives (drive through a fake `ChildBuilder` capturing `opts.SystemPrompt`). With `Expects
  == nil`, the prompt is untouched.
- **Implement**: in `spawn.go`, when `Expects != nil`, augment `opts.SystemPrompt` before the
  `buildChild` call (append the directive; serialize the schema to compact JSON). Keep the
  augmentation in the spawner so all three adapters inherit it for free.

### Task 4 — Result side: validate, structure, note, and log (D1–D3)
- **Test** (`internal/agent/spawn_test.go`, fake `ChildBuilder` returning canned text):
  - **match**: child returns conforming JSON ⇒ `SpawnDone.Structured` populated;
    `Result` ends with `(matched expected schema)`; `AgentHandle.Result()` returns the value.
  - **mismatch** (non-conforming or unparseable): `Structured == nil`; `Result` is the raw
    text + `(result did NOT match expected schema: <first error>)`; a `KindError` /
    `Name:"schema_mismatch"` event is on the child node (`node.CopyEvents()`); the spawn does
    **not** error (`Err == nil`).
  - **lenient extraction**: conforming JSON wrapped in fences / with trailing prose still
    matches.
  - **`Result()` before `Wait()` / both drain-safe**: calling `Result()` and `Wait()` in
    either order returns consistent values (memoized `SpawnDone`).
- **Implement**: between `buildChild` return and the `doneCh <- SpawnDone{...}` send
  (`spawn.go:~250-266`), when `Expects != nil` and `runErr == nil`: run
  `validateAgainstSchema`; on success set `structured = parsed` and append the match note to
  `result`; on failure append the mismatch note and call `node.AddEvent(AgentEvent{Kind:
  KindError, Name: "schema_mismatch", Payload: {"error": <first err>}, TS: now})` and write a
  labeled trace entry (reuse the existing trace writer path used elsewhere in the child
  goroutine, if reachable; otherwise the tree event + note satisfy D3's observability).
  Send `SpawnDone{Result: result, Err: runErr, Structured: structured}`.
  Add `AgentHandle.Result() (any, error)` + drain-safe memoization.

### Task 5 — Tool surface (D4): advertise `expects`, round-trip through the seam
- **Test** (`internal/tools/spawn_agent_test.go`): `Parameters()["properties"]` contains
  `expects` (type object); a model-authored args JSON with `expects` round-trips
  `spawnAgentInput.Expects` → `SpawnRequest.Expects` (assert the injected `SpawnFunc` receives
  it). Absent `expects` ⇒ `SpawnRequest.Expects == nil` (no behavior change).
- **Implement**: add the `expects` property to `Parameters()`; add the field to
  `spawnAgentInput`; thread `input.Expects` into the `SpawnRequest` in `Execute()`.

### Task 6 — Wire `Expects` through all three `cmd/fuse` adapters
- **Grep first** at build time: `rg 'SpawnOpts\{' cmd/fuse` and `rg 'req\.Worker' cmd/fuse`
  to reconfirm the site set (expected: `main.go`, `research_probe.go`, `shell.go`).
- **Implement**: in each adapter's `agent.SpawnOpts{...}` literal add `Expects: req.Expects`.
  The adapters already return `done.Result, done.Err` — `Structured` is not consumed by
  `SpawnFunc` (its signature stays `(string, error)`), which is correct: the note is in
  `Result`; `Structured` is reachable only via `AgentHandle.Result()` for future programmatic
  callers (change 0026). No adapter-signature change.
- **Test**: a `cmd/fuse` unit/integration test (or extend an existing one) asserting the
  `expects` reaches the `SpawnOpts` at one representative site; the other two are covered by
  the grep-enforced identical edit + build.

### Task 7 — Real-binary gateway-seam verification (learning verify-tool-loop-at-gateway-seam)
- **Test** (`cmd/fuse`, modeled on `blackboard_gateway_e2e_test.go`): drive the real binary
  against a scripted gateway double whose child turn returns (a) conforming and (b)
  non-conforming JSON. Assert the parent turn's `spawn_agent` tool result carries the
  `(matched expected schema)` note in case (a) and the `(did NOT match … )` note in case (b),
  and that the spawn did not error. This closes the seam the TUI harness cannot reach.

### Task 8 — Full-suite gate
- `go build ./...`, `go vet ./...`, `go test ./...` all green from the feature worktree.
- Confirm `go.mod`/`go.sum` are tidy and the new dependency is pinned.

## Out of scope (from spec — do not build)
- A programmatic consumer of `Structured` (change 0026 is first).
- Bounded re-ask of the child on mismatch.
- Nested schemas for sub-sub-agents; result-schema propagation through the tree display
  beyond the mismatch event; `ensures` (parent-side post-delegation validation).

## Risks
- **Dead code until 0026** (`Structured`/`Result()`) — accepted per D1; the producer-side
  value (clean child JSON + model note) stands alone.
- **New dependency** — scoped to result validation; version-pinned; the Task-2 fidelity tests
  justify it over a hand-rolled shallow check. May warrant an ADR at review.
- **False-negative validation** (good data, bad wrapping) — mitigated by lenient extraction
  before validation (Task 2).
- **Missed wiring site** — foreclosed by the Task-6 grep enumeration across all three adapters.
