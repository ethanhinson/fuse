---
id: 52
slug: delegated-identity-on-bash-egress-is-forward-proxy-only
title: Delegated identity on `bash` egress is forward-proxy only — a credentialed destination is reached in plaintext HTTP or refused, never tunnelled
status: Accepted
date: 2026-09-01
supersedes: []
reverses: []
relates_to: [36, 44, 51]
change: 64
---

## Context

Change 0064 lets an operator's egress allowlist entry optionally name a change #52 `CredentialSource` audience (the `credential:` field, over the `internal/toolidentity` seam), so the egress proxy injects a delegated, audience-bound credential when it opens the upstream connection. ADR-0044 permits exactly this, narrowly: a **declared** target is the one place `bash` may borrow ADR-0036's delegation mechanism, because a declared target gives the seam a choke point and an audience binding. ADR-0051 settled how the contained workload reaches that proxy at all (mounted socket + loopback forwarder).

The implementation then collided with a protocol fact. **To attach an `Authorization` header, the proxy must be able to see the request.** Under HTTP `CONNECT` the tunnel is opaque — the proxy sees a destination and then ciphertext — so a credentialed destination reached over `CONNECT` cannot carry fuse's credential at all.

The first implementation tried to parse HTTP requests off the *inside* of the tunnel. A whole-branch review found this made the feature **entirely unreachable for real clients**: with `HTTP_PROXY` set, `curl`/`git`/`pip` send **absolute-form** requests for `http://` targets (which that code refused with 405) and a **TLS ClientHello** for `https://` targets (which cannot be parsed at all). The only request shape that reached the injection point was one no ordinary client produces.

## Decision

Delegated identity on `bash` egress is a **forward-proxy** feature, not a tunnel feature.

1. **Absolute-form, non-`CONNECT` requests are served as a real forward proxy.** That is the shape clients actually send when `HTTP_PROXY` is set for an `http://` target, and it is where credential injection happens — matched against the same allowlist, with the host **canonicalized exactly once at a shared entry point**.
2. **`CONNECT` to a `credential:`-declared destination is refused explicitly**, with a bounded refusal reason. It is never tunnelled — so a delegated-identity destination is never reached *without* its identity.
3. **`CONNECT` to a plain (non-credentialed) declared destination still splices raw bytes**, so TLS works normally for plain allow-through entries.
4. **TLS interception is explicitly not built.** Consequently `credential:` entries are **plaintext-HTTP-only**, and the config loader emits a **load-time warning** naming that constraint, so an operator learns it at the config surface rather than at a refusal.
5. **The identity-carrying request is addressed to the operator-declared destination.** The virtual host is pinned to the canonical authority resolved at the entry point, so a model-chosen `Host` spelling cannot steer fuse's minted credential to a different backend sitting behind a vhost frontend.

## Consequences

**The feature is narrower than the spec implied.** The spec's own sample `credential:` entry used port 8443 — a shape this decision refuses. Operators wiring delegated identity today are limited to plaintext HTTP destinations, which in practice means same-host or trusted-network backends reached over the mounted-socket datapath of ADR-0051.

**The fail direction is closed everywhere, and each closure is pinned by a test**: no credential source wired ⇒ refused; resolution error ⇒ refused; empty credential ⇒ refused; unparseable bytes ⇒ forwarded nowhere. The refusal is always explicit and bounded, never a silent downgrade to an uncredentialed request — which is the property that makes point 2 a security decision rather than an ergonomics one.

**TLS delegation remains open work.** Carrying a delegated credential to an `https://` destination requires an interception decision — minting and trusting a proxy CA inside the container, with everything that implies for what the proxy can read — and this change deliberately did not make it. A future ADR either accepts interception under stated limits or leaves `credential:` plaintext-only permanently.
