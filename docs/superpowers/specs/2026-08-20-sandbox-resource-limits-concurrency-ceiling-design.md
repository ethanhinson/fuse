<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0077 — sandbox resource limits — cgroup caps per container and a concurrency ceiling on in-flight Execs](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0077-sandbox-resource-limits-concurrency-ceiling.md)**
<!-- docket:backlink:end -->

# sandbox resource limits — cgroup caps + concurrency ceiling — design

> Change: [#0077](../../changes/active/0077-sandbox-resource-limits-concurrency-ceiling.md) ·
> Decision of record: [ADR-0044](../../adrs/0044-bash-tool-contained-not-credentialed.md) ·
> Builds on: [#0063](../../changes/active/0063-bash-container-substrate-env-scrub-off-switch.md) (substrate, PR #79 — **not yet merged**)

Change #0063 gave the `bash` tool a container substrate with correct **lifetime** management —
`run --rm` per `Exec`, a per-principal warm pool, an idle-TTL reaper, deterministic `Close`.
It gave it no **resource** management. The run argv carries no `--memory`, `--cpus`,
`--pids-limit`, or `--ulimit`; nothing bounds the number of `docker run` processes in flight;
and a cold image pays an unbounded inline pull under the command's own deadline. Those are
three distinct denial-of-service surfaces in a shipped feature, and this change closes all
three behind trusted operator config.

This spec designs **ahead of** #0063's merge. Every integration point below is named against
the substrate as it stands on PR #79's branch; the implementer's reconcile pass re-validates
them at build time and appends any drift to the change's reconcile log.

---

## Scope (this change)

Four axes, in the local container substrate and the layer immediately above it:

1. **Per-container cgroup caps** — `--memory` / `--memory-swap`, `--cpus`, `--pids-limit`, and
   `--ulimit` bounds, added to the container run argv, sourced from trusted operator config
   only, with fail-safe defaults that differ by posture (hosted vs. local).
2. **A soft, high-bounded concurrency ceiling** on in-flight `Exec`s — a dedicated
   sandbox-layer admission gate providing a per-tenant soft share under a global runaway
   backstop, where waiting is the normal outcome and refusal is reserved for the pathological
   case.
3. **A loop-facing and an operator-facing signal** — a bounded backpressure note attached to
   the tool `Output` the model reads, plus a new bounded sandbox event projected to new
   `fuse_sandbox_*` metric families.
4. **Bounded image acquisition** — an explicit, separately-timed pre-pull so that a cold image
   can never consume a command's own deadline.

**Explicitly out of scope**, each with its owner:

- **Network / egress floor** — #0064 (`--network none` + allowlist). The `TODO(#0064)` marker in
  `argv` stays where it is; this change adds flags around it and does not claim it.
- **Per-tenant filesystem scoping of the mount** — #0065. The disk bound here is a
  host-level `--ulimit fsize`, which caps *one file*, not a tenant's share of the mount.
  Per-tenant FS *isolation* remains #0065's, and the `TODO(#0065)` marker likewise stays.
- **The k8s handler and Helm chart that consume this model** — #0075 / #0076. This change
  defines host-side enforcement and the config seam those map onto: the `limits:` block
  becomes Pod `resources.limits`, and the `concurrency:` block becomes a namespace
  `ResourceQuota` plus a replica/parallelism bound. That mapping is stated here in one
  sentence and designed there.
- **The microVM handler's own resource model** — a follow-on to ADR-0044's microVM note. The
  microVM handler is not built, and axis A is the container handler's argv. Axis B (admission)
  is deliberately substrate-agnostic and will cover a microVM handler for free when one lands.

---

## Design decisions settled in grooming

| # | Open question | Decision |
|---|---|---|
| 1 | Where do caps come from | **Trusted operator config only** — the same trust boundary as the off-switch and the env allowlist. Never a wire field, never a tool argument, never `working_dir`, never model output. Loaded once from `<root>/.fuse/sandbox.local.yml` at trusted startup. |
| 2 | Default caps, hosted vs. local | **Posture-split fail-safe defaults.** An unconfigured *hosted* posture is bounded by built-in defaults; an unconfigured *local* posture emits no cap flags at all, matching #0063's allow-all-locally stance. |
| 3 | Where the concurrency ceiling lives | **A dedicated admission gate in `internal/tools/sandbox`, NOT `agent.Scheduler`.** |
| 4 | Refuse vs. queue | **Soft queue, high bound.** Execs wait for a slot up to their own context deadline; the queue drains naturally. Refusal happens only on a separate, deliberately-generous *waiter* overflow — the runaway backstop. |
| 5 | Cap scope | **Per-tenant soft share under a global backstop**, keyed on `loopauth.Principal.Tenant` (ADR-0034 tenancy, consistent with #0065). |
| 6 | How backpressure surfaces | **Both directions.** A bounded note appended to the model-visible `Output` when the wait crossed a threshold, *and* a bounded event projected to metrics. |
| 7 | The rejection metric | **A dedicated `fuse_sandbox_rejected_total`**, not `KindSandboxHealth{reason:"acquire_failed"}` — so #0077 carries no hard dependency on #0074. |
| 8 | Image pull | **Explicit pre-pull under its own timeout, then `--pull=never` on run.** |

### A. Per-container cgroup caps — settled

The caps bound what **one** container consumes. They refuse nothing and admit nothing; a
command that exceeds them is killed by the kernel, not by fuse. This is deliberately the
*uncontroversial* axis: it has no policy surface beyond the numbers.

They are sourced from trusted operator config **only**. The reasoning is the same one that
makes the off-switch file-only: a value that a model could influence is a value a model can
raise, and a resource cap a model can raise is not a cap. In particular the caps are not
derived from `working_dir`, from the `command`, or from any per-call argument — they are
resolved once at `Service` construction, before any model has run, alongside `Image` and
`EnvPassthrough` (ADR-0044, ADR-0006/0019 trust-boundary discipline).

**Posture split, stated explicitly.** #0063 established that local dogfooding is allow-all
(the off-switch exists, the network is permissive, the whole tree is mounted). Silently
capping a developer's local `bash` at 2 GB would break legitimate local builds for no threat
model — locally, fuse is not defending the machine from its own operator. So:

- **Hosted posture** (`Service.Hosted() == true`, the ADR-0034 loop-server binding) — unset
  fields resolve to built-in bounded defaults. "Forgot to configure it" is still bounded.
- **Local posture** — unset fields resolve to *unset*, and **no cap flag is emitted**. An
  operator who wants local caps writes them down; the config is honoured identically in both
  postures when present.

The microVM handler's own resource model is out of scope (ADR-0044 microVM follow-on); this
axis is argv on the container handler.

### B. Soft, high-bounded concurrency queue — settled

**The queue is the queue.** An `Exec` that finds no free slot **waits**, bounded by its own
context deadline — the deadline the `bash` tool already derives from the call's `timeout`
argument. It does not fail fast, it does not get a shorter deadline than it asked for, and it
does not need the caller to retry. Queue depth converts into latency, and latency drains.
This is transparent backpressure, not a load gate.

**The bound is a runaway backstop, not a normal-load gate.** The global bound is chosen high
enough that ordinary multi-loop operation never touches it; if operators routinely see the
queue, the bound is misconfigured. Refusal is reserved for the genuinely pathological case
and is a *separate* number (waiter overflow, below), not the concurrency bound itself.

**Per-tenant soft share under a global backstop.** A single tenant's burst queues against its
own per-tenant share, so other tenants keep flowing at full speed; the global bound is the
aggregate guard on the host. Keyed on `loopauth.Principal.Tenant` — the same tenancy axis
ADR-0034 establishes and #0065 will scope the filesystem by. Not keyed on the full
`Principal` (a busy tenant with many subjects would multiply its own share) and not keyed on
loop (a tenant spawning loops would multiply it likewise).

**Admission lives in the sandbox layer, not `agent.Scheduler`.** The stub asked whether
ADR-0007's `Scheduler` (`internal/agent/scheduler.go`), as the existing admission authority,
should own this so there is one authority rather than two. The answer is no, for two
independent reasons, both recorded here so this is not re-litigated at build time:

1. **It mis-scopes the resource.** `agent.Scheduler` admits *subagents within one spawn
   tree*. The resource being protected here is the **host's** concurrent-container budget,
   which is shared across every loop, every spawn tree, and every tenant in the process. A
   spawn tree is not the unit of a host budget; a per-tree admission ceiling would let N trees
   each admit their maximum and still swamp the host, which is the exact failure this change
   exists to prevent.
2. **It breaks the import boundary.** `internal/tools/sandbox` is documented as a leaf: it may
   import `internal/loopauth` and `internal/event` and must never import `internal/agent`,
   `internal/observe`, or `internal/tools`. That leaf property is what lets `internal/tools`
   depend on it without a cycle, and what keeps the substrate testable without the agent
   runtime. Routing admission through `agent.Scheduler` would invert that dependency.

These are two different admission questions that happen to share a word. Keeping them
separate is not "two authorities"; it is one authority per resource.

**Composition with the warm pool — no double-count, no deadlock.** The pool's rule is *at most
one warm entry per principal*; the gate's rule is *at most N concurrent in-flight Execs*.
Different axes, and they must not be conflated:

- The gate is acquired **around `Exec`**, never around pool checkout. On the container
  handler, `Handler.Acquire` builds a struct and starts nothing — the actual `docker run`
  process is spawned per `Exec` — so gating `Exec` counts real concurrent container processes
  exactly, with no over- or under-count.
- Gating checkout instead would be both wrong and deadlock-prone: a Runner is held across a
  whole `bash` call (acquire → exec → release), so slots would be consumed by idle holders,
  and a loop holding a checkout while waiting for a slot to run its next command would hold a
  resource it is waiting on.
- The gate holds no pool lock and the pool holds no gate ticket. A ticket's lifetime is
  strictly inside one `Exec`, released on every return path including the error and
  deadline paths.

### C. Loop + observability signal — settled

Feed **both** consumers, because they need different things.

**The loop.** When an `Exec` waited past a threshold, a **bounded** note is appended to the
`Output` the model reads — closed-form text plus one rounded duration, e.g.
`[sandbox: waited 4s for a free execution slot; consider fewer parallel commands]`. The model
can then adapt explicitly rather than silently experiencing mysterious latency. The note is
rendered in `internal/tools` at `Result` construction, **never** written into
`sandbox.Output.Combined` and never into an event payload.

**The operator.** A new bounded sandbox lifecycle event, projected through the existing
`internal/observe` pipeline to new `fuse_sandbox_*` families, consistent with #0063's. The
payload-free discipline is absolute: bounded facts only — a closed-enum outcome, a closed-enum
scope, a wait duration in milliseconds — and never the command, the environment, or the
output. The sandbox package emits; the projector observes. No meter is registered and no OTEL
span is opened inside `internal/tools/sandbox`.

**The rejection metric: a dedicated counter.** Two options were weighed:

- *Reuse `KindSandboxHealth` with `reason: "acquire_failed"`* once #0074 lands its emitter.
  Cheap — no new kind, no new family, and the reason enum already exists.
- *Define `fuse_sandbox_rejected_total`.* Costs a new kind and a new family.

**Recommendation and decision: the dedicated counter.** Reuse would make #0077 hard-depend on
#0074's emitter for its *only* refusal signal, and would conflate two operationally distinct
conditions under one alert: `acquire_failed` means *the substrate is broken* (page someone),
while an admission refusal means *the host is saturated* (capacity, possibly a runaway tenant).
An operator alerting on `fuse_sandbox_unhealthy_total{reason="acquire_failed"}` should not be
woken by a load event. The two signals stay separable; #0074 remains a `related`, not a
`depends_on`.

### D. Bounded image acquisition — settled

An **explicit pre-pull with its own timeout**, then `--pull=never` on `run`. The pull is
performed once per resolved image under a context derived from `context.Background()` with the
configured `pull_timeout` — deliberately *not* from the caller's context, so a short-`timeout`
`bash` call cannot cancel a pull that a subsequent call would have benefited from. The caller
waits on that shared in-flight pull only up to its own deadline; if the caller's deadline fires
first it gets a normal timeout result while the pull completes in the background and the next
`Exec` finds the image warm. `--pull=never` on `run` then makes it structurally impossible for
an `Exec` to trigger an unbounded pull. Small, one mechanism, no policy surface.

---

## Architecture

### 1. Config surface (`internal/tools/sandbox/config.go`)

Two new blocks in the existing trusted-local file. Both are optional; the file remains
absent on almost every machine.

```yaml
# .fuse/sandbox.local.yml — trusted local operator config; gitignored
contained: true
handler: container
image: <pinned-digest-ref>
env_passthrough: [FOO, BAR]
pool:
  idle_ttl: 5m

limits:                      # per-container cgroup caps (container handler only)
  memory: 2g                 # --memory (and --memory-swap, pinned equal)
  cpus: "2.0"                # --cpus
  pids: 512                  # --pids-limit
  nofile: 4096               # --ulimit nofile=N:N
  fsize: 2g                  # --ulimit fsize=<bytes>
  pull_timeout: 2m           # bound on the explicit pre-pull

concurrency:                 # admission gate (substrate-agnostic)
  max_inflight: 64           # global runaway backstop
  max_inflight_per_tenant: 16
  max_queued: 256            # waiters above this are REFUSED (the pathological case)
  note_threshold: 2s         # waits at or above this attach the backpressure note
```

**Unset must be distinguishable from zero.** `rawConfig` already uses pointer scalars for
exactly this reason, and the new blocks follow it: `limits.memory: 0` (an operator explicitly
asking for no cap) and an absent `limits.memory` (an operator who said nothing) resolve
differently. A `Limits`/`Concurrency` value therefore carries per-field presence, and
resolution happens in two stages:

- `LoadConfig(root)` parses only *what the operator wrote*. It knows nothing about posture. A
  malformed or non-positive value degrades to unset with a loud `Warning` — new `WarnReason`
  values (`bad_limit`, `bad_concurrency`) join the existing closed enum, and the returned
  `Config` remains directly usable, per the existing "a Warning is never an error" contract.
- `NewService(cfg, opts...)` applies **posture defaults** to unset fields, because `Service` is
  where `WithHostedPosture` is known. This keeps the loader posture-free and puts the
  fail-safe decision in the one place that can make it.

**Posture defaults for unset fields:**

| Field | Hosted (unset ⇒) | Local (unset ⇒) |
|---|---|---|
| `memory` | 2g (with `--memory-swap` pinned equal) | no flag |
| `cpus` | 2.0 | no flag |
| `pids` | 512 | no flag |
| `nofile` | 4096 | no flag |
| `fsize` | 2g | no flag |
| `pull_timeout` | 2m | 2m |
| `max_inflight` | 64 | 64 |
| `max_inflight_per_tenant` | 16 | 16 |
| `max_queued` | 256 | 256 |
| `note_threshold` | 2s | 2s |

The **caps** split by posture; the **concurrency backstop and pull timeout do not**. That is
intentional: the backstop is a queue that never refuses under ordinary load, so leaving it on
locally costs a developer nothing and protects a laptop from a fork-bomb-adjacent runaway just
as usefully as it protects a host. Caps, by contrast, actively kill legitimate local builds.

The exact hosted default numbers are the one judgment call here; they are chosen to comfortably
run a typical build/test command and are operator-overridable in one line. They are recorded in
the config file's own documented example so the knob is discoverable.

### 2. Argv additions (`internal/tools/sandbox/container.go`)

`containerRunner.argv` gains a cap block. Placement: after `--rm -i` and before the
`TODO(#0064)` network marker, so #0064 lands cleanly beside it and does not conflict.

```
run --rm -i
    [--memory <M> --memory-swap <M>]     # pinned equal — see below
    [--cpus <C>]
    [--pids-limit <P>]
    [--ulimit nofile=<N>:<N>]
    [--ulimit fsize=<F>]
    # TODO(#0064): --network none floor + allowlist
    --env K=V ...
    -v <root>:/workspace -w <workdir>
    --pull=never
    <image> /bin/sh -c <cmd>
```

Every bracketed flag is emitted **only when the resolved limit is set**. An unset limit emits
nothing at all — never a sentinel value, never `--memory 0` (which some runtimes read as
"unlimited" and others reject).

Three details a builder must not have to rediscover:

- **`--memory-swap` is pinned equal to `--memory`.** Docker's default when `--memory` is set
  and `--memory-swap` is not is *twice* the memory limit, so a lone `--memory 2g` actually
  permits 4 GB of memory+swap. Pinning them equal is what makes the number mean what it says.
- **`--ulimit fsize` bounds a single file, not the mount.** It is real protection against
  `dd if=/dev/zero of=big` and it is *not* a disk quota. This is documented at the config field
  and repeated in Risks; a true per-tenant disk quota is #0065's filesystem work.
- **`--cpus` is a rendered decimal string**, not a float formatted by whatever `%v` gives —
  argv is golden-tested, so rendering must be deterministic and locale-independent.

The caps flow into `containerHandler` at construction (a new `containerOption`, alongside
`withTrustedRoot`), so — like the mount root — they are settled once, before any model runs,
with no method that can change them afterwards.

`--pull=never` is supported by all three detected CLIs (docker, nerdctl, podman), preserving
the one-argv-builder-serves-all-three property `containerCLIs` documents.

### 3. Pre-pull (`internal/tools/sandbox/container.go`)

`containerHandler` gains a lazily-triggered, single-flight pre-pull for its resolved image:

- The first `Acquire` (or first `Exec`, whichever the implementer finds cleaner to test —
  `Acquire` is preferred, since a pull failure there maps naturally onto `acquire_failed`)
  starts `<cli> pull <image>` under `context.WithTimeout(context.Background(), pull_timeout)`.
- Concurrent callers join the in-flight pull rather than starting their own.
- A caller waits on it only until **its own** context expires; a caller whose deadline fires
  first returns a normal timeout result and the pull continues.
- A **failed** pull is an `Acquire` failure — it emits `KindSandboxHealth{reason: "pull_failed"}`
  (the enum value already exists and is already in the recorder's `sandboxHealthReasons`
  vocabulary, so no new metric work is needed for this path) and is retried on a later
  `Acquire` rather than cached as a permanent failure.
- With the pull settled, `--pull=never` guarantees `run` never blocks on a registry.

### 4. The admission gate (`internal/tools/sandbox/admission.go` — new)

A new type in the sandbox package, **owned by `Service`, not by `Pool`**. This is the
load-bearing placement decision:

`Pool` is constructed **lazily per `bashTool`**, i.e. effectively per loop
(`bashTool.substrate()` builds it on first use). A gate on the `Pool` would therefore be a
*per-loop* gate — N loops would each get their own full budget, and the host would be
protected by nothing. `Service` is constructed **once per process** at the composition root
and already owns frozen config, posture, and handler selection. The gate belongs there.

```go
// Gate is the sandbox-layer admission control on CONCURRENT in-flight Execs.
// It is substrate-agnostic: it counts executions, not containers, so it covers
// the host handler and a future microVM handler unchanged.
type Gate struct { /* global sema, per-tenant semas, waiter counter */ }

// Ticket is one admitted execution. Release is idempotent.
type Ticket interface{ Release() }

// Admit blocks until a slot is free, ctx expires, or the waiter bound is
// exceeded. It reports how long the caller waited, so the layer above can
// decide whether that is worth telling the model about.
func (g *Gate) Admit(ctx context.Context, tenant event.TenantID) (Ticket, time.Duration, error)
```

**Acquisition order: per-tenant slot first, then the global slot.** The order is fixed and
global, so there is no cycle and no deadlock. The direction matters: a caller blocked on the
global bound holds only *its own tenant's* slot, so the blocking is self-inflicted. The reverse
order would let a saturated tenant's waiters sit on global slots and stall every other tenant —
the exact cross-tenant starvation the per-tenant share exists to prevent.

**Outcomes:**

| Condition | Outcome |
|---|---|
| Slot free | Admitted immediately; wait ≈ 0; **no event emitted** |
| Slots busy, waiters under `max_queued` | Waits; admitted when a slot frees; wait reported |
| Caller's ctx expires while waiting | Ordinary deadline result (`TimedOut`), the path `bash` already handles. Not a refusal. |
| Waiters already at `max_queued` | **Refused immediately** — `ErrSandboxAtCapacity`, a new sentinel |

The waiter bound is what makes "refuse only in genuinely pathological cases" precise: refusal
requires `max_inflight` executions running *and* `max_queued` more piled up behind them. At the
defaults that is 320 simultaneous outstanding shell commands on one host.

Per-tenant semaphores are created lazily and reaped when idle at full capacity, so tenant count
does not become an unbounded map. The empty tenant normalises to `event.DefaultTenant`, matching
the storage layer's convention.

**Wiring into the seam.** `PoolSource` (the unexported-method interface `*Service` satisfies)
gains an accessor for the gate, and `pooledRunner.Exec` becomes:

```
Exec(ctx, cmd, workingDir):
    if released -> ErrRunnerReleased (unchanged)
    ticket, waited, err := gate.Admit(ctx, entry.principal.Tenant)
    if err != nil -> Output{ExitCode: -1}, err        // capacity refusal
    defer ticket.Release()                            // every path, including panic
    out, err := entry.runner.Exec(ctx, cmd, workingDir)
    out.Waited = waited
    return out, err
```

The ticket's lifetime is strictly one `Exec`. Nothing outside `Exec` holds one.

`sandbox.Output` gains one field:

```go
// Waited is how long this Exec spent in the admission queue before a slot
// freed. Zero on the uncontended path. It is a FACT about scheduling, not
// output: rendering it for a model is the caller's decision, and it is never
// part of Combined and never enters an event payload.
Waited time.Duration
```

### 5. Backpressure note (`internal/tools/bash.go`)

`bashTool.Execute` already owns every `Result` construction path. It gains one bounded
renderer, applied to the success and non-zero-exit paths (and harmlessly skipped on the
substrate-error path, where the error text is the point):

- `out.Waited >= note_threshold` ⇒ append exactly one line to `Result.Output`:
  `\n[sandbox: waited 4s for a free execution slot; consider fewer parallel commands]`
- Below the threshold ⇒ nothing is appended. The uncontended case is byte-identical to today.

Closed-form template plus one duration rounded to a coarse unit — bounded by construction, so
it cannot become a channel for arbitrary text. It is appended **after** the command's output so
a model (or a test) parsing the head of the output is unaffected.

The **capacity refusal** surfaces separately and explicitly, as a tool error the model can
react to: `Result{IsError: true, Output: "sandbox at capacity: no execution slot available;
retry shortly or run fewer commands in parallel"}`. It names a recoverable condition and
suggests the recovery, so the model retries or serialises rather than treating `bash` as broken.

The threshold lives in config so an operator can silence the note entirely (a very large value)
without touching the gate.

### 6. Event + metric wiring

**New event kind** (`internal/event/event.go`, pinned in `event_test.go` — the wire format is
replayed, so every new kind is contract-tested):

```go
// KindSandboxAdmission records an admission decision that is worth an operator's
// attention: a wait past the note threshold, or a capacity refusal. Fast-path
// admissions emit NOTHING — one event per bash call would double the sandbox
// stream's volume to record "nothing happened".
KindSandboxAdmission Kind = "sandbox.admission"

type SandboxAdmissionPayload struct {
    Handler string `json:"handler"`           // closed enum: container|host|microvm
    Outcome string `json:"outcome"`           // closed enum: queued|refused
    Scope   string `json:"scope"`             // closed enum: global|tenant (which bound bound it)
    WaitMS  int64  `json:"wait_ms,omitempty"` // queue time; absent on an immediate refusal
}
```

Payload-free discipline holds exactly as it does for the four #0063 kinds: three closed enums
and a latency number. No command, no environment, no output, no error string. Tenant, loop, and
node come from the envelope and are never duplicated into the payload.

**Emission** follows #0063's established layering. The gate reports in bounded terms through an
observer seam shaped like `PoolHooks` — a new `GateHooks{Queued, Refused func(AdmissionInfo)}`
option on `Service`/`Gate` — and `internal/tools/sandbox_events.go` translates
`sandbox.AdmissionInfo` into `event.SandboxAdmissionPayload`, exactly as `SandboxEventHooks`
already does for acquire/release/reap. The sandbox package acquires no dependency on the event
vocabulary. Because the gate lives on the process-scoped `Service` while the event store is
per-loop, the hook is installed at the composition root wherever the loop's `EventStore` is
available; bindings with no per-loop store (one-shot, shell, research-probe, mcp-server) get
inert hooks, matching the existing nil-store behaviour.

**Projection** (`internal/observe/projector.go`):

- `classify()` maps `KindSandboxAdmission` to `OperationSandbox`, with `OutcomeSuccess` for
  `queued` (the command ran) and `OutcomeError` for `refused` (it did not).
- `decorateSandbox` lifts `Handler` onto the existing field and adds two bounded projected
  fields to `Record` — `AdmissionOutcome` and `AdmissionScope` — plus reuse of a numeric wait.
  These follow the `Verdict`/`DecisionLayer` precedent: bounded classifications on `Record`,
  never raw payload.

**Metrics** (`internal/observe/prometheus/recorder.go`), joining the five existing
`fuse_sandbox_*` families with matching label discipline (`tenant_id` under the metrics policy,
plus bounded enums; the label vocabularies are closed at the wire contract, so two new closed
maps `sandboxAdmissionOutcomes` and `sandboxAdmissionScopes` join `sandboxHandlers` /
`sandboxCauses` / `sandboxHealthReasons`):

| Family | Type | Labels | Meaning |
|---|---|---|---|
| `fuse_sandbox_exec_queued_total` | counter | `tenant_id`, `handler` | Execs that waited past the note threshold — the "backpressure is happening" rate |
| `fuse_sandbox_queue_wait_seconds` | histogram | `tenant_id`, `handler` | Distribution of queue time; the high quantiles are the saturation signal |
| `fuse_sandbox_rejected_total` | counter | `tenant_id`, `handler`, `scope` | Capacity refusals — the runaway signal, and the alert target |

**No queue-depth gauge**, and this is a deliberate omission rather than an oversight. A gauge
`Set()` from a notable-events-only stream is stale-high by construction: it would record depth
at the last *interesting* admission and then sit there while the queue drained silently,
telling operators the host is saturated when it is idle. Making it honest would require an
event per admission *and* per release — doubling stream volume for a number that
`rate(fuse_sandbox_exec_queued_total)` and the wait histogram's p95/p99 already express. If a
true real-time depth gauge is wanted later, it belongs on a direct collector, not on the
projection; that is noted in Risks and left to a follow-on.

The new families are added to the recorder's declared family table (the one at
`recorder.go:35` that pins name/type/labels) and to the collector registration list, so the
existing contract test covers them.

**Dashboards/alerts** (`deploy/observability`): a queue panel beside the existing Sandbox set —
queued rate, wait heatmap, rejected rate — and one alert rule: `fuse_sandbox_rejected_total`
rate > 0 over a window, which by design should never fire under correct configuration. New
rules pass `deploy/observability/validate.go`.

### 7. What #0075/#0076 map onto

One sentence, as scoped: `limits:` becomes a Pod's `resources.limits`
(memory/cpu/pids) in the k8s handler, and `concurrency:` becomes a namespace `ResourceQuota`
plus a parallelism bound — both templated from this same config block by the Helm chart. Neither
is designed here.

---

## Test plan

Nearly all of this is plain Go tests requiring no model at all. Where live verification is
needed, it uses cheap gateway models — never Claude/Anthropic (project rule).

**Pure unit — no container CLI required:**

1. **Argv caps, table-driven** (`container_test.go`, extending the existing golden-argv tests):
   config → exact flags. Cases: all limits set ⇒ every flag present with correct rendering and
   ordering; each limit individually unset ⇒ *that* flag absent and the others unchanged; all
   unset ⇒ argv byte-identical to the #0063 baseline (the "we did not change the uncontended
   case" guard); `--memory-swap` always present and equal whenever `--memory` is;
   `--cpus` rendered deterministically as a decimal; `--pull=never` always present.
2. **Config load** (`config_test.go`): the new `limits:`/`concurrency:` blocks parse; an
   unparsable value degrades to unset with the new `WarnReason` and a usable `Config`; a
   non-positive value likewise; an *explicit* zero is distinguishable from absent; an
   unrecognised key inside the new blocks still trips `KnownFields(true)` and discards the file
   toward the safe default.
3. **Posture defaults** (`service_test.go`): unset + hosted ⇒ every hosted default applied and
   argv carries every cap flag; unset + local ⇒ no cap flags emitted at all, but the
   concurrency backstop and pull timeout ARE applied in both postures. Explicit config is
   honoured identically in both postures (posture only fills *unset* fields).
4. **Trust boundary** (`container_test.go`): a `working_dir`, a command string, and an
   environment variable that all name plausible cap values change nothing about argv. The
   regression guard that caps are not model-reachable.
5. **Admission gate concurrency** (`admission_test.go`, run under `-race` — a sequential test
   cannot see any of this):
   - N ≫ bound concurrent `Admit` calls: at no observed instant does in-flight exceed
     `max_inflight`; a shared counter with a max-watermark, asserted.
   - The queue **drains**: all N eventually admit and complete, none errors, given deadlines
     longer than the work.
   - Per-tenant fairness: tenant A saturates its own share; tenant B's `Admit` still returns
     promptly. The cross-tenant starvation guard — and the one that fails if acquisition order
     is inverted.
   - Runaway refusal: with `max_inflight` held and `max_queued` waiters parked, the next
     `Admit` returns `ErrSandboxAtCapacity` **immediately** (not after a wait).
   - Deadline: a waiter whose ctx expires returns a ctx error, not a capacity error — the two
     conditions stay distinguishable.
   - No leak: after every caller returns, in-flight is 0 and a fresh `Admit` succeeds
     instantly — i.e. tickets released on the error, refusal, and deadline paths, not just the
     happy one (the teardown-on-every-early-return invariant, applied to slots).
6. **Gate/pool composition** (`pool_test.go`): a checked-out warm Runner held across a gap
   consumes **no** slot while idle; two Execs on the *same* Runner consume one slot at a time
   and never deadlock; a pool `Close` with a checkout blocked in `Admit` does not hang.
7. **Backpressure note** (`bash_test.go`): a fake substrate reporting `Output.Waited` above the
   threshold ⇒ the note is present, exactly once, appended after the command output, and the
   command output itself is unmodified; below the threshold ⇒ no note; a capacity refusal ⇒
   `IsError` with the capacity message and no partial-output confusion. Assert the note never
   appears in `Output.Combined` handed to any event path.
8. **Pre-pull bounding** (`container_test.go`, injected `execRunner`): a pull that hangs is cut
   at `pull_timeout`, not at the caller's deadline; a caller with a *shorter* deadline returns a
   timeout while the pull is still running, and the next `Acquire` finds it complete;
   concurrent `Acquire`s trigger exactly **one** `pull` invocation; a failed pull emits
   `sandbox.health{reason:"pull_failed"}` and is retried on a later `Acquire`.
9. **Event contract** (`event_test.go`): `KindSandboxAdmission` round-trips through the
   events.jsonl encoder and is pinned; `SandboxAdmissionPayload` carries only the bounded
   fields — a table asserting no command/env/output field exists on the type.
10. **Projection** (`projector_test.go`): the new kind projects to `OperationSandbox` with
    `OutcomeSuccess` for `queued` and `OutcomeError` for `refused`; bounded fields land; the
    resulting `Record` retains no raw payload. Mirrors the existing sandbox-projection tests.
11. **Recorder** (`recorder_test.go`): each new family exists with the declared type and label
    set; unknown enum values fall back through `boundedLabel` rather than creating a series.

**Gated on a real container CLI (skipped with a clear message otherwise, so CI without Docker
still passes — the #0063 `container_integration_test.go` pattern):**

12. **Caps actually bind:** a real container with a small `--memory` running an allocation
    loop is OOM-killed rather than consuming host RAM; a small `--pids-limit` refuses a fork
    storm; `--ulimit fsize` truncates an oversized write. Kept small and deliberately crude —
    the point is that the flags reach the runtime, not that the kernel works.
13. **End-to-end metrics:** real admission events → the real projector → the real recorder → a
    `/metrics` scrape shows `fuse_sandbox_exec_queued_total` and `fuse_sandbox_rejected_total`
    moved and `fuse_sandbox_queue_wait_seconds` has observations. Reuses #0063's e2e pattern
    directly; needs no container CLI at all if driven from synthesised events, and should be
    written that way so it runs everywhere.
14. **Alert-rule validation:** the new rule passes `deploy/observability/validate.go`.

No test in this plan requires a model. If an end-to-end agent smoke is wanted for the
backpressure note (does a model actually adapt when told?), it runs against a cheap gateway
model and is explicitly optional — the note's *presence* is unit-tested, and the model's
reaction to it is not a property this change can assert.

---

## Risks & mitigations

- **Default caps too tight ⇒ legitimate workloads break; too loose ⇒ no protection.** The
  posture split is the primary mitigation: local development, where the false-positive cost is
  highest and the threat model is weakest, gets no caps by default. Hosted defaults are sized
  for a typical build/test command and are a one-line override. A cap kill is visible: an
  OOM-killed container already emits `sandbox.health{reason:"oom"}` and increments
  `fuse_sandbox_unhealthy_total` (#0063), so "we capped too tight" is diagnosable from the
  dashboard rather than as a mystery non-zero exit.
- **Queue bound too low ⇒ spurious refusals; too high ⇒ memory held by waiters.** Refusal is
  deliberately *not* the concurrency bound but the separate, much larger waiter bound, so the
  bar for a refusal is very high (`max_inflight` running *and* `max_queued` parked). A waiter
  costs a blocked goroutine and its already-allocated request, so `max_queued` is bounded to
  keep that finite. `fuse_sandbox_rejected_total` firing at all is an alert, because under
  correct configuration it should be zero.
- **Caps are advisory or absent on some runtimes.** Rootless podman without cgroup-v2 delegation
  silently ignores `--pids-limit`/`--memory`; Docker Desktop on macOS enforces inside its own
  VM, so `--memory` bounds the container within the VM's allocation rather than the Mac's RAM.
  These flags are therefore **best-effort defence in depth, not a guarantee** — that is stated
  in the config documentation, and it is precisely why axis B exists independently: the
  concurrency ceiling is enforced in fuse's own process and holds regardless of what the
  runtime honours. The implementer should not add runtime capability probing; a silently-ignored
  flag is a runtime posture question, and pretending to detect it would be worse than
  documenting it.
- **Double-counting or deadlock between the pool and the gate.** Mitigated structurally: the
  gate is acquired around `Exec` only, a ticket never outlives one `Exec`, no lock is held
  across an `Admit`, and the two-semaphore order is fixed (tenant → global). Test 6 asserts an
  idle checkout consumes no slot and that `Close` with a blocked waiter does not hang.
- **The backpressure note leaking into payloads.** The note is rendered in `internal/tools` at
  `Result` construction and `Output.Combined` is never mutated. `sandbox.Output` is already
  documented as never appearing in an event payload; the new `Waited` field is a duration, and
  the admission payload carries a bucketed wait rather than any rendered text. Test 7 asserts
  it.
- **`--ulimit fsize` is not a disk quota.** It bounds a single file, so a command can still fill
  the mount with many small files. Documented at the config field; a real per-tenant disk quota
  requires the filesystem scoping that is #0065's, and this change deliberately does not
  pretend to deliver it.
- **No real-time queue-depth gauge.** Operators alerting on instantaneous saturation get rate
  and latency signals instead of a depth number. Accepted deliberately (see Architecture §6) as
  better than a gauge that lies; a direct collector can add one later without changing the event
  contract.
- **Designed ahead of an unmerged #0063.** Every integration point named here — `argv`'s flag
  block, `PoolSource`, `pooledRunner.Exec`, `PoolHooks`' shape, `Config`'s loader, the recorder's
  family table — is against PR #79's branch. The implementer's reconcile pass re-reads them
  before planning and records any drift in the change's reconcile log. The decisions above are
  robust to reasonable drift because they are placement decisions (gate on `Service`, caps in
  `argv`, note in `bash.go`), not line references.
- **Config surface growth.** `.fuse/sandbox.local.yml` gains ten knobs. Mitigated by every one
  being optional with a documented default, by `KnownFields(true)` making a typo loud rather
  than silent, and by all degradation paths already resolving toward the safe configuration.

---

## Assumptions

Judgment calls made in authoring this spec, in place of asking:

1. **Hosted default numbers** (memory 2g, cpus 2.0, pids 512, nofile 4096, fsize 2g) are
   invented here as "comfortably runs a typical build/test command." The grooming settled that
   fail-safe defaults must exist and must be sane; it did not settle the digits. They are
   config-overridable in one line and are the cheapest thing in this spec to change.
2. **Concurrency default numbers** (`max_inflight: 64`, `max_inflight_per_tenant: 16`,
   `max_queued: 256`, `note_threshold: 2s`) are likewise chosen to satisfy "a HIGH global bound
   that ordinary load never touches" rather than measured.
3. **`max_queued` as the refusal mechanism.** Grooming settled "refuse only in genuinely
   pathological cases" but not what makes a case pathological. A waiter-count overflow — separate
   from and much larger than the concurrency bound — is the mechanism chosen, because it keeps
   the concurrency bound purely soft, as decided.
4. **The concurrency backstop and pull timeout apply in both postures**, while caps split by
   posture. Grooming settled the local/hosted split for *caps*; extending it to the queue would
   have left a laptop unprotected against exactly the runaway the queue exists to catch, at zero
   cost when the bound is never touched.
5. **The gate lives on `Service`, not `Pool`.** Grooming said "a new type in
   `internal/tools/sandbox`, alongside or within the `Pool`." Reading the code, `Pool` is
   per-loop and `Service` is per-process, so a `Pool`-owned gate would not bound the host at
   all. Placement is on `Service`, exposed through the existing `PoolSource` seam. This is a
   refinement of the settled decision (sandbox layer, dedicated gate), not a departure from it.
6. **Semaphore acquisition order is tenant-then-global.** Not discussed in grooming; the reverse
   order defeats the per-tenant fairness that *was* settled.
7. **A new `sandbox.admission` event kind** rather than overloading an existing one. Grooming
   settled a dedicated rejection counter and a queue/wait event; a new kind is the only shape
   that carries both without widening an existing payload.
8. **Only notable admissions emit an event** (queued past threshold, or refused). Grooming did
   not address emission volume; emitting per `Exec` would roughly double the sandbox stream.
9. **No queue-depth gauge.** Grooming listed a depth gauge as a candidate family; it is
   deliberately not delivered, for the staleness reason in §6, with rate and latency families in
   its place. This is the one place this spec declines a suggestion from the settled brief, and
   it is flagged here rather than buried.
10. **Backpressure note applies on the container, host, and future microVM substrates**, since
    the gate is substrate-agnostic. Grooming framed axis A as container-only but did not scope
    axis B by substrate; making the gate universal is simpler and strictly safer.
11. **Pre-pull triggers on first `Acquire`**, not at `Service` construction, so process startup
    is never blocked by a registry.
