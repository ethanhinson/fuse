---
id: 99
slug: 'trusted-issuers-verify-jwks-asymmetric-keys-with-kid-selecti'
title: 'Trusted issuers verify JWKS asymmetric keys with kid selection and tenant prefixes'
status: 'proposed'
priority: 'high'
type: 'feat'
deliverable: 'pr'
created: '2026-11-01'
updated: '2026-11-01'
depends_on: []
stacked_on:
related: []
discovered_from: []
adrs: []
spec:
plan:
results:
auto_groomable:
hold:
branch_prefix:
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
<!-- docket:artifacts:end -->

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
