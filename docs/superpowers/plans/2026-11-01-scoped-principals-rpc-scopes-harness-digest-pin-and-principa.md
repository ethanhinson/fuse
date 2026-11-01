<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0100 — Scoped principals: RPC scopes, harness digest pin and principal record on every loop](../../changes/active/0100-scoped-principals-rpc-scopes-harness-digest-pin-and-principa.md)**
<!-- docket:backlink:end -->

# Scoped principals: RPC scopes, harness digest pin, principal record - Implementation Plan

> Executed by `docket-build`: one task at a time. Each task is TDD: write the failing test, run it red, write the minimal code, run it green, commit. Every test uses fakes, `httptest` servers, the in-repo scripted gateway, or the in-repo MCP stub. No live model traffic.

**Goal:** A bearer token can be narrowed to specific RPCs (`scope`), pinned to exactly one harness spec (`harness_digest`), and every loop started over the networked binding records which credential started it (`loop.principal`). Every delegated tool-identity token minted for a loop is recorded in that loop's stream (`tool.identity.minted`). The Go SDK can fetch a fresh bearer per request (`TokenProvider`).

**Architecture:**
- `internal/loopauth`: `Principal` gains `Scopes`, `HarnessDigest`, `ClientID`, `Issuer`, `TokenID`. Scope vocabulary constants and two predicates live here: `Allows(scope)` (true when unrestricted or listed) and `Grants(scope)` (true only when explicitly listed). `IssuerVerifier` reads `scope`, `harness_digest`, `azp`, `jti`.
- `internal/config` + `cmd/fuse/loop_serve_net.go`: static `loop_server.auth[].scopes` becomes `Principal.Scopes`.
- `internal/loopconnect`: the handler enforces scopes per RPC, applies the `loop:admin` owner bypass, enforces the digest pin before the runtime is touched, and hands the runtime a principal record.
- `internal/event`: new kinds `loop.principal` and `tool.identity.minted` with their payloads.
- `internal/runtime`: `LoopConfig.Principal` (a plain `*event.LoopPrincipalPayload`, so the runtime still never imports auth, ADR-0030) is appended right after `loop.harness`, once, never on resume.
- `internal/toolidentity`: `AuditRecord` gains `Loop`.
- `cmd/fuse`: a per-host `loopMintAudit` sink maps loop id to that loop's own event store (bound in the loop factory's `BuildAgent`, unbound in `LoopTeardown`). It is wired into the broker for every loop the loop-server host runs, including spec (harness) loops.
- `sdk/fuse`: `Credentials.TokenProvider`.

**Tech stack:** Go, connect-go, `internal/loopauth`, `internal/loopconnect`, `internal/runtime`, `internal/event`, `internal/toolidentity`, `cmd/fuse`, `sdk/fuse`.

---

## Spec drift found while planning (verified against the feature tree at 1d0c68c)

1. **`Principal` must stay comparable.** The change says `Scopes []string`. `loopauth.Principal` is a map key (`internal/tools/sandbox/pool.go`: `entries map[loopauth.Principal]*poolEntry`) and is compared with `!=` in production (`pool.go` `certifyEntry` and `releaseTo`) and in many tests. A slice field breaks compilation across the sandbox. **Decision:** `Principal.Scopes` has type `loopauth.Scopes`, a canonical string (distinct scopes, sorted, joined by one space, the RFC 8693 wire form). `Scopes.List()` returns the `[]string` the change describes. The semantics do not change: empty means unrestricted.
2. **The SDK type is `Credentials`, not `RemoteCredentials`** (`sdk/fuse/fuse.go`). `TokenProvider` goes on `Credentials`, which `NewRemote` already takes.
3. **The "docs/ loop-server auth section" is in `README.md`** ("Authentication and multi-tenancy (change #49)" and "Trusted token issuers"), not under `docs/`. Docs go there.
4. **The acceptance criteria contradict the scope table unless `loop:admin` satisfies `loop:send`/`loop:observe`.** The AC says a token with `scope: "loop:start loop:admin"` can `Send` to another subject's loop, but the table says `Send` needs `loop:send`. **Decision:** `loop:admin` satisfies the scope requirement of `Send` and `Observe` and also bypasses the owner check. It does not satisfy `loop:start` or `capabilities:read`. Refusal messages still name the canonical scope (`loop:send`, `loop:observe`).
5. **Mint audit reach.** The change says "in `loop-serve-net`". The loop factory is shared by both server bindings (`assembleLoopServerRuntimeDeps`), so the sink is wired there. The stdio `loop-server` also records mint events (more audit, never less). `loop.principal` stays networked-only, because only the Connect edge has a principal to record.

## Decisions made in this plan (record them in the PR body; the ADR is recorded by the docket ADR role, not by a build task)

ADR content: scopes are a token claim (issuer `scope`, static `scopes`). They are enforced at the Connect edge, and an absent claim means unrestricted. `loop:admin` widens owner authorization within the tenant only. The digest pin is enforced at `StartLoop` before the runtime. `loop.principal` and `tool.identity.minted` are the durable audit records, and neither ever holds token material.

