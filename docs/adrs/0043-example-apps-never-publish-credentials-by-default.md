---
id: 43
slug: example-apps-never-publish-credentials-by-default
title: Example/demo apps never publish credentials by default — loopback bind + fail-closed token endpoints
status: Accepted
date: 2026-08-15
supersedes: []
reverses: []
relates_to: [34, 36]
change: 60
---

## Context

Change 0060 consolidated fuse's two example apps into one (`examples/wander`) and added a
token/user picker to demonstrate per-principal MCP identity. To drive the picker,
`examples/wander/server.js` grew a `GET /demo-users.json` endpoint returning the demo bearer
tokens read from a config file named by `FUSE_DEMO_CONFIG`.

A whole-branch review found this was a credential surface:

- The endpoint was guarded only by a `console.warn` when the config was overridden. A warning
  is not a guard.
- Node's `server.listen(PORT)` with no host argument binds `0.0.0.0`/`::`, not loopback. Any
  peer on the network could `GET` the tokens and then drive the operator's local
  `fuse loop-serve-net` through the same server's `/fuse.loop.v1.*` reverse proxy, which
  forwards `Authorization` verbatim.
- Pointing `FUSE_DEMO_CONFIG` at `~/.fuse/config.yml` would have handed out every real
  `loop_server.auth` token to anyone who asked.

The all-interfaces bind was pre-existing (from change 0056); change 0060 is what turned it into
a credential surface. That is the general shape worth capturing: a demo server's pre-existing
network posture becomes a real exposure the moment a later change teaches it to serve secrets.

## Decision

Example and demo HTTP servers in this repo follow four rules:

1. **Bind explicitly to `127.0.0.1`.** Remote access is via an SSH tunnel, documented in the
   example's README. There is deliberately **no env knob** to widen the bind — an override knob
   is the same failure mode one level of indirection away.
2. **Any endpoint that publishes credentials fails CLOSED.** It serves only when the resolved
   config path is the in-repo checked-in demo file, **or** when the operator sets a second
   explicit opt-in env var (`FUSE_DEMO_PUBLISH_TOKENS=1`). A warning is never a guard.
3. **The refusal path must never read the credential file**, so nothing can leak through an
   error body or diagnostic.
4. **Demo credentials in checked-in config are obviously fake** and documented as demo-only.

## Consequences

- **Enables:** a demo can show real per-principal identity without turning a developer's laptop
  into an open credential dispenser on whatever network it happens to be on.
- **Costs:** remote demoing requires an SSH tunnel; the browser identity CI lane must pass the
  explicit opt-in because it copies the config to a temp dir — a genuine override, correctly
  treated as one.
- **Gives up:** the convenience of pointing the demo at a real `~/.fuse/config.yml`. That is now
  a deliberate, opt-in-gated act rather than an accident of an env var.

Implementation: `examples/wander/server.js`, `examples/wander/server_exposure_test.go`
(build tag `browser`), `examples/wander/README.md`. Branch
`feat/wander-live-rentals-mcp-demo-light-up-59-s-live-data-backend`, fix commit `9013766`.
