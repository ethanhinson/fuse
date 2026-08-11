<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0052 — Tool/resource identity propagation — per-call RFC 8693 token exchange to downstream MCP/APIs](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0052-tool-identity-propagation.md)**
<!-- docket:backlink:end -->

# Tool/resource identity propagation — results
Change: #52 · Branch: feat/tool-identity-propagation · PR: <pending> · Plan: docs/superpowers/plans/2026-08-11-tool-identity-propagation-plan.md · ADRs: 36

## Verify (human)

Automated: `go build ./...` green; `go test ./...` green (30 packages); `go test -race` green across the touched packages (toolidentity, permissions, mcp, config, cmd/fuse, tui); `go vet` clean on touched packages. Two adversarial whole-branch reviews were run — the second found one CRITICAL (D5 mediation seam unwired) and several HIGH/MEDIUM items, all fixed and regression-tested in this branch (see Findings). Live-model verification was not required: the load-bearing checks here are auth/token-flow + mediation, exercised with httptest doubles and a real built-in STS — no Anthropic (project policy).

Beyond the automated suite, at the merge gate please confirm:

- [ ] **Skill-layer degradation is acceptable.** The configured plan/build/review/finish skills (`superpowers:*`) are **not installed in this environment**, so per docket's Skill-layer missing-skill rule those roles degraded to `auto`: the plan and this results file were authored directly, the build was executed inline (TDD, with the heavy MCP-egress and review tasks dispatched to docket-build-premium / a review subagent), and the review ran as a dispatched adversarial subagent. No behavior change to the shipped code — this is a build-process note.
- [ ] **Real MCP identity round-trip (optional, manual).** The per-caller distinct-token, audience-binding, and no-leak guarantees are asserted against httptest MCP doubles with a real broker + built-in STS. There is no end-to-end test against a live external OAuth-resource-server MCP server (fuse has none to dogfood yet). If you want end-to-end assurance, stand up an MCP server that validates an audience-bound bearer, configure it with `auth.type: identity` + an `audience`, set `tool_identity.signing_key` in `~/.fuse/config.yml`, and confirm from `fuse shell` that a tool call presents a per-call delegation token the server accepts.
- [ ] **Config surface.** `tool_identity` (signing_key/ttl/local_subject) and per-server `audience`/`scopes` + `auth.type: identity|oauth-exchange` are new. `tool_identity` is a credential surface honored ONLY from trusted `~/.fuse/config.yml` (a repo-plantable `.fuse.local.yml` is dropped with a warning, ADR-0006). Confirm the config shape reads as intended before documenting it for users.

## Findings

- **MCP tools are wired into the agent registry only on the shell path.** One-shot (`fuse "<task>"`) and the loop-server / loop-serve-net bindings do NOT attach configured MCP servers today (verified on origin/main). So the identity-propagation seam is reachable end-to-end only from the interactive shell, and the multi-tenant *loop-server* acceptance (two loops, distinct principals, same MCP server) is proven at the **egress-seam level** (real broker + STS, `TestEgress_TwoPrincipalsDistinctTokens`) rather than through a loop-server MCP path that does not exist. This is a codebase-reality scope note, not a design cut.
- **Single-tenant built-in STS on the CLI/shell path.** `buildToolIdentitySource` keys the built-in STS only for `event.DefaultTenant` (correct for the single-user shell — the only MCP-wired path). An explicit `TODO(#52-followup)` marks that a future multi-tenant loop-server MCP wiring must build TenantKeys for every tenant, else non-default tenants fail closed.
- **Review round 2 fixed a genuine gap: D5 complete mediation was a seam never wired.** The `TargetMediator` existed but no concrete mediator was constructed at the composition root — `g.mediator` was always nil, so the "undeclared/unlisted target denied even in ModeOff" guarantee was false in the binary. Fixed by `configTargetMediator` (a root-of-trust tool→target allowlist) wired into `buildGate` whenever identity propagation is active, with a regression test asserting the *shipped* gate denies an unconfigured target (`TestBuildGate_WiresMediatorWhenIdentityActive`).
- **JWS alg-confusion hardening.** `BuiltinSTS.Verify` now validates `alg==HS256` in the header before trusting the signature (closes the `alg:none` footgun).
- **Fixed a teardown deadlock in the MCP egress tests.** The capturing SSE double registered `t.Cleanup(client.stop)` while tests used `defer srv.Close()`; `defer` runs before `t.Cleanup`, so `httptest.Server.Close()` blocked on the live SSE goroutine. Moved server shutdown to `t.Cleanup` (LIFO → client stops first). This was the cause of the 600s package timeouts.
- **Decision recorded in ADR-0036** (`tool-authz-delegated-downstream-rfc8693-egress-seam`): per-call RFC 8693 delegation (act-claim) token exchange at a pluggable egress seam, a credential broker (built-in minimal STS default, no long-lived OAuth secrets, per-tenant isolation), tiered OAuth/explicit-static downstream auth, per-call complete mediation on the existing approval gate, and the MCP-spec constraints honored (no passthrough, RFC 8707 audience binding) — superseding the implicit static-per-server-bearer model.

## Follow-ups (recorded, not filed — auto-capture disabled this repo)

1. **Wire MCP into the one-shot and loop-server bindings**, then extend the built-in STS to multi-tenant keys, so identity propagation is reachable on the deployed multi-tenant path (the `TODO(#52-followup)` in `cmd/fuse/tool_identity.go`).
2. **External-AS `TokenExchanger`** (Keycloak/Auth0/Vault) and a **separate out-of-process broker service** — both ride the seams this PR fixed; the built-in STS + in-process broker is the shippable slice.
3. **Cross-issuer token exchange and per-target impersonation mode** — deferred behind the `TokenExchanger` seam.
4. **Per-principal/tenant scope ceilings on the mediator** — this PR ships the tool→target declaration allowlist; richer scope-ceiling enforcement rides the same `TargetMediator` seam once multi-tenant grants exist (#49 follow-on).
5. **`MCP-Resource` header is fuse-invented** for the audience binding (the MCP tools/call JSON-RPC body shape is fixed). A spec-standard resource-indicator carriage should be adopted if/when the MCP spec defines one for the streamable transport.