1. **Unrestricted versus explicit.** `Allows(s)` is used for every per-RPC requirement: empty `Scopes` allows everything (today's behaviour). `Grants(s)` is used ONLY for the `loop:admin` owner bypass, so an unrestricted (legacy) token never gains admin.
2. **An explicitly empty scope is refused, not unrestricted.** An issuer token whose `scope` claim is present but has no scope after splitting on whitespace (`""`, `"   "`) is refused at verification. A static entry with `scopes: []` (present and empty) is a config validation problem ("omit `scopes` for an unrestricted token"). Otherwise "the issuer said nothing allowed" would silently become "everything allowed".
3. **Non-string claim types are refused.** A `scope`, `harness_digest`, `azp` or `jti` that is not a JSON string fails claim decoding, so the token is refused (uniform `ErrInvalidToken`).
4. **Unknown scope strings are ignored but still restrict.** `scope: "foo:bar"` makes `Scopes` non-empty, so every RPC is refused for lack of its scope. A static scope entry that contains whitespace (`"loop:start loop:send"` as one list item) is a config validation problem.
5. **Check order per RPC:** span start, then the scope check, then the tenant/spoof check, then (StartLoop) the digest pin, then the orchestrate-target authorization, then the runtime. Observe checks the scope before any subscribe or replay.
6. **Digest pin compare** is exact string equality against `harness.Digest(m.Harness)` (`sha256:<lowercase hex>`). No normalization. A mismatch fails closed, so a malformed claim cannot widen anything.
7. **The `loop.principal` payload** is `{subject, client_id, issuer, scopes, token_id}`. `scopes` is always a JSON array (`[]` when unrestricted). It is recorded only on a fresh start (not on resume). A failure to append it fails the launch (the same discipline as `loop.harness`), so an unattributed networked loop never runs.
8. **`tool.identity.minted`** is recorded only for `Tier == "oauth"` records (the static tier mints nothing) that carry a loop id bound on this host. Its payload is exactly `{subject, audience, scope, loop_id}`, where `scope` is the target scopes joined by one space. An append failure is logged to stderr and never fails the tool call (the audit sink has no error path).
9. **SDK `TokenProvider`:** when set it is called once per request (unary) and once per stream open, and `Token` is ignored. An error fails that request with `fuse: token provider: <err>` (wrapping the provider error) and the request is never sent. A provider that returns `""` sends no `Authorization` header, as an empty `Token` does today.

## Learnings applied

- `security-knob-inert-at-composition-root`: every enforcing piece must be proven through the real composition root, not only by constructing the object in its package. Task 11 drives `buildLoopVerifier(cfg)` through the real Connect server (`authTestServer`). Task 10 drives `buildLoopServerRuntimeDeps` so the mint sink is shown to be constructed and bound in `cmd/fuse`.
- `patch-every-cloned-child-builder` / `relocated-emission-resource-every-projected-field`: the loop server has TWO `mcpAttach` sites (`assembleLoopServerRuntimeDeps` for operator loops, `loopServerHost.settingsFor` for harness-spec loops). The audit sink must reach both. Re-derive with `grep -n 'mcpAttach' cmd/fuse/*.go` before Task 10.
- `per-instance-resource-needs-teardown-on-every-early-return`: the per-loop store binding added in `BuildAgent` is released in `loopServerHost.teardown`, which the runtime already calls on the build-failure early return as well as at completion.
- `canonicalize-once-before-every-matching-layer`: scopes are canonicalized once, at the trusted edge (verifier and config to `loopauth.Scopes`). Every later check reads the canonical set.
- `spec-asserted-error-code-verify-emitter-first` / `reconcile-verify-claims-against-origin-not-working-tree`: the four drift items above were verified against code, not taken from the spec text.
- `cache-over-tenant-scoped-source-reassert-key-on-hit`: the `loop:admin` bypass skips only the owner comparison. The tenant-scoped `Resolve` still runs, so cross-tenant stays `NotFound`.

---

## Task 1: loopauth - `Scopes`, scope vocabulary, new `Principal` fields

**Tier:** standard

**Files:**
- Create: `internal/loopauth/scopes.go`
- Create: `internal/loopauth/scopes_test.go`
- Modify: `internal/loopauth/verifier.go` (the `Principal` struct)

**Step 1: Write failing tests** in `internal/loopauth/scopes_test.go`:
- `TestNewScopesCanonicalizes`: `NewScopes("loop:send", " loop:start ", "loop:send", "")` equals `Scopes("loop:send loop:start")`. `NewScopes()` equals `Scopes("")`.
- `TestParseScopesSplitsOnWhitespace`: `ParseScopes("loop:observe  loop:start\tcapabilities:read")` equals `NewScopes("capabilities:read","loop:observe","loop:start")`.
- `TestScopesListIsNonNil`: `Scopes("").List()` is a non-nil empty slice. `NewScopes("b","a").List()` is `[]string{"a","b"}`.
- `TestPrincipalAllowsAndGrants`: table-driven.
  - `Principal{}`: `Allows(ScopeLoopStart)` is true and `Grants(ScopeLoopAdmin)` is false.
  - `Principal{Scopes: NewScopes(ScopeLoopObserve)}`: `Allows(ScopeLoopObserve)` is true, `Allows(ScopeLoopStart)` is false, `Grants(ScopeLoopObserve)` is true.
  - `Principal{Scopes: NewScopes("foo:bar")}`: `Allows` is false for every vocabulary constant.
- `TestPrincipalStaysComparable`: compile-time guard `var _ = map[Principal]bool{}`, plus `Principal{Tenant:"t", Scopes: NewScopes("a")} == Principal{Tenant:"t", Scopes: NewScopes("a")}`. Comment: the sandbox pool keys by Principal.
- `TestScopeVocabulary`: the six constants equal `loop:start`, `loop:send`, `loop:observe`, `loop:admin`, `capabilities:read`, `secrets:inline`.

**Step 2: Run red:** `go test ./internal/loopauth -run 'TestNewScopes|TestParseScopes|TestScopesList|TestPrincipalAllows|TestPrincipalStays|TestScopeVocabulary' -count=1` (compile failure is the expected red).

**Step 3: Implement.**
- `scopes.go`: the six `Scope*` string constants, each with a doc comment (`ScopeSecretsInline` is "defined here, enforced by a later change"). Then:
  ```go
  // Scopes is a canonical scope set: the distinct non-empty scope strings,
  // sorted, joined by one space (the RFC 8693 section 4.2 wire form). It is a
  // string, not a slice, so Principal stays comparable: the sandbox pool keys
  // warm runners by Principal. The empty set means unrestricted.
  type Scopes string
  func NewScopes(items ...string) Scopes      // trim each, drop empty, dedupe, sort, join " "
  func ParseScopes(claim string) Scopes       // NewScopes(strings.Fields(claim)...)
  func (s Scopes) List() []string             // non-nil; []string{} for ""
  func (s Scopes) Contains(scope string) bool
  func (s Scopes) Empty() bool
  ```
  The `Principal` methods:
  ```go
  // Allows reports whether p may perform an operation that requires scope:
  // an unrestricted principal (no scopes) allows everything.
  func (p Principal) Allows(scope string) bool { return p.Scopes.Empty() || p.Scopes.Contains(scope) }
  // Grants reports whether scope was EXPLICITLY granted. It is used for widening
  // grants (loop:admin), which an unrestricted principal never receives implicitly.
  func (p Principal) Grants(scope string) bool { return p.Scopes.Contains(scope) }
  ```
- `verifier.go`: add to `Principal`, after `ObservabilityOperator`, each with a doc comment: `Scopes Scopes` (empty is unrestricted, set only from a trusted verifier), `HarnessDigest string` (when set, StartLoop must carry a spec with exactly this digest), `ClientID string` (JWT `azp`, audit only), `Issuer string` (the issuer that verified the token, audit only, empty for static tokens), `TokenID string` (JWT `jti`, audit only, never used for replay detection in this change). Keep the "carries no credentials" sentence true: none of these is token material.

**Step 4: Run green**, then `go build ./... && go vet ./internal/loopauth ./internal/tools/sandbox`. The sandbox must still compile unchanged.

**Step 5: Commit:** `loopauth: add scope vocabulary and audit fields to Principal`

---

## Task 2: IssuerVerifier reads `scope`, `harness_digest`, `azp`, `jti`

**Tier:** standard

**Files:**
- Modify: `internal/loopauth/issuer.go`
- Modify: `internal/loopauth/issuer_test.go`

**Step 1: Write failing tests** in `issuer_test.go`, reusing `mint`, `hs256`, `goodClaims`, `with`, `newTestIssuerVerifier`:
- `TestIssuerTokenCarriesScopesAndAuditClaims`: claims `with(goodClaims(), "scope", "loop:start loop:observe loop:start", "harness_digest", "sha256:abc", "azp", "client-1", "jti", "x")`. `Verify` returns a Principal with `Scopes == NewScopes("loop:observe","loop:start")`, `HarnessDigest == "sha256:abc"`, `ClientID == "client-1"`, `TokenID == "x"`, and `Issuer` equal to the issuer's `Name`.
- `TestIssuerTokenWithoutScopeIsUnrestricted`: `goodClaims()` gives `Scopes == ""`, `HarnessDigest == ""`, and `Issuer` still set.
- Extend `TestIssuerTokenRefused`'s table (or add `TestIssuerTokenRefusesBadScopeAndAuditClaims`). Each case must produce the zero Principal and `ErrInvalidToken`, and the `verify` reason must be non-nil:
  - `scope: ""`; `scope: "   "` (explicitly empty, decision 2)
  - `scope: ["loop:start"]` (array, decision 3)
  - `harness_digest: 7`; `azp: true`; `jti: {}` (non-string)
- Update the existing equality assertions in `TestFirstOfStaticThenIssuer` (around lines 267 and 271) so they include the new `Issuer` field the issuer path now sets. Do not weaken them to field-by-field checks.

**Step 2: Run red:** `go test ./internal/loopauth -run 'TestIssuer|TestFirstOf' -count=1`.

**Step 3: Implement** in `issuer.go`:
- `issuerClaims` gains `Scope *string \`json:"scope"\``, `HarnessDigest string \`json:"harness_digest"\``, `Azp string \`json:"azp"\``, `Jti string \`json:"jti"\``. A non-string value then fails `dec.Decode` and returns "malformed claims".
- After the tenant check, before the return:
  ```go
  var scopes Scopes
  if c.Scope != nil {
      scopes = ParseScopes(*c.Scope)
      if scopes.Empty() {
          return Principal{}, errors.New("scope claim is present but empty")
      }
  }
  ```
- Return `Principal{Tenant, Subject, ObservabilityOperator, Scopes: scopes, HarnessDigest: c.HarnessDigest, ClientID: c.Azp, Issuer: is.Name, TokenID: c.Jti}`.
- Update the `IssuerVerifier`/`Verify` doc comments to list the four optional claims.

**Step 4: Run green:** `go test ./internal/loopauth -count=1 -race`.

**Step 5: Commit:** `loopauth: issuer tokens carry scope, harness digest pin, azp and jti`

---

## Task 3: Static tokens get `scopes`

**Tier:** standard

**Files:**
- Modify: `internal/config/schema.go` (`AuthTokenConfig`)
- Modify: `internal/config/validate.go`
- Modify: `internal/config/loader_test.go` (or a new `internal/config/auth_scopes_test.go`)
- Modify: `cmd/fuse/loop_serve_net.go` (`buildLoopVerifier`)
- Modify: `cmd/fuse/loop_serve_net_auth_test.go` (or a new `cmd/fuse/loop_serve_net_scopes_test.go`)

**Step 1: Write failing tests.**
- `internal/config`: `TestAuthTokenScopesLoad`: load a trusted `~/.fuse/config.yml` (copy the existing pattern near `loader_test.go:1699`) with
  ```yaml
  loop_server:
    auth:
      - {token: a, tenant: acme, subject: x, scopes: [loop:observe]}
      - {token: b, tenant: acme, subject: y}
  ```
  Assert `Auth[0].Scopes == []string{"loop:observe"}` and `Auth[1].Scopes == nil`.
- `TestAuthTokenScopesValidation`: table over `Config{LoopServer: LoopServerConfig{Auth: ...}}.Problems()`:
  - `Scopes: []string{}` (non-nil, empty) gives a problem at path `loop_server.auth[0].scopes` whose message says to omit it for an unrestricted token.
  - `Scopes: []string{"loop:start loop:send"}` gives a problem (a whitespace-containing entry).
  - `Scopes: []string{""}` gives a problem.
  - `Scopes: []string{"loop:observe","unknown:thing"}` gives no problem (unknown is ignored, decision 4).
  - `Scopes: nil` gives no problem.
  Also prove that YAML `scopes: []` reaches the validator as non-nil and empty: load it through the loader and assert `Validate()` returns that problem. If yaml decoding turns `[]` into nil, the loader cannot tell "omitted" from "empty". In that case, stop and report it in the task result rather than silently dropping the rule.
- `cmd/fuse`: `TestBuildLoopVerifierStaticScopes`: `buildLoopVerifier` over a config with the two entries above. `Verify(ctx,"a")` has `Scopes == loopauth.NewScopes("loop:observe")`. `Verify(ctx,"b")` has `Scopes == ""`.

**Step 2: Run red:** `go test ./internal/config -run 'TestAuthTokenScopes' -count=1 && go test ./cmd/fuse -run TestBuildLoopVerifierStaticScopes -count=1`.

**Step 3: Implement.**
- `AuthTokenConfig` gains `Scopes []string \`yaml:"scopes"\``. Extend its doc comment: optional; omitted means unrestricted; names from the loopauth vocabulary; unknown names are ignored but still restrict.
- `validate.go` `Problems()`: for each `c.LoopServer.Auth[i]`, if `a.Scopes != nil && len(a.Scopes) == 0`, add a problem `loop_server.auth[i].scopes` with message "is empty; omit scopes for an unrestricted token or name at least one scope". For each entry `s` with `s == "" || strings.ContainsAny(s, " \t\r\n")`, add `loop_server.auth[i].scopes[j]` with message "%q is not a single scope name". Do NOT echo `a.Token` in any message.
- `buildLoopVerifier`: set `Scopes: loopauth.NewScopes(a.Scopes...)` on the static Principal. Leave the dev token unrestricted.

**Step 4: Run green**, plus `go test ./internal/config ./cmd/fuse -run 'Auth|Verifier|Issuer' -count=1`.

**Step 5: Commit:** `config: static loop_server.auth tokens accept scopes`

---

## Task 4: loopconnect - per-RPC scope enforcement and the `loop:admin` owner bypass

**Tier:** premium (authorization edge; mistakes here are silent widenings)

**Files:**
- Modify: `internal/loopconnect/handler.go` (`authorizeLoop`, `StartLoop`, `Send`, a new `requireScope`)
- Modify: `internal/loopconnect/observe.go` (`Observe`)
- Modify: `internal/loopconnect/capabilities.go` (`GetCapabilities`)
- Create: `internal/loopconnect/scopes_test.go`

**Step 1: Write failing tests** in `scopes_test.go`, using `newAuthzServer`, `tokenVerifier`, `newFakeRegistry`, `bearer` and `fakeRuntime` from `handler_authz_test.go`/`handler_test.go`. Add a `startCalls int` counter to `fakeRuntime.StartLoop` if one does not already exist. Tokens (all tenant `acme`):
`obs` = alice `{loop:observe}`; `start` = alice `{loop:start}`; `send` = bob `{loop:send}`; `admin` = bob `{loop:start loop:admin}`; `caps` = alice `{capabilities:read}`; `none` = alice unrestricted; `junk` = alice `{foo:bar}`.
- `TestScopeObserveOnlyToken`: seed `loop-a` owned by alice. With `obs`: `Observe(loop-a)` opens (the first `Receive` does not fail with PermissionDenied; use the existing observe test pattern with a fakeRuntime `attachFn` returning one event). `StartLoop` gives `CodePermissionDenied` and `err` contains `loop:start`, and the seam is not called (`startCalls == 0`). `Send(loop-a)` gives `CodePermissionDenied` containing `loop:send`.
- `TestScopeUnrestrictedTokenUnchanged`: `none` can StartLoop, Send and Observe its own loop (the existing behaviour). `none` sending to bob's loop is still `PermissionDenied` (an unrestricted token never gets admin, decision 1).
- `TestScopeAdminActsOnOtherSubjectsLoop`: seed `loop-a` owned by alice. `admin` (bob) `Send(loop-a)` succeeds and `Observe(loop-a)` opens. `send` (bob, no admin) `Send(loop-a)` gives `PermissionDenied` (the owner rule).
- `TestScopeAdminCrossTenantStillNotFound`: seed `loop-g` under tenant `globex`. `admin` (tenant acme) `Send(loop-g)` gives `CodeNotFound`.
- `TestScopeAdminOrchestrate`: seed `loop-a` owned by alice. `admin` `StartLoop{OrchestrateLoopId: loop-a}` passes authorization (the seam is called). A bob token with `{loop:start}` only gets `PermissionDenied`.
- `TestScopeCapabilities`: wire a stub `CapabilitySource` (see `capabilities_test.go`). `caps` succeeds. `obs` gives `PermissionDenied` containing `capabilities:read`.
- `TestScopeUnknownOnlyRestrictsEverything`: `junk` gets `PermissionDenied` on all four RPCs.
- `TestScopeNoPrincipalPathUnchanged`: `newTestServer(t, rt)` (no interceptor) StartLoop still succeeds. A missing principal never triggers a scope check.

**Step 2: Run red:** `go test ./internal/loopconnect -run 'TestScope' -count=1`.

**Step 3: Implement.**
- In `handler.go`:
  ```go
  // requireScope refuses a request whose authenticated principal is not allowed
  // scope. alsoSatisfiedBy names scopes that satisfy the requirement when
  // EXPLICITLY granted (loop:admin for Send/Observe). No principal (an
  // un-intercepted path) means no check, as for the owner rule.
  func requireScope(ctx context.Context, scope string, alsoSatisfiedBy ...string) *connect.Error {
      p, ok := PrincipalFrom(ctx)
      if !ok || p.Allows(scope) {
          return nil
      }
      for _, s := range alsoSatisfiedBy {
          if p.Grants(s) {
              return nil
          }
      }
      return connect.NewError(connect.CodePermissionDenied,
          fmt.Errorf("loopconnect: token lacks scope %q", scope))
  }
  ```
- Call it first, right after the api span is set up, in each RPC. Record `apiOutcome` on refusal the way each RPC already does:
  - `StartLoop`: `requireScope(ctx, loopauth.ScopeLoopStart)`
  - `Send`: `requireScope(ctx, loopauth.ScopeLoopSend, loopauth.ScopeLoopAdmin)`
  - `Observe`: `requireScope(ctx, loopauth.ScopeLoopObserve, loopauth.ScopeLoopAdmin)`, before `effectiveTenant` and before any subscribe or replay
  - `GetCapabilities`: `requireScope(ctx, loopauth.ScopeCapabilitiesRead)` via its `fail` helper
- `authorizeLoop`: change the owner comparison to `if rec.Owner != "" && rec.Owner != p.Subject && !p.Grants(loopauth.ScopeLoopAdmin)`. Update its doc comment: loop:admin skips only the owner comparison, and the tenant-scoped resolve (NotFound) still applies.
- Update the `Handler`/RPC doc comments to state the per-RPC scope.

**Step 4: Run green:** `go test ./internal/loopconnect -count=1 -race`. All existing authz tests must pass unchanged.

**Step 5: Commit:** `loopconnect: enforce per-RPC token scopes; loop:admin acts on any loop in the tenant`

---

## Task 5: loopconnect - harness digest pin

**Tier:** standard

**Files:**
- Modify: `internal/loopconnect/handler.go` (`StartLoop`)
- Modify: `internal/loopconnect/harness_test.go`

**Step 1: Write failing tests** in `harness_test.go` (use `newAuthzServer` with a `tokenVerifier`). Spec `S := testSpec-like &harnessv1.HarnessSpec{...}` (reuse whatever helper `harness_test.go` already uses). `S2` is `S` with one field changed (for example a different `MaxTurns`). Token `pinned` = `{Tenant:"acme", Subject:"alice", HarnessDigest: harness.Digest(S)}`.
- `TestDigestPinMatchingSpecStarts`: `StartLoop{Harness: S}` succeeds, `resp.Msg.HarnessDigest == harness.Digest(S)`, and the seam was called.
- `TestDigestPinMismatchRefused`: `StartLoop{Harness: S2}` gives `CodePermissionDenied`, the seam was NOT called (`startCalls == 0`), and the registry holds no record for tenant acme (`reg.List(ctx,"acme")` is empty).
- `TestDigestPinMissingSpecRefused`: `StartLoop{}` (no harness) gives `CodePermissionDenied` and the seam was not called.
- `TestNoPinAnySpecStarts`: an unpinned token with `S2` succeeds (unchanged behaviour).

**Step 2: Run red:** `go test ./internal/loopconnect -run 'TestDigestPin|TestNoPin' -count=1`.

**Step 3: Implement** in `StartLoop`, after `effectiveTenant` and before the orchestrate authorization:
```go
if havePrincipal && p.HarnessDigest != "" {
    if m.Harness == nil {
        aerr := connect.NewError(connect.CodePermissionDenied,
            fmt.Errorf("loopconnect: token is pinned to harness %s; the request carries no harness", p.HarnessDigest))
        apiOutcome = observeotel.OutcomeFromError(ctx, aerr)
        return nil, aerr
    }
    if got := harness.Digest(m.Harness); got != p.HarnessDigest {
        aerr := connect.NewError(connect.CodePermissionDenied,
            fmt.Errorf("loopconnect: harness digest %s does not match the token's pin %s", got, p.HarnessDigest))
        apiOutcome = observeotel.OutcomeFromError(ctx, aerr)
        return nil, aerr
    }
}
```
Add a comment: exact compare (decision 6), checked before the runtime so a refused request registers nothing.

**Step 4: Run green:** `go test ./internal/loopconnect -count=1 -race`.

**Step 5: Commit:** `loopconnect: refuse StartLoop when the spec digest does not match the token's pin`

---

## Task 6: event - `loop.principal` and `tool.identity.minted`

**Tier:** economy

**Files:**
- Modify: `internal/event/event.go`
- Modify: `internal/event/event_test.go`

**Step 1: Write failing tests.**
- Add `KindLoopPrincipal: "loop.principal"` and `KindToolIdentityMinted: "tool.identity.minted"` to the kinds table in `event_test.go`.
- `TestLoopPrincipalPayloadShape`: marshal `LoopPrincipalPayload{Subject:"s", ClientID:"c", Issuer:"i", Scopes: []string{}, TokenID:"t"}` with `MarshalPayload` and unmarshal it into `map[string]any`. The keys are exactly `subject, client_id, issuer, scopes, token_id`, and `scopes` is a JSON array (not null).
- `TestToolIdentityMintedPayloadShape`: the keys are exactly `subject, audience, scope, loop_id`.

**Step 2: Run red:** `go test ./internal/event -count=1`.

**Step 3: Implement** in `event.go`, after `KindMCPServer`, with doc comments:
- `KindLoopPrincipal Kind = "loop.principal"`: the credential that started the loop. It is recorded once at a networked StartLoop, right after `loop.harness` (first when no spec), and never on resume. It holds identity and audit claims only, never token material. It is absent for loops started without an authenticated principal (stdio binding, local CLI).
- `KindToolIdentityMinted Kind = "tool.identity.minted"`: one per delegated tool-identity token minted for a call made by this loop. It never holds the token.
- The payloads:
  ```go
  type LoopPrincipalPayload struct {
      Subject  string   `json:"subject"`
      ClientID string   `json:"client_id"`
      Issuer   string   `json:"issuer"`
      Scopes   []string `json:"scopes"` // [] = unrestricted
      TokenID  string   `json:"token_id"`
  }
  type ToolIdentityMintedPayload struct {
      Subject  string `json:"subject"`
      Audience string `json:"audience"`
      Scope    string `json:"scope"` // space-separated
      LoopID   string `json:"loop_id"`
  }
  ```

**Step 4: Run green.**

**Step 5: Commit:** `event: add loop.principal and tool.identity.minted kinds`

---

## Task 7: runtime - record the principal right after `loop.harness`

**Tier:** standard

**Files:**
- Modify: `internal/runtime/runtime.go` (`LoopConfig`)
- Modify: `internal/runtime/inproc.go` (`launchLoop`, right after the harness-record block, around line 594)
- Modify: `internal/runtime/harness.go` (a `principalEvent` helper next to `harnessEvent`)
- Create: `internal/runtime/principal_test.go`

**Step 1: Write failing tests** in `principal_test.go`, reusing `fakeHarness`, `processBuild`, `scriptedCompleter`, `testSpec`, `kindsOf`, `failingAppendStore` and `newFakeRegistry` from `harness_test.go`. Principal record `rec := &event.LoopPrincipalPayload{Subject:"alice", ClientID:"client-1", Issuer:"cms", Scopes: []string{"loop:start"}, TokenID:"x"}`.
- `TestPrincipalRecordedAfterHarness`: durable FS store (`fsstore.NewDurableFSStore(t.TempDir())`) with `Harness: fh`. `StartLoop{Task, Tenant:"acme", Harness: testSpec(), Principal: rec}`, then wait. `Attach` history: `hist[0].Kind == KindLoopHarness`, `hist[1].Kind == KindLoopPrincipal`, there is exactly one `loop.principal`, and its payload decodes equal to `*rec`.
- `TestPrincipalRecordedFirstWithoutHarness`: no spec, `BuildAgent: processBuild(...)`, `InMemoryEvents: true` or the durable store. `hist[0].Kind == KindLoopPrincipal`.
- `TestNoPrincipalNoRecord`: `Principal: nil` gives no `loop.principal` in history.
- `TestPrincipalNotRecordedAgainOnResume`: mirror `TestHarnessIsRecordedOnceAndResumeRebuildsFromIt` (an interactive loop, a park, a reap, a Resume on a second runtime). After the resume there is still exactly one `loop.principal`.
- `TestFailedPrincipalRecordLeavesTheLoopNotLive`: mirror `TestFailedHarnessRecordLeavesTheLoopNotLive` with no spec and `Principal: rec` over `failingAppendStore`. StartLoop errors, the record is not live, and no agent was built.

**Step 2: Run red:** `go test ./internal/runtime -run 'Principal' -count=1`.

**Step 3: Implement.**
- `LoopConfig` gains, after `Harness`:
  ```go
  // Principal, when non-nil, is the authenticated credential that started this
  // loop, recorded as the loop.principal event right after loop.harness. It is a
  // plain event payload, not an auth type: the runtime seam never imports auth
  // (ADR-0030). The Connect edge sets it; the stdio and local bindings leave it nil.
  Principal *event.LoopPrincipalPayload
  ```
- `harness.go`: `func principalEvent(rootID string, p *event.LoopPrincipalPayload) (event.Event, error)` copies `*p`, normalizes a nil `Scopes` to `[]string{}`, and marshals with `event.MarshalPayload`.
- `inproc.go`: directly after the harness block, `if cfg.Principal != nil && !opts.resume { ... }`, with the same append and the same failure teardown as the harness block (close the store if closable, `failLaunch()`, return `fmt.Errorf("runtime: record principal: %w", err)`).

**Step 4: Run green:** `go test ./internal/runtime -count=1 -race`.

**Step 5: Commit:** `runtime: record the starting principal as loop.principal after loop.harness`

---

## Task 8: loopconnect threads the principal record; the stdio binding never does

**Tier:** economy

**Files:**
- Modify: `internal/loopconnect/handler.go` (`StartLoop`'s `runtime.LoopConfig`)
- Modify: `internal/loopconnect/scopes_test.go` (or `handler_authz_test.go`)
- Modify: `internal/loopserver/server_test.go`

**Step 1: Write failing tests.**
- `TestStartLoopThreadsPrincipalRecord` (loopconnect): token `{Tenant:"acme", Subject:"alice", ClientID:"client-1", Issuer:"cms", TokenID:"x", Scopes: NewScopes("loop:start","loop:observe")}`. After StartLoop, `rt.sawStartCfg.Principal` equals `&event.LoopPrincipalPayload{Subject:"alice", ClientID:"client-1", Issuer:"cms", Scopes: []string{"loop:observe","loop:start"}, TokenID:"x"}`. Also, an unrestricted token gives `Scopes == []string{}` (non-nil).
- `TestStartLoopNoPrincipalNoRecord` (loopconnect): `newTestServer` (no interceptor) gives `sawStartCfg.Principal == nil`.
- `internal/loopserver`: in the existing fake-runtime StartLoop test (find the one that captures the `LoopConfig`), assert `cfg.Principal == nil`. This is the stdio-binding half of the acceptance criterion.

**Step 2: Run red.**

**Step 3: Implement:** in `StartLoop`, when `havePrincipal`, set `Principal: &event.LoopPrincipalPayload{Subject: p.Subject, ClientID: p.ClientID, Issuer: p.Issuer, Scopes: p.Scopes.List(), TokenID: p.TokenID}` on the `LoopConfig`. Never put `p.HarnessDigest` or anything token-derived beyond these fields into it.

**Step 4: Run green:** `go test ./internal/loopconnect ./internal/loopserver -count=1 -race`.

**Step 5: Commit:** `loopconnect: hand the runtime a principal record at StartLoop`

---

## Task 9: toolidentity - `AuditRecord.Loop`

**Tier:** economy

**Files:**
- Modify: `internal/toolidentity/broker.go`
- Modify: `internal/toolidentity/broker_test.go`

**Step 1: Write failing tests** in `broker_test.go` with a recording `AuditSink` (reuse one if present):
- `TestBrokerAuditCarriesLoop`: oauth-tier target, ctx `WithLoop(ctx, "loop-1")`. The recorded `AuditRecord.Loop == "loop-1"` and `Tier == "oauth"`. Static tier with the same ctx gives `Loop == "loop-1"` and `Tier == "static"`. With no loop on ctx, `Loop == ""`.
- `TestAuditRecordStringHasNoToken`: if not already covered, `String()` contains `loop=loop-1` and never the minted token value.

**Step 2: Run red.**

**Step 3: Implement:** add `Loop string` to `AuditRecord` (doc: the loop the call came from, from `LoopFrom(ctx)`; empty when none). In `CredentialFor`, read `loop, _ := LoopFrom(ctx)` once at the top and set `Loop: loop` on both emitted records. Add `loop=%s` to `String()`.

**Step 4: Run green:** `go test ./internal/toolidentity -count=1 -race`.

**Step 5: Commit:** `toolidentity: audit records name the loop a mint was for`

---

## Task 10: cmd/fuse - mint audit sink appends `tool.identity.minted` to the loop's own stream

**Tier:** premium (composition root; the inert-knob trap applies)

**Files:**
- Create: `cmd/fuse/mint_audit.go`
- Create: `cmd/fuse/mint_audit_test.go`
- Modify: `cmd/fuse/tool_identity.go` (`newToolIdentityBroker`, `buildToolIdentitySource`, `mcpAttach`)
- Modify: `cmd/fuse/sandbox.go` (the `newToolIdentityBroker` call: pass `nil`)
- Modify: `cmd/fuse/loop_server.go` (`assembleLoopServerRuntimeDeps`, `loopServerHost`, `factory`, `teardown`)
- Modify: `cmd/fuse/loop_harness.go` (`settingsFor`)

Before starting, run `grep -n 'mcpAttach\|newToolIdentityBroker\|buildToolIdentitySource' cmd/fuse/*.go` and update every caller the compiler flags.

**Step 1: Write failing tests** in `mint_audit_test.go`:
- `TestLoopMintAuditRecordsOAuthMintsToBoundLoop` (unit): `a := newLoopMintAudit()`, `store := event.NewMemStore(0)`, `reg := tools.NewRegistry()`, `a.bind(reg, "loop-1", store)`. `a.Record(toolidentity.AuditRecord{Subject:"alice", Tenant:"acme", Audience:"https://stub.example", Scopes: []string{"read","write"}, Tier:"oauth", Loop:"loop-1"})`. `store.Replay(0)` holds one `KindToolIdentityMinted` with `NodeID == "loop-1"`, and its payload decodes to `{subject: alice, audience: https://stub.example, scope: "read write", loop_id: loop-1}` with exactly those four keys.
- `TestLoopMintAuditIgnoresStaticUnknownAndUnbound` (unit): a `Tier:"static"` record appends nothing. `Loop:"other"` appends nothing. `Loop:""` appends nothing. After `a.unbind(reg)`, a `loop-1` oauth record appends nothing. A nil `*loopMintAudit` has `Record`/`bind`/`unbind` as no-ops (no panic).
- `TestLoopServerMintEventsReachTheLoopStream` (composition, the security-knob guard): `stub := newStubMCPServer(t, "do_stub")`, `cfg := loopServerMCPCfg(stub.URL(), "acme")`, `deps := buildLoopServerRuntimeDeps(nil, cfg, newTestModelRegistry(), "cloud/x", defaultToolRegistry(nil, cfg.Research, nil), spawnAgentBlock, permissions.AlwaysApprove, nil)`. `loopReg := deps.NewToolRegistry()`, deferred `deps.LoopTeardown(loopReg)`. `store := event.NewMemStore(0)`, `tree := agent.NewAgentTreeWithRootID("root", "cloud/x", 1, "loop-mint-1")`. `deps.BuildAgent(store, tree, "cloud/x", loopReg)` must not error. `ctx := deps.LoopContext(ctx10s, runtime.LoopConfig{Tenant:"acme", Subject:"alice", LoopID:"loop-mint-1"})`. Then `loopReg.Execute(ctx, "mcp:stub/do_stub", "{}")` twice, both non-error. The store holds exactly two `tool.identity.minted` events, each with `subject == "alice"`, `audience == "https://stub.example"`, `loop_id == "loop-mint-1"`. No event payload anywhere in the store contains `"eyJ"` (the prefix of every minted JWT), and each minted payload decodes to exactly the four keys.
- `TestHarnessLoopSettingsCarryMintAudit`: build a `loopServerHost` via `assembleLoopServerRuntimeDeps` (or the deps' `Harness` field), call `settingsFor` with a resolved config that has an identity MCP server, and assert that a mint through the resulting `mcpOpts` reaches the bound store. If the harness resolver makes that awkward, assert structurally instead: `settingsFor(...).mcpOpts` was built with the host's sink (expose a test-only accessor or compare by constructing through `mcpAttachAudited` with a spy). Whichever form is used, the test must fail if `settingsFor` calls the unaudited `mcpAttach`.

**Step 2: Run red:** `go test ./cmd/fuse -run 'TestLoopMintAudit|TestLoopServerMintEvents|TestHarnessLoopSettingsCarryMintAudit' -count=1`.

**Step 3: Implement.**
- `mint_audit.go`:
  ```go
  // loopMintAudit is the tool-identity broker's audit sink on the loop-server
  // host: it appends one tool.identity.minted event to the stream of the loop a
  // delegated token was minted for. Loops are bound in the loop factory's
  // BuildAgent (the first place holding the loop's own store) and unbound in
  // LoopTeardown (which also runs on a StartLoop early return). A record for a
  // loop this host is not running, or one without a loop, is dropped.
  type loopMintAudit struct {
      byLoop sync.Map // loop id -> event.EventStore
      byReg  sync.Map // *tools.Registry -> loop id
  }
  func newLoopMintAudit() *loopMintAudit
  func (a *loopMintAudit) bind(reg *tools.Registry, loopID string, store event.EventStore)
  func (a *loopMintAudit) unbind(reg *tools.Registry)
  func (a *loopMintAudit) Record(r toolidentity.AuditRecord) // Tier=="oauth" && Loop!="" && bound -> Append
  ```
  `Record` builds `event.ToolIdentityMintedPayload{Subject: r.Subject, Audience: r.Audience, Scope: strings.Join(r.Scopes, " "), LoopID: r.Loop}` and appends `event.Event{NodeID: r.Loop, Kind: event.KindToolIdentityMinted, Payload: payload}`. On a marshal or append error, `fmt.Fprintf(os.Stderr, "loop-server: record tool.identity.minted for %s: %v\n", r.Loop, err)` and return. All methods are nil-receiver safe. Add `var _ toolidentity.AuditSink = (*loopMintAudit)(nil)`.
- `tool_identity.go`: `newToolIdentityBroker(cfg, audit toolidentity.AuditSink)` passes `audit` to `toolidentity.NewBroker`. Add `buildToolIdentitySourceAudited(cfg, audit)` (the current body) and keep `buildToolIdentitySource(cfg)` as a wrapper with `nil`. Add `mcpAttachAudited(cfg, w, audit)` (the current body, calling the audited builder) and keep `mcpAttach(cfg, w)` as a wrapper with `nil`. Pass the sink only when non-nil, so a typed-nil interface never reaches the broker (`var sink toolidentity.AuditSink; if audit != nil { sink = audit }`). Update the doc comments on both wrappers.
- `sandbox.go`: `newToolIdentityBroker(appCfg, nil)` (the egress proxy's mints are out of scope; add a one-line comment saying so).
- `loop_server.go` `assembleLoopServerRuntimeDeps`: create `mintAudit := newLoopMintAudit()` and use `mcpOpts, _ := mcpAttachAudited(cfg, os.Stderr, mintAudit)`. Add `mintAudit *loopMintAudit` to `loopServerHost` and set it. In `factory`, capture `mintAudit := h.mintAudit`. In `BuildAgent`, inside `if loopToolReg != nil {`, before `emitMCPServerEvents`, call `mintAudit.bind(loopToolReg, tree.RootID(), store)`. In `teardown`, call `h.mintAudit.unbind(loopReg)`. Update the comment block above `LoopContext` and `mcpAttach` to mention the sink.
- `loop_harness.go` `settingsFor`: `mcpOpts, _ := mcpAttachAudited(cfg, io.Discard, h.mintAudit)`.

**Step 4: Run green:** `go test ./cmd/fuse -run 'Mint|MCP|ToolIdentity|LoopServer' -count=1 -race`, then `go build ./...`.

**Step 5: Commit:** `loop server: record each delegated tool-identity mint in the loop's stream`

---

## Task 11: End-to-end through the production verifier and the real Connect server

**Tier:** standard

**Files:**
- Create: `cmd/fuse/loop_serve_net_scopes_test.go`

Use `authTestServer(t, v)` with `v` from `buildLoopVerifier(cfg)`, `bearerClient`, `mintIssuerToken`, `issuerClaims` and `waitForTurnEndByObserve` (`cmd/fuse/loop_serve_net_auth_test.go`, `loop_serve_net_issuer_test.go`). Add a local helper `observeUntilTurnEnd(t, client, loopID) []*loopv1.Event` that collects events until `turn.end`. The config is `issuerTestConfig()` with `Tenants: []string{"acme","globex"}`, plus static auth entries as needed.

**Step 1: Write the tests** (each one asserts an acceptance criterion through `buildLoopVerifier`, not a hand-built verifier):
- `TestScopedIssuerTokenEndToEnd`: alice `scope:"loop:start loop:observe"` starts a loop and waits for turn end. Alice `scope:"loop:observe"`: `Observe` succeeds; `StartLoop` gives `PermissionDenied` with a message containing `loop:start`; `Send` gives `PermissionDenied` containing `loop:send`. A token with no `scope` behaves as before (it starts a loop).
- `TestAdminScopeEndToEnd`: bob `scope:"loop:start loop:admin"` `Send`s to alice's loop and succeeds. Bob `scope:"loop:start loop:send"` gets `PermissionDenied`. A bob token in tenant `globex` with admin gets `NotFound` sending to alice's acme loop.
- `TestDigestPinEndToEnd`: alice `harness_digest: harness.Digest(S)`. `StartLoop{}` gives `PermissionDenied`, and `StartLoop{Harness: S2}` gives `PermissionDenied`. In both cases `registry.List(ctx, "acme")` gains no record. Success case: `StartLoop{Harness: S}` with `S := &harnessv1.HarnessSpec{MaxTurns: &one}` succeeds and `resp.Msg.HarnessDigest == claim`. If `authTestServer`'s default config refuses that spec for reasons unrelated to the pin, keep the success case covered by Task 5's handler test and say so in the task report. Do not change `authTestServer`'s semantics for other tests.
- `TestStaticScopesEndToEnd`: config auth `{token: obs, tenant: acme, subject: ops, scopes: [loop:observe]}` and `{token: full, tenant: acme, subject: ops}`. `obs` StartLoop gives `PermissionDenied` containing `loop:start`. `full` StartLoop succeeds.
- `TestLoopPrincipalEventEndToEnd`: alice `scope:"loop:start loop:observe"`, `azp:"client-1"`, `jti:"x"`. After StartLoop, observe to turn end. There is exactly one `loop.principal` event with `subject == "cms-user-alice"`, `client_id == "client-1"`, `issuer == "cms"`, `scopes == ["loop:observe","loop:start"]`, `token_id == "x"`. No event payload contains the raw bearer string.

**Step 2: Run:** `go test ./cmd/fuse -run 'EndToEnd' -count=1 -race`. These should pass on the first run, because Tasks 1 to 10 built the pieces. If any fails, the composition root is missing a wire: fix the wiring, not the test.

**Step 3: Commit:** `loop-serve-net: end-to-end tests for scopes, admin, digest pin and loop.principal`

---

## Task 12: SDK - `Credentials.TokenProvider`

**Tier:** standard

**Files:**
- Modify: `sdk/fuse/fuse.go` (`Credentials`)
- Modify: `sdk/fuse/remote.go` (`NewRemote`, `bearerInterceptor`)
- Create: `sdk/fuse/token_provider_test.go`

**Step 1: Write failing tests** in `token_provider_test.go`. Build a server from `loopv1connect.NewLoopServiceHandler` over a small handler that embeds `loopv1connect.UnimplementedLoopServiceHandler`, records `req.Header().Get("Authorization")` on `GetCapabilities` and `StartLoop`, and records the header on `Observe` (sending one event, then returning).
- `TestTokenProviderCalledPerRequest`: the provider returns `tok-1`, `tok-2`, `tok-3` on successive calls. Calling `client.Capabilities(ctx)` twice and opening `Observe` once records `Bearer tok-1`, `Bearer tok-2`, `Bearer tok-3` in order. Set `Token: "static"` too, and assert it never appears.
- `TestTokenProviderErrorFailsRequest`: the provider returns `errBoom`. `Capabilities` returns an error with `errors.Is(err, errBoom)`, and the server recorded no request. `Observe` open returns an error with `errors.Is(err, errBoom)`.
- `TestTokenProviderEmptySendsNoHeader`: the provider returns `""`, and the server records an empty `Authorization`.
- `TestStaticTokenUnchanged`: no provider, `Token:"t"` gives `Bearer t` (a regression guard).

**Step 2: Run red:** `go test ./sdk/fuse -run 'TestTokenProvider|TestStaticTokenUnchanged' -count=1`.

**Step 3: Implement.**
- `Credentials` gains:
  ```go
  // TokenProvider, when set, is called on every request (each unary call and
  // each stream open) for the bearer token, and Token is ignored. It lets a
  // client present short-lived issuer tokens. An error fails that request
  // before it is sent. There is no caching or refresh loop: cache inside the
  // provider if needed.
  TokenProvider func(ctx context.Context) (string, error)
  ```
- `bearerInterceptor{token string; provider func(context.Context) (string, error)}` gets a `resolve(ctx) (string, error)` method, and `NewRemote` passes both.
- `WrapUnary`: on a provider error, return `nil, fmt.Errorf("fuse: token provider: %w", err)` without calling `next`.
- `WrapStreamingClient`: call the provider before `next`. On error, return a `failedStreamConn{StreamingClientConn: next(ctx, spec), err: wrapped}` whose `Send`, `Receive`, `CloseRequest` and `CloseResponse` return the error (`Spec`/`Peer`/headers delegate). Close the underlying conn's request and response so nothing leaks. Confirm `client.Observe` surfaces the error (connect-go's server-stream call returns the `Send` error). Pin it with the test.
- `remote.go`'s `mapConnectError` passes a non-connect error through unchanged. Keep it that way, so `errors.Is(err, errBoom)` holds.

**Step 4: Run green:** `go test ./sdk/fuse -count=1 -race`.

**Step 5: Commit:** `sdk: Credentials.TokenProvider supplies a bearer token per request`

---

## Task 13: Docs

**Tier:** economy

**Files:**
- Modify: `README.md` ("Authentication and multi-tenancy (change #49)" and "Trusted token issuers")
- Modify: `sdk/README.md`

**Step 1: Edit `README.md`:**
- In the static `auth:` YAML example, add `scopes: [loop:observe]` to one entry, with a comment: optional; omitted means unrestricted.
- Add a "Scopes" subsection after the authorization bullets. It contains:
  - a table mapping each RPC to its required scope: `StartLoop` needs `loop:start`; `Send` needs `loop:send` (or `loop:admin`); `Observe` needs `loop:observe` (or `loop:admin`); `GetCapabilities` needs `capabilities:read`
  - `loop:admin` acts on any loop in the token's tenant regardless of owner (cross-tenant is still `NotFound`) and is never implied by an unrestricted token
  - `secrets:inline` is reserved
  - no scopes means unrestricted; an unknown scope name is ignored but still restricts; an explicitly empty scope (`scope: ""`, `scopes: []`) is refused
  - a refusal is `PermissionDenied` naming the missing scope
- Add a "Harness digest pin" paragraph: a token with `harness_digest: sha256:<hex>` (the `harness_digest` `StartLoop` returns, `harness.Digest`) can only start a loop whose spec has exactly that digest; no spec or a different spec is `PermissionDenied`, and nothing is created.
- Add an "Audit records" paragraph: `loop.principal` (`subject`, `client_id`, `issuer`, `scopes`, `token_id`) right after `loop.harness` on every networked StartLoop; `tool.identity.minted` (`subject`, `audience`, `scope`, `loop_id`) per delegated token mint. Neither holds a token.
- Issuer claims table: add rows for `scope` (optional space-separated string; non-string or empty is refused), `harness_digest` (optional string), `azp` (optional string, recorded as `client_id`), `jti` (optional string, recorded as `token_id`; not a replay cache). Extend the TypeScript minting example's comment to show where `scope` goes.

**Step 2: Edit `sdk/README.md`:** document `Credentials.TokenProvider` with a short Go example (`TokenProvider: func(ctx context.Context) (string, error) { return mint(ctx) }`) and the error behaviour.

**Step 3: Verify:** `go test ./... -run XXX_NONE -count=1` (compile only), then `make test`.

**Step 4: Commit:** `docs: token scopes, harness digest pin, audit events and SDK TokenProvider`

---

## Final verification (docket-build's suite gate)

- `make test` green.
- `go vet ./...` clean.
- `grep -rn 'newLoopMintAudit\|mcpAttachAudited' cmd/fuse/*.go | grep -v _test`: non-test callers exist in `loop_server.go` and `loop_harness.go` (the composition-root wiring is real).
- `grep -rn 'Principal:' internal/loopserver/*.go | grep -v _test` gives nothing (the stdio binding never sets a principal record).
