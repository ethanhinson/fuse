<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0135 — MCP OAuth tokens live in a token store adapter and never open a browser on a server](../../changes/active/0135-mcp-oauth-tokens-live-in-a-token-store-adapter-and-never-ope.md)**
<!-- docket:backlink:end -->

# MCP OAuth tokens live in a token store adapter and never open a browser on a server

- Type: fix
- Priority: high

## Why

`internal/mcp/oauth.go:263-304` reads/writes `~/.fuse/mcp-tokens/<server>.json`; `getOAuth2Token` (`:332-409`) falls back to `runBrowserFlow` (`:110-175`: local listener, `open`/`xdg-open`, 2-minute wait), called from `dial` (`manager.go:352-361`) inside every loop's `NewToolRegistry`, i.e. inside StartLoop and Resume. Only operator config can use `oauth2` (`harnessresolve/resolve.go:209-226`). Under (a) tokens diverge and concurrent refreshes invalidate single-use refresh tokens; under (b) a replica without the file hangs two minutes then drops the server.

## What changes

1. `internal/mcp`:
   ```go
   type TokenStore interface {
       Load(ctx, server string) (*TokenSet, error)                 // ErrNoToken
       Save(ctx, server string, t *TokenSet) error
       Refresh(ctx, server string, fn func(old *TokenSet) (*TokenSet, error)) (*TokenSet, error) // serialised across instances
   }
   ```
   `FileTokenStore` (today's path, `Refresh` under a file lock), `pgtokens` (`//go:build pgstore`; `mcp_tokens(server, ciphertext, updated_at)`; AES-GCM with `mcp.token_key` from config/env; `Refresh` under `pg_advisory_xact_lock(hashtext(server))` and re-reads before refreshing so a concurrent refresher reuses the new token), `MemTokenStore`.
2. `ManagerOption` `WithTokenStore(store)`; `GetAccessToken` becomes a method using the store. `WithHeadless(true)` (set by the server bindings): no browser flow; `ErrNoToken` is returned and the server is marked `auth_required` in `mcp.server` events and `GetCapabilities` instead of being silently skipped.
3. `fuse mcps login <server>` (CLI) runs the browser flow and `Save`s into the configured store so operators seed the fleet once.
4. Conformance `mcptest.RunTokenStoreConformance`: round trip, `ErrNoToken`, `Refresh` runs `fn` once under concurrent callers across two store instances over one backend, ciphertext never contains the token (pg).
5. Tests: two Managers over one `pgtokens` (tagged) or one `FileTokenStore` dir with an expired token and a fake OAuth server counting refresh calls ⇒ exactly one refresh; a headless Manager with no token returns within 1s with `auth_required`.

## Out of scope

Spec-defined OAuth; changing the browser flow for the CLI; token rotation policy.

## Acceptance criteria

1. Conformance passes for file, mem, pg stores.
2. Server bindings never call `runBrowserFlow` (test: headless Manager + missing token ⇒ `ErrNoToken` within 1s, `mcp.server{status:"auth_required"}`).
3. Two instances refreshing the same expired token perform one refresh (counter on the fake OAuth server).
4. `fuse mcps login` stores a token the loop server then uses.
5. Existing 0089 static-credential and MCP suites pass.
6. ADR: tokens are a shared adapter; servers are headless.
