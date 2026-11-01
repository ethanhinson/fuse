<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0103 — Per-tenant loop limits: live-loop cap and start rate with per-tenant overrides](../../changes/active/0103-per-tenant-loop-limits-live-loop-cap-and-start-rate-with-per.md)**
<!-- docket:backlink:end -->

# Per-tenant loop limits - Implementation Plan

> Executed by `docket-build`: one task at a time, each task TDD (failing test, minimal code, passing test, commit). Every test uses fakes (fake runtime, fake registry, fake clock) or a scripted `httptest` gateway. No live model traffic.

> Authored inline by the plan writer: `superpowers:writing-plans` is not available in this environment (missing-skill rule).

**Goal:** An operator can bound, per tenant, how many loops a tenant keeps alive (`max_live_loops`) and how fast it starts them (`starts_per_minute`) on a `fuse loop-serve-net` server. Config lives under `loop_server.limits` with a `default` block and per-tenant overrides. Zero (or no block) means unlimited, which is today's behaviour. A refused `StartLoop` returns `CodeResourceExhausted` with a message naming the limit, and for the rate case a `Retry-After` header in whole seconds. `GetCapabilities` reports the caller's effective limits.

**Architecture:**
- `internal/config`: `LoopServerConfig.Limits` (`yaml:"limits"`) holding `Default LoopLimitConfig` and `Tenants map[string]LoopLimitConfig`. Honoured only from the trusted home file, like the rest of `loop_server`. Validated: no negative values, every tenant key is `_default` or a valid tenant id.
- `internal/tenantlimit` (new leaf package, imports only `internal/event` and the stdlib): `Config`/`Limits` with per-axis override resolution, a per-tenant token bucket for the start rate (process-local), a live-loop admission check backed by an injected `LiveCounter`, and `RegistryCounter`, which counts registry records that are `Live` with an unexpired lease. Admission holds a per-tenant lock across count-and-reserve, so concurrent starts cannot both slip under the cap.
- `internal/loopconnect`: `Handler.WithLimits(*tenantlimit.Limiter)`. `StartLoop` calls `Admit` after authentication, the tenant-spoof check and the orchestrate authorization, and before `rt.StartLoop`. It maps a `*tenantlimit.RefusedError` to `CodeResourceExhausted` and sets `Retry-After` on the error metadata. `GetCapabilities` fills `Limits` from the same limiter, so the advertisement and the enforcement read one source.
- `proto/fuse/loop/v1/loop.proto`: new `TenantLimits` message and `GetCapabilitiesResponse.limits = 7`. Regenerated Go and TS stubs. The public Go SDK maps it to `fuse.Capabilities.Limits`.
- `cmd/fuse/loop_serve_net.go`: `loopTenantLimiter(cfg, deps.Registry, now)` builds the limiter (nil when no limit is configured) and threads it through `serveNetOptions.limits` into the handler. A configured limit with no registry refuses to start.

**Tech stack:** Go, connect-go, buf/protoc-gen-go/protoc-gen-connect-go/protoc-gen-es, `internal/config`, `internal/loopconnect`, `cmd/fuse`, `sdk/fuse`.

**Learnings applied:**
- `security-knob-inert-at-composition-root`: a limiter that every unit test builds by hand can be inert in the binary. Task 6 runs the real `fuse loop-serve-net` command with a `limits` config and asserts both refusals over the wire, and fails if `NewLimiter` has no non-test caller in `cmd/fuse`.
- `race-invisible-to-race-detector-without-concurrent-test`: the limiter's maps are read and written on every concurrent `StartLoop`. Task 2 includes a `-race` test that drives many concurrent `Admit` calls against one tenant and asserts the cap is never exceeded.
- `cache-over-tenant-scoped-source-reassert-key-on-hit`: every limiter map (buckets, pending reservations, locks) is keyed by the normalized tenant, and the live count reads the tenant-scoped `Registry.List(tenant)`. Tests assert tenant B is unaffected by tenant A.
- `spec-asserted-error-code-verify-emitter-first`: `CodeResourceExhausted` is new as a server emission at this edge. Before this change the only mention in `internal/loopconnect` is the client-side `retryableCode` in `resilience.go`, which treats it as retryable with a short backoff (25ms to 1s). `RetryUnary` has no non-test caller today (verify with `grep -rn RetryUnary --include='*.go' . | grep -v _test`), so nothing in the shipped binary hot-retries a limit refusal. Leave `resilience.go` unchanged and mention it in the PR body. A future caller of `RetryUnary` around `StartLoop` should honour `Retry-After`. The handler is the only emitter, and the tests assert the code at the client.
- `reconcile-verify-claims-against-origin-not-working-tree`: verified on the feature tree at `4b23acc` that `Deps.Registry` is always non-nil for `loop-serve-net` (`selectDurableBackend` returns the fs store as its own registry when there is no DSN), that `LoopRegistry.List(ctx, tenant)` exists in fsstore and pgstore, and that the runtime stamps `LeaseExpiry` at `Register` time (`internal/runtime/inproc.go` around line 586), so a just-started loop counts at once.

