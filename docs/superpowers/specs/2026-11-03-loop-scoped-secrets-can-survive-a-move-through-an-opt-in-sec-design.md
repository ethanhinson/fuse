<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0136 — Loop-scoped secrets can survive a move through an opt-in secret store adapter](../../changes/active/0136-loop-scoped-secrets-can-survive-a-move-through-an-opt-in-sec.md)**
<!-- docket:backlink:end -->

# Loop-scoped secrets can survive a move through an opt-in secret store adapter

- Type: feat
- Priority: medium

## Why

`LoopConfig.Secrets` are dropped right after resolution (`internal/runtime/inproc.go:440-443`), `Resume` builds its config without them (`:1198-1203`), and re-resolution fails with a `RuleSecretRef` violation (`internal/harnessresolve/resolve.go:157-206`). ADR-0076 chose memory-only on purpose. The consequence is that every failover path for such loops is a dead end; active change 0108 only makes the error visible.

## What changes

1. `internal/loopsecret`:
   ```go
   type Store interface {
       Put(ctx, key event.StreamKey, secrets []Secret, expiry time.Time) error
       Get(ctx, key event.StreamKey) ([]Secret, error)   // ErrNone
       Delete(ctx, key event.StreamKey) error
   }
   ```
   `NoStore` (always `ErrNone`; the default), `pgstore` (`//go:build pgstore`; `loop_secrets(tenant_id, loop_id, ciphertext, expires_at)`; AES-GCM with a data key wrapped by `loop_server.secrets.store_key` via a `KeyProvider` seam — env/config now, KMS later; AAD = tenant+loop so a row cannot be replayed to another loop), `MemStore` for tests. Values are redacted in every rendering as today.
2. Runtime: `Deps.LoopSecrets loopsecret.Store`. On StartLoop with secrets and a non-`NoStore` store, `Put` BEFORE `resolveHarness` with `expiry = now + IdleTTL + LeaseTTL` and refresh the expiry on each turn boundary; `Resume` calls `Get` and passes the values on the resolution context exactly as StartLoop does; `Delete` on loop end, on cancel (`loop.cancelled`), and on `Shutdown` hand-off only when the store is `NoStore` (otherwise the hand-off is the point).
3. Config: `loop_server.secrets.loop_scoped_store: none|postgres` (default `none`), advertised in `GetCapabilities SpecPolicy.loop_secrets_resumable`.
4. Conformance `loopsecrettest.RunStoreConformance`: round trip, wrong tenant is `ErrNone`, expiry honoured, delete, ciphertext does not contain plaintext, AAD binding (a row copied to another loop id fails to decrypt).
5. Tests: two connect servers over one pgstore + one secret store; StartLoop with a spec whose MCP server uses `client_secret_ref` with a loop-scoped value against a fake MCP server checking the header; A is cut, lease expires, Send on B resumes and the fake MCP server receives the same secret; with `none`, Send on B returns the 0108 refusal.

## Out of scope

Precedence/host-binding changes; any RPC that returns values; KMS implementation.

## Acceptance criteria

1. Conformance passes for `NoStore` (trivially), `MemStore`, `pgstore` (tagged).
2. The two-server failover test passes with `postgres`; with `none` behaviour is byte-identical to today.
3. Values never appear in events, logs, or `loop.harness` (existing 0102/0110 guards extended to the store path).
4. Rows are deleted at loop end and cancel; expired rows are unreadable.
5. ADR superseding ADR-0076 for the opt-in store only, keeping memory-only as the default.
