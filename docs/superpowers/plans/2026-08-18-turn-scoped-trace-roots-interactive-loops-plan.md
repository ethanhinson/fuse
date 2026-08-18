<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0071 — Turn-scoped trace roots for interactive loops — end loop.run at first park, per-turn root spans](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0071-turn-scoped-trace-roots-interactive-loops.md)**
<!-- docket:backlink:end -->

# Turn-scoped trace roots for interactive loops — implementation plan

Change 0071 · Plan authored 2026-08-18 UTC

> **Plan-role degradation.** The configured plan skill (`superpowers:writing-plans`) is not
> invocable in this environment, so per the docket convention's *Skill layer* missing-skill
> rule the plan role degraded to `auto` and this plan file was authored by the implementer
> directly. The artifact contract (a plan file on the feature branch, recorded in `plan:`)
> is unchanged.

Spec: `docs/superpowers/specs/2026-08-18-turn-scoped-trace-roots-design.md` (on the `docket`
branch). Read the spec's D1–D4 plus both **Reconciled 2026-08-18** notes before starting —
the notes correct two mechanism claims the original spec got wrong.

## Context the tasks assume

Everything below is against `origin/main` at branch point `075ca0a`.

- `internal/runtime/inproc.go:325` `launchLoop` — starts `fuse.loop.run` at line 326 and
  guards the end with a plain `ended bool` closure (`end(out)`, lines 327–334). Called by
  both `StartLoop` and `Resume`; `interactive := cfg.Interactive || opts.resume` (line 346).
- `internal/observe/contracts.go:58` `Observer` interface; `:71` the optional
  `StartFromCarrier(ctx, *event.TraceCarrier, delayed bool, Descriptor)` capability; `:91`
  the free function `observe.StartFromCarrier(o, ...)` that probes for it and falls back to
  plain `Start`. `internal/observe/otel/observer.go:37` implements it; line 46 is the
  `WithNewRoot()` + `WithLinks(...)` branch taken when `delayed` is true.
- `internal/agent/humanmsg.go:425` `HumanInjector` (concrete struct, `nodeID` + `bus`);
  `:444` `Wait(ctx)` — **the park/wake primitive**. Entering `Wait` is the park; `Wait`
  returning `nil` is the wake; a non-nil return ends the run.
- `internal/agent/loop.go:593–604` — the interactive terminal branch that emits
  `event.KindLoopParked` and then calls `humanInjector.Wait(ctx)`.
- The runtime constructs the injector itself at `inproc.go` (`a.SetHumanInjector(
  agent.NewHumanInjector(rootNode.ID, lp.humanBus))`), which is why the hook below needs no
  change to `Agent.Run` and no new `Agent` setter.
- `internal/runtime/inproc_test.go:21` `recordingObserver` — the existing test double that
  records `(kind, name)` per `Start`. It does **not** record fields, handles, ends, or
  carrier/delayed arguments; several tasks need it extended.

## Design decisions this plan fixes (do not re-litigate mid-build)

1. **The park/wake seam is `HumanInjector`, not `Agent`.** Add optional callbacks to
   `HumanInjector` that the runtime sets. This keeps the agent package free of any tracing
   concept and adds no `Agent` surface. The alternative (an `Agent`-level turn hook) is
   rejected: it would duplicate state the injector already owns.
2. **Turn spans are started through `observe.StartFromCarrier(..., delayed=true)`**, never a
   new Observer method — ADR-0040 provider-neutrality, and the spec's explicit non-goal.
3. **`fuse.turn.index` is 1-based and 1 is the turn inside `loop.run`**, so the first
   `fuse.loop.turn` span carries index 2.
4. **One-shot is byte-identical.** Every new code path is behind `interactive`.

## Tasks

Each task ends in one commit on `feat/turn-scoped-trace-roots-interactive-loops`, tests
first, and leaves the full suite green (`make test`). Tasks are ordered so the suite is
green at every commit.

---

### Task 1 — Make `launchLoop`'s span end genuinely idempotent (`premium`)

**Why premium:** this is a concurrency correctness change on the seam every later task adds
callers to; getting the guard wrong produces a race that `-race` only catches under a test
that actually drives two enders concurrently.