**Decisions made in this plan (put them in the PR body; the ADR is recorded outside this plan by the implement-next flow):**
1. **Per-axis override.** A tenant override axis that is `> 0` replaces the default for that axis. An omitted or `0` override axis inherits the default. This mirrors `throughput.providers` (`internal/ratelimit.Config`). So `tenants.big: {max_live_loops: 10}` keeps the default `starts_per_minute`. An override cannot make one axis unlimited while the default limits it: set a large number instead. Document this in the README.
2. **Live means what the runtime means.** A record counts when `Live && (LeaseExpiry.IsZero() || !now.After(LeaseExpiry))`. This is exactly the runtime's own "not abandoned" test (`reapIfAbandoned`, `internal/runtime/inproc.go` around line 1330). A zero lease counts as live, because the runtime treats it as never reapable.
3. **Check order: rate, then cap.** The rate check is in memory and cheap, so it runs first and protects the registry `List` from a hammering client. Every `StartLoop` that passes authentication and reaches the limiter spends one start token, including one the cap then refuses, and one whose `rt.StartLoop` later fails. `starts_per_minute` therefore bounds start attempts.
4. **Bucket shape.** Capacity equals `starts_per_minute` (one minute of burst), refill is `starts_per_minute / 60` tokens per second, and a new tenant's bucket starts full. Same shape as `internal/ratelimit.axis`. `Retry-After` is `ceil((1 - tokens) / refillPerSec)` seconds, at least 1.
5. **Cap admission is count plus in-flight reservations, under a per-tenant lock.** `Admit` takes a per-tenant mutex, counts live records, and refuses when `live + pending >= max`. Otherwise it increments `pending` and returns a `release` func. The handler defers `release()` until `StartLoop` returns. Between the runtime's `Register` and `release` a loop counts twice. That can only cause a spurious refusal (fail closed), never an over-admit. Only the count is serialized, not `rt.StartLoop`.
6. **A registry count error refuses the start** with `CodeInternal` ("cannot count live loops"). Fail closed: an unreadable registry must not disable the cap.
7. **Scope of the cap is `StartLoop` admission.** A `Send` that transparently resumes a finished loop (change 0054) is not refused and spends no start token: refusing it would break a reconnecting chat. A resume can therefore take a tenant above its cap until loops finish. Record this in the PR body as a known limit.
8. **Counters are per process.** Rate buckets live in memory per replica, so N replicas allow up to N times the rate. The cap reads the shared registry and holds across replicas, apart from the cross-replica start race (two replicas can each admit the last slot). Distributed limiting is out of scope.
9. **No limiter when no limit is configured.** `loopTenantLimiter` returns nil when `Config.Any()` is false. `Handler.limits == nil` is the unlimited fast path, and `GetCapabilities` reports zeros.
10. **The wire message is `TenantLimits`**, field `GetCapabilitiesResponse.limits = 7`, with `uint32 max_live_loops = 1; uint32 starts_per_minute = 2;`. The handler always sets it (zeros when unlimited), so a client never has to tell "absent" from "unlimited".

---

## Task 1: Config - `loop_server.limits` schema, trusted-only load, validation

**Tier:** standard

**Files:**
- Modify: `internal/config/schema.go`
- Modify: `internal/config/loader.go`
- Modify: `internal/config/validate.go`
- Create: `internal/config/limits_test.go`

**Step 1: Write failing tests** in `internal/config/limits_test.go`, modelled on `internal/config/secrets_test.go`:
- `TestLoopServerLimitsLoadFromTrustedFileOnly`: YAML
  ```yaml
  loop_server:
    limits:
      default: {max_live_loops: 2, starts_per_minute: 3}
      tenants:
        big: {max_live_loops: 10}
  ```
  `FromYAML(doc, "home")` resolves `c.LoopServer.Limits.Default == LoopLimitConfig{MaxLiveLoops: 2, StartsPerMinute: 3}` and `c.LoopServer.Limits.Tenants["big"].MaxLiveLoops == 10`. Then, as the secrets test does, swap `warnw` for a buffer and `mergeData(&u, doc, ".fuse.local.yml", false, nil)` into `Default()`. Assert `u.LoopServer.Limits` is the zero value and that the warning mentions `loop_server`.
- `TestLoopServerLimitsDefaultIsUnlimited`: `Default().LoopServer.Limits` is the zero value and `Default().Problems()` is empty.
- `TestLoopServerLimitsProblems`, table-driven, one problem expected per case, asserting `Path` and `Message`:
  - `Default.MaxLiveLoops: -1` gives path `loop_server.limits.default.max_live_loops`, message `must be >= 0, got -1`.
  - `Default.StartsPerMinute: -5` gives path `loop_server.limits.default.starts_per_minute`, message `must be >= 0, got -5`.
  - `Tenants{"acme": {MaxLiveLoops: -1}}` gives path `loop_server.limits.tenants.acme.max_live_loops`.
  - `Tenants{"Acme": {MaxLiveLoops: 1}}` gives path `loop_server.limits.tenants`, message `tenant "Acme" must be _default or lowercase letters, digits, '-' or '_' (starting alphanumeric)`.
  - `Tenants{"": {MaxLiveLoops: 1}}` gives the same path and the same message with `""`.
  - Valid: `Tenants{"_default": {...}, "team-a": {...}}` gives no problems.
- `TestSpecLayerCannotSetLoopServerLimits`: if there is an existing spec-layer test that asserts `loop_server` is dropped from a spec (search `internal/config/overlay_test.go` and `harness/` for `LoopServer`), extend it to include `limits`. If no such test exists, skip this item: the spec layer already zeroes the whole `raw.LoopServer` (`loader.go` around line 450).

