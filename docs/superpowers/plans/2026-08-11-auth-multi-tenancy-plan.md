<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0049 — Auth / multi-tenancy — loop_id ownership and per-tenant isolation for the deployed service](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0049-auth-multi-tenancy.md)**
<!-- docket:backlink:end -->

# Implementation plan — Auth / multi-tenancy (change #49)

**Spec:** `docs/superpowers/specs/2026-08-11-auth-multi-tenancy-design.md` (on the docket branch)
**Branch:** `feat/auth-multi-tenancy` cut from `origin/main` @ e21cd22
**Transport:** Connect/protobuf `fuse.loop.v1` (ADR-0033), NOT the removed #48 WS binding.

> Authored by docket-implement-next's plan step. NOTE: the configured plan skill
> (`superpowers:writing-plans`) is not installed on this machine, so per docket's Skill-layer
> missing-skill rule the plan role degraded to `auto` and this file was authored directly. Same
> for build/review below if those skills are likewise absent — see the PR body.

## Guardrails (hold across every task)

- **Policy-free seam (ADR-0030):** `internal/runtime` imports **no** `cmd/fuse` and **no** auth
  package. Identity + authorization live at the Connect edge (`internal/loopconnect`) and the
  composition root (`cmd/fuse/loop_serve_net.go`). The one runtime-side growth (the lease heartbeat
  renewer + reap) operates only on the durable registry the seam already depends on — it imports no
  auth.
- **App-enforced tenancy (ADR-0031):** every registry/store op stays keyed by `StreamKey{Tenant,Loop}`.
  New `Owner` + lease fields are added to **both** backends (fsstore sidecar + pgstore `loops` table)
  behind the shared conformance suite `internal/event/eventtest/conformance.go` — neither may diverge.
- **Events unchanged (ADR-0024/0025):** no new event kinds, no Seq-model change.
- **TDD:** each task writes a failing test first, then the minimal code to pass, then refactors.
  `go build ./... && go test ./...` green at each task boundary. The pgstore path is build-tagged
  (`//go:build pgstore`) and needs a DB; assert the fsstore path in the default suite and keep the
  pgstore change behind the same conformance contract.
- **Live verification uses a scripted `LLM_GATEWAY_URL` double — never Claude/Anthropic.** Drive the
  real backend for at least one acceptance (`smoke-over-fake-backend-proves-wire-not-system`).
- **Stdio binding #2 (`internal/loopserver`) is untouched** — it keeps its JSON-RPC codes + local
  trust model.

## Task 1 — `LoopRecord` gains `Owner` + lease fields (registry types + conformance)

**Files:** `internal/event/registry.go`, `internal/event/eventtest/conformance.go`

- Add to `LoopRecord`: `Owner string` (authorization subject, distinct from `OwnerNodeID` the
  liveness/node id) and `LeaseExpiry time.Time` (zero = no lease).
- Extend `LoopRegistry` with a lease-renew method: `Heartbeat(ctx, key StreamKey, ownerNodeID string, expiry time.Time) error` (returns `ErrLoopUnknown` for an unregistered key). Keep `Register`/`SetLive`/`Resolve`/`List` shapes; `Register` now persists `Owner` and an initial `LeaseExpiry` carried on the `LoopRecord`.
- **Tests first** (in `conformance.go`, so BOTH backends run them): (a) `Register` with `Owner` set → `Resolve` returns the same `Owner`; (b) `Heartbeat` advances `LeaseExpiry` and `UpdatedAt`, leaves `Owner`/`Live` untouched; (c) `Heartbeat` on an unknown key → `ErrLoopUnknown`; (d) cross-tenant isolation still holds for the new fields.

**Done when:** `conformance.go` asserts Owner + lease round-trips; `go build ./...` compiles (backends still stubbed → fsstore/pgstore tasks make them pass).

## Task 2 — fsstore backend: persist `Owner` + lease, implement `Heartbeat`

**Files:** `internal/event/fsstore/registry.go`

- Extend `loopSidecar` with `owner` + `lease_expiry` JSON fields; write/read them in the atomic
  `loop.json` upsert. Preserve `CreatedAt` on re-register (existing behavior); persist `Owner` on
  first register, keep it stable across `SetLive`/`Heartbeat`.
