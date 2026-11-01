---
id: 72
slug: 'trusted-issuers-may-use-asymmetric-jwks-keys-tenant-prefixes'
title: 'Trusted issuers may use asymmetric JWKS keys, tenant prefixes and lazy tenant keys'
status: 'Accepted'
date: '2026-11-01'
supersedes: []
reverses: []
relates_to: []
change: 99
---

## Context

Issuers were HS256-only with fixed tenant lists, forcing shared secrets, restart-based rotation and per-tenant config.

## Decision

IssuerConfig accepts exactly one of signing_key, jwks or jwks_url. Tokens select keys by kid; ES256 maps to EC P-256 and EdDSA to Ed25519, HS256 only to shared-secret issuers, and every failure is the same Unauthenticated error. jwks_url documents are cached, refreshed every jwks_refresh and refetched on an unknown kid at most once a minute; an initial fetch failure is fatal. tenant_prefix admits any valid lowercase tenant id with that prefix. The built-in tool-identity STS derives a tenant key lazily on first use.

## Consequences

Rotation needs no restart and operators never hold issuer private keys. Unauthenticated callers can trigger at most one JWKS refetch a minute. Lazy derivation means any tenant a verifier produces has a key, so key availability no longer gates tenant admission.

## Alternatives considered

OIDC discovery and RSA were left out of scope; per-tenant eager config was rejected as it needs restarts for each tenant.
