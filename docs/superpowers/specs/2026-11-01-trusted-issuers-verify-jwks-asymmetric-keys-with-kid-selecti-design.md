<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0099 — Trusted issuers verify JWKS asymmetric keys with kid selection and tenant prefixes](../../changes/archive/2026-11-01-0099-trusted-issuers-verify-jwks-asymmetric-keys-with-kid-selecti.md)**
<!-- docket:backlink:end -->

# Trusted issuers verify JWKS asymmetric keys with kid selection and tenant prefixes

- Type: feat
- Priority: high

## Why

`loop_server.issuers` accepts only one HS256 shared secret per issuer (`internal/loopauth/issuer.go`), so an issuing service and the fuse server hold the same secret, rotation means a coordinated restart with a window of refusals, and a server operator cannot verify tokens from a service whose private key it never sees. The issuer's allowed tenants are a fixed list (`Issuer.Tenants`), so a service that creates tenants on demand (one per team, one per project) needs a config change and restart on the server for each new tenant. `toolIdentityTenantKeys` (`cmd/fuse/tool_identity.go`) derives tool-identity signing keys only for tenants named in config, so a tenant asserted by such a service would have no key and identity-propagating MCP egress would fail for it.

## What changes

- `IssuerConfig` gains `jwks` (inline list of public JWKs with `kid`, `kty` EC P-256 or OKP Ed25519) and `jwks_url` (https URL of a JWKS document; cached, refreshed every `jwks_refresh` (default 5m) and on an unknown `kid` at most once per minute); `signing_key` (HS256) stays supported. An issuer must have exactly one of `signing_key`, `jwks`, `jwks_url`.
- `IssuerVerifier` reads the JOSE `kid` header, selects the key by `kid` within the issuer named by `iss`, and accepts `alg` ES256 or EdDSA for JWK issuers and HS256 for shared-secret issuers; any other `alg`, a missing `kid` on a JWK issuer, or an `alg` that does not match the key type is refused before any signature check.
- `IssuerConfig.tenant_prefix` (string): a token's `tenant` claim is accepted when it is in `tenants` or starts with `tenant_prefix` and is a valid tenant id. Either or both may be set; neither keeps today's rule (only `_default`).
- `toolidentity` STS derives a tenant's key lazily on first use (`HMAC-SHA256(signingKey, "fuse-tool-identity-tenant:"+tenant)`), so any tenant a verifier produces has a key; the eager map stays for tests and the default tenant verbatim.
- `fuse loop-serve-net` startup refuses a `jwks_url` that is not https (except `localhost`/loopback) and logs the issuer name, key count and kids loaded; a JWKS fetch failure at startup is fatal when no cached document exists.
- ADR recording: asymmetric issuer keys with kid rotation; tenant prefixes; lazy tenant key derivation.

## Out of scope

token scopes or any new claim (F2); revocation lists or introspection; OIDC discovery (`/.well-known/openid-configuration`); mTLS; rotating the tool-identity root key.

## Acceptance criteria

- Config with `issuer: svc, jwks: [{kid: a, ...ES256 public key...}], audience: fuse, tenant_prefix: team-` accepts a token signed by key `a` naming `tenant: team-42` and refuses `tenant: other-42`, `tenant: Team-42` and a token with `kid: b`.
- A `jwks_url` issuer loads keys at startup; after the document gains a new `kid` a token signed by it is accepted within one refresh interval without restart; a token with an unknown `kid` triggers at most one refetch per minute (test with a fake HTTP server counting requests).
- A token with `alg: HS256` presented to a JWK issuer, `alg: ES256` to a shared-secret issuer, `alg: none`, or `kid` naming a key of a different issuer is refused with `CodeUnauthenticated` and an identical error message in every case.
- Existing HS256 issuer tests pass unchanged; a config with both `signing_key` and `jwks` fails `Config.Validate` naming the issuer.
- With `tenant_prefix: team-`, a loop started under `team-42` with an identity-propagating MCP server gets a delegation token that verifies under `team-42`'s derived key and not under `team-43`'s (extend the existing per-tenant key isolation test).
- `fuse loop-serve-net --help` and the issuers doc section describe `jwks`, `jwks_url`, `jwks_refresh`, `tenant_prefix`; `make test` green.
