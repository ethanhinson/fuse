<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0061 — Wire observability into local run paths (fuse shell + one-shot + runtime bindings)](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0061-observe-local-run-paths.md)**
<!-- docket:backlink:end -->

# Plan — Wire observability into local run paths (change #0061)

Spec: `docs/superpowers/specs/2026-08-13-observe-local-run-paths-design.md` (on the `docket` branch).
Base: `origin/main` @ `e6e637f`.

> **Role degradation:** `skills.plan` = `superpowers:writing-plans` was not invocable in this
> environment, so this plan was authored inline under the `auto` fallback.

## Goal

Piece 1 only: thread **one session-shared** `observe.Observer` into every locally-built agent —
root **and** children — on the `fuse shell`, one-shot `fuse <task>`, and research-probe paths.
Empty observability config must remain byte-identical to today (`observe.NoopObserver{}`, no
endpoint, no output).

## Non-goals

- Piece 2 (payload-free JSON access-log projection over the shell's event stream).
- Bearer-token / operator auth for the shell metrics endpoint.
- Any composition or config change to the `loop-serve-net` path. (Its `buildAgentCore` **call
  sites** are updated mechanically by the signature change — see Task 1.)
- `loop_server.go:94`'s `observe.NoopObserver{}` default stays as-is.

## Settled decisions (do not re-litigate)

1. Metrics-endpoint bind failure → **warn to stderr and continue**. Observability never breaks the shell.
2. Invalid observability config (`cfg.Validate()` error inside `newObservability`) → **fail startup fast**, surface the error. Not a silent degrade to Noop.
3. **One shared observer instance per session**, built once at the entry point.
4. Telemetry stays gated behind `observability.metrics.enabled` / `traces.enabled` / `logging.enabled`.
5. Agent-side setter is `(*Agent).SetObserver` (`internal/agent/agent.go:146`). `agent.WithObserver` (`internal/agent/spawn.go:265`) is a **Spawner `Option`** and is already correctly used at the binding sites.

## Testing policy (repo rule — binding)

Any live model traffic in tests uses the **cheap gateway models** via a scripted
`LLM_GATEWAY_URL` / `httptest` double (see the existing pattern in
`cmd/fuse/runtime_binding_test.go`). **Never** Anthropic/Claude models. Prefer pure
wiring assertions with a recording `observe.Observer` double over any model call at all.

Suite: `go build ./...` && `go test ./...` (Makefile).

---

## Task 1 — Thread `observer` through the shared agent-build seam

**Files:** `cmd/fuse/run.go`, plus every call site.

1. Add a trailing parameter `observer observe.Observer` to:
   - `buildAgentCore(...)` (`run.go:803`)
   - `buildAgentWithRendererAndTrace(...)` (`run.go:334`)
2. In `buildAgentCore`, after the gateway-adapter and `cli/`-adapter branches converge on the
   single `*agent.Agent`, call `a.SetObserver(observer)`. One call covers both branches.
   `SetObserver` already maps `nil` → `observe.NoopObserver{}`, so no extra nil guard is needed —
   but pass `observe.NoopObserver{}` explicitly at call sites that do not opt in, for readability.
3. Update **every** call site, passing the observer already in scope where one exists and
   `observe.NoopObserver{}` otherwise. **Enumerate the sites by grep at implementation time**
   (`git grep -n 'buildAgentCore(\|buildAgentWithRendererAndTrace('`) — do not trust this list;
   it is a starting point only (learnings: `patch-every-cloned-child-builder`).
   Known at plan time:
   - `run.go:339` (inside `buildAgentWithRendererAndTrace` → forwards its param)
   - `runtime_binding.go:170`, `:245` (one-shot child / root)
   - `runtime_binding.go:378`, `:447` (research-probe child / root)
   - `runtime_binding.go:599`, `:696` (shell child / root)
   - `loop_server.go:239`, `:282` (loop-server child / root) — pass that binding's existing
     `observer` variable at **both**; this closes the loop-server root gap as a mechanical
     consequence.
   - `shell.go:234` (shell TUI `build` closure) — Task 3 supplies the real value; wire the
     parameter here in Task 1 with the value that closure will receive.
   - `shell_test.go:28`, `:68` and any other test call sites.

**Test (write first):** in `cmd/fuse`, a recording observer double
(`type recordingObserver struct{ mu sync.Mutex; descs []observe.Descriptor }` implementing
`Start(ctx, Descriptor) (context.Context, Handle)`), then assert that an agent built via
`buildAgentCore` with that observer has it installed. Since `Agent.observer` is unexported, assert
observably: drive one turn against a scripted gateway double (cheap-model shape, no real provider)
and assert the recorder saw at least one `Start`. If a turn is too heavy for this task, assert
instead at the seam Task 2 covers and keep Task 1's test to a compile-level call-site sweep.

**Done when:** `go build ./...` is green and every call site compiles with an explicit observer.

---

## Task 2 — Un-hardcode the three `runtime_binding.go` bindings

**File:** `cmd/fuse/runtime_binding.go`.

Replace `observer := observe.NoopObserver{}` at lines **87**, **304**, **539** with an observer
supplied by the caller, defaulting to Noop when `nil`:

- `buildOneShotRuntimeDeps(...)` — add a trailing `observer observe.Observer` parameter.
- `buildResearchProbeRuntimeDeps(in researchProbeDepsInput)` — add an `observer observe.Observer`
  field to `researchProbeDepsInput`.
- `buildShellRuntimeDeps(in shellDepsInput)` — add an `observer observe.Observer` field to
  `shellDepsInput`.

Each site begins with `if observer == nil { observer = observe.NoopObserver{} }`, mirroring
`buildLoopServerRuntimeDepsWithObserver` (`loop_server.go:100-103`).

The existing `a.SetObserver(observer)` (`:180/392/617`) and `agent.WithObserver(observer)`
(`:200/410/662`) calls then carry the real observer with no further edits. Additionally pass the
observer into the root `buildAgentCore` / `buildAgentWithRendererAndTrace` calls added in Task 1
(`:245`, `:447`, `:696`).

**Test (write first):** for each of the three bindings, construct `runtime.Deps` with a recording
observer, build the root agent through `Deps.BuildAgent`, spawn one child through the returned
`ChildBuilder`, and assert the recorder observed both. Existing tests
(`runtime_binding_test.go:127`, `two_bindings_parity_test.go:106`, `one_shot_mcp_test.go:47`)
show the construction shape and the gateway-double pattern to reuse.

**Done when:** the three bindings honor a supplied observer, and passing `nil` reproduces today's
behavior exactly.

---

## Task 3 — `fuse shell`: construct the observe layer once, thread it everywhere

**File:** `cmd/fuse/shell.go` (`runShell`, `:34`).

1. Early in `runShell` (after `cfg` is in hand, before the tool registry / tree wiring):

   ```go
   ctx := context.Background()
   obs, err := newObservability(ctx, cfg, stdout)
   if err != nil {
       fmt.Fprintf(stderr, "shell: observability: %v\n", err)
       return 1                    // decision 2: fail fast on invalid config
   }
   defer func() {
       sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
       defer cancel()
       if cerr := obs.Close(sctx); cerr != nil {
           fmt.Fprintf(stderr, "shell: observability shutdown: %v\n", cerr)
       }
   }()
   ```

   Mirror `loop_serve_net.go:196-210`. `context` and `time` are already imported in `shell.go`.

2. If `cfg.Observability.Metrics.Bind != ""`, start the scrape endpoint with a **nil** verifier and
   **warn-and-continue** on failure (decision 1) — this is the one place the shell deliberately
   diverges from `loop-serve-net`, which aborts:

   ```go
   if cfg.Observability.Metrics.Bind != "" {
       if err := obs.startMetricsEndpoint(ctx, nil); err != nil {
           fmt.Fprintf(stderr, "shell: metrics endpoint: %v (continuing)\n", err)
       }
   }
   ```

   Confirm the exact config field path for `metrics.bind` at implementation time; if
   `startMetricsEndpoint` already no-ops on an empty bind, drop the outer guard and just warn.

3. Pass `obs.observer` into the `build` closure's `buildAgentWithRendererAndTrace` call
   (`shell.go:234`) — this is the TUI root.
4. Pass `obs.observer` into `shellDepsInput` at `shell.go:305`.

**Test (write first):**
- Empty/default config → the shell's observer is `observe.NoopObserver{}` and no metrics listener
  is opened (byte-unchanged default).
- Invalid observability config → `runShell` returns non-zero and the validation error reaches
  stderr, **without** starting the TUI.
- `metrics.bind` pointed at an already-bound port (open a `net.Listener` in the test first) → a
  warning reaches stderr and startup continues past the observability block.

Extract the observability setup into a small helper (e.g. `setupShellObservability(ctx, cfg, stderr)
(*observabilityService, int, bool)`) if that is what makes the fail-fast / warn-and-continue paths
testable without launching bubbletea. Keep `runShell`'s own behavior identical.

**Done when:** the three shell behaviors above are asserted and green.

---

## Task 4 — One-shot and research-probe entry points

**Files:** `cmd/fuse/main.go` (`run`, `:27`), `cmd/fuse/research_probe.go` (`:135`).

Apply the same construct-once shape as Task 3 (fail fast on `newObservability` error; `defer
obs.Close`; start the metrics endpoint with warn-and-continue when `metrics.bind` is set), then pass
`obs.observer` into `buildOneShotRuntimeDeps` (`main.go:170`) and `researchProbeDepsInput`
(`research_probe.go:135`).

One-shot is short-lived, so ensure `obs.Close` runs on **every** return path from `run()`,
including the `StartLoop` error return and the `h.Wait()` error return (learnings:
`per-instance-resource-needs-teardown-on-every-early-return`). A `defer` placed before the first
early return covers this.

**Test (write first):** with observability enabled via a test config, run the one-shot path against
a scripted gateway double and assert a recording observer saw the root turn; with empty config,
assert Noop and no listener.

---

## Task 5 — Full-suite gate

`go build ./...` and `go test ./...` from the feature worktree. All green, no skips introduced.

Re-run `git grep -n 'observe.NoopObserver{}' cmd/fuse/` and confirm the only remaining hardcodes are
the intentional ones: `loop_server.go:94` (the deliberate no-observer variant) and the `nil`-guard
defaults added in Task 2.

## Risks

- **Signature churn.** `buildAgentCore` / `buildAgentWithRendererAndTrace` already take 13-14
  positional parameters; adding a 14th/15th is ugly but is what the spec chose. Do **not**
  opportunistically refactor to an options struct in this change — that is a distinct refactor and
  would swamp the diff the reviewer must read.
- **Missed call site.** Mitigated by grepping for the call sites at implementation time rather than
  from this plan's list, and by the compiler (a positional parameter cannot be silently omitted).
- **Double observability construction.** Only the entry point may call `newObservability`; a
  binding that constructs its own would violate the one-observer-per-session decision and would
  double-register Prometheus collectors.
