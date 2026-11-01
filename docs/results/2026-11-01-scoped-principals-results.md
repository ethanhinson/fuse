<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0100 — Scoped principals: RPC scopes, harness digest pin and principal record on every loop](../changes/active/0100-scoped-principals-rpc-scopes-harness-digest-pin-and-principa.md)**
<!-- docket:backlink:end -->

# Scoped principals: RPC scopes, harness digest pin and principal record on every loop — Results

**Human action:** No action is required before merge. One optional check of scoped-token behavior against a running networked server is described below.

## Outcome

A bearer token for the networked loop server could previously do everything within its tenant, and nothing in a loop's event stream said who started it. Tokens can now carry scopes (`loop:start`, `loop:send`, `loop:observe`, `loop:admin`, `capabilities:read`, `secrets:inline`), enforced at the Connect edge on StartLoop, Send, Observe and GetCapabilities. A token with no scopes stays unrestricted, so existing deployments behave as before. `loop:admin` lets a token act on any loop in its tenant. A token pinned to a harness digest can only start that exact harness; a mismatch or a missing harness is refused before any runtime is touched.

Every networked loop now records a `loop.principal` event (subject, client id, issuer, scopes, token id, never token material) right after `loop.harness`, and oauth-tier tool-identity mints append a `tool.identity.minted` event to the loop's own stream. The Go SDK `Credentials` gains a `TokenProvider` for short-lived tokens. Static config accepts `scopes`, and the README documents all of it.

Departures from the spec: `Principal.Scopes` is a canonical space-joined `loopauth.Scopes` string type (not `[]string`) because `Principal` is a map key; `loop:admin` also satisfies send and observe; an explicitly empty scope (`scope: ""`, `scopes: []`) is refused instead of read as unrestricted. A failed `loop.principal` append fails the launch.

## Human actions and testing

### Optional — scoped token against a live server

Automated end-to-end tests already cover this through the real Connect server; this is only a smoke check.

Prerequisites: a networked loop server with a static token configured with `scopes: [loop:observe]`.

1. Call StartLoop with that token.
   Expected: PermissionDenied naming the missing scope `loop:start`.
2. Call Observe on an existing loop owned by the token's subject.
   Expected: the stream opens.

## Verification performed

- Full suite (`make test`) green at the final head via the gate driver.
- Each task ran focused tests with RED-first or mutation evidence for the authorization, digest-pin and audit guards.
- Whole-branch review (deep tier): 5 findings; 4 fixed in-branch (token-claim null handling, SDK stream provider-error sent a request, chart values doc) and 1 reported; full table in the PR body.

## Known issues and follow-ups

- The sandbox egress proxy mints delegated tokens with a nil audit sink and no loop id on its context, so those mints are not recorded as `tool.identity.minted`. Confirmed by review. Practical impact: the audit trail covers MCP mints only. Next action: capture a change to stamp the loop id on egress pool entries. Related to #57 (egress identity for built-in HTTP tools), which is deferred; weigh whether this strengthens the case to revive it.
- Adding a func field to the public SDK `Credentials` makes it non-comparable. External code comparing `Credentials` values or using them as map keys stops compiling. Nothing in this repository does. Accepted; no workaround other than not comparing. Mentioned in the PR body.
