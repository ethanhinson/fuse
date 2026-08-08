<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0023 — Shared result blackboard for inter-agent communication](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0023-agent-blackboard.md)**
<!-- docket:backlink:end -->

# Shared result blackboard for inter-agent communication

**Change:** [#0023](../../changes/active/0023-agent-blackboard.md) · **Status:** design · **Date:** 2026-08-08

## Problem

fuse's subagent model (change 0012, `done`) is **spawn-and-collect**: a parent spawns a
child via `spawn_agent`, waits for its final assistant message, and consumes it. There is no
shared memory between agents running in the same session — a child cannot see what a sibling
found, a parent cannot push mid-run guidance to a running child, and the only coordination
primitive is serial dependency (spawn B after A returns). This forecloses three families of
multi-agent pattern:

- **Ensemble** — several agents explore different hypotheses and pool their findings.
- **Debate / critique** — agents read and challenge each other's intermediate results.
- **Producer / consumer** — one agent writes to a shared slot another blocks on and reads.

A **session-scoped, in-memory, thread-safe key-value blackboard** — owned by the root
`AgentTree` and visible to every agent in the session — enables all three while leaving the
existing spawn/collect architecture untouched. Agents coordinate by reading and writing
structured values to shared keys; a blocking `wait` operation is the enabler for
producer/consumer patterns.

This change is the substrate for two downstream changes: **#0025 (agent-to-agent messaging)**
and **#0026 (workflow composition)** both `depends_on: [23]`. **#0024
(structured-delegation)** is related but independent. Keeping this change's scope tight (no
persistence, no ACLs, no size caps) preserves design room for those.

## Design decisions (settled)

The interactive brainstorm settled four points. `superpowers:brainstorming` was unavailable
in the grooming session, so the design was reached inline with the human (docket Skill-layer
missing-skill fallback) and is recorded here as final.

### D1 — `blackboard_wait` yields its scheduler slot and requires a timeout

`blackboard_wait` blocks the calling agent. That agent occupies a scheduler slot. This is
**exactly** the shape that deadlocked change 0012: a bounded concurrency cap that charges a
*blocked* holder starves the pool (learning
`slot-cap-yield-while-blocked-on-children`). Therefore:

- `Blackboard.Wait` **releases the caller's scheduler slot** while blocked and reacquires it
  on resume, reusing the existing machinery — `AgentTree.YieldSlot(node)` /
  `UnyieldSlot(ctx, node)` (`internal/agent/tree.go:416`/`427`, mirrored on `Scheduler` at
  `scheduler.go:881`/`899`). This is the same fix family that resolved 0012's blocked-holder
  deadlock, refcounted so a holder may yield across nested waits.
- **A timeout is required** on the `blackboard_wait` tool call — there are no infinite waits.
  `Wait` unblocks on the first of: **key set** → `(value, nil)`; **`ctx.Done()`** (turn
  cancelled) → `(nil, ctx.Err())`; **timeout elapsed** → `(nil, timeout-error)`.

### D2 — Full Blackboard tab in the tree overlay is in scope

The change ships a dedicated **"Blackboard" tab** in the agent-tree detail pane rendering the
live key/value set with **agent-wrote indicators**, alongside the core type and tool. Not a
fast-follow.

### D3 — Wait liveness is timeout-only for v1

No tree-idle wake, no cross-agent producer tracking. A consumer that waits on a key whose
producer errored, was never spawned, or finished without writing it **costs its full timeout,
then returns the timeout error**. The required timeout (D1) is the sole liveness guard.
Fail-safe and predictable; smarter liveness is a documented follow-up.

### D4 — Provenance is tracked per key

Each entry stores the **writing agent's node id and label** plus a write timestamp:
`entry = { value, writerID, writerLabel, writtenAt }`. Required for D2's wrote-by
indicators; cheap. Last-writer-wins on `Put` (no ACLs — any agent may overwrite any key,
per the confirmed out-of-scope list).

## What we build

### `internal/agent/blackboard.go` — the `Blackboard` type

A thread-safe store keyed by `string`, holding JSON-encodable structured values with
provenance. Guarded by a single `sync.RWMutex`.

```go
type BlackboardEntry struct {
    Value       any       // JSON-decoded structured value
    WriterID    string    // AgentNode.ID of the writer
    WriterLabel string    // AgentNode label, for display
    WrittenAt   time.Time // wall-clock of the last Put
}

type Blackboard struct {
    mu      sync.RWMutex
    entries map[string]BlackboardEntry
    tree    *AgentTree // for YieldSlot/UnyieldSlot during Wait; may be nil in unit tests
    // one broadcast mechanism per key so Wait can be notified on Put:
    waiters map[string][]chan struct{}
}
```

