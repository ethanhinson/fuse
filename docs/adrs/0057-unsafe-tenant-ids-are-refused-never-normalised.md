---
id: 57
slug: unsafe-tenant-ids-are-refused-never-normalised
title: Unsafe tenant ids are refused, never normalised — the one legitimate collapse happens at the auth edge
status: Accepted
date: 2026-09-04
supersedes: []
reverses: []
relates_to: [34, 44, 55, 56]
change: 65
---

## Context

Change 0065 turns `Principal.Tenant` into a host directory name under a per-tenant workspace parent.
That makes the tenant id a filesystem-significant value for the first time, and a deep review of the
mapping found two ways it could silently merge two tenants onto one directory tree.

Produced by change 0065, commits `d6047bf`, `e7e1293`, `7056930` on
`feat/bash-per-tenant-filesystem-isolation`.

## Decision

Three rules, one principle: **refuse, never rewrite**.

### 1. Uppercase is refused

`tenantDirName`'s allowlist is lowercase-only. On a case-insensitive filesystem (APFS, the
documented dev platform) `Acme` and `acme` are ONE directory, yet `filepath.Rel` yields distinct
segments, so both pass the structural containment check and both mount the same tree. Verified
empirically before the fix.

The rejected alternative was lowercase-**normalising** the id — itself the collision-by-string-
manipulation the function's own comment forbids: two distinct authenticated identities would
silently become one tenant. A deployment minting uppercase ids now gets a loud `ErrNoTenantRoot`
(degraded-safe: nothing mounted); the remedy is normalising at the identity edge, where a human
decides.

### 2. A dot-leading id is refused

`.` and `..` were already rejected, but `...` and longer runs were not. These are legal,
non-traversing names — no containment escape — but a tenant tree invisible to a casual `ls` of the
parent works against the operator legibility the layout policy otherwise maintains.

Narrowed deliberately from the reviewer's suggestion (require an alphanumeric first character): that
would refuse `_default`, which rule 3 makes load-bearing. A leading `_` or `-` is visible in a
listing, so the legibility argument does not reach it.

### 3. An omitted tenant collapses at the auth edge, once

`loop_server.auth` documents `tenant` as optional, and `internal/config/loader_test.go` pins the
empty-tenant entry as a supported shape. Under per-tenant mounts an empty tenant hits the resolver's
refuse-empty invariant and breaks EVERY bash call for that principal.

Fixed by mapping `"" -> event.DefaultTenant` in `buildLoopVerifier`
(`cmd/fuse/loop_serve_net.go`), joining two sibling edges that already normalise the same value
(`principalFromConfig`, `toolIdentityTenantKeys`). The resolver's refuse-empty floor is UNTOUCHED —
the collapse is explicit, one-directional, and at the boundary where a token becomes an identity.

### Why 3 is not a violation of 1

`Acme`/`acme` are two DIFFERENT strings that a filesystem merges behind the operator's back; two
omitted tenants are the SAME string naming one tenant twice. They already share one durable event
stream (`<baseDir>/_default/`) and one tool-identity signing key today, so the shared mount grants no
reach the store did not already grant. Rule 1 prevents an invisible merge; rule 3 makes a visible,
already-existing one consistent.

The reviewer's alternative for 3 — reject an empty tenant in `Config.Validate` when hosted — was
found unimplementable as specified: `Validate()` is posture-free and runs for every subcommand
before dispatch, so `hosted` is not knowable there.

## Consequences

- The tenant-id charset is now a documented operator-facing contract: `AuthTokenConfig` in
  `internal/config/schema.go` states both the lowercase rule and the dot rule.
- A deployment whose IdP mints uppercase or dot-leading tenant ids must normalise upstream — a
  deliberate, loud, upgrade-visible break, chosen over a silent cross-tenant merge.
- Any FUTURE host path derived from an identity string (per-principal egress socket names, for
  instance) inherits the same case-insensitivity hazard and is not covered here.