- Implement `Heartbeat`: read sidecar (ErrLoopUnknown if absent), set `LeaseExpiry` + `UpdatedAt`,
  atomic rewrite.

**Done when:** the fsstore run of the Task-1 conformance suite is green (`go test ./internal/event/fsstore/...`).

## Task 3 — pgstore backend: `loops` schema + `Owner`/lease, implement `Heartbeat`

**Files:** `internal/event/pgstore/store.go`, `internal/event/pgstore/schema.sql` (build tag `pgstore`)

- Add columns `owner text NOT NULL DEFAULT ''` and `lease_expiry timestamptz` to the `loops` table.
- `Register` upsert writes `owner` + `lease_expiry`; `Resolve`/`List` select them; implement
  `Heartbeat` as an `UPDATE ... SET lease_expiry=$_, updated_at=now() WHERE (tenant,loop)` returning
  `ErrLoopUnknown` on 0 rows.
- The migration must be idempotent (`ALTER TABLE ... ADD COLUMN IF NOT EXISTS`) so an existing DB
  upgrades cleanly.

**Done when:** the pgstore conformance run passes under `-tags pgstore` against a DB (documented as
requiring a database; the default `go test ./...` compiles the non-tagged path). Keep the pgstore
assertion behind the SAME conformance suite as fsstore.

## Task 4 — `Verifier` seam + static bearer-token impl

**Files:** new `internal/loopauth/verifier.go` (+ `verifier_test.go`)

- `Principal{Tenant event.TenantID; Subject string}` and `Verifier interface { Verify(ctx, token string) (Principal, error) }`.
- `StaticVerifier`: constructed from a token→Principal map; `Verify` returns the mapped principal or
  a sentinel `ErrInvalidToken` (also for empty token). No dependency on runtime or cmd/fuse.
- **Tests first:** known token → principal; unknown/empty token → `ErrInvalidToken`; a `_default`
  dev token maps to `event.DefaultTenant`.

**Done when:** `go test ./internal/loopauth/...` green. (Package name avoids importing runtime/auth
into the seam; `internal/loopconnect` and `cmd/fuse` import it, `internal/runtime` never does.)

## Task 5 — Connect auth interceptor (identity extraction, unary + streaming)

**Files:** new `internal/loopconnect/auth.go` (+ `auth_test.go`)

- A `connect.Interceptor` (`WrapUnary` + `WrapStreamingHandler`) that reads `Authorization: Bearer <t>`
  (`req.Header().Get` unary; `conn.RequestHeader().Get` streaming), calls `Verifier.Verify`, and
  stores the `Principal` on the request `context.Context` (a private context key + `PrincipalFrom(ctx)`
  accessor). Missing/invalid token → `connect.NewError(connect.CodeUnauthenticated, ...)` for both
  unary and streaming, short-circuiting before the handler.
- **Tests first:** unary with valid token → handler sees principal in ctx; unary missing/invalid →
  `CodeUnauthenticated`, handler NOT called; streaming valid → principal in ctx; streaming missing →
  `CodeUnauthenticated`. Use a stub `Verifier` and a stub next-handler.

**Done when:** `go test ./internal/loopconnect/...` green for the interceptor.

## Task 6 — Enforce tenant + ownership in the handlers (authorize at the edge)

**Files:** `internal/loopconnect/handler.go`, `internal/loopconnect/observe.go`

- The `Handler` gains a `LoopRegistry` reference (read-only `Resolve`) so the edge can authorize
  without the seam importing auth. `NewHandler` grows a variadic/option to inject it (keep the
  existing constructor working for the pure-transport tests, or default to a nil-registry that skips
  authz — decide at build to keep existing tests green; preferred: an explicit `WithRegistry`).
- **StartLoop:** read `PrincipalFrom(ctx)`; if the wire `m.Tenant` is non-empty it MUST equal
  `principal.Tenant` else `CodePermissionDenied`; call the seam with `principal.Tenant`; after the
  handle exists, record `Owner = principal.Subject` on the registry (via a small edge helper — either
  a `Register` re-assert or a dedicated set-owner path; the seam's `StartLoop` already Registers with
  `OwnerNodeID`, so the edge sets `Owner` right after, or the registry `Register` is extended to carry
  it — choose the minimal path that keeps the seam auth-free).