**Step 2: Run them, confirm they fail:** `go test ./internal/config -run 'TestLoopServerLimits' -count=1`. The expected red is a compile failure on `Limits` / `LoopLimitConfig`.

**Step 3: Implement.**
- `schema.go`: add to `LoopServerConfig`, after `Secrets`:
  ```go
  Limits   LoopLimitsConfig     `yaml:"limits"`
  ```
  and new types next to it:
  ```go
  // LoopLimitsConfig is loop_server.limits (change 0103): per-tenant bounds on
  // how many loops a tenant keeps alive and how fast it starts them. Default
  // applies to every tenant; Tenants overrides it per tenant id, axis by axis
  // (an override axis > 0 replaces the default axis, 0 or omitted inherits it).
  // Zero everywhere means unlimited. Operator-only, trusted-file only.
  type LoopLimitsConfig struct {
  	Default LoopLimitConfig            `yaml:"default"`
  	Tenants map[string]LoopLimitConfig `yaml:"tenants"`
  }

  // LoopLimitConfig is one tenant's limits. 0 on an axis means unlimited
  // (in Default) or inherit-the-default (in a Tenants override).
  type LoopLimitConfig struct {
  	MaxLiveLoops    int `yaml:"max_live_loops"`
  	StartsPerMinute int `yaml:"starts_per_minute"`
  }

  // Configured reports whether any limit (default or override) is set.
  func (l LoopLimitsConfig) Configured() bool { ... true if any field != 0 in Default or any Tenants entry, OR len(Tenants) > 0 ... }
  ```
  `Configured` returns true whenever `len(Tenants) > 0`, even with all-zero entries, so the loader carries the block and validation sees bad tenant keys. Extend the `LoopServerConfig` doc comment with a `Limits` bullet.
- `loader.go` (around line 662): add `|| raw.LoopServer.Limits.Configured()` to the condition. Inside the trusted branch add `if raw.LoopServer.Limits.Configured() { c.LoopServer.Limits = raw.LoopServer.Limits }`. Update the comment to say limits are operator policy: a repo file must not loosen or tighten them. The existing warning text already names `loop_server`.
- `validate.go`: after `validateSecrets`, add `ps = append(ps, limitProblems(c.LoopServer.Limits)...)` with:
  ```go
  // limitProblems checks loop_server.limits (change 0103): no negative axis, and
  // every override key is a tenant id a principal can actually carry.
  func limitProblems(l LoopLimitsConfig) []Problem
  ```
  Check the default axes, then the tenant keys in sorted order (use the existing `sortedKeys` helper if it fits `map[string]LoopLimitConfig`, else `slices.Sorted(maps.Keys(...))`). For each key: if `key != "_default" && !loopauth.ValidTenantID(key)`, add the `loop_server.limits.tenants` problem (`validate.go` already imports `loopauth`). Otherwise check that key's two axes under `loop_server.limits.tenants.<key>.<axis>`. Messages use the existing `"must be >= 0, got %d"` form.

**Step 4: Run tests, confirm green:** `go test ./internal/config -count=1`.

**Step 5: Commit:** `config: loop_server.limits for per-tenant live-loop cap and start rate`.

---

## Task 2: `internal/tenantlimit` - resolution, rate bucket, cap admission

**Tier:** premium (the admission accounting is the enforcement; a TOCTOU or a leaked reservation is a silent over-admit or a permanent lockout)

**Files:**
- Create: `internal/tenantlimit/limiter.go`
- Create: `internal/tenantlimit/limiter_test.go`

