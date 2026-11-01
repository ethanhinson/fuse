<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0099 — Trusted issuers verify JWKS asymmetric keys with kid selection and tenant prefixes](../../changes/archive/2026-11-01-0099-trusted-issuers-verify-jwks-asymmetric-keys-with-kid-selecti.md)**
<!-- docket:backlink:end -->

# Trusted issuers JWKS plan

Inline-authored plan (plan-writer dispatch not used; warning noted in PR).

1. config: add `jwks`, `jwks_url`, `jwks_refresh`, `tenant_prefix` to IssuerConfig; validate exactly one key source, JWK shape, https jwks_url (loopback exempt), prefix validity.
2. loopauth: JWK key parsing (EC P-256, OKP Ed25519), kid selection, alg/key-type matching, uniform failures, tenant prefix, jwks_url fetcher with cache, refresh interval and 1/min unknown-kid refetch.
3. cmd/fuse: loopIssuers wiring; startup fetch (fatal without cache) and log of name/key count/kids; lazy tenant key derivation in toolidentity STS via key-derivation func.
4. tests for each acceptance criterion; docs and --help text; `make test`.