Methods:

- `Put(key string, value any, writerID, writerLabel string)` — upsert; stamps provenance;
  closes/signals any registered waiter channels for `key`.
- `Get(key string) (BlackboardEntry, bool)` — read under `RLock`.
- `Delete(key string)` — remove a key (does **not** cancel outstanding waits — a subsequent
  `Put` still wakes them; delete-then-never-write is a timeout case).
- `Keys(pattern string) []string` — glob-match keys for discovery, using `path.Match`
  semantics; empty/`"*"` pattern returns all keys. Sorted for stable display and testing.
- `Wait(ctx context.Context, key string, timeout time.Duration) (any, error)` — if the key
  is already set, return immediately. Otherwise register a waiter channel, **yield the
  caller's scheduler slot** (`tree.YieldSlot`), and `select` on `key-set | ctx.Done() |
  time.After(timeout)`, reacquiring the slot (`tree.UnyieldSlot`) in a `defer` on every exit
  path. Returns the value on set, `ctx.Err()` on cancel, a timeout error otherwise.
- `Snapshot() map[string]BlackboardEntry` — a **copied** consistent snapshot under `RLock`
  for race-free TUI reads (the same race-safe-snapshot discipline `NodeView` uses in 0012).

**Wait / slot-yield contract (the highest-risk detail).** `Wait` must call `YieldSlot`
**before** blocking and `UnyieldSlot` **after**, on *every* return path, and must register its
waiter channel **before** the has-key re-check to avoid the lost-wakeup race (a `Put` landing
between the initial check and channel registration). The `tree` handle is how `Wait` reaches
the yield API; when `tree == nil` (pure unit tests of the store in isolation) `Wait` skips the
yield but still blocks/times-out correctly.

### `internal/tools/blackboard.go` — the `blackboard` built-in tool

Modeled on `internal/tools/spawn_agent.go` (`NewSpawnAgentTool` / `Name` / `Description` /
`Parameters` / `Execute`). One tool exposing five operations. The tool holds a `*Blackboard`
handle and the calling `AgentNode`'s id/label (so `Put` records provenance without the model
supplying it).

Operations (dispatched on an `op` parameter, or as five discrete tool names — **decided at
build: five discrete names** `blackboard_write` / `blackboard_read` / `blackboard_wait` /
`blackboard_keys` / `blackboard_delete`, matching the stub and giving the model clearer
affordances):

- `blackboard_write(key, value)` — `value` is a **JSON string** the tool parses with
  `json.Unmarshal` into `any`; malformed JSON returns a **tool error** (never a panic),
  surfaced to the model so it can retry. On success, `Put(key, parsed, node.ID, node.Label)`.
- `blackboard_read(key)` — returns the value (JSON-encoded back to the model) or a
  "key not set" result.
- `blackboard_wait(key, timeout)` — `timeout` **required** (seconds, positive, bounded by a
  sane ceiling); calls `Wait`; returns the value or a timeout/cancel error result.
- `blackboard_keys(pattern)` — returns the glob-matched key list.
- `blackboard_delete(key)` — removes the key; idempotent.

Tool-error (not Go-error) discipline throughout: bad JSON, missing/empty key, non-positive or
over-ceiling timeout all return a `Result` the model reads and can act on.

### Ownership and child access

The `Blackboard` is created and owned by the root `AgentTree`
(`NewAgentTree` / `NewAgentTreeWithConcurrency`, `internal/agent/tree.go:248`/`256`) — one
per session, lifespan equal to the tree's (one user turn or one-shot run). It is passed a
`tree` back-reference for the slot-yield. Every agent — root and all descendants — shares the
**root** blackboard (not a per-subtree one): a nested spawn at depth ≥ 2 sees the same keys as
the root, which is what makes cross-subtree producer/consumer work. Children reach it through
the tree handle already threaded into the `WithChildBuilder` closures (`childTree` in
`shell.go` / `research_probe.go`).

### Tool wiring — all builders, always wired

The blackboard tool is registered in **every** place a tool registry is built for an agent —
root registrations **and** all cloned child builders. Per learning
`patch-every-cloned-child-builder`, **enumerate the sites by grep at build time**, never from
this list; today the sites are `cmd/fuse/run.go` (the `run()` one-shot builder),
`cmd/fuse/shell.go`, and `cmd/fuse/research_probe.go` — **re-grep for a fourth**
(`cmd/fuse/workflow.go` builds spawn wiring and must be checked). A regression assertion (see
Tests) fails if any child builder or root omits the tool.

**Unlike `spawn_agent`, the blackboard tool is ALWAYS wired** — it is **not** gated by
`shouldWireChildSpawn` / the `spawn_agent`-subset guard, because reading and writing shared
state is not a spawn capability and withholding spawning from a child should not blind it to
the blackboard. The one exception: if the parent's explicit `tools` subset names a set that
excludes the blackboard tool, that explicit exclusion is honored (the subset mechanism already
handles this — the tool is simply not force-included the way `spawn_agent` is).

### `internal/tui` — the Blackboard tab

Add a **"Blackboard"** tab to the agent-tree detail pane, following the existing
tab/drilldown pattern (`agents_model_drilldown_test.go`). It renders `Snapshot()`: each key
with its value (truncated/wrapped to pane width) and an **agent-wrote indicator** derived from
`WriterLabel` (e.g. `analysis_result  ⟨written by research/facet-2⟩`). Reads go through
`Snapshot()` for race safety; untrusted value bytes are sanitized and hard-wrapped per
`sanitize-untrusted-bytes-fixed-width-tui` before display.

## Out of scope (confirmed)

- **Cross-session persistence** — in-memory only, no disk write-back.
- **Access control / ACLs** — any agent writes any key; last-writer-wins. A follow-up.
- **Value size caps** — bounded implicitly by context budget; no hard limit.
- **Smarter wait liveness** (tree-idle wake, producer-death detection) — timeout-only for v1
  (D3); a documented follow-up.
- **Direct agent-to-agent messaging** — that is #0025, which builds on this.

## Tests

- **Store unit tests** (`go test -race`): concurrent `Put`/`Get` race-safe; `Keys` glob
  (exact, prefix `foo/*`, `*`, no-match); provenance recorded and last-writer-wins;
  `Snapshot` returns an independent copy.
- **Wait behavior**: `Wait` returns immediately when the key is already set; returns the value
  on a **late** `Put` (producer writes after consumer blocks); returns a timeout error when
  the key is never set; returns `ctx.Err()` on context cancel; **no lost wakeup** when `Put`
  races the registration.
- **Slot-yield regression (highest-risk)**: saturate the scheduler cap with producer agents,
  have a consumer `blackboard_wait`, and assert the whole thing **completes** rather than
  deadlocks — reproducing the freeze shape from 0012's `scheduler_queue_test.go` yield test
  and asserting the consumer's slot was yielded while blocked.
- **Wiring assertion**: assert the blackboard tool is present in **every** child builder site
  (all three-plus, enumerated by grep) and at every root registration; assert it is **not**
  gated by `shouldWireChildSpawn` (present even when child spawning is withheld), but **is**
  absent when an explicit `tools` subset excludes it.
- **Model-sees-tool verification**: run the **real binary** against a scripted
  `LLM_GATEWAY_URL` gateway double that logs each request's `tools[]`, asserting the
  `blackboard_*` tools appear for both root and child turns (learning
  `verify-tool-loop-at-gateway-seam` — the TUI harness fakes the Completer seam and never
  exercises the `cmd/fuse` wiring).
- **TUI tab**: `teatest` render of the Blackboard tab via the final model's `View()` with
  `termenv.TrueColor` forced (learning `teatest-final-frame-via-finalmodel-view`); assert
  keys, values, and wrote-by indicators render and that oversized/untrusted values are wrapped
  and sanitized.

## Risks & mitigations

- **Deadlock via blocking wait** (the dominant risk) — mitigated by D1's mandatory slot-yield
  reusing 0012's proven `YieldSlot`/`UnyieldSlot`, plus the saturation regression test above.
  This is the one behavior most likely to freeze a live run and least likely to surface in a
  naive unit test.
- **Lost wakeup** (a `Put` between the has-key check and waiter registration) — mitigated by
  registering the waiter channel before the re-check and signalling under the lock.
- **Missed wiring site** — mitigated by the grep-at-build-time rule and the wiring assertion
  test that enumerates every builder.
- **Unbounded waits** — foreclosed by the required, ceiling-bounded timeout on
  `blackboard_wait`.

## Follow-ups (not this change)

- ACLs / key namespacing per agent.
- Wait liveness beyond timeout (tree-idle / producer-death wake).
- Value size accounting against the context budget.
- #0025 agent-to-agent messaging and #0026 workflow composition build on this substrate.
