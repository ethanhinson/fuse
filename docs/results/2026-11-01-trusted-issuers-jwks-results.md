<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0099 — Trusted issuers verify JWKS asymmetric keys with kid selection and tenant prefixes](../changes/archive/2026-11-01-0099-trusted-issuers-verify-jwks-asymmetric-keys-with-kid-selecti.md)**
<!-- docket:backlink:end -->

# Results: Trusted issuers verify JWKS asymmetric keys

**Human action:** No action is required before merge; the optional walkthrough below adds confidence beyond the scripted tests.

## Outcome

`loop_server.issuers` entries may now use exactly one of `signing_key` (HS256, unchanged), `jwks` (inline EC P-256 or Ed25519 public keys) or `jwks_url` (cached https JWKS document). Tokens select their key by `kid`; ES256 and EdDSA are accepted for JWK issuers, HS256 only for shared-secret issuers, and every refusal is the same Unauthenticated error. `tenant_prefix` admits any valid lowercase tenant id with that prefix, and the built-in tool-identity STS derives each tenant's key on first use. `--help` and the README describe the new keys. Decisions are recorded in ADR-0072.

## Human actions and testing

- **Optional:** point a local `fuse loop-serve-net` at a small HTTP server serving a JWKS with one Ed25519 key, mint an EdDSA token with `kid` and `tenant: team-1` (issuer configured with `tenant_prefix: team-`), and call the server with it. Expect acceptance; a token with an unknown `kid` or tenant `other-1` should be refused. Nothing persistent changes. Scripted tests already cover this logic.

## Verification performed

- Full suite (`make test`) green at the final head via the native gate driver.
- Review ran at the standard tier. Fixed in-branch: failed JWKS refreshes were retried on every token (now one attempt per interval, on a detached context); a lock was held across the fetch (now stale-while-revalidate); an empty document cleared the cache (now treated as a failed fetch); redirects could downgrade https (now refused).

## Known issues and follow-ups

- **Lazy tenant keys remove the STS's own tenant allowlist.** When it occurs: any tenant the loop verifier admits now mints tool-identity tokens, whether or not config names it. Practical impact: tenant admission depends entirely on the verifier. Confirmed and intended by the design; workaround: none needed. Next action: human triage. Backlog verdict: No existing change fits (checked 2).
- The plan was authored inline because the plan-writer dispatch was not used in this run.
