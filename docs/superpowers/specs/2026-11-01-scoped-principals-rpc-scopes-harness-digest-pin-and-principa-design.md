<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0100 — Scoped principals: RPC scopes, harness digest pin and principal record on every loop](../../changes/archive/2026-11-01-0100-scoped-principals-rpc-scopes-harness-digest-pin-and-principa.md)**
<!-- docket:backlink:end -->

# Scoped principals: RPC scopes, harness digest pin and principal record on every loop

- Type: feat
- Priority: high

## Why

a `Principal` is `{Tenant, Subject, ObservabilityOperator}`, so every bearer a hosting operator issues is all-or-nothing within its tenant: a credential meant only to observe a loop can start one; one meant to start one specific harness can start any spec. `StartLoop` computes `harness_digest` and returns it but never checks it against anything, so an issuer cannot say "this token may run exactly this spec". No event in a loop's durable stream names the credential that started it, and the tool-identity broker runs with a nil audit sink in production (`cmd/fuse/tool_identity.go:249`), so an operator cannot answer "who started this loop and under which client".

## What changes

- `loopauth.Principal` gains `Scopes []string`, `HarnessDigest string`, `ClientID string` (JWT `azp`), `Issuer string`, `TokenID string` (JWT `jti`). `IssuerVerifier` reads `scope` (space-separated string per RFC 8693 §4.2), `harness_digest`, `azp`, `jti`. Static tokens get `scopes` (optional list) in `AuthTokenConfig`.
- Scope vocabulary, as constants in `loopauth`: `loop:start`, `loop:send`, `loop:observe`, `loop:admin`, `capabilities:read`, `secrets:inline` (defined here, used by F4). Empty `Scopes` means unrestricted (today's behaviour). The handler enforces: `StartLoop` needs `loop:start`; `Send` needs `loop:send`; `Observe` needs `loop:observe`; `GetCapabilities` needs `capabilities:read`; an unknown scope string is ignored. Refusal is `CodePermissionDenied` with a message naming the missing scope.
- `loop:admin` lets `Send`/`Observe`/orchestrate act on any loop in the principal's tenant regardless of `Owner`; without it the owner rule is unchanged.
- Digest pin: when `Principal.HarnessDigest` is set, `StartLoop` refuses (`CodePermissionDenied`) a request whose `harness.Digest(m.Harness)` differs or whose `harness` is unset; the check runs before the runtime is touched.
- A new event `loop.principal` (`event.KindLoopPrincipal`, payload `{subject, client_id, issuer, scopes, token_id}`) is appended right after `loop.harness` at `StartLoop` when a principal is present; redaction rules unchanged (no token material).
- The tool-identity broker gets an audit sink in `loop-serve-net` that appends a `tool.identity.minted` event (`{subject, audience, scope, loop_id}`) to the loop's stream.
- `sdk/fuse` `RemoteCredentials` gains `TokenProvider func(ctx) (string, error)`; when set it is called per request instead of `Token`. (Small, keeps SDK clients able to use short-lived issuer tokens; no refresh loop.)
- ADR recording: scopes are a claim, enforced at the Connect edge, unrestricted when absent; digest pin; principal and mint audit events.

## Out of scope

`jti` replay cache or revocation; scope-based narrowing of the HarnessSpec (model or MCP allowlists are F3); per-loop ACLs beyond owner/admin; SDK token refresh loops; TS SDK parity (file as later).

## Acceptance criteria

- An issuer token with `scope: "loop:observe"` can `Observe` a loop of its subject and gets `CodePermissionDenied` naming `loop:start` from `StartLoop` and `loop:send` from `Send`; a token with no `scope` claim behaves exactly as before (existing tests pass).
- A token with `scope: "loop:start loop:admin"` can `Send` to a loop owned by another subject in the same tenant; without `loop:admin` it gets `CodePermissionDenied`; cross-tenant stays `CodeNotFound`.
- A token with `harness_digest: sha256:…` of spec S: `StartLoop` with S succeeds and the response digest equals the claim; with S' (one field changed) or no spec it is refused with `CodePermissionDenied` and no loop is registered.
- Static token config `scopes: [loop:observe]` restricts the same way; `scopes` omitted is unrestricted.
- After `StartLoop` with an issuer token carrying `azp: client-1`, `jti: x`, the loop's stream contains one `loop.principal` event with `subject`, `client_id: client-1`, `issuer`, `scopes`, `token_id: x`; with the stdio binding no such event is emitted.
- A loop that calls an identity-propagating MCP server appends one `tool.identity.minted` event per mint, never containing the token.
- `sdk/fuse` test: a `TokenProvider` returning a new value per call sets that value on each request's `Authorization` header.
- Docs: `docs/` loop-server auth section lists the scopes and the digest pin; `make test` green.
