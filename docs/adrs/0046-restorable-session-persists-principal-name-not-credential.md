---
id: 46
slug: restorable-session-persists-principal-name-not-credential
title: A restorable browser session persists the principal's name, never its credential
status: Accepted
date: 2026-08-17
supersedes: []
reverses: []
relates_to: [34, 43]
change: 62
---

## Context

An example app that keeps a conversation alive across a page reload has to remember, in browser
storage, *which* loop to re-observe. That much is unavoidable: the durable event stream can replay a
loop's history, but only if the client can still name the loop after its in-memory state is gone.

Two facts make "remember the loop" harder than it looks:

- **A loop is owned by a principal.** Under ADR-0034 the edge resolves a loop tenant-scoped and
  checks the caller's subject against the loop's owner — `internal/loopconnect/handler.go`'s
  `authorizeLoop` returns `NotFound` for an unknown loop and `PermissionDenied` for a loop owned by
  a different subject. So a stored loop id is only meaningful *together with* the principal it was
  created under; replaying it under anyone else is a cross-owner Observe the server rejects.
- **An example app is multi-identity on purpose.** A demo that shows off tenancy lets the user switch
  principals — a picker of demo identities, plus a paste-your-own bearer token escape hatch. So the
  principal in effect at restore time is not guaranteed to be the one the loop was started under.

The obvious way to make restore survive an identity switch is to persist the whole credential —
bearer token included — in `localStorage`, and re-present it on load. That is precisely the shape
ADR-0043 exists to resist, and an example app is the worst possible place to model it, because
example apps are read as reference implementations and copied wholesale into real ones. The pattern
would then be "a long-lived bearer token in web storage" in somebody's production app, sourced from
us.

## Decision

**A restorable session persists the principal's NAME, never its credential.**

Concretely, for any fuse example app that restores a session across page loads:

1. **Store an identity reference, not an identity.** Browser storage holds only the loop id plus the
   naming coordinates of its owner — in `examples/wander`, `{loopId, tenant, subject}` under the
   versioned key `wander.session.v1`. No token, and nothing from which a token can be derived.
2. **Re-resolve the credential from the app's own directory at load time.** The stored principal name
   is looked up against the demo-principal directory the example server publishes; the token comes
   from that live lookup, never from storage.
3. **Re-run the principal-match guard before observing, and treat the guard as authoritative.** The
   re-resolved principal must match the stored one. Any mismatch — a different active principal, a
   parse failure, an unknown or withdrawn principal — collapses to a clean fresh session rather than
   an attempted restore. A cross-owner Observe is therefore never issued; the server's
   `PermissionDenied` is a backstop, not the mechanism.
4. **Storage may NAME a principal but never MINT one.** Every principal a stored record can name is
   one the app's picker already hands out on a single click, so adopting an identity out of storage
   grants nothing the user could not have selected directly. That equivalence is what makes the
   pattern safe, and it is the property to preserve when copying it: if an app ever gains a principal
   that storage can name but the UI cannot freely offer, this design no longer holds and the naming
   set must be narrowed to the freely-offered ones.
5. **A credential the app cannot re-resolve is not persisted.** A session started under a pasted
   custom token has no directory entry to re-resolve from, so it is deliberately not stored and does
   not restore.

## Consequences

- Refresh-to-restore works for the identities the demo actually showcases, which is the whole point
  of surfacing change #54's durable resume in the browser — and it works without ever writing a
  bearer token to `localStorage`, keeping ADR-0043's "example apps never publish credentials by
  default" intact at the client edge as well as the server edge.
- The client-side guard and the ADR-0034 edge check are belt-and-braces, in the right order: the
  client refuses to *ask* for a loop it cannot prove it owns, and the edge refuses to *answer* if it
  asks anyway. Neither is load-bearing alone.
- **Accepted cost:** a session started under a pasted custom token does not survive a reload. That is
  a real usability hole for the paste path, and it is chosen over the alternative — persisting the
  token — every time. The limitation is documented in `examples/wander/README.md` so it reads as a
  decision rather than a bug.
- Restore now depends on the principal directory the example server publishes. A principal removed
  from that directory silently stops restoring (falling back to a fresh session) — which is the
  correct failure direction: the credential source is authoritative and revocable, and storage cannot
  outlive it.
- The storage key is versioned (`…​.v1`), so a future change to the record's shape retires the old key
  rather than mis-parsing it; a stale or unparseable record already falls into the fresh-session path.
