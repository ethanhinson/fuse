<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0085 — Start a loop from a HarnessSpec](../changes/active/0085-start-a-loop-from-a-harnessspec.md)**
<!-- docket:backlink:end -->

# Start a loop from a HarnessSpec - Results

**Human action:** Review and merge the PR. There are no deploy or migration steps. One optional live walkthrough is below. The operator-facing choice to make is whether to give each `loop_server.secrets` entry a `hosts` list. Without one, any tenant allowed to use a secret can read its value.

## Outcome

A client can now start a loop from a `fuse.harness.v1` `HarnessSpec` on every binding: Connect `StartLoopRequest.harness`, stdio `loop.start` `params.harness`, in-process `runtime.LoopConfig.Harness`, Go SDK `StartLoopConfig.Harness` and TS SDK `startLoop({harness})`. Each loop on one shared fuse server can therefore run its own models, skills, remote MCP servers, permission posture, brakes and turn caps. Without a spec, nothing changes.

How the server treats a spec:
- **Floor.** The server's own config is the floor. A spec is layered over it, and fields marked `tighten_only` may only lower the operator's value. Permission deny lists only grow. Gateway, sandbox, auth, observability and host paths always come from the server.
- **Validation.** A bad spec is refused before anything is created. Every problem comes back at once with its field path: shape, config, model alias, `fuse.tighten_only`, `fuse.skill_ref`, `fuse.secret_ref` and `fuse.operator_only`.
- **Skills.** Named skills must be installed on the server. A non-empty list limits the loop to exactly those skills.
- **Secrets.** `secret_ref` names resolve through the operator's `loop_server.secrets` allowlist. Each entry gives an env var or file, the tenants allowed to use it, and optionally the hosts it may be sent to. With no entries, every reference is refused.
- **Durability.** The spec and its digest are the first event of the loop's stream (`loop.harness`). A resume on any replica rebuilds the same harness. StartLoop returns the digest, so a client can detect a server that ignored the spec.

Two pre-existing bugs found along the way were fixed in separate commits:
- Reattaching to a finished interactive loop on the same server failed with `context canceled`.
- A fresh `loop-serve-net` host using the filesystem store never became ready.

Departures from the groomed spec, all tightenings that came out of review (see ADR-0061's update):
- Spec MCP servers may not use oauth2, identity or oauth-exchange auth.
- Secrets can be bound to destination hosts.
- The record stores the spec's encoded bytes.
- StartLoop acknowledges the spec with its digest.

## Human actions and testing

### Optional - see a spec take effect on a real server

This walkthrough repeats the end-to-end check below on your machine. Automated tests cover the same behavior with a scripted gateway.

Setup:
1. Run the LiteLLM gateway at `localhost:4000`.
2. Create an empty directory `$H` and write a minimal `$H/.fuse/config.yml`:
   - `gateway.url: http://localhost:4000/v1`
   - `models.default: glm`, with `glm` and `minimax` aliases pointing at cheap cloud models
   - `throughput.requests_per_minute: 600`
3. Start the server:

   ```
   HOME=$H LLM_GATEWAY_KEY=<key> fuse loop-serve-net --addr 127.0.0.1:8799
   ```

Steps:
1. From `proto/`, start a loop with a spec:

   ```
   buf curl --schema . --protocol connect --http2-prior-knowledge -H "Authorization: Bearer fuse-dev-token" -d '{"task":"Reply with just OK.","harness":{"models":{"defaultAlias":"minimax"},"maxTurns":1}}' http://127.0.0.1:8799/fuse.loop.v1.LoopService/StartLoop
   ```

   Expected: a `loopId` and a `harnessDigest`.
2. Observe that loop with the same tool (`.../Observe` with `{"loopId":"<id>","fromSeq":0}`).
   Expected:
   - the first event is `loop.harness`
   - `model.call.start` shows the minimax model, not glm
3. Repeat step 1 with `"throughput":{"requestsPerMinute":5000}` in the harness.
   Expected: `invalid_argument` whose detail lists `throughput.requests_per_minute` ("may only tighten this server's limit of 600").

Cleanup: stop the server and delete `$H`.

## Verification performed

- `make test`, `make lint` (go vet) and `make proto` (zero drift) pass at the final head. The `make test` evidence comes from docket's gate.
- `make sdk-ts-test` passes (23 tests).
- `go test -race ./...` passes, and was run again on the touched packages after the review fixes.
- Live end-to-end on `fuse loop-serve-net` over Connect, using the local LiteLLM gateway with cheap models only (glm, minimax):
  - An invalid spec returned five violations with field paths, and nothing was created.
  - A valid spec ran on `cloud/minimax-m3` and stopped after one turn at `max_turns: 1`. The server default is glm.
  - Skill scoping held: the spec loop listed only `research`, while a no-spec loop listed `research, notes`.
  - An interactive spec loop resumed after a server restart with the same model and skills, and had exactly one `loop.harness` record.
  - stdio `fuse loop-server` refused a bad spec with `-32602` and `data.violations`.
  - After the review fixes: `harnessDigest` is returned and matches the record, and an oauth2 spec server is refused at `mcp_servers[0].auth.type`.
  - The resolved secret value appeared nowhere in the store, the logs or the responses.
- Whole-branch review (deep tier): 10 findings, 1 blocker, 4 important, 5 minor.
  - 9 were fixed in-branch.
  - The em dash in the docket-generated spec backlink is recorded, not edited.
  - The full table is in the PR body.
- Backlog check for the follow-ups below covered 4 proposed or deferred changes (#29, #57, #74, #83). None fits.

## Known issues and follow-ups

### The StartLoop `model` field is ignored by the loop-server bindings (pre-existing)

**When:** a Connect or stdio client sets `model` on StartLoop.
**What happens:** the loop runs the server's default model anyway, because the loop-server `BuildAgent` uses the server's default alias. Under a harness spec, the spec's `models.default_alias` picks the model, so spec clients are unaffected.
**Status:** confirmed by reading the code.
**Workaround:** use a spec's `default_alias`.
**Why not fixed here:** fixing it changes wire semantics. Resume passes a gateway id rather than an alias, and existing clients send arbitrary strings.
**Next action:** No existing change fits (checked 4). Capture a new change.

### A spec cannot introduce a tighten-only limit the operator left unset

**When:** a spec sets a tighten-only limit, for example `throughput.requests_per_minute`, on a server that sets none.
**What happens:** the spec is refused (`fuse.tighten_only`), even though a limit there is effectively tighter. This is the reused ADR-0006 rule, which treats introducing a value as loosening.
**Next action:** decide per axis where 0 means "unlimited", then allow introducing a value there. No existing change fits (checked 4). Capture a new change.

### Operators cannot pin permission mode for hosted spec loops

**What happens:** permission mode and the auto-mode keys are client-settable per ADR-0060. Only the deny lists have an operator floor (they are unioned).
**Next action:** add an operator pin if hosted deployments need one. No existing change fits (checked 4). Capture a new change.

### `static` MCP auth fails to dial (pre-existing)

**What happens:** the spec and config accept `static`, but `internal/mcp` `GetAccessToken` rejects it as an unknown auth type.
**Status:** confirmed by reading the code. It is not exercised by tests.
**Next action:** No existing change fits (checked 4). Capture a new change.

### Deferred by design

These items were deferred by design:
- fetching specs by reference or revision from a registry
- stdio MCP servers in specs
- per-tenant model gateways
- mounting `HarnessService.ValidateHarness` on `loop-serve-net`
- per-tenant audience allowlists that would let spec MCP servers use identity or oauth-exchange auth

No existing change fits (checked 4). Capture them as needed.
