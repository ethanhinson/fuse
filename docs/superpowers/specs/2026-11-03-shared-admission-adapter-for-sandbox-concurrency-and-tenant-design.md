<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0133 — Shared admission adapter for sandbox concurrency and tenant start rate](../../changes/archive/2026-11-07-0133-shared-admission-adapter-for-sandbox-concurrency-and-tenant.md)**
<!-- docket:backlink:end -->

# Shared admission adapter for sandbox concurrency and tenant start rate

- Type: feat
- Priority: medium

## Why

- Sandbox admission: `internal/tools/sandbox/admission.go:78-90` (`global *semaphore; tenants map[TenantID]*tenantGate`) on a process-scoped `Service` (`service.go:91,428`). N replicas ⇒ N× `max_inflight(_per_tenant)`.
- Tenant start rate: `internal/tenantlimit` keeps per-tenant minute buckets in memory; `docs/deploying.md:168-171` documents "applies per pod". The live-loop cap already counts through the registry (`tenantlimit.RegistryCounter`, `cmd/fuse/loop_serve_net.go:559-563`).

## What changes

1. `internal/admission`:
   ```go
   type Scope struct { Tenant event.TenantID; Name string }         // e.g. {t, "sandbox.inflight"}, {"", "sandbox.inflight"}, {t, "loop.starts"}
   type Admitter interface {
       Acquire(ctx, scope Scope, limit int, ttl time.Duration) (release func(), err error) // counted slots with a lease; ErrOverLimit
       Hit(ctx, scope Scope, limit int, window time.Duration) (n int, allowed bool, err error) // fixed-window counter
   }
   ```
   `admission.Local` (semaphores + windows, exact; the current behaviour) and `admission/pg` (`//go:build pgstore`): `admission_slots(scope, holder, expires_at)` with `INSERT … WHERE (SELECT count(*) …) < $limit` in one statement, and `admission_windows(scope, window_start, n)` upsert returning `n`; slot leases are renewed by the holder every `ttl/3` and expire on crash. `admission.Mem` for tests.
2. Conformance `admissiontest.RunAdmitterConformance`: cap respected under concurrent acquirers from two Admitter instances (shared backend); release frees a slot; expired slot frees without release; window resets; tenant scopes independent; wrong scope never counts.
3. Sandbox: `Gate` delegates to an `Admitter` (`sandbox.WithAdmitter`), the queue/wait semantics and `sandbox.admission` events unchanged; the fleet limit is advertised in `GetCapabilities` as `fleet_wide: true|false`.
4. Tenant limiter: `StartsPerMinute` uses `Hit`; `Local` keeps 0107's eviction behaviour.
5. Config: `admission.backend: local|postgres` (default `local`; the Helm chart sets `postgres` when a DSN is configured, with a values comment).
6. Two-runtime test: two sandbox `Service`s over one `admission/pg` (or `Mem` shared) with `max_inflight_per_tenant=2`; four concurrent `bash` calls across both runtimes ⇒ exactly two run at once (observable via the host substrate and a barrier), two queue; the same for `starts_per_minute=3` with five `StartLoop`s across two connect servers ⇒ two refused with the existing limit error.

## Out of scope

Provider RPM/TPM buckets; fairness/priority; removing local admission.

## Acceptance criteria

1. Conformance passes for `Local`, `Mem`, and `pg` (tagged).
2. Two-runtime cap tests pass for sandbox inflight and start rate.
3. A crashed holder's slot is reclaimed after `ttl` (test with a stopped renewer).
4. `docs/deploying.md` no longer says the start rate is per pod when `admission.backend=postgres`; `GetCapabilities` reports `fleet_wide`.
5. Existing 0077/0103 suites pass with `Local`.
6. ADR: admission is an adapter; counts and windows are shared when a shared backend is configured.