**Step 1: Write failing tests** in `internal/tenantlimit/limiter_test.go`. Use a mutable fake clock (`type fakeClock struct{ mu sync.Mutex; t time.Time }` with `Now()` and `Advance(d)`), and a counter func backed by a mutex-guarded `map[event.TenantID]int` (lock both get and set: see `mutex-test-double-concurrent-provider`).
- `TestConfigForPerAxisOverride`: `Config{Default: {MaxLiveLoops: 2, StartsPerMinute: 3}, Tenants: {"big": {MaxLiveLoops: 10}}}`. `For("big") == {10, 3}`, `For("small") == {2, 3}`, `For("") == For("_default") == {2, 3}`. With `Tenants["_default"] = {MaxLiveLoops: 7}`, `For("")` gives `MaxLiveLoops == 7`.
- `TestConfigAny`: zero config gives false. Default-only gives true. An override-only config with a nonzero axis gives true.
- `TestAdmitUnlimitedNeverCounts`: `Config{}` with a counter that calls `t.Fatal`. `Admit` returns nil error and a non-nil no-op `release`. A nil `*Limiter`'s `Admit` and `For` are also safe and unlimited.
- `TestAdmitCapRefusesAtMax`: `MaxLiveLoops: 2`, counter returns 2 for `"a"`. `Admit(ctx,"a")` returns a `*RefusedError` with `Axis == AxisLiveLoops`, `Limit == 2`, `RetryAfter == 0`, and an `Error()` containing `max_live_loops` and `2`. Tenant `"b"` (count 0) is admitted.
- `TestAdmitCapCountsInFlightReservations`: `MaxLiveLoops: 2`, counter returns 0. Two `Admit`s succeed without release. The third is refused. After one `release()` the next succeeds. Calling the same `release` twice decrements only once (idempotent).
- `TestAdmitCapCounterErrorRefuses`: the counter returns `errors.New("boom")`. `Admit` returns an error that is NOT a `*RefusedError` and wraps `boom`. No reservation is leaked: with the counter fixed, `MaxLiveLoops: 1` and count 0, the next `Admit` succeeds.
- `TestAdmitRateRefusesWithRetryAfter`: `StartsPerMinute: 3` with the fake clock. Three `Admit`s succeed and the fourth returns a `*RefusedError` with `Axis == AxisStartRate`, `Limit == 3`, and `RetryAfter == 20*time.Second` (deficit 1 token at 0.05 tokens/s). `Advance(20*time.Second)` and the next `Admit` succeeds. Another `"b"` tenant's first start succeeds while `"a"` is exhausted.
- `TestAdmitRateRetryAfterRoundsUpToWholeSeconds`: `StartsPerMinute: 7` (refill 7/60 per second). Exhaust, `Advance(1*time.Second)`, refuse, and assert `RetryAfter` is a whole number of seconds `>= 1`, equal to `ceil((1 - tokens)/refill)` computed in the test.
- `TestAdmitRateCheckedBeforeCap`: `{MaxLiveLoops: 1, StartsPerMinute: 1}`, counter returns 1. The first `Admit` is refused by the cap. The second is refused by the RATE (`AxisStartRate`): the first attempt spent the token (Decision 3).
- `TestRegistryCounterCountsLiveUnexpired`: an in-test `event.LoopRegistry` fake (or a small `List`-only fake satisfying the interface) holding, for tenant `"a"`: live with a future lease (counts), live with a zero lease (counts), live with an expired lease (does not count), not live (does not count). Plus one live record for tenant `"b"`. `RegistryCounter(reg, clock.Now)(ctx, "a") == 2`. A `List` error propagates.
- `TestAdmitConcurrentNeverExceedsCap` (run under `-race`): `MaxLiveLoops: 5` with a counter that returns the number of currently "registered" loops for the tenant (a mutex-guarded int the test goroutine increments after a successful `Admit`, before `release`, to model the runtime's `Register`). Launch 50 goroutines, each `Admit`, then on success increment "registered", `release()`. Never decrement, so the loops stay live. Assert exactly 5 admissions and that "registered" never exceeds 5.

**Step 2: Run them, confirm they fail:** `go test ./internal/tenantlimit -count=1`. The package does not exist yet.

**Step 3: Implement** `internal/tenantlimit/limiter.go`:
```go
// Package tenantlimit bounds, per tenant, how many loops a tenant keeps alive
// and how fast it starts them on a shared loop server (change 0103). It is the
// operator floor the Connect edge enforces after authentication: the start rate
// is a process-local token bucket per tenant, and the live-loop cap counts the
// durable registry (the cross-process truth) plus this process's in-flight
// starts. It imports only internal/event and the standard library.
package tenantlimit

type Limits struct{ MaxLiveLoops, StartsPerMinute int }
type Config struct {
	Default Limits
	Tenants map[string]Limits
}
func (c Config) Any() bool
func (c Config) For(tenant event.TenantID) Limits // normalizes; per-axis override (Decision 1)

type Axis string
const (
	AxisLiveLoops Axis = "max_live_loops"
	AxisStartRate Axis = "starts_per_minute"
)

// RefusedError is a start refused by a limit. RetryAfter is set only for the
// rate axis.
type RefusedError struct {
	Tenant     event.TenantID
	Axis       Axis
	Limit      int
	RetryAfter time.Duration
}
func (e *RefusedError) Error() string
// live:  `tenant "a" is at its loop_server.limits max_live_loops (2); wait for a loop to finish`
// rate:  `tenant "a" exceeded its loop_server.limits starts_per_minute (3); retry after 20s`

type LiveCounter func(ctx context.Context, tenant event.TenantID) (int, error)

// RegistryCounter counts the tenant's records that are live and not abandoned,
// by the runtime's own rule (Decision 2).
func RegistryCounter(reg event.LoopRegistry, now func() time.Time) LiveCounter

type Limiter struct {
	cfg   Config
	count LiveCounter
	now   func() time.Time

	mu      sync.Mutex
	buckets map[event.TenantID]*bucket
	pending map[event.TenantID]int
	locks   map[event.TenantID]*sync.Mutex
}

// New builds a Limiter. now defaults to time.Now. It refuses a config that sets
// any max_live_loops with a nil counter: a cap with nothing to count is inert.
func New(cfg Config, count LiveCounter, now func() time.Time) (*Limiter, error)

func (l *Limiter) For(tenant event.TenantID) Limits                                    // nil-safe: zero
func (l *Limiter) Admit(ctx context.Context, tenant event.TenantID) (release func(), err error) // nil-safe: no-op
```
`Admit` algorithm (tenant normalized with `event.NormalizeTenant` first):
1. `lim := l.cfg.For(t)`. If both axes are 0, return `noop, nil`.
2. Rate (if `lim.StartsPerMinute > 0`): under `l.mu`, get or create the bucket (capacity = rate, full, `last = now`), refill to `now`. If `tokens < 1`, compute `RetryAfter` (Decision 4) and return `nil, &RefusedError{...}`. Otherwise `tokens -= 1`.
3. Cap (if `lim.MaxLiveLoops > 0`): get or create the per-tenant `*sync.Mutex` under `l.mu`, then `tl.Lock(); defer tl.Unlock()`. `n, err := l.count(ctx, t)`. On error return `nil, fmt.Errorf("tenantlimit: count live loops for %q: %w", t, err)`. Under `l.mu`: if `n + l.pending[t] >= lim.MaxLiveLoops`, return the live refusal. Otherwise `l.pending[t]++`.
4. Return a `sync.Once`-guarded `release` that decrements `pending[t]` under `l.mu` and deletes the key at 0. If only the rate axis is set, return `noop`.

Keep the bucket refill math in a small private `bucket` type (`capacity`, `refillPerSec`, `tokens`, `last`), as `internal/ratelimit.axis` does. Do not import `internal/ratelimit`: that type is unexported and blocks waiters, while this one refuses.

**Step 4: Run tests, confirm green:** `go test -race ./internal/tenantlimit -count=1`.

**Step 5: Commit:** `tenantlimit: per-tenant start-rate bucket and registry-backed live-loop cap`.

---

## Task 3: Wire contract - `TenantLimits` on `GetCapabilitiesResponse`, regenerate stubs

**Tier:** economy

**Files:**
- Modify: `proto/fuse/loop/v1/loop.proto`
- Regenerate: `internal/loopwire/v1/loop.pb.go`, `proto/gen/ts/fuse/loop/v1/loop_pb.ts` (and anything else `generate.sh` rewrites; commit exactly what it produces)

**Step 1: Edit the proto.** In `GetCapabilitiesResponse`, after `ServerInfo server = 6;`:
```proto
  // The caller's tenant's effective loop limits (loop_server.limits, change
  // 0103): the default with this tenant's overrides applied. 0 means unlimited.
  // Always set; all-zero when the server configures no limits.
  TenantLimits limits = 7;
```
Add after `message ServerInfo`:
```proto
// TenantLimits bounds one tenant's loops on this server. A StartLoop beyond
// either limit fails with RESOURCE_EXHAUSTED; the rate refusal carries a
// Retry-After header in seconds. Rate counters are per server process.
message TenantLimits {
  // Most loops the tenant may have live at once (0: unlimited).
  uint32 max_live_loops = 1;
  // Most StartLoop calls per minute, as a token bucket with one minute of burst
  // (0: unlimited).
  uint32 starts_per_minute = 2;
}
```
Also add one sentence to the `StartLoop` rpc comment (or the `StartLoopRequest` doc): a start beyond the tenant's limits fails `RESOURCE_EXHAUSTED`.

**Step 2: Regenerate.** The codegen tools are installed in `$HOME/go/bin` but are not on `PATH`. The installed versions match the pins: protoc-gen-go v1.36.12, protoc-gen-connect-go 1.20.0, buf 1.75.0. The TS plugin needs `proto/node_modules`, which is absent in this worktree.
```sh
cd <worktree>/proto && npm ci
cd <worktree> && PATH="$HOME/go/bin:$PATH" ./proto/generate.sh
```
`buf lint` must pass (field and message naming). If `npm ci` or the BSR dependency fetch fails for lack of network, return BLOCKED with the error. Never hand-edit generated files. `node_modules/` is gitignored, so do not commit it.

**Step 3: Verify:** `go build ./... && go test ./internal/loopconnect ./sdk/fuse -count=1` (still green: the new field is unset and unused so far). `git status` shows only the proto plus the regenerated stubs.

**Step 4: Commit:** `proto: GetCapabilitiesResponse carries the tenant's loop limits`.

---

## Task 4: Connect edge - enforce on `StartLoop`, advertise on `GetCapabilities`

**Tier:** premium (the edge mapping is the user-visible contract: code, message, Retry-After, and no reservation leak on any return path)

**Files:**
- Modify: `internal/loopconnect/handler.go`
- Modify: `internal/loopconnect/capabilities.go`
- Modify: `internal/capabilities/capabilities.go`
- Create: `internal/loopconnect/limits_test.go`
- Modify: `internal/loopconnect/capabilities_test.go`

**Step 1: Write failing tests** in `internal/loopconnect/limits_test.go`. Reuse `fakeRuntime`, `fakeHandle` (`handler_test.go`), `fakeRegistry`, `tokenVerifier` and `bearer` (`handler_authz_test.go`). Add:
- A `registeringRuntime` struct embedding `*fakeRuntime`, with `reg *fakeRegistry`, a `lease func() time.Time`, a mutex and a counter. It overrides `StartLoop`: it mints `loop-<n>`, `Register`s `event.LoopRecord{Key: {NormalizeTenant(cfg.Tenant), id}, OwnerNodeID: "node", Live: true, LeaseExpiry: lease()}` into `reg` (modelling the runtime seam), and returns `fakeHandle{id}`. A `startErr` field makes it fail without registering.
- A helper `newLimitServer(t, rt runtime.Runtime, reg event.LoopRegistry, lim *tenantlimit.Limiter, v loopauth.Verifier) loopv1connect.LoopServiceClient`, the same as `newAuthzServer` plus `.WithLimits(lim)`.
- A fake clock shared by the limiter (`tenantlimit.New(cfg, tenantlimit.RegistryCounter(reg, clk.Now), clk.Now)`) and the lease stamps (`lease = clk.Now().Add(30*time.Second)`).

Tests (tokens: `tok-a` for tenant `a`, `tok-b` for tenant `b`, `tok-big` for tenant `big`):
- `TestStartLoopLiveCapRefusesThirdAndRecovers` (acceptance 1): `Default{MaxLiveLoops: 2}`. Two `StartLoop`s for `a` succeed. The third returns `connect.CodeResourceExhausted`, its message contains `max_live_loops`, and `cerr.Meta().Get("Retry-After") == ""`. Tenant `b` succeeds while `a` is capped. Then `reg.SetLive(ctx, {a, loop-1}, false, "node")` and the next `a` start succeeds.
- `TestStartLoopLiveCapRecoversOnLeaseExpiry` (acceptance 1, lease branch): cap 2, two starts. Advance the clock past the 30s lease with no heartbeat and the next start succeeds.
- `TestStartLoopRateRefusesWithRetryAfter` (acceptance 2): `Default{StartsPerMinute: 3}`. Three starts succeed. The fourth is `CodeResourceExhausted`, its message contains `starts_per_minute`, and `Retry-After == "20"`. Advance 20s and the next start succeeds.
- `TestStartLoopTenantOverride` (acceptance 3): `Default{MaxLiveLoops: 2}, Tenants{"big": {MaxLiveLoops: 10}}`. `big` starts 10 and the 11th is refused. `a` is refused at its 3rd.
- `TestStartLoopRefusalNeverReachesRuntime`: after a refusal, the runtime's start count did not increase (the seam was not called).
- `TestStartLoopFailedStartReleasesReservation`: cap 1, runtime `startErr` set. The start returns `CodeInternal` (existing mapping). Clear `startErr` and the next start succeeds. This proves `release` runs on the error path.
- `TestStartLoopSpoofCheckedBeforeLimits`: rate 1, a spoofed wire tenant gets `PermissionDenied`, and a legitimate start right after still succeeds (no token spent by the spoof).
- `TestStartLoopCountErrorIsInternal`: a registry whose `List` errors (wrap `fakeRegistry` and override `List`), with cap 1. The start fails `CodeInternal` and the runtime was not called.
- `TestStartLoopNoLimiterUnchanged`: no `WithLimits`. 20 starts all succeed.

In `internal/loopconnect/capabilities_test.go` add:
- `TestGetCapabilitiesReportsTenantLimits`: limiter with `Default{2,3}, Tenants{"big":{MaxLiveLoops:10}}`. `GetCapabilities` as `big` gives `Limits == {MaxLiveLoops:10, StartsPerMinute:3}`, and as `a` gives `{2,3}`. Follow the existing capabilities test setup for the `CapabilitySource` fake.
- `TestGetCapabilitiesLimitsZeroWithoutLimiter` (acceptance 4): no limiter. `resp.Msg.Limits` is non-nil and both fields are 0.

**Step 2: Run them, confirm they fail:** `go test ./internal/loopconnect -run 'Limit|Override|Rate|LiveCap|Reservation|CountError' -count=1` (compile failure on `WithLimits` / `GetLimits`).

**Step 3: Implement.**
- `internal/capabilities/capabilities.go`: add `Limits Limits` to `Capabilities` (`// Limits are the tenant's effective loop limits (0 = unlimited).`) and `type Limits struct{ MaxLiveLoops, StartsPerMinute int }`. The source does not fill it. The edge does, from the limiter that enforces (one source).
- `handler.go`:
  - Add the field `limits *tenantlimit.Limiter`, with a doc comment that nil means unlimited (the fast path), and `func (h *Handler) WithLimits(l *tenantlimit.Limiter) *Handler`, chainable, where nil keeps unlimited.
  - Update the package doc's import list sentence to mention `internal/tenantlimit`.
  - In `StartLoop`, directly after the `OrchestrateLoopId` authorization block and before `loopBase`:
    ```go
    // Per-tenant limits (change 0103) apply after authentication and the spoof
    // and orchestrate checks, so an unauthenticated or refused caller spends
    // nothing, and before the seam, so a refused start creates no loop.
    release, lerr := h.limits.Admit(ctx, tenant)
    if lerr != nil {
        cerr := limitError(lerr)
        apiOutcome = observeotel.OutcomeFromError(ctx, cerr)
        return nil, cerr
    }
    defer release()
    ```
  - Add:
    ```go
    // limitError maps an Admit failure: a limit refusal is ResourceExhausted
    // naming the limit, with Retry-After (whole seconds) for the rate axis; a
    // failure to count live loops is Internal (fail closed).
    func limitError(err error) *connect.Error
    ```
    Use `errors.As(err, &refused)`. For `RetryAfter > 0`, `cerr.Meta().Set("Retry-After", strconv.Itoa(int(math.Ceil(refused.RetryAfter.Seconds()))))`.
- `capabilities.go`: in `GetCapabilities`, after `c, err := h.caps.Capabilities(...)` succeeds, set `lim := h.limits.For(tenant)` and `c.Limits = capabilities.Limits{MaxLiveLoops: lim.MaxLiveLoops, StartsPerMinute: lim.StartsPerMinute}`. In `toProtoCapabilities`, always set `out.Limits = &loopv1.TenantLimits{MaxLiveLoops: clampUint32(c.Limits.MaxLiveLoops), StartsPerMinute: clampUint32(c.Limits.StartsPerMinute)}`.

**Step 4: Run tests, confirm green:** `go test -race ./internal/loopconnect ./internal/capabilities -count=1`.

**Step 5: Commit:** `loopconnect: enforce per-tenant loop limits on StartLoop and advertise them`.

---

## Task 5: Public Go SDK - `Capabilities.Limits`

**Tier:** economy

**Files:**
- Modify: `sdk/fuse/capabilities.go`
- Modify: `sdk/fuse/remote.go`
- Modify: `sdk/fuse/capabilities_test.go`

**Step 1: Write a failing test** in `sdk/fuse/capabilities_test.go`, following the file's existing pattern for testing `fromProtoCapabilities` or a fake server: a response with `Limits: &loopv1.TenantLimits{MaxLiveLoops: 4, StartsPerMinute: 9}` maps to `Capabilities.Limits == fuse.Limits{MaxLiveLoops: 4, StartsPerMinute: 9}`, and a response with `Limits: nil` maps to the zero value.

**Step 2: Run it, confirm it fails:** `go test ./sdk/fuse -run Capabilit -count=1`.

**Step 3: Implement.** In `capabilities.go` add `Limits Limits` to `Capabilities` (doc: the tenant's effective loop limits on this server, 0 meaning unlimited; a `StartLoop` beyond them fails, and the rate refusal tells the caller when to retry) and `type Limits struct { MaxLiveLoops int; StartsPerMinute int }` with field docs. In `remote.go` `fromProtoCapabilities` set `Limits: Limits{MaxLiveLoops: int(m.GetLimits().GetMaxLiveLoops()), StartsPerMinute: int(m.GetLimits().GetStartsPerMinute())}`. Do not add a new SDK error sentinel for `ResourceExhausted` (out of scope): `mapConnectError`'s default arm already surfaces it.

**Step 4: Run tests, confirm green:** `go test ./sdk/fuse -count=1`.

**Step 5: Commit:** `sdk: expose the tenant's loop limits in Capabilities`.

---

## Task 6: Composition root - build and wire the limiter in `loop-serve-net`; end-to-end acceptance

**Tier:** standard

**Files:**
- Modify: `cmd/fuse/loop_serve_net.go`
- Create: `cmd/fuse/loop_serve_net_limits_test.go`

**Step 1: Write failing tests** in `cmd/fuse/loop_serve_net_limits_test.go`:
- `TestLoopTenantLimiterFromConfig`: `loopTenantLimiter(cfg, reg, now)` returns `nil, nil` when `cfg.LoopServer.Limits` is zero. It returns a non-nil limiter whose `For("big")`/`For("x")` reflect the config when limits are set. It returns an error when `max_live_loops` is set and `reg` is nil.
- `TestLoopServeNetEnforcesTenantLimitsOverH2C`, modelled step for step on `TestLoopServeNetAdvertisesCapabilitiesOverH2C` in `cmd/fuse/loop_serve_net_capabilities_test.go`: scripted gateway answering `done`, temp `HOME`, the `netListen`/`serveNetContext` swap, and `run([]string{"loop-serve-net", ...})` in a goroutine with the same cleanup. Config:
  ```yaml
  loop_server:
    auth:
      - {token: tok-a, tenant: tenant-a, subject: a}
      - {token: tok-b, tenant: tenant-b, subject: b}
      - {token: tok-c, tenant: tenant-c, subject: c}
    limits:
      default: {max_live_loops: 1}
      tenants:
        tenant-b: {max_live_loops: 3}
        tenant-c: {starts_per_minute: 1}
  ```
  Use the raw `loopv1connect` client over `h2cClient()` with `bearerClientInterceptor` per token, and the same readiness retry loop on the first call. Assertions:
  - `GetCapabilities` as tenant-a gives `limits {max_live_loops:1, starts_per_minute:0}`. As tenant-b: `{3, 0}`. As tenant-c: `{1, 1}`.
  - tenant-a `StartLoop{Task:"t", Interactive:true}` succeeds (an interactive loop parks after its turn and stays live). A second tenant-a interactive start gives `CodeResourceExhausted` with a message containing `max_live_loops`.
  - tenant-b starts two interactive loops and both succeed (override, and unaffected by tenant-a).
  - tenant-c's first start succeeds. The second gives `CodeResourceExhausted` with a message containing `starts_per_minute` and a non-empty integer `Retry-After` (parse it with `strconv.Atoi`, `> 0`).
  If the parked loops make the 5-second shutdown wait flaky, look at how `cmd/fuse/loop_serve_net_rentals_acceptance_test.go` (or another interactive loop-serve-net test) tears down. Do not raise the timeout blindly.
- `TestLoopServeNetConstructsLimiter` (wiring guard, `security-knob-inert-at-composition-root`): read `loop_serve_net.go` from disk. Assert it contains `loopTenantLimiter(` and that `serveNetWithOptions` passes `limits:`. Alternatively, `go/parser` the `cmd/fuse` non-test files and assert at least one non-test call to `tenantlimit.New`. Prefer the parser form if it stays short.
- Extend `TestLoopServeNetAdvertisesCapabilitiesOverH2C` (no `limits` block): assert `resp.Msg.GetLimits() != nil` and both fields are 0 (acceptance 4, over the real binary).

**Step 2: Run them, confirm they fail:** `go test ./cmd/fuse -run 'TenantLimit|LoopServeNetEnforces|ConstructsLimiter|AdvertisesCapabilities' -count=1`.

**Step 3: Implement** in `cmd/fuse/loop_serve_net.go`:
- ```go
  // loopTenantLimiter builds the per-tenant loop limiter (change 0103) from
  // loop_server.limits, counting live loops in the durable registry. No limit
  // configured: nil (the edge's unlimited fast path). A cap with no registry to
  // count is refused rather than served inert.
  func loopTenantLimiter(cfg config.Config, reg event.LoopRegistry, now func() time.Time) (*tenantlimit.Limiter, error)
  ```
  Convert `config.LoopLimitsConfig` to `tenantlimit.Config` field by field. If `!tcfg.Any()`, return nil. Use `tenantlimit.RegistryCounter(reg, now)` when `reg != nil`, else a nil counter, so that `tenantlimit.New` refuses a cap with nothing to count.
- In `runLoopServeNet`, after `rt := runtime.New(deps)`, call `limiter, lerr := loopTenantLimiter(cfg, deps.Registry, time.Now)`. On error, call `shutdownObservability()`, print `loop-serve-net: limits: %v` and return 1. When `limiter != nil`, print one stderr line naming the default limits and the number of tenant overrides (operators should see that the floor is on).
- Add `limits *tenantlimit.Limiter` to `serveNetOptions` (doc: nil is unlimited) and pass `limits: limiter` in the `serveNetWithOptions` call. In `serveNetWithOptions`, chain `.WithLimits(sopts.limits)` onto the handler construction.
- Extend the `fs.Usage` text: under `loop_server:` add
  ```
      limits:                    # per-tenant loop limits; 0 or omitted => unlimited
        default: {max_live_loops: 20, starts_per_minute: 60}
        tenants:
          big: {max_live_loops: 100}   # per axis: a set value replaces the default
  ```
  and a paragraph: a `StartLoop` beyond a limit fails `ResourceExhausted` (the rate refusal carries `Retry-After`). Rate counters are per process, while the live-loop count reads the shared registry. `GetCapabilities` reports the caller's limits. Also add "and the tenant's loop limits" to the existing GetCapabilities usage paragraph.

**Step 4: Run tests, confirm green:** `go test -race ./cmd/fuse -run 'TenantLimit|LoopServeNet' -count=1`, then `go test ./cmd/fuse -count=1`.

**Step 5: Commit:** `loop-serve-net: build and wire per-tenant loop limits`.

---

## Task 7: Documentation

**Tier:** economy

**Files:**
- Modify: `README.md` (the `loop_server` section around line 515 and the RPC list around line 630)
- Modify: `docs/deploying.md` (the "Config is a Secret" paragraph around line 160)

**Step 1: Edit.**
- README: add a `#### Per-tenant loop limits` subsection after the trusted-issuers subsection. Include a YAML example (`default` and `tenants`), the per-axis override rule (and that an override cannot unset one axis: use a large value), the zero-means-unlimited rule, the `ResourceExhausted` and `Retry-After` behaviour, the check order (rate before cap, and every authenticated start attempt spends a start token), the per-process rate versus registry-backed cap, and that a `Send` resuming a finished loop is not refused. In the RPC list: note under `StartLoop` that it may fail `ResourceExhausted`, and add a `limits` bullet under `GetCapabilities` (the caller's effective `max_live_loops` / `starts_per_minute`, 0 meaning unlimited).
- deploying.md: in the sentence listing the kept `config.loop_server` keys, add `limits`. Add one sentence saying that with several replicas the start rate applies per pod.

**Step 2: Verify:** `go test ./... -run 'Doc|Readme|README' -count=1` if any doc-lint test exists (`grep -rln 'README.md' --include='*_test.go' .`). Otherwise just proofread.

**Step 3: Commit:** `docs: per-tenant loop limits`.

---

## Task 8: Final verification

**Tier:** economy

**Step 1:** `make test` (runs `observability-validate` then `go test ./...`). It must be green.
**Step 2:** `go test -race ./internal/tenantlimit ./internal/loopconnect ./cmd/fuse -count=1`.
**Step 3:** `go vet ./...`.
**Step 4:** Check `git status` is clean, and `grep -rn "tenantlimit.New\|loopTenantLimiter(" cmd/fuse --include='*.go' | grep -v _test` shows the composition-root call.
**Step 5:** No commit unless a fix was needed. If one was, commit it as `fix: <what>`.

---

## Acceptance coverage map

| Acceptance criterion | Covered by |
|---|---|
| `max_live_loops: 2`: third start for A is `ResourceExhausted`; recovers after a finish or lease expiry; B unaffected | Task 4 `TestStartLoopLiveCapRefusesThirdAndRecovers`, `TestStartLoopLiveCapRecoversOnLeaseExpiry`; Task 2 cap tests; Task 6 end to end (cap 1) |
| `starts_per_minute: 3` with a fake clock: fourth refused with `Retry-After`; succeeds after the clock advances | Task 2 `TestAdmitRateRefusesWithRetryAfter`; Task 4 `TestStartLoopRateRefusesWithRetryAfter`; Task 6 end to end (rate 1, real clock) |
| `tenants.big.max_live_loops: 10` overrides for `big` only | Task 2 `TestConfigForPerAxisOverride`; Task 4 `TestStartLoopTenantOverride`; Task 6 tenant-b |
| No `limits` block: existing tests unchanged; `GetCapabilities` reports zeros | Task 4 `TestStartLoopNoLimiterUnchanged`, `TestGetCapabilitiesLimitsZeroWithoutLimiter`; Task 6 extension of the existing capabilities h2c test; whole suite in Task 8 |
| `make test` green | Task 8 |
