<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0102 — Loop-scoped secret values on StartLoop, host-bound and never recorded](../../changes/archive/2026-11-01-0102-loop-scoped-secret-values-on-startloop-host-bound-and-never.md)**
<!-- docket:backlink:end -->

# Loop-scoped secret values on StartLoop, host-bound and never recorded - Implementation Plan

> Executed by `docket-build`: one task at a time. Each task is TDD: write the failing test, run it red, write the minimal code, run it green, commit. Every test uses fakes, `httptest` servers, the in-repo scripted/recording gateway, or the in-repo fake MCP server (`mcpAuthFakeServer`). No live model traffic.

**Goal:** A client starting a loop over the networked binding can carry its own credential values for the `secret_ref` names in its harness spec (`StartLoopRequest.secrets`). Each value is bound to the hostnames it may be sent to, held only in memory for resolving that loop, never recorded in the event store, never logged, never echoed in an error. An operator `loop_server.secrets` entry for the tenant always wins over a loop-scoped one. Sending secrets needs the `secrets:inline` scope.

**Architecture:**
- `internal/loopsecret` (NEW leaf package, stdlib-only imports): the `Secret{Name, Value, Hosts}` type with redacting `fmt`/`slog`/JSON renderings, the wire limits (`MaxSecrets = 32`, `MaxValueBytes = 16 << 10`) and `CheckLimits`, plus `WithSecrets(ctx, ss)` / `From(ctx)` context helpers. It carries values from the Connect edge to the resolver without the runtime seam importing anything auth-shaped (ADR-0030).
- `proto/fuse/loop/v1/loop.proto`: `message LoopSecret {string name = 1; string value = 2; repeated string hosts = 3;}` and `StartLoopRequest.secrets = 7`. Go and TS stubs regenerated.
- `internal/loopconnect`: `StartLoop` requires `secrets:inline` (via `requireScope`) when `secrets` is non-empty, applies `CheckLimits` (`CodeInvalidArgument`, never echoing a value), converts the wire entries to `[]loopsecret.Secret` and passes them in `runtime.LoopConfig.Secrets`.
- `internal/runtime`: `LoopConfig.Secrets []loopsecret.Secret`. `resolveHarness` puts them on the ctx it passes to `Deps.Harness.ResolveHarness` only; `launchLoop` clears `cfg.Secrets` right after resolution so nothing downstream (`Deps.LoopContext`, the loop goroutine) ever holds them. Resume never has them.
- `internal/harnessresolve`: `Resolver.resolveSecrets` validates the loop-scoped entries (violations at `secrets[i].name|value|hosts`), then for each `secret_ref` tries the operator `SecretResolver` first and falls back to the loop-scoped entry of that name only when the destination hostname is in its `hosts`. A host mismatch is a violation at the ref's field naming the host rule. One shared host canonicalizer (lower-case, trailing dot stripped) is used by the operator host rule, the loop-scoped host rule and the spec policy.
- `cmd/fuse`: no new wiring is expected (`loopServerHost.ResolveHarness` already passes its ctx to `Resolver.Resolve`); Tasks 7-8 prove that at the composition root and over the real Connect server.
- `sdk/fuse`: `StartLoopConfig.Secrets []LoopSecret` (`LoopSecret = loopsecret.Secret`) and a `StartLoopConfig.WithSecrets(...)` builder; both backends carry them.

**Tech stack:** Go, protobuf/buf, connect-go, `internal/loopsecret`, `internal/loopconnect`, `internal/runtime`, `internal/harnessresolve`, `cmd/fuse`, `sdk/fuse`.

---

## Spec drift found while planning (verified against the feature tree at 7e6acc5)

1. **The SDK has no option functions on `StartLoop`.** The change says "`StartLoop` options gain `WithSecrets(...)`", but `sdk/fuse` takes a `StartLoopConfig` struct (`sdk/fuse/fuse.go`). **Decision:** add `StartLoopConfig.Secrets` (the existing style) plus a value-receiver builder `func (c StartLoopConfig) WithSecrets(s ...LoopSecret) StartLoopConfig`, so the name the change uses exists without introducing a second configuration style.
2. **"Unrestricted principals included"** is read together with the acceptance criterion "an unrestricted principal succeeds": the scope gate uses `Principal.Allows(secrets:inline)` through the existing `requireScope`, so an unrestricted token passes and a scoped token must list `secrets:inline`. A request with no principal (an un-intercepted test path) is not checked, like every other scope.
3. **Secrets without a harness spec.** Only a harness spec has `secret_ref`s. `runtime.resolveHarness` returns before resolution when `Harness == nil`, so secrets sent without a spec are never read and are dropped. The proto comment says so. The scope gate and the limits still apply (they run at the edge before the runtime).
4. **Resume.** Values are memory-only, so a resumed loop (another replica, or after a drain) has none. A resumed spec loop whose `secret_ref` was satisfied only by a loop-scoped secret fails re-resolution with the ordinary `secret_ref` violation (fail closed). The proto comment says so. No code for this beyond clearing; it follows from `Resume` building a fresh `LoopConfig`.
5. **`GetCapabilities` is untouched** (fields 7 and 8 included): loop-scoped names belong to one request, not to the tenant, and are never advertised.

## Decisions made in this plan (record them in the PR body; the ADR is recorded separately by the parent)