**Test first.** In `internal/runtime/` add a test that drives two concurrent calls of the
loop-span end path and asserts exactly one `End` reaches the handle. Because `end` is an
unexported closure, drive it through the real seam: extend `recordingObserver` (see Task 2's
shared extension — do Task 2's recorder work here if Task 1 lands first) so its returned
handle counts `End` calls under a mutex, then start an interactive loop and race a reaper
cancellation against run completion. Run the package with `-race`.

**Implement.** Replace the `ended bool` in `launchLoop` with a `sync.Once` (or a mutex-guarded
bool). Keep the `end(out observe.Outcome)` signature exactly — every existing call site
(lines ~382, 396, 404, 408, 412, 441, and the run goroutine) stays untouched. Note the
first-writer-wins semantics explicitly in a comment: the first outcome recorded is the one
that lands, so the park path (Task 4) ending with `success` before a later cancellation is
intentional.

**Done when:** `go test -race ./internal/runtime/...` green; no call-site diff beyond the
guard itself.

---

### Task 2 — Extend `recordingObserver` to record fields, ends, and carrier/delayed (`economy`)

**Why economy:** mechanical test-double work, fully specified, no design latitude.

**Implement.** In `internal/runtime/inproc_test.go`:

- Record `d.Fields` alongside `kind`/`name` in `observedOperation`.
- Return a recording handle (not `observe.NoopHandle{}`) that counts `End` calls and captures
  the `observe.Outcome`, mutex-protected (learnings:
  `mutex-test-double-concurrent-provider` — lock **both** sides, the double is shared with
  the run goroutine).
- Add a `StartFromCarrier(ctx, c *event.TraceCarrier, delayed bool, d observe.Descriptor)`
  method so `recordingObserver` satisfies the optional capability, recording `delayed` and
  whether a carrier was present.
- Add query helpers: `opsNamed(name)`, `fieldValue(op, key)`, `endsFor(name)`.

**Critical:** adding `StartFromCarrier` changes which branch `observe.StartFromCarrier` takes
for **every existing test in the package**. Re-run the whole package and fix any test that
implicitly relied on the fallback-to-`Start` path.

**Done when:** `go test ./internal/runtime/...` green with no behavior change in
non-test code (this task touches only `_test.go`).

---

### Task 3 — Park/wake callbacks on `HumanInjector` (`standard`)

**Test first.** In `internal/agent/humanmsg_test.go` (or a new `humanmsg_park_test.go`):

- A `HumanInjector` with callbacks set fires `onPark` exactly once on entering `Wait` and
  `onWake` exactly once when a message arrives and `Wait` returns nil.
- `Wait` returning `ctx.Err()` fires `onPark` but **not** `onWake`.
- `Wait` on a nil bus (`errNoBus`) fires **neither** — it never actually parks.
- A nil-callback injector behaves byte-identically to today (regression guard for every
  existing binding).
- A nil `*HumanInjector` receiver still returns `errNoBus` without panicking (`Wait` already
  nil-guards; keep that true).

**Implement.** In `internal/agent/humanmsg.go`:

```go
// SetTurnBoundary registers optional park/wake callbacks. onPark fires just before the
// injector blocks awaiting a human message; onWake fires only when Wait returns nil (a
// message is ready). Both are nil by default, making this additive for every existing
// binding. Set once at build time, before Run.
func (h *HumanInjector) SetTurnBoundary(onPark, onWake func())
```

Store the two funcs on the struct; call them in `Wait` around `bus.WaitForMessage`, after
the `errNoBus` early return. Callbacks are invoked synchronously on the run goroutine — say
so in the doc comment, because Task 4's implementations start/end spans and must not block.

**Do NOT** add any tracing type or `internal/observe` import to `internal/agent`.

**Done when:** `go test ./internal/agent/...` green; `internal/agent` imports unchanged.

---

### Task 4 — Turn-scoped spans in `launchLoop` (`premium`)

**Why premium:** this is the change's core; it carries the real risk (leaked spans, a
one-shot regression, a wrong parent) and touches the lifecycle seam.

**Test first** — in `internal/runtime/`, using the Task-2 recorder:

1. **One-shot regression (the gate).** A non-interactive `StartLoop` run produces exactly one
   `loop`/`run` operation, zero `loop`/`turn` operations, one `End`, and the same field set
   as today. Assert against the **real** production span shape, not a shared fixture
   (learnings: `parity-test-feeds-each-side-its-own-production-source`).
2. **First park ends `loop.run`.** An interactive loop that reaches its first park has
   `loop.run` ended with `OutcomeSuccess` **while the run goroutine is still alive** — assert
   the end happened before `Wait()` on the handle returns.
3. **Second turn is a linked root.** After a `Send`, exactly one `loop`/`turn` operation is
   started via `StartFromCarrier` with `delayed == true` and a non-nil carrier, carrying
   `loop_id`, `tenant` (normalized), and `fuse.turn.index == 2`. A third turn yields index 3.
4. **Turn ends at the next park**, with the outcome derived the same way `loop.run`'s is.
5. **Resume emits a linked turn root and does NOT restart `loop.run`.** Drive
   `Resume` (see `resume_test.go` for the existing harness) and assert zero new `loop`/`run`
   operations and one `loop`/`turn`.
