---
slug: identity-derived-path-collides-case-insensitively
title: An identity-derived path segment silently merges two identities on a case-insensitive filesystem
hook: Deriving a host path from an authenticated identity (tenant, principal, account) to isolate it? On a case-insensitive filesystem (APFS, NTFS) `Acme` and `acme` are ONE directory, so two distinct identities share one tree while every containment check still passes green. Refuse unsafe ids at the boundary; never normalise them, which merges the identities instead of separating them.
topics: [multi-tenancy, filesystem, isolation, security, trust-boundary, go, blind-spot]
changes: [65]
promotion_state: candidate
created: 2026-09-05
updated: 2026-09-05
---

## The rule

When an authenticated identity is used to derive a **filesystem path** that is supposed to
isolate that identity, the path layer's collision rules — not the identity layer's — decide
whether isolation actually holds. A case-insensitive filesystem folds `Acme` and `acme` into
one directory, so two separately-authenticated tenants land in one tree.

**Refuse, don't normalise.** Lowercasing (or otherwise canonicalising) the id makes the
collision *deliberate*: two distinct authenticated identities now provably share one
directory. Refusing an id that cannot be rendered as exactly one safe path segment keeps the
two identities separate and fails closed. Normalisation belongs at the identity edge, where a
human decides that `Acme` and `acme` are the same principal — never at the path layer, which
has no standing to make that call.

The refusal set that matters in practice: empty, `.`, `..`, leading `.` (hidden dirs, and it
also catches `...`), anything outside a strict `[a-z0-9._-]` allowlist, and a length bound.

## Why the tests don't catch it

This is the dangerous part, and the reason it needs to fire unprompted.

Every containment assertion still **passes** under the collision. Canonicalise-then-compare,
`..` rejection, symlink resolution, "working_dir cannot escape its root" — all green, because
nothing escaped anything: the two tenants were *given the same root*. The isolation property
was never expressed as an assertion the collision could violate, so a full green suite is
consistent with total cross-tenant disclosure.

What catches it is a **positive** isolation test — tenant A writes a sentinel, tenant B tries
to read it and must fail — plus asserting that two distinct ids resolve to two distinct,
non-overlapping roots. Negative escape tests alone cannot see it.

## The sibling rule that comes with it

Give each identity exactly one **direct child** of a declared parent, and never return the
parent itself. Siblings are mutually non-overlapping by construction, so "A's root contains
B's" becomes unrepresentable rather than something a test has to rule out. Nesting identity
trees re-opens by layout what the path checks were meant to close.

## The absent-identity trap

An empty or unauthenticated identity must resolve to **no root**, not to a shared default.
Storage keying and filesystem isolation want opposite things from an absent identity: storage
wants every row to land *somewhere* (hence `NormalizeTenant("") → "_default"`), whereas an
empty tenant sharing a directory with whatever real tenant happens to be named `_default` is
exactly the disclosure the isolation exists to prevent. Reusing the storage-layer normaliser
at the filesystem layer looks like consistency and is a vulnerability.

## Where else this applies

Any host artifact whose name is derived from an identity string — not just directories.
Per-principal socket paths are the obvious neighbour, and the same fold applies to them.

## War story

**2026-09-05 (#65, PR #87).** `docket-review-deep` raised it as a **blocker** on the
per-tenant bind-mount work: the resolver built `<parent>/<tenant>` from `Principal.Tenant`,
and on the APFS build machine `Acme` and `acme` were one directory. Two authenticated tenants
would have shared one workspace tree — while the full suite, including every `working_dir`
containment test inherited from change 0063, stayed green.

Normalising to lowercase was considered and **rejected**: it merges two authenticated
identities rather than separating them. The fix refuses any tenant id that cannot be rendered
as one safe path segment (`ErrNoTenantRoot`), which is a breaking change for uppercase and
dot-leading ids and is the correct trade — the remedy is to normalise at the identity edge,
where that decision belongs.

The same review pass produced ADR-0057 (unsafe tenant ids are refused, never normalised).
An adjacent risk was reported but not filed: per-principal egress socket names are derived
from an identity string the same way, and were not observed to collide.