1. **Precedence:** operator first. `r.Secrets.ResolveSecret(tenant, name, destination)` succeeding wins outright and the loop-scoped value is ignored. On any operator refusal the loop-scoped entry of that name is tried. This never discloses an operator value (the fallback only ever uses the client's own value).
2. **Loop-scoped entry validation** (all `harness.RuleSecretRef`, all reported in the same `*harness.ValidationError` as the spec's other violations; an invalid entry is excluded from lookup):
   - `secrets[i].name`: must match `config.SecretRefPattern`; a name repeated across entries is a violation at every later duplicate's `secrets[j].name`.
   - `secrets[i].value`: must not be empty.
   - `secrets[i].hosts`: must not be empty (the change's rule); an empty-string element is a violation at `secrets[i].hosts[j]`.
3. **Host rule message** (at the ref's own path, e.g. `mcp_servers[0].auth.client_secret_ref`): `loop secret "my-token" may only be sent to the hosts it lists, and "other.test" is not one of them` (the destination's canonical hostname; never the value). A ref whose name matches neither source keeps today's fixed message `secret %q is not available to this tenant`.
4. **One host canonicalizer** `canonicalHost(h) = strings.ToLower(strings.TrimSuffix(h, "."))` in `internal/harnessresolve`, used by: the operator `hosts` rule in `ConfigSecrets` (previously `strings.EqualFold`, which missed the trailing-dot spelling), the new loop-scoped rule, and `checkURL`/`hostMatchesAny` in `policy.go` (which already normalized this way inline). Learning: canonicalize once before every matching layer.
5. **Limits** are an edge check (`CodeInvalidArgument`): more than 32 entries, or any value longer than 16384 bytes. Messages name the count or the index and byte length only: `loopconnect: 33 secrets exceeds the limit of 32`, `loopconnect: secrets[3].value is 16385 bytes, over the limit of 16384`.
6. **Order at the edge:** `loop:start` scope, then (when `secrets` is non-empty) `secrets:inline` scope, then limits, then the existing tenant/digest-pin/orchestrate/tenant-limit checks. A refused request never reaches the runtime and spends no tenant-limit slot.
7. **Redaction is defense in depth:** `loopsecret.Secret` implements `fmt.Formatter` (every verb), `String`, `GoString`, `slog.LogValuer` and `json.Marshaler`, each rendering the value as `[REDACTED]`, so a `%+v` of a `LoopConfig` or a structured log of one can never print a value.

## Learnings applied

- `security-knob-inert-at-composition-root`: the resolver test alone proves nothing about the binary. Task 7 drives `runtime.StartLoop` over the production loop-server deps (`harnessServer`), and Task 8 drives the real Connect server with the production verifier (`buildLoopVerifier`), so the edge-to-resolver path is asserted where it ships.
- `runtime-deps-field-overwrites-builder-injection`: assert at the `StartLoop` level, not by calling the resolver directly; Task 5 also asserts that `Deps.LoopContext` sees `cfg.Secrets == nil` (the later reader never holds values).
- `canonicalize-once-before-every-matching-layer`: decision 4.
- `break-import-cycle-with-agent-free-subpackage`: `internal/loopsecret` is a stdlib-only leaf, pinned by a `go list -deps` test.

---

## Task 1: `internal/loopsecret` leaf package

**Tier:** standard

**Files:**
- Create: `internal/loopsecret/loopsecret.go`
- Create: `internal/loopsecret/loopsecret_test.go`

**Step 1: Write failing tests** in `loopsecret_test.go` (package `loopsecret`):
- `TestSecretNeverRendersItsValue`: `s := Secret{Name: "my-token", Value: "v-SENTINEL-123", Hosts: []string{"mcp.acme.test"}}`. For each of `fmt.Sprint(s)`, `fmt.Sprintf("%v", s)`, `"%+v"`, `"%#v"`, `"%s"`, `"%q"`, `fmt.Sprintf("%+v", []Secret{s})`, `fmt.Sprintf("%+v", struct{ S []Secret }{[]Secret{s}})`, `s.String()`, `s.GoString()`, `json.Marshal(s)`, and a `slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "s", s)` buffer: the output does not contain `v-SENTINEL-123`, does contain `my-token` and `[REDACTED]`.
- `TestCheckLimits`: 32 entries with 16384-byte values is nil; 33 entries is an error whose text contains `33` and `32`; one entry with a 16385-byte value (containing the sentinel) is an error whose text contains `secrets[0]` and `16385` and NOT the sentinel; empty/nil slice is nil.
- `TestContextRoundTrip`: `From(context.Background())` is nil; `From(WithSecrets(ctx, ss))` equals `ss` (`reflect.DeepEqual`); `WithSecrets(ctx, nil)` returns `ctx` unchanged (same value); mutating the caller's slice and its `Hosts` after `WithSecrets` does not change what `From` returns (defensive copy).
- `TestIsAStdlibOnlyLeaf`: `go list -deps github.com/ethanhinson/fuse/internal/loopsecret` (skip when `go` is not on PATH, as `internal/runtime/import_direction_test.go` does) lists no `github.com/ethanhinson/fuse/` package other than itself.

**Step 2: Run red:** `go test ./internal/loopsecret -count=1`.

**Step 3: Implement** `loopsecret.go`:
- Package doc: loop-scoped secret values (change 0102) travel from the Connect edge to the harness resolver in memory only; never log, persist or put a value in an error; this package imports only the standard library so the runtime seam can carry it (ADR-0030).
- `type Secret struct { Name string; Value string; Hosts []string }` with doc comments.
- `const redacted = "[REDACTED]"`; `func (s Secret) String() string` returns `fmt.Sprintf("loopsecret.Secret{Name:%q, Hosts:%q, Value:%s}", s.Name, s.Hosts, redacted)`; `GoString` returns the same; `Format(f fmt.State, verb rune)` writes `s.String()` for every verb (use `io.WriteString(f, ...)`); `LogValue() slog.Value` returns `slog.GroupValue(slog.String("name", s.Name), slog.Any("hosts", s.Hosts), slog.String("value", redacted))`; `MarshalJSON` marshals `struct{Name string; Hosts []string; Value string}{s.Name, s.Hosts, redacted}` with json tags `name`, `hosts`, `value`.
- `const ( MaxSecrets = 32; MaxValueBytes = 16 << 10 )`.
- `func CheckLimits(ss []Secret) error`: `len(ss) > MaxSecrets` gives `fmt.Errorf("%d secrets exceeds the limit of %d", len(ss), MaxSecrets)`; first `len(s.Value) > MaxValueBytes` gives `fmt.Errorf("secrets[%d].value is %d bytes, over the limit of %d", i, len(s.Value), MaxValueBytes)`. (No package prefix: the handler wraps it.)
- `type ctxKey struct{}`; `WithSecrets(ctx, ss)`: `len(ss) == 0` returns ctx; otherwise stores a deep copy (copy the slice and each `Hosts`). `From(ctx) []Secret` returns the stored slice or nil.

**Step 4: Run green:** `go test ./internal/loopsecret -count=1 -race && go vet ./internal/loopsecret`.

**Step 5: Commit:** `loopsecret: in-memory loop-scoped secret values with redacting renderings`

---

## Task 2: Wire contract - `LoopSecret` and `StartLoopRequest.secrets`

**Tier:** economy

**Files:**
- Modify: `proto/fuse/loop/v1/loop.proto`
- Regenerate: `internal/loopwire/v1/loop.pb.go`, `proto/gen/ts/fuse/loop/v1/loop_pb.ts` (whatever `proto/generate.sh` rewrites)
- Modify: `internal/loopwire/v1/loopwire_test.go`

**Step 1: Write a failing test** in `loopwire_test.go`: `TestStartLoopRequestCarriesSecrets` builds `&loopv1.StartLoopRequest{Task: "t", Secrets: []*loopv1.LoopSecret{{Name: "my-token", Value: "v", Hosts: []string{"mcp.acme.test"}}}}`, `proto.Marshal`s and `proto.Unmarshal`s it, and asserts `proto.Equal`. It also asserts field number 7 via `(&loopv1.StartLoopRequest{}).ProtoReflect().Descriptor().Fields().ByName("secrets").Number() == 7` and that `LoopSecret`'s `name/value/hosts` are 1/2/3.

**Step 2: Run red:** `go test ./internal/loopwire/... -count=1` (compile failure).

**Step 3: Implement.** In `loop.proto`, add to `StartLoopRequest`:
```proto
  // secrets carries values for the harness spec's secret_ref names (change
  // 0102). They are loop-scoped and in memory only: the server uses them to
  // resolve this request's harness, never records them (loop.harness keeps
  // names only), and never logs them or puts them in an error. An operator
  // loop_server.secrets entry for the tenant takes precedence over an entry
  // here; otherwise an entry resolves its name only for a destination whose
  // hostname is in its hosts. Requires the secrets:inline scope. At most 32
  // entries, each value at most 16 KiB (InvalidArgument otherwise). Ignored
  // when harness is unset. A resumed loop no longer has them.
  repeated LoopSecret secrets = 7;
```
and a new message after `StartLoopRequest`:
```proto
// LoopSecret is one loop-scoped secret value (see StartLoopRequest.secrets).
message LoopSecret {
  // name is the secret_ref name the harness spec uses (same shape as
  // loop_server.secrets names).
  string name = 1;
  // value is the secret itself. Never recorded, logged or echoed.
  string value = 2;
  // hosts are the hostnames the value may be sent to; required.
  repeated string hosts = 3;
}
```
Regenerate: `cd proto && npm ci && PATH="$HOME/go/bin:$PATH" ./generate.sh` (buf, protoc-gen-go, protoc-gen-connect-go live in `~/go/bin`; the TS plugin comes from `npm ci`). If buf cannot reach the BSR for the protovalidate dep, stop and report (BLOCKED); do not hand-edit generated files.

**Step 4: Run green:** `go test ./internal/loopwire/... -count=1` (with `PATH="$HOME/go/bin:$PATH"` so `TestGeneratedStubsHaveNoDrift` runs rather than skips) and `go build ./...`.

**Step 5: Commit:** `proto: StartLoopRequest.secrets carries loop-scoped LoopSecret values`

---

## Task 3: harnessresolve - one host canonicalizer for every host rule

**Tier:** standard

**Files:**
- Modify: `internal/harnessresolve/secrets.go`
- Modify: `internal/harnessresolve/policy.go`
- Create: `internal/harnessresolve/secrets_test.go` (or extend `resolve_test.go` if `ConfigSecrets` tests already live there; grep first)

**Step 1: Write failing tests:**
- `TestDestinationAllowedCanonicalizesBothSides`: `destinationAllowed([]string{"MCP.Acme.Test."}, "https://mcp.acme.test/x")`, `destinationAllowed([]string{"mcp.acme.test"}, "https://MCP.ACME.TEST./x")` and `destinationAllowed([]string{"mcp.acme.test"}, "https://mcp.acme.test:8443/x")` are true; `destinationAllowed([]string{"mcp.acme.test"}, "https://other.test/")`, `destinationAllowed(nil, "https://mcp.acme.test/")`, `destinationAllowed([]string{"mcp.acme.test"}, "://bad")` and `destinationAllowed([]string{""}, "https:///path")` are false.
- `TestConfigSecretsHostRuleAcceptsTrailingDot`: an operator entry `{Name: "tok", Env: "FUSE_TEST_HR", Hosts: ["mcp.acme.test"]}` (set the env with `t.Setenv`) resolves for `https://mcp.acme.test./` (red today: `EqualFold` misses the trailing dot) and still refuses `https://other.test/` with `ErrSecretUnavailable`.

**Step 2: Run red:** `go test ./internal/harnessresolve -run 'TestDestinationAllowed|TestConfigSecretsHostRule' -count=1`.

**Step 3: Implement** in `secrets.go`:
```go
// canonicalHost is the one spelling every host rule compares: lower case, no
// trailing root dot. The operator secret hosts rule, the loop-scoped secret
// hosts rule and the spec policy all match through it, so no two layers can
// disagree on a spelling.
func canonicalHost(h string) string { return strings.ToLower(strings.TrimSuffix(h, ".")) }

// destinationAllowed reports whether destination's hostname is one of hosts.
// An unparsable destination, an empty hostname or an empty hosts list is never
// allowed.
func destinationAllowed(hosts []string, destination string) bool { ... }
```
Use it in `configSecrets.ResolveSecret` (`if len(e.Hosts) > 0 && !destinationAllowed(e.Hosts, destination)`). In `policy.go`, replace the two inline `strings.ToLower(strings.TrimSuffix(..., "."))` spellings (`checkURL` and `hostMatchesAny`) with `canonicalHost(...)`; behavior there is unchanged.

**Step 4: Run green:** `go test ./internal/harnessresolve -count=1 -race`. All existing policy and secrets tests pass unchanged.

**Step 5: Commit:** `harnessresolve: one host canonicalizer for operator secret hosts and spec policy`

---

## Task 4: harnessresolve - resolve loop-scoped secrets after operator secrets

**Tier:** premium (a resolution mistake sends a credential to the wrong host or masks an operator secret)

**Files:**
- Modify: `internal/harnessresolve/resolve.go` (`resolveSecrets`, package doc)
- Create: `internal/harnessresolve/loopsecrets.go` (entry validation and lookup)
- Create: `internal/harnessresolve/loopsecrets_test.go`

**Step 1: Write failing tests** in `loopsecrets_test.go` using `operator(t)`, `mapSecrets`, `violations` from `resolve_test.go`. A helper spec `mcpSpec(url, ref)` is one MCP server `{Name: "s", Transport: HTTP, Url: url, Auth: {Type: BEARER, ClientSecretRef: ref}}`. Use `const loopValue = "loop-VALUE-never-shown"`. Every test passes secrets with `loopsecret.WithSecrets(context.Background(), ...)`.
- `TestLoopSecretResolvesForAListedHost`: no operator entry; secrets `[{my-token, loopValue, [mcp.acme.test]}]`; spec `mcpSpec("https://mcp.acme.test/", "my-token")`; `Resolve(ctx, "acme", spec)` succeeds and `res.Config.MCPServers[0].Auth.ClientSecret == loopValue`.
- `TestLoopSecretRefusedForAnUnlistedHost`: same with `hosts: [other.test]`: exactly one violation, path `mcp_servers[0].auth.client_secret_ref`, rule `harness.RuleSecretRef`, message contains `may only be sent to the hosts it lists` and `mcp.acme.test`.
- `TestOperatorSecretWinsOverLoopSecret`: `Secrets: mapSecrets{"acme/my-token": secretValue}` plus a loop secret of the same name for the same host: `ClientSecret == secretValue` (the operator value), never `loopValue`.
- `TestLoopSecretFallsBackWhenOperatorRefuses`: operator map has `other-tenant/my-token` only; tenant `acme` with a loop secret resolves to `loopValue`.
- `TestLoopSecretEntryViolations`: one call with secrets `[{"", v, [h]}, {"bad name!", v, [h]}, {"ok", "", [h]}, {"nohosts", v, nil}, {"blankhost", v, ["h", ""]}, {"dup", v, [h]}, {"dup", v, [h]}]` and a spec with no refs: violations (path + rule) are exactly `secrets[0].name`, `secrets[1].name`, `secrets[2].value`, `secrets[3].hosts`, `secrets[4].hosts[1]`, `secrets[6].name`, all `RuleSecretRef`, in `Combine`'s sorted order.
- `TestInvalidLoopSecretIsNotUsed`: a loop secret `{my-token, loopValue, nil}` (empty hosts) with a spec referencing `my-token`: violations include `secrets[0].hosts` and the ref's fixed `secret "my-token" is not available to this tenant`; resolution fails.
- `TestNoViolationOrErrorEchoesALoopSecretValue`: run every failing case above and assert `err.Error()` and every `Violation.Message` exclude `loopValue` (use distinct values containing a sentinel substring per entry).
- `TestNoLoopSecretsKeepsTodaysBehaviour`: `Resolve` with a plain `context.Background()` and the existing operator tests' inputs gives the same results (the existing `resolve_test.go` suite already covers this; just run it).
- Research header refs: `TestLoopSecretResolvesAResearchHeader`: a spec with `research.custom.url = "https://search.acme.test/"` and a header `secret_ref: hdr` resolves from a loop secret with `hosts: [search.acme.test]` (look at how `resolve_test.go` builds a research custom spec and mirror it).

**Step 2: Run red:** `go test ./internal/harnessresolve -run 'LoopSecret|OperatorSecretWins' -count=1`.

**Step 3: Implement.**
- `loopsecrets.go`:
  ```go
  var secretNameRE = regexp.MustCompile(config.SecretRefPattern)

  // loopSecrets indexes the request's loop-scoped secrets (change 0102) by
  // name and reports every invalid entry at its request path. An invalid entry
  // is left out of the index, so it never resolves anything.
  func loopSecrets(ss []loopsecret.Secret) (map[string]loopsecret.Secret, []harnessconfig.Problem)
  ```
  Rules per decision 2. Messages: name `must match <pattern>` / `duplicates the name of an earlier loop secret`; value `must not be empty`; hosts `a loop secret must list the hosts its value may be sent to`; hosts[j] `must not be empty`. Messages never include a value. A duplicate is excluded too (only the first entry of a name is indexed when it is itself valid).
- `resolveSecrets`: read `loop, problems := loopSecrets(loopsecret.From(ctx))` first; for each ref: operator first (unchanged call); on error, if `ls, ok := loop[ref.name]`, then `destinationAllowed(ls.Hosts, ref.destination)` gives the value, else a host-rule problem at `ref.path` (decision 3; hostname via `url.Parse(ref.destination)` then `canonicalHost(u.Hostname())`, empty on parse failure); if no loop entry, today's fixed problem. Keep the `failed` map semantics (a name failing for any destination is removed from `values`). Return entry problems appended to ref problems.
- Update the package doc and `resolveSecrets` doc: loop-scoped values come from the ctx (`loopsecret.From`), operator entries take precedence, and a resolved `Config` may hold either kind of value (memory only).

**Step 4: Run green:** `go test ./internal/harnessresolve -count=1 -race`.

**Step 5: Commit:** `harnessresolve: resolve loop-scoped secrets after operator secrets, host-bound`

---

## Task 5: runtime - `LoopConfig.Secrets` reaches the resolver and nothing else

**Tier:** standard

**Files:**
- Modify: `internal/runtime/runtime.go` (`LoopConfig.Secrets`)
- Modify: `internal/runtime/harness.go` (`resolveHarness`)
- Modify: `internal/runtime/inproc.go` (`launchLoop`: clear after resolution)
- Modify: `internal/runtime/harness_test.go`

**Step 1: Write failing tests** in `harness_test.go`:
- Extend `fakeHarness` with `sawSecrets [][]loopsecret.Secret`, appended from `loopsecret.From(ctx)` in `ResolveHarness` (under the mutex).
- `TestStartLoopHandsLoopSecretsToTheResolverOnly`: `ss := []loopsecret.Secret{{Name: "my-token", Value: "rt-SENTINEL", Hosts: []string{"mcp.acme.test"}}}`. Deps as in `TestStartLoopWithHarnessBuildsFromTheResolverFactory` plus `LoopContext: func(ctx context.Context, cfg LoopConfig) context.Context { capture cfg.Secrets and loopsecret.From(ctx); return ctx }`. `StartLoop(ctx, LoopConfig{Task: "hi", Tenant: "acme", Harness: testSpec(), Secrets: ss})`, `Wait`. Assert: the resolver saw exactly `ss`; the `LoopContext` hook saw `cfg.Secrets == nil` and `loopsecret.From(ctx) == nil`; `Attach` of the loop's events contains no payload with `rt-SENTINEL`.
- `TestLoopSecretsWithoutHarnessAreDropped`: `StartLoop` with `Secrets: ss` and no harness: the resolver is never called (existing pattern in `TestStartLoopWithoutHarnessNeverCallsTheResolver`), and the `LoopContext` hook sees `cfg.Secrets == nil`.
- `TestLoopConfigNeverPrintsASecretValue`: `fmt.Sprintf("%+v", LoopConfig{Secrets: ss})` excludes `rt-SENTINEL` (pins the `loopsecret` formatter through the containing struct).

**Step 2: Run red:** `go test ./internal/runtime -run 'LoopSecret|LoopConfigNeverPrints' -count=1`.

**Step 3: Implement.**
- `LoopConfig`: add after `Harness`:
  ```go
  // Secrets are the loop-scoped secret values the request carried (change
  // 0102). The runtime never reads them: it hands them to Deps.Harness on the
  // resolution context only (loopsecret.WithSecrets) and drops them right
  // after, so Deps.LoopContext, the loop and its stream never hold them.
  // Ignored without a Harness. A resumed loop has none.
  Secrets []loopsecret.Secret
  ```
- `resolveHarness`: `f, err := r.deps.Harness.ResolveHarness(loopsecret.WithSecrets(ctx, cfg.Secrets), event.NormalizeTenant(cfg.Tenant), cfg.Harness)`. The wrapped ctx is local to that call.
- `launchLoop`: immediately after `factory, err := r.resolveHarness(ctx, cfg)` and its error check, add `cfg.Secrets = nil // memory-only: nothing past resolution holds them (change 0102)`. Confirm by reading `launchLoop` that `cfg` is passed by value from here on (it is a value parameter) and that no earlier statement copies `cfg` anywhere.
- Confirm `internal/runtime` still imports no auth package (`policy_free_test.go`, `import_direction_test.go` stay green).

**Step 4: Run green:** `go test ./internal/runtime -count=1 -race`.

**Step 5: Commit:** `runtime: hand loop-scoped secrets to the harness resolver only, then drop them`

---

## Task 6: loopconnect - `secrets:inline` scope, limits, and threading

**Tier:** premium (authorization edge)

**Files:**
- Modify: `internal/loopconnect/handler.go` (`StartLoop`, a `loopSecrets` converter)
- Modify: `internal/loopconnect/scopes_test.go`
- Create: `internal/loopconnect/secrets_test.go`

**Step 1: Write failing tests.**
- In `scopes_test.go`, add tokens to `scopeTokens()`: `"start-inline": {Tenant: "acme", Subject: "alice", Scopes: loopauth.NewScopes(loopauth.ScopeLoopStart, loopauth.ScopeSecretsInline)}` and `"inline-only": {..., Scopes: loopauth.NewScopes(loopauth.ScopeSecretsInline)}`. Add `func (f *scopeFixture) startWithSecrets(token string, ss []*loopv1.LoopSecret) error`.
- In `secrets_test.go` (package `loopconnect`), `ss := []*loopv1.LoopSecret{{Name: "my-token", Value: "edge-SENTINEL", Hosts: []string{"mcp.acme.test"}}}`:
  - `TestSecretsNeedSecretsInline`: `start` + secrets gives `wantDenied(..., loopauth.ScopeSecretsInline)` and `f.rt.startCalls == 0`; `start-inline` + secrets succeeds; `none` (unrestricted) + secrets succeeds; `start` WITHOUT secrets still succeeds (no new requirement); `inline-only` + secrets is denied naming `loop:start` (the start scope is checked first).
  - `TestSecretsReachTheRuntime`: after the `start-inline` start, `f.rt.sawStartCfg.Secrets` equals `[]loopsecret.Secret{{Name: "my-token", Value: "edge-SENTINEL", Hosts: []string{"mcp.acme.test"}}}`.
  - `TestSecretsLimits`: with `start-inline`: 33 entries gives `CodeInvalidArgument` and `startCalls == 0`; 32 entries succeeds; one entry whose value is `strings.Repeat("x", 16385-len(sentinel)) + sentinel` gives `CodeInvalidArgument` whose `err.Error()` excludes the sentinel; a 16384-byte value succeeds.
  - `TestSecretsRefusedBeforeTenantLimits`: with a `tenantlimit` limiter wired via `WithLimits` (see `limits_test.go` for construction), a denied (`start` + secrets) and an oversized request spend no slot: a following valid start still admits.
  - `TestSecretsNoPrincipalPath`: `newTestServer(t, rt)` (no interceptor) accepts secrets (no scope check without a principal) and threads them.
  - `TestRefusalNeverEchoesASecretValue`: every refusal above: `err.Error()` and `connect.CodeOf` path never contain `edge-SENTINEL`.

**Step 2: Run red:** `go test ./internal/loopconnect -run 'Secrets|RefusalNeverEchoes' -count=1`.

**Step 3: Implement** in `StartLoop`, right after the `loop:start` `requireScope` and `m := req.Msg` (move `m := req.Msg` up if needed):
```go
// Loop-scoped secret values (change 0102) need their own scope; an
// unrestricted principal passes as for every scope. The limits apply before
// anything else is spent, and no refusal ever names a value.
secrets := loopSecrets(m.Secrets)
if len(secrets) > 0 {
    if aerr := requireScope(ctx, loopauth.ScopeSecretsInline); aerr != nil {
        apiOutcome = observeotel.OutcomeFromError(ctx, aerr)
        return nil, aerr
    }
    if err := loopsecret.CheckLimits(secrets); err != nil {
        cerr := connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("loopconnect: %w", err))
        apiOutcome = observeotel.OutcomeFromError(ctx, cerr)
        return nil, cerr
    }
}
```
and pass `Secrets: secrets` in the `runtime.LoopConfig`. `loopSecrets(in []*loopv1.LoopSecret) []loopsecret.Secret` returns nil for empty input and copies `Hosts` (`slices.Clone`). Update the `StartLoop` doc comment (secrets need `secrets:inline`; limits; never echoed).

**Step 4: Run green:** `go test ./internal/loopconnect -count=1 -race`. Every existing test passes unchanged.

**Step 5: Commit:** `loopconnect: StartLoop secrets require secrets:inline, are bounded, and reach the runtime`

---

## Task 7: Composition root - loop-scoped secrets through the production loop-server deps

**Tier:** standard

**Files:**
- Create: `cmd/fuse/loop_secrets_test.go`
- Modify (only if a test is red for a wiring reason): `cmd/fuse/loop_harness.go`

These tests are the composition-root proof (learning `security-knob-inert-at-composition-root`). With Tasks 4-5 in, they are expected to pass on first run because `loopServerHost.ResolveHarness` forwards its ctx; write them first anyway and, if any is red, fix the wiring rather than the test.

**Step 1: Write the tests** using `harnessServer`, `harnessOperator`, `mcpAuthFakeServer` from `loop_harness_test.go`:
- `TestLoopScopedSecretReachesTheMCPServer`: `srv, auths := mcpAuthFakeServer(t)`; spec `{MaxTurns: 1, McpServers: [{Name: "s", Transport: STREAMABLE_HTTP, Url: srv.URL, Auth: {Type: BEARER, ClientSecretRef: "my-token"}}]}`; `rt.StartLoop(ctx, runtime.LoopConfig{Task: "go", Harness: spec, Secrets: []loopsecret.Secret{{Name: "my-token", Value: "cr-SENTINEL", Hosts: []string{"127.0.0.1"}}}})`; `Wait`. Some request to the fake carried `Bearer cr-SENTINEL`; the `mcp.server` event is connected; no event payload from `rt.Attach` contains `cr-SENTINEL`; no gateway request body (`gw.snapshot()`, marshal each to JSON) contains it.
- `TestLoopScopedSecretHostMismatchIsRefused`: same spec with `Hosts: ["other.test"]`: `StartLoop` fails with a `*harness.ValidationError` whose only violation is `mcp_servers[0].auth.client_secret_ref` / `RuleSecretRef` with the host-rule message; the fake MCP server got no request; the gateway got no request.
- `TestOperatorSecretWinsAtTheCompositionRoot`: operator YAML `harnessOperator + "loop_server:\n  secrets:\n    - {name: my-token, env: FUSE_TEST_OP_TOKEN}\n"` with `t.Setenv("FUSE_TEST_OP_TOKEN", "operator-v")`; loop secret `my-token` = `client-v` for `127.0.0.1`; the fake saw `Bearer operator-v` and never `Bearer client-v`.

**Step 2: Run:** `go test ./cmd/fuse -run 'LoopScopedSecret|OperatorSecretWinsAtTheCompositionRoot' -count=1`. If red for wiring, fix and re-run.

**Step 3: Commit:** `cmd/fuse: prove loop-scoped secrets at the loop-server composition root`

---

## Task 8: End to end over the real Connect server - scope, limits, and redaction

**Tier:** premium (this is the acceptance proof of "never recorded")

**Files:**
- Modify: `cmd/fuse/loop_secrets_test.go`

**Step 1: Write the tests.** A helper `secretsNetServer(t, operatorYAML) (base string, storeDir string, logs func() string, gw *recordingGateway)`:
- `gw := &recordingGateway{}`; `deps, host := harnessServerWith(t, operatorYAML, gw)`; `dir := t.TempDir()`; `store := fsstore.NewDurableFSStore(filepath.Join(dir, "store"))`; `deps.DurableStore, deps.Registry = store, store`; `deps.BaseDir = filepath.Join(dir, "base")`; `rt := runtime.New(deps)`.
- `v, _, err := buildLoopVerifier(host.operator.cfg)`; listen on `127.0.0.1:0`; `go serveNet(ctx, ln, rt, v, store)` with the cancel-and-wait cleanup copied from `authTestServer`.
- Log capture: before starting, redirect the server's log sinks into one file under `dir`: `log.SetOutput(f)`, `slog.SetDefault(slog.New(slog.NewTextHandler(f, nil)))`, `os.Stderr = f`, `issuerLogWriter = f`, restoring all four in `t.Cleanup` (register the restore after the server cleanup so the server stops first). Grep `cmd/fuse/loop_serve_net.go`, `internal/mcp` and `internal/runtime` for other writers (`fmt.Fprint*(os.Stderr`, `log.`, `slog.`) and redirect any sink they use that the four above do not cover. `logs()` returns the file's contents.
- Operator YAML: `harnessOperator` plus
  ```yaml
  loop_server:
    auth:
      - {token: start-tok, tenant: acme, subject: alice, scopes: [loop:start, loop:observe]}
      - {token: inline-tok, tenant: acme, subject: alice, scopes: [loop:start, loop:observe, secrets:inline]}
      - {token: full-tok, tenant: acme, subject: ops}
  ```

Tests (value `const e2eValue = "e2e-SENTINEL-d41d8"`; `srv, auths := mcpAuthFakeServer(t)`; the spec is Task 7's with `Url: srv.URL`; `secrets := []*loopv1.LoopSecret{{Name: "my-token", Value: e2eValue, Hosts: []string{"127.0.0.1"}}}`):
- `TestLoopSecretsScopeEndToEnd`: `start-tok` + secrets gives `assertDenied(..., loopauth.ScopeSecretsInline)`; `inline-tok` + secrets succeeds; `full-tok` + secrets succeeds. For each success, `waitForTurnEndByObserve` and check the fake saw `Bearer e2eValue`.
- `TestLoopSecretsLimitsEndToEnd`: with `inline-tok`, 33 entries and one 16385-byte value (ending in `e2eValue`) are each `CodeInvalidArgument`, and neither error text contains `e2eValue`.
- `TestLoopSecretValueIsNeverRecorded` (the acceptance grep): with `inline-tok`, start the good loop and wait for its turn end; then force a failure (same request with `Hosts: ["other.test"]`), assert `CodeInvalidArgument`, and assert neither `err.Error()` nor any error detail (iterate `connectErr.Details()`, `Value()`, and `protojson.Marshal` each) contains `e2eValue`. Then cancel the server (call the cleanup path explicitly, or wait for the loop to finish via observe) and:
  - walk `dir` (`filepath.WalkDir`), reading every regular file (gunzip a `.gz` with `compress/gzip`), and assert none contains `e2eValue` (excluding the log file, checked next);
  - assert `logs()` does not contain `e2eValue`;
  - assert no gateway request body contains `e2eValue` (the model never sees it);
  - assert the store DID record the loop (at least one file mentions `my-token`, the name), so the grep is not vacuous.

**Step 2: Run:** `go test ./cmd/fuse -run 'LoopSecrets|LoopSecretValueIsNeverRecorded' -count=1 -race`. Expected to pass once Tasks 1-7 are in; a red here is a real leak or a wiring gap: fix production code, never the assertion.

**Step 3: Commit:** `cmd/fuse: end-to-end proof that loop-scoped secrets need secrets:inline and are never recorded`

---

## Task 9: SDK - `StartLoopConfig.Secrets` and `WithSecrets`

**Tier:** economy

**Files:**
- Modify: `sdk/fuse/fuse.go` (`LoopSecret`, `StartLoopConfig.Secrets`, `WithSecrets`)
- Modify: `sdk/fuse/local.go`, `sdk/fuse/remote.go` (`startLoop`)
- Modify: `sdk/fuse/harness_test.go`

**Step 1: Write a failing test** `TestStartLoopCarriesLoopSecrets` in `harness_test.go`, following `TestStartLoopCarriesTheHarnessSpec`'s two-backend table: `cfg := StartLoopConfig{Task: "t", Harness: sdkSpec()}.WithSecrets(LoopSecret{Name: "my-token", Value: "sdk-v", Hosts: []string{"mcp.acme.test"}})`; after `c.StartLoop(ctx, cfg)`, `f.sawStartCfg.Secrets` equals `[]loopsecret.Secret{{Name: "my-token", Value: "sdk-v", Hosts: []string{"mcp.acme.test"}}}` for both `local` and `remote` (the remote path goes through the real `loopconnect.Handler`, whose dev verifier principal is unrestricted). Also assert `WithSecrets` appends (two calls give two entries) and does not mutate the receiver.

**Step 2: Run red:** `go test ./sdk/fuse -run TestStartLoopCarriesLoopSecrets -count=1`.

**Step 3: Implement.**
- `fuse.go`: `type LoopSecret = loopsecret.Secret` with a doc comment (loop-scoped value for a harness `secret_ref`, sent only to its `Hosts`, never recorded; prints redacted). `StartLoopConfig.Secrets []LoopSecret` with a doc comment mirroring the proto (needs `secrets:inline`, operator entries win, at most 32 / 16 KiB, ignored without `Harness`). `func (c StartLoopConfig) WithSecrets(s ...LoopSecret) StartLoopConfig { c.Secrets = append(slices.Clone(c.Secrets), s...); return c }`.
- `local.go`: `Secrets: cfg.Secrets` in `runtime.LoopConfig`.
- `remote.go`: `Secrets: toWireSecrets(cfg.Secrets)` building `[]*loopv1.LoopSecret` (nil for empty).

**Step 4: Run green:** `go test ./sdk/fuse -count=1 -race`.

**Step 5: Commit:** `sdk/fuse: StartLoopConfig carries loop-scoped secrets (WithSecrets)`

---

## Task 10: Docs

**Tier:** economy

**Files:**
- Modify: `README.md` (the scope table area near "`secrets:inline` is reserved." and the `secret_ref` / capabilities section near line 733)
- Modify: `sdk/README.md` ("Start a loop from a harness spec")

**Step 1:** No test; documentation only.

**Step 2: Write.**
- `README.md`: replace "`secrets:inline` is reserved." with: "`secrets:inline` is required to send `StartLoopRequest.secrets` (loop-scoped secret values); an unrestricted token has it." Add a short "Loop-scoped secrets" paragraph next to the `loop_server.secrets` docs: a client may send `secrets: [{name, value, hosts}]` with `StartLoop` to satisfy its harness spec's `secret_ref` names; values are memory-only, never recorded or logged; an operator entry for the tenant wins; an entry resolves only for a destination whose hostname is in its `hosts` (required); at most 32 entries, 16 KiB per value; ignored without a harness spec; a resumed loop no longer has them; never listed by `GetCapabilities`.
- `sdk/README.md`: one paragraph and a snippet: `fuse.StartLoopConfig{Task: ..., Harness: spec}.WithSecrets(fuse.LoopSecret{Name: "my-token", Value: tok, Hosts: []string{"mcp.acme.test"}})`.
- Follow the repository's writing style (plain, no em dashes).

**Step 3: Commit:** `docs: loop-scoped secrets on StartLoop and the secrets:inline scope`

---

## Final verification (docket-build's suite gate)

- `make test` green (with `PATH="$HOME/go/bin:$PATH"` so the proto drift guard runs).
- `go vet ./...` clean.
- `go list -deps ./internal/loopsecret | grep ethanhinson/fuse/` lists only `internal/loopsecret`.
- `grep -rn 'Secrets:' internal/loopconnect/handler.go` shows the `runtime.LoopConfig` threading; `grep -rn 'loopsecret.WithSecrets' internal/runtime/*.go | grep -v _test` shows exactly the `resolveHarness` call; `grep -n 'cfg.Secrets = nil' internal/runtime/inproc.go` shows the drop.
- `git diff 7e6acc5 -- proto/fuse/loop/v1/loop.proto` touches only `StartLoopRequest` and the new `LoopSecret` (no `GetCapabilitiesResponse` change).
