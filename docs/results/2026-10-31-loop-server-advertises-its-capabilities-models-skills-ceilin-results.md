<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0086 — Loop server advertises its capabilities - models, skills, ceilings](../changes/active/0086-loop-server-advertises-its-capabilities-models-skills-ceilin.md)**
<!-- docket:backlink:end -->

# Loop server advertises its capabilities - models, skills, ceilings - Results

**Human action:** Review and merge the PR. One optional walkthrough is offered below; no human-only verification gap blocks merge.

## Outcome

A client that starts loops from a HarnessSpec no longer has to guess what the server supports. The networked loop server now answers a new authenticated, read-only `LoopService.GetCapabilities` call. For the caller's tenant it returns:

- the models it routes: alias, gateway id, effective context window, effective max output tokens, persona, default flag, and optional operator `label`, `description` and `tags`
- the installed skills
- one ceiling per tighten-only field, keyed by the same field path a StartLoop refusal uses
- the secret ref names that resolve for that tenant (names only)
- the MCP auth kinds a spec may use
- the server version and digest algorithm

Model entries in config gain optional `label`, `description`, `tags` and `advertise` keys. `advertise: false` hides an entry, and `cli/` routes are never listed. The Go SDK gains `Client.Capabilities(ctx)`. A local client, or an older server, returns `ErrCapabilitiesUnsupported`.

Every part of the answer comes from the same code StartLoop enforces with. A golden test builds specs from the advertisement and runs them through the real StartLoop, proving the two agree.

Departures from the design:

- **The request carries a `tenant` field** (ADR recorded). It has StartLoop's semantics: if it is set and does not match the authenticated principal, the call fails with PermissionDenied. Filtering always uses the principal's tenant. The design asked for an empty request, but that could never produce the PermissionDenied it required.
- **Ceilings are strict.** A spec value must be strictly below the advertised value. A value equal to the ceiling is refused, and the docs say so.
- **Max output tokens is the effective value.** When an entry has no `max_tokens`, the server's top-level `max_tokens` is advertised, because that is what a loop actually runs with.
- **StartLoop now refuses an MCP auth enum number the proto does not declare,** as an operator-only field. It used to let one through.

## Human actions and testing

### Optional - query a running server

Use this if you want to see the answer from a real server. Automated end-to-end tests already cover this path through the real `fuse loop-serve-net` command over h2c.

Prerequisites: a local fuse build from this branch and a config with at least two model entries, one of them set to `advertise: false`. Use a cheap non-Anthropic gateway model such as `cloud/minimax-m3`.

1. Start `fuse loop-serve-net` with that config and a bearer token for one tenant.
   Expected: the server starts. It refuses to start only if no capability source is wired.
2. From a small Go program, use the SDK remote client to call `Capabilities(ctx)` with that tenant's credentials.
   Expected:
   - the advertised model is listed, and the hidden one and any `cli/` route are not
   - the ceilings match the operator config
   - only that tenant's secret ref names appear
3. Call it again with `Credentials.Tenant` set to a different tenant.
   Expected: PermissionDenied.

Cleanup: stop the server.

## Verification performed

- The full suite (`make test`) passed through the build gate at the final head, after the review fixes.
- Each task's focused tests went red, then green. Workers also ran mutation probes on the guards, each of which turned the tests red:
  - the tenant filter and the tenant spoof check
  - the secret availability rule
  - the shared MCP auth predicate
  - the ceiling derivation
  - the capability source wiring
- The race detector passed on the capabilities, ceiling and secret-ref tests.
- `gofmt` and `go vet ./...` are clean. `make proto` regenerates with no drift.
- golangci-lint is not installed on the build machine, so it did not run.
- Whole-branch review (deep tier): 4 findings.
  - Fixed in-branch: the ceiling docs (strict rule), max output tokens now advertised as the effective value, and a test-only helper moved out of production code.
  - Deferred: the stale spec sentence (see below).
  - The full table is in the PR body.

## Known issues and follow-ups

### The spec on this branch still describes an empty request

The design spec copy on the feature branch says the GetCapabilities request is empty in v1. The shipped wire contract has a `tenant` field. A reader comparing the spec with `loop.proto` sees a mismatch.

This is confirmed but documentation-only, and the ADR records the actual decision and its reason. The spec copy is a branch artifact owned by Docket's workflow, so it was not edited in-branch. Suggested next action: amend the spec sentence when it is next revised.

### `CEILING_DIRECTION_MIN` is documented but unused

Every tighten-only field today is a "may only lower" (MAX) ceiling. MIN is declared, and documented as strict to mirror MAX, but nothing emits it, so no enforcement code proves its wording.

There is no impact until a field uses MIN. Suggested next action: re-check the MIN wording when the first MIN field is added.
