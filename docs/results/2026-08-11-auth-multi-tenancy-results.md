<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0049 — Auth / multi-tenancy — loop_id ownership and per-tenant isolation for the deployed service](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0049-auth-multi-tenancy.md)**
<!-- docket:backlink:end -->

# Auth / multi-tenancy — results
Change: #49 · Branch: feat/auth-multi-tenancy · PR: <pending> · Plan: docs/superpowers/plans/2026-08-11-auth-multi-tenancy-plan.md · ADRs: 34

## Verify (human)

Automated: `go build ./... && go test ./...` green (fsstore path; pgstore behind `-tags pgstore` verified against a real Postgres 16 container during build). `go vet ./...` clean, `go test -race ./internal/runtime/...` clean. The whole-branch review found no CRITICAL/MAJOR issues. Beyond that, at the merge gate please confirm:

- [ ] **Deployment auth config.** Before deploying `fuse loop-serve-net`, set a real `loop_server.auth` token list in trusted config (`~/.fuse/config.yml`). With no config a single **loudly-logged built-in dev token `fuse-dev-token`** (→ `_default` tenant) is synthesized so local use works — it is world-known. A deploy that forgets to configure auth would accept that token. (See Follow-up 2 for an optional hardening.)
- [ ] **Re-own end-to-end (optional manual).** The expired-lease re-own path is unit-tested at the runtime layer (`internal/runtime/lease_test.go`: `TestResolveReOwnsExpiredLease` and siblings). A true second-instance-death simulation over the live Connect wire is NOT a cmd/fuse acceptance test (heavy). If you want end-to-end assurance for the "redeploy then attach from your phone" story, exercise it manually: start a loop, kill the owning process, start a fresh instance, re-`Observe(from_seq)` with the same bearer token, confirm it resumes.

## Findings

- **Transport had been replaced under the spec.** The spec was written against change #48's JSON-over-WebSocket + HTTP-replay binding, which change #55 (ADR-0033, supersedes ADR-0032) **removed** in favor of a Connect/protobuf `fuse.loop.v1` transport over h2c. The reconcile pass re-mapped the design onto Connect (interceptor instead of WS handshake header; `connect.Code*` instead of JSON-RPC codes; the `Observe` server-stream already subsumes the old HTTP replay). Design intent unchanged; details in the change's `## Reconcile log`.
- **`Owner` (subject) vs `OwnerNodeID` (node) were conflated** in the original spec. The authorization owner is a NEW `LoopRecord.Owner` field distinct from the existing `OwnerNodeID` (liveness/instance id). Recorded in **ADR-0034**.
- **Scope shrank vs the original spec:** the `Runtime` seam already carried tenant (#47/#55), the `r.loops` cache already re-asserts tenant on hit, and all three proto requests already carried a `tenant` field — so this change added only edge-side identity/authz + the registry `Owner`/lease fields, not the seam-threading the spec anticipated.
- **Decisions recorded in ADR-0034** (`edge-enforced-auth-multi-tenancy-loop-ownership`): Verifier seam, edge-only enforcement over the policy-free runtime seam, token-authoritative tenant, `Owner` vs `OwnerNodeID`, the Connect-code authz taxonomy with a bounded intra-tenant existence oracle, the backend-uniform liveness lease, and the fail-usable (not fail-open) no-verifier posture.

## Follow-ups

1. **`SetOwner` registry method (quality, optional).** `recordOwner` at StartLoop does a Resolve-then-`Register` full upsert to set `Owner`, which can transiently write back an older `LeaseExpiry` if the heartbeat renewer fires in the gap. It is benign and self-healing (TTL ≫ heartbeat interval, next tick re-advances). A dedicated `SetOwner(key, subject)` registry method touching only the `owner` column would make it airtight. Not filed as a stub (auto-capture disabled this repo); a human may groom it.
2. **Randomize the dev token per process (security hardening, optional).** The no-verifier fallback synthesizes a fixed constant `fuse-dev-token`. Randomizing it per process start (and printing it) would remove the world-known-credential footgun for a mis-deployed server, at the cost of the fixed-value smoke test. Loud logging mitigates it today.
3. **End-to-end re-own acceptance is deferred to a manual/future test** (see Verify above) — the transport+auth+reconnect acceptance ran live; the two-instance re-own is covered at the runtime unit layer only. Per `smoke-over-fake-backend-proves-wire-not-system`, this is recorded rather than overclaimed.
