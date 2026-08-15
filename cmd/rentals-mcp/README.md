# rentals-mcp

Serves the Wander demo's rentals MCP server (`internal/mcpdemo/rentals`) on a real
address over HTTP/SSE: `GET /sse` for the event stream, `POST /messages` for JSON-RPC.
It exposes three tools — `search_rentals`, `favorite_listing`, `list_favorites` — and
adjudicates every call by the *verified delegated token's* identity, so a favorite always
lands in the calling principal's set.

```sh
go run ./cmd/rentals-mcp \
  --addr 127.0.0.1:8091 \
  --audience https://rentals.example \
  --signing-key "$FUSE_TOOL_IDENTITY_SIGNING_KEY" \
  --tenants acme,globex
```

## Flags

Every flag has an environment-variable fallback; the flag wins when both are set.

| Flag | Env | Default | Meaning |
| --- | --- | --- | --- |
| `--addr` | `RENTALS_ADDR` | `127.0.0.1:8091` | Listen address. Port `0` binds an ephemeral port. |
| `--audience` | `RENTALS_AUDIENCE` | — | **Required.** The RFC 8707 resource id this server accepts. A token bound to a different audience is rejected. |
| `--signing-key` | `RENTALS_SIGNING_KEY` | — | fuse's tool-identity signing key. Per-tenant verification keys are derived from it exactly as fuse's built-in STS derives them, so it must match the `tool_identity.signing_key` of the fuse instance calling this server. |
| `--tenants` | `RENTALS_TENANTS` | — | Comma-separated tenant ids to key from `--signing-key`. Required with (and requires) `--signing-key`. Include `_default` if any caller uses fuse's default tenant. |
| `--tenant-key` | — | — | An explicit verification key as `tenant=<hex>`. Repeatable, combinable with `--signing-key`, and wins on collision. |
| `--favorites-dir` | `RENTALS_FAVORITES_DIR` | *(empty)* | Directory for durable per-principal favorites. Empty ⇒ in-memory, lost on restart. |
| `--data` | `RENTALS_DATA` | `auto` | Listing backend: `canned`, `live`, or `auto`. |
| `--max-results` | — | `5` | Maximum listings returned by the live backend. |

## Tenant keys

At least one verification key is required — with none, every caller is unauthorized, so
the server refuses to start. Supply them either by derivation (`--signing-key` +
`--tenants`, the normal path for a fuse-fronted demo) or explicitly (`--tenant-key`).
A missing `--audience`, a malformed `--tenant-key`, or an unreadable favorites directory
all fail loudly at startup rather than serving an unusable server.

One tenant is keyed differently, and it must be: `_default` (fuse's `event.DefaultTenant`)
takes the signing key **verbatim**, with no derivation — because fuse's own built-in STS
signs `_default` tokens with the raw key, keeping its single-user CLI/shell paths
unchanged. Every *named* tenant gets the HMAC-derived key. This server mirrors that split,
so listing `_default` in `--tenants` is all that is needed for a default-tenant caller
(such as a loop-server auth entry that omits `tenant:`) to verify.

## Data backends

- `canned` — the deterministic in-repo listings. Hermetic; no network, no credential.
- `live` — web-search-backed listings, resolved through `internal/research.Resolve`
  (`TAVILY_API_KEY`, `BRAVE_SEARCH_API_KEY`, or a custom endpoint). A missing credential
  is a startup error.
- `auto` (default) — live when a credential is configured, otherwise **canned**. This is
  why the server, and every test, runs on canned data with no key present.

The live backend is demo-grade: city and price are best-effort text extraction from
search results, not a data contract, and any provider error or timeout degrades to an
empty result set rather than a failed tool call.

## Shutdown

`SIGINT`/`SIGTERM` trigger a graceful shutdown with a 10s grace period, after which live
connections are forced closed. There is deliberately no `WriteTimeout`: `/sse` is an
unbounded server-push stream and a write deadline would sever every live MCP session at
exactly that interval.
