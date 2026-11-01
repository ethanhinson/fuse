# Per-tenant loop limits: live-loop cap and start rate with per-tenant overrides

Change 0103 — Per-tenant loop limits: live-loop cap and start rate with per-tenant overrides

- Type: feat
- Priority: medium

## Why

throughput limits (`throughput.*`) are per loop; nothing bounds how many loops one tenant may keep alive or how fast it may start them on a shared networked server, so one credential holder can exhaust the process for every other tenant. `internal/runtime/inproc.go` notes per-tenant policy as a follow-up.

## What changes

- `LoopServerConfig.Limits` (`loop_server.limits`): `default {max_live_loops, starts_per_minute}` and `tenants: {<tenant>: {...}}` overrides; zero means unlimited (today's behaviour).
- Enforced at the Connect edge after authentication: a `StartLoop` beyond `starts_per_minute` (token bucket per tenant) or when the tenant's live-loop count (registry records with `Live` and an unexpired lease) is at `max_live_loops` returns `CodeResourceExhausted` with a message naming the limit and, for the rate case, a `Retry-After` header in seconds.
- Counters are per process; the registry count is the durable truth for the cap.
- `GetCapabilitiesResponse` gains `Limits {max_live_loops, starts_per_minute}` for the caller's tenant.
- ADR recording: per-tenant limits are an operator floor at the edge, process-local for rate, registry-backed for the cap.

## Out of scope

spend quotas (tokens per tenant per day); per-subject limits; distributed rate limiting across processes; limits as token claims (file as later).

## Acceptance criteria

- `limits.default.max_live_loops: 2`: the third `StartLoop` for tenant A returns `CodeResourceExhausted`; after one loop finishes (or its lease expires) the next start succeeds; tenant B is unaffected.
- `limits.default.starts_per_minute: 3` with a fake clock: the fourth start within a minute is refused with `Retry-After`; after the clock advances it succeeds.
- `limits.tenants.big.max_live_loops: 10` overrides the default for `big` only.
- No `limits` block: existing tests pass unchanged; `GetCapabilities` reports zeros.
- `make test` green.