- **Send / Observe:** read the principal; validate wire tenant must-equal-or-empty; `Resolve` the
  `(principal.Tenant, loop)` record — `ErrLoopUnknown` → `CodeNotFound`; `record.Owner != ""` &&
  `record.Owner != principal.Subject` → `CodePermissionDenied`; else proceed to the existing seam call
  with `principal.Tenant`.
- **Tests first:** valid owner passes; cross-owner (same tenant) → `CodePermissionDenied`;
  cross-tenant / unknown → `CodeNotFound`; tenant-spoof (wire tenant ≠ principal) → `CodePermissionDenied`.

**Done when:** handler/observe authz tests green; existing pure-transport tests still green.

## Task 7 — Owner liveness lease: heartbeat renewer + reap/re-own (runtime side)

**Files:** `internal/runtime/inproc.go` (+ tests)

- On `StartLoop`, after `Register`, start a background renewer goroutine that calls
  `Registry.Heartbeat(key, ownerNodeID, now+TTL)` every ~⅓ TTL until the loop's run context ends;
  stop it at completion alongside the existing `SetLive(false)`.
- **Reap/re-own on resolve:** in `resolveDurable`, when a record is `Live == true && now > LeaseExpiry`,
  treat it as abandoned — the resolving (cold) instance may re-own: `Heartbeat` to renew under its own
  `ownerNodeID` and proceed (for a parked persistent loop per #53, resume keyed off
  `event.KindLoopParked`; a completed one-shot serves replay only). A live, non-expired record is left
  to its owner.
- TTL is a `runtime.Deps`/config value with a sane default (tens of seconds); renewal at ~⅓ TTL.
- **Tests first (no real model — use the existing runtime test doubles / scripted gateway):**
  (a) a live loop's lease is renewed over time (advance a fake clock, assert `LeaseExpiry` advances);
  (b) a record with `Live:true` and an expired lease is treated as abandoned and re-ownable on resolve;
  (c) a non-expired live record is NOT reaped.

**Done when:** runtime lease tests green; `internal/runtime` still imports no auth/cmd.

## Task 8 — Compose at the server root + reconnect/re-own acceptance

**Files:** `cmd/fuse/loop_serve_net.go`, config surface, an acceptance test (scripted gateway)

- Build a `Verifier` from config (static token→principal map / secret source; a dev `_default` token
  keeps local `loop-serve-net` usable) and wire the auth interceptor into
  `loopv1connect.NewLoopServiceHandler(handler, connect.WithInterceptors(authInterceptor))`; inject the
  registry into the handler (`WithRegistry`) for authz; thread the lease TTL into `runtime.Deps`.
- Config: verifier config + lease TTL keys (leaning: auth required for the networked binding; stdio +
  local CLI bindings unaffected — they never hit the Connect edge).
- **Acceptance tests (real backend, scripted `LLM_GATEWAY_URL`, loud skip):**
  (1) auth pass/deny end-to-end over the real Connect server;
  (2) tenant-spoof rejected;
  (3) cross-tenant not-found vs cross-owner permission-denied;
  (4) **reconnect happy path** — re-present token, `Observe(from_seq=lastSeq)`, no loss / no dup,
      driving live + replay CONCURRENTLY (force an append into the subscribe→replay gap per
      `replay-live-handoff-dedup-at-watermark`);
  (5) **re-own after simulated owner death** — expire the lease, reconnect as the owner on a fresh
      instance, resume serving.

**Done when:** `go build ./... && go test ./...` green (pgstore path behind its tag); the acceptance
suite passes against the real runtime; skips (if any toolchain-gated) are loud.

## Task 9 — Docs + help text

**Files:** `cmd/fuse/loop_serve_net.go` help/flags, README note.

- Document the bearer-token requirement, the static verifier config, and the lease TTL for
  `fuse loop-serve-net`. Whole-suite gate.

## Sequencing & risk

- Tasks 1→2→3 (registry + both backends) are the durable foundation; 4→5→6 (verifier, interceptor,
  edge authz) the auth layer; 7 the lease lifecycle; 8 composition + the hard acceptance properties; 9
  docs. Highest-risk tasks: **6** (edge authz without leaking auth into the seam) and **8** (the
  concurrent reconnect/no-dup + re-own properties). Keep the hard no-loss/no-dup assertion on the
  authoritative Go side.