6. **Reap/teardown leaves no open span.** Reaping a parked session (see
   `session_lifetime_test.go` for the frozen-clock reaper harness) ends the in-flight turn
   span; total starts == total ends. Run with `-race`.

**Implement** in `internal/runtime/inproc.go`, all of it gated on `interactive`:

- Add a small per-loop `turnTracer` value (unexported, in `inproc.go` or a new
  `turnspan.go` in the same package) holding: the observer, the durable trace carrier, the
  `loop_id`/`tenant` fields, a mutex, the current turn index (starting at 1), and the
  currently-open turn handle.
- **onPark** (wired via Task 3's `SetTurnBoundary`): if a turn span is open, end it; else
  this is the first park — call `end(observe.OutcomeSuccess)` on the `loop.run` span.
  Idempotent per Task 1.
- **onWake**: increment the index and start `fuse.loop.turn` via
  `observe.StartFromCarrier(r.deps.Observer, sessionCtx, carrier, true, observe.Descriptor{
  Kind: observe.OperationLoop, Name: "turn", Fields: ...})`, keeping the returned handle and
  **the returned context**.
- **Parenting.** Child spans must parent to the turn. The returned context has to reach the
  agent's next turn. Determine the mechanism during the build — the two candidates are
  (a) a mutable per-loop context holder the agent's observer calls read, or (b) threading
  the turn context through the existing `sessionCtx` decoration seam. If neither works
  without widening `Agent`'s surface, **stop and record the constraint in the results file
  rather than widening it silently**; the acceptance criterion that matters most
  (a rooted, promptly-exported turn trace) holds even if children still parent to the
  session for this change.
- **Carrier source.** Take the carrier from `r.deps.Observer.TraceCarrier(loopCtx)` at
  launch (via the same capability probe pattern — do not type-assert to the otel type), and
  hold it on the loop so it survives park and resume. This is the `agent.eventTrace` durable
  carrier the spec names.
- **Defensive teardown.** In the run goroutine's completion path — and on every early return
  after the turnTracer exists — end any open turn span before `end(out)`. Symmetric with the
  store `Close` and `LoopTeardown` already there (learnings:
  `per-instance-resource-needs-teardown-on-every-early-return` — every early return, not
  just the happy path).
- **Resume.** `opts.resume` already forces `interactive`; ensure the resume path constructs
  the turnTracer with the restored carrier and seeds the index from the restored state (or
  from 1 if unavailable — document which), and does **not** re-end an already-ended
  `loop.run`.

**Done when:** all six tests above pass, `go test -race ./...` green.

---

### Task 5 — Wire and verify the observer actually reaches production (`standard`)

**Why this task exists:** learnings `runtime-deps-field-overwrites-builder-injection` — a
seam test that calls `Deps.BuildAgent` directly proves nothing about the real `StartLoop`
path, because the runtime is the later writer of `Deps.Observer` onto the agent. Every new
assertion in Task 4 must go through `runtime.New`/`StartLoop`/`Resume`, not the builder.

**Implement.** Audit Task 4's tests for any that bypass `StartLoop`; convert or supplement
them. Then grep every binding that constructs `runtime.Deps` (`cmd/fuse`, the loop server,
the research probe) and confirm none needs a change — turn spans ride the existing
`Deps.Observer`, so this should be a no-op audit. Record the enumerated sites in the commit
message.

**Done when:** every new assertion is reached through the real `StartLoop`/`Resume` path.

---

### Task 6 — Live verification against a cheap gateway model (`standard`)

Acceptance criterion 1 is a live check and is **not** covered by the suite.

Run the wander demo (or the smallest interactive-loop path that exercises park/Send/park)
against a **cheap gateway model — never Claude** (this is a standing project rule; see
`LLM_GATEWAY_URL` usage in the existing acceptance tests). Confirm, while the session is
still parked and alive:

- the first-turn trace exists, rooted at `fuse.loop.run`, ended;
- one complete `fuse.loop.turn`-rooted trace per completed later turn, each carrying
  `loop_id`, `tenant`, `fuse.turn.index`, and a link to the session root.

If Tempo/Grafana is not reachable in this environment, fall back to an OTEL stdout/in-memory
exporter and assert the same span topology, and **say so in the results file** — a live
criterion verified by a substitute is a recorded deviation, not a silent pass.

**Done when:** the observation (or the substitution) is recorded for the results file.

---

## Suite gate

One full `make test` run at the end, with `-race` where the runtime and agent packages are
concerned. The build-evidence record's `head_sha` must equal branch HEAD.

## Out of scope (do not drift into these)

- ADR-0045's TUI timestamp bucketing (`internal/tui/agents_model.go:1468`).
- Metrics or logging projections; Tempo/Grafana config; SDK surface.
- Any change to one-shot span names, attributes, or shape.
