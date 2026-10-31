<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0086 — Loop server advertises its capabilities - models, skills, ceilings](../../changes/active/0086-loop-server-advertises-its-capabilities-models-skills-ceilin.md)**
<!-- docket:backlink:end -->

# Loop Server Capability Discovery Implementation Plan

> **For agentic workers:** `docket-build` executes this plan task by task. Each task is one commit
> and starts with a failing test. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Add an authenticated, read-only `LoopService.GetCapabilities` RPC. For the caller's tenant
it returns the models the server routes, the installed skills, the tighten-only ceilings (keyed by
violation field path), the secret refs that resolve, the MCP auth kinds a spec may use, and the
server version and digest algorithm. Add the public Go SDK method `Client.Capabilities(ctx)`.

**Architecture:** Each part of the answer comes from the same code StartLoop enforces with, so
advertisement and enforcement cannot drift apart.

- Ceilings come from `harnessresolve.Resolver`. They are derived from the
  `(fuse.harness.v1.ui).tighten_only` descriptor options, and they read operator values through
  `operatorLimit`, the same accessor `tightenProblems` uses.
- MCP auth kinds come from one predicate, `specAuthAllowed`. It also drives `operatorOnlyProblems`.
- Secret names come from `ConfigSecrets`, through the same `available` rule that `ResolveSecret`
  uses.
- Skills come from the `*skills.Set` that `ResolveHarness` looks names up in.
- Models come from the operator's `*model.Registry`, leaving out `cli/` routes and entries marked
  `advertise: false`.

The new `internal/capabilities` package holds plain Go types. `loopServerHost.Capabilities` in
`cmd/fuse` assembles the answer. `internal/loopconnect` authenticates the call, takes the tenant
from the principal, and maps the answer to proto. Proto never appears outside the edge.

**Tech Stack:** Go, connect-go, protobuf (buf, protoc-gen-go, protoc-gen-connect-go, protoc-gen-es),
`google.protobuf.Value` (structpb), `golang.org/x/net/http2` for the h2c test client.

**Rules for every task:** Use deterministic fakes or scripted gateway doubles only, never a live
model provider. No em dash characters in code, comments, docs, or commits; use `-`. Do not mention
any downstream application or customer. No agent co-author trailer on commits. The test command is
`make test`.

---

## Reconcile notes (read first)

1. **`GetCapabilitiesRequest` has a `tenant` field. This deviates from the spec on purpose.** The
   spec wants `PermissionDenied` "through the same loopauth path" as StartLoop. The only
   `PermissionDenied` that path emits is the wire-tenant spoof check in
   `loopconnect.Handler.effectiveTenant`. Issuer tenant refusals come back as `Unauthenticated`. An
   empty request could therefore never produce the spec's `PermissionDenied`
   (`spec-asserted-error-code-verify-emitter-first`). So the request carries `string tenant = 1`,
   with `StartLoopRequest.tenant` semantics: when it is set, it must match the principal. Filtering
   always uses the principal's tenant and never the field. The SDK fills the field from
   `Credentials.Tenant`.
2. **Workflow pool ceilings are listed per operator-defined workflow**, at
   `workflows["<name>"].pool.<field>`. That is the path `tightenField` already reports. A spec
   workflow the operator does not define belongs to the client (`config.mergeSpecWorkflows`), so it
   has no ceiling.
3. **Every tighten-only field is `CEILING_DIRECTION_MAX`** ("may only lower"). `MIN` is declared but
   nothing emits it.
4. **Host-restricted secrets are listed.** The listing applies the tenant rule and the
   non-empty-source rule. `hosts:` still constrains where StartLoop sends the value. Document this in
   the proto and the README.
5. **The TUI model editor must preserve the new keys.** `internal/tui/models_editor.go` rebuilds the
   entry and `config.SetModel` overwrites it. Without preservation, editing a hidden model would
   re-advertise it. Task 1 preserves the keys but does not display them.
6. **Codegen:** `buf`, `protoc-gen-go`, and `protoc-gen-connect-go` live in `$HOME/go/bin`, which is
   not on PATH, and `proto/node_modules` is absent. Run `cd proto && npm ci`, then
   `PATH="$HOME/go/bin:$PATH" make proto`.

Learnings applied:

- `parity-test-feeds-each-side-its-own-production-source`: the golden test (Task 8) uses the real
  host and the real `runtime.StartLoop`.
- `security-knob-inert-at-composition-root`: Task 9 makes startup refuse a missing source, and
  Task 10 runs the real `fuse loop-serve-net` command.

## File structure

| File | Change |
|---|---|
| `internal/config/schema.go`, `internal/model/registry.go`, `internal/configcheck/configcheck.go` | New model keys, threaded into the registry |
| `internal/tui/models_editor.go`, `internal/harnessconfig/harnessconfig.go` | Preserve the keys on edit; report them as `FromConfig` omissions; export `AuthTypeToConfig` |
| `internal/agent/loop.go`, `harness/digest.go` | Export `DefaultContextWindow` and `DigestAlgorithm` |
| `internal/capabilities/capabilities.go` (new) | Answer types; `Models`, `Skills`, `ServerInfo` |
| `internal/harnessresolve/ceilings.go` (new), `resolve.go` | `Resolver.Ceilings`, shared `operatorLimit` |
| `internal/harnessresolve/advertise.go` (new), `secrets.go`, `resolve.go` | `SpecMCPAuthKinds`, `SecretLister`, `Resolver.SecretRefs` |
| `proto/fuse/loop/v1/loop.proto` plus regenerated `internal/loopwire/v1/*` and `proto/gen/ts/.../loop_pb.ts` | RPC and messages |
| `internal/loopconnect/capabilities.go` (new), `handler.go` | `CapabilitySource`, `WithCapabilities`, `GetCapabilities` |
| `cmd/fuse/loop_harness.go`, `cmd/fuse/loop_serve_net.go` | `resolver()`, `Capabilities`, wiring, usage text |
| `sdk/fuse/capabilities.go` (new), `fuse.go`, `remote.go`, `local.go` | `Client.Capabilities` |
| `README.md`, `sdk/README.md` | Docs |

---

### Task 1: Operator model metadata keys

**Tier hint:** standard

**Files:** `internal/config/schema.go`, `internal/model/registry.go`,
`internal/configcheck/configcheck.go`, `internal/tui/models_editor.go`,
`internal/harnessconfig/harnessconfig.go`. Tests: `internal/config/models_metadata_test.go` (new)
plus the existing test files of the other packages.

- [ ] **Step 1: Write failing tests**

`internal/config/models_metadata_test.go`:

```go
package config

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestModelMetadataKeysLoad(t *testing.T) {
	cfg, err := FromYAML([]byte(`
models:
  default: fast
  fast: {id: cloud/fast, label: Fast coder, description: Cheap, tags: [tools, house-special]}
  hidden: {id: cloud/hidden, advertise: false}
`), "test")
	if err != nil {
		t.Fatal(err)
	}
	fast := cfg.Models.Entries["fast"]
	if fast.Label != "Fast coder" || fast.Description != "Cheap" || !reflect.DeepEqual(fast.Tags, []string{"tools", "house-special"}) || fast.Advertise != nil {
		t.Fatalf("fast = %+v", fast)
	}
	if a := cfg.Models.Entries["hidden"].Advertise; a == nil || *a {
		t.Fatalf("hidden.advertise = %v, want false", a)
	}
}

func TestOldModelEntriesLoadUnchanged(t *testing.T) {
	cfg, err := FromYAML([]byte("models:\n  old: {id: cloud/old, max_tokens: 100, context_window: 2000, persona: coding, system_prefix: /x}\n"), "test")
	if err != nil {
		t.Fatal(err)
	}
	want := ModelConfig{ID: "cloud/old", MaxTokens: 100, ContextWindow: 2000, Persona: "coding", SystemPrefix: "/x"}
	if got := cfg.Models.Entries["old"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("old = %+v", got)
	}
}

// A TUI save of an entry without metadata must not sprout the new keys.
func TestModelMetadataKeysOmittedWhenEmpty(t *testing.T) {
	b, _ := yaml.Marshal(ModelConfig{ID: "cloud/x"})
	for _, k := range []string{"label:", "description:", "tags:", "advertise:"} {
		if strings.Contains(string(b), k) {
			t.Errorf("empty entry encoded %s\n%s", k, b)
		}
	}
}
```

Add these tests in the other packages:

- `internal/configcheck/configcheck_test.go` `TestRegistryCarriesModelMetadata`. A config entry with
  `Label`, `Description`, and `Tags` resolves through `Registry(cfg)` with those values. An entry
  with `Advertise: &false` resolves with `Hidden == true`, and an entry with `Advertise` unset
  resolves with `Hidden == false`.
- `internal/tui/models_editor_test.go`. Extend `TestModelsEditorEditPreservesAliasAndUpdates`: seed
  `beta` with `Label: "Beta", Description: "b", Tags: []string{"tools"}, Hidden: true`, then assert
  all four survive the edit, the same way `SystemPrefix` does. Add
  `TestToConfigModelCarriesMetadata`: `Hidden: true` writes `Advertise` as a pointer to false, and
  `Hidden: false` leaves `Advertise` nil.
- `internal/harnessconfig/harnessconfig_test.go` `TestFromConfigOmitsModelMetadata`. A single model
  `fast` with all four keys set yields omissions `models.fast.label`, `.description`, `.tags`, and
  `.advertise`, while `spec.Models.Entries["fast"].Id` is still `cloud/fast`.

- [ ] **Step 2:** Run `go test ./internal/config/ ./internal/configcheck/ ./internal/tui/ ./internal/harnessconfig/`.
  Expected: compile failure on the unknown fields.

- [ ] **Step 3: Implement**

`config.ModelConfig`. Append:

```go
	// Operator display metadata advertised by GetCapabilities (change 0086);
	// nothing else reads it. Well-known tags: tools, vision, reasoning,
	// long-context; other strings pass through.
	Label       string   `yaml:"label,omitempty"`
	Description string   `yaml:"description,omitempty"`
	Tags        []string `yaml:"tags,omitempty"`
	// Advertise false hides the entry from GetCapabilities; nil means advertised.
	Advertise *bool `yaml:"advertise,omitempty"`
```

`model.ModelConfig`. Append `Label, Description string`, `Tags []string`, and `Hidden bool`, with a
comment noting that Hidden comes from config `advertise: false` and that its zero value means
advertised, so `DefaultRegistry` needs no change.

`configcheck.Registry`. Add `Label: mc.Label, Description: mc.Description, Tags: slices.Clone(mc.Tags), Hidden: mc.Advertise != nil && !*mc.Advertise`.

`models_editor.go`. In the `if f.editing` preserve block, also copy `prev.Label`, `prev.Description`,
`prev.Tags`, and `prev.Hidden`. Add a comment: dropping them would silently re-advertise a hidden
model. In `toConfigModel`, map `Label`, `Description`, and `Tags`, and when `mc.Hidden` is true set
`Advertise` to a pointer to `false`.

`harnessconfig.FromConfig`. In the models loop, call
`omit("models."+alias+".<key>", reasonOperator)` for each of `label`, `description`, `tags`, and
`advertise` that is set.

- [ ] **Step 4:** Run the same command. Expected: PASS, including the existing
  `TestRoundTripConfigSpecConfig`.
- [ ] **Step 5:** `git add internal/config/models_metadata_test.go && git commit -am "config: optional label, description, tags and advertise on model entries"`

---

### Task 2: Export the context-window default and the digest algorithm

**Tier hint:** economy

- [ ] **Step 1: Failing tests.** In `harness/digest_test.go`, add a test asserting
  `DigestAlgorithm == "sha256"` and that `Digest(nil)` starts with `DigestAlgorithm + ":"`. Match the
  file's package qualifier. In an `internal/agent` test, assert `DefaultContextWindow == 128_000`.
- [ ] **Step 2:** Run `go test ./harness/ ./internal/agent/`. Expected: compile failure.
- [ ] **Step 3: Implement.** In `internal/agent/loop.go`, rename `defaultContextWindow` to the
  exported `DefaultContextWindow`, which has one use, at line 389. Its comment should say the loop
  server advertises it as a model's effective window. In `harness/digest.go`, add
  `const DigestAlgorithm = "sha256"`, documented as the id GetCapabilities advertises, and have
  `DigestBytes` return `DigestAlgorithm + ":" + hex...`.
- [ ] **Step 4:** Run the same command. Expected: PASS.
- [ ] **Step 5:** `git commit -am "harness, agent: export the digest algorithm and the context-window default"`

---

### Task 3: `internal/capabilities` - answer types and listings

**Tier hint:** standard

**Files:** Create `internal/capabilities/capabilities.go` and `capabilities_test.go`.

- [ ] **Step 1: Failing tests**

```go
func TestModelsListsWhatTheServerCanRun(t *testing.T) {
	reg := model.NewRegistry("fast", map[string]model.ModelConfig{
		"fast":   {ID: "cloud/fast", MaxTokens: 4096, Persona: "coding", Label: "Fast", Description: "d", Tags: []string{"tools", "custom"}},
		"big":    {ID: "cloud/big", MaxTokens: 16384, ContextWindow: 1_000_000, Persona: "general"},
		"max":    {ID: "cli/claude-max", Persona: "coding"},
		"secret": {ID: "cloud/secret", Hidden: true},
	})
	want := []Model{
		{Alias: "big", GatewayID: "cloud/big", MaxOutputTokens: 16384, ContextWindow: 1_000_000, Persona: "general"},
		{Alias: "fast", GatewayID: "cloud/fast", MaxOutputTokens: 4096, ContextWindow: 128_000, ContextWindowIsDefault: true,
			Persona: "coding", IsDefault: true, Label: "Fast", Description: "d", Tags: []string{"tools", "custom"}},
	}
	if got := Models(reg, 128_000); !reflect.DeepEqual(got, want) {
		t.Fatalf("Models =\n %+v\nwant\n %+v", got, want)
	}
}
```

Add these tests too:

- `TestModelsDoesNotAliasRegistryTags`. Mutating a returned `Tags` element must not change the
  registry's slice.
- `TestModelsNilRegistry`. `Models(nil, 1)` is empty.
- `TestSkillsListsInstalledSkillsOncePerName`. Write two `SKILL.md` directories in a temp dir with
  the same name, plus one with another name, the way `cmd/fuse/loop_harness_test.go:harnessServer`
  writes skills, then call `skills.Load`. Assert that `Skills(set)` lists each name once, in
  discovery order, with the description `set.Lookup(name)` returns. If `skills.Load` rejects
  duplicates, assert instead that the order equals `set.All()`. Also assert `Skills(nil)` is empty.
- `TestServerInfo`. Assert that `ServerInfo()` equals
  `Server{Version: version.Version, DigestAlgorithm: harness.DigestAlgorithm}`.

- [ ] **Step 2:** Run `go test ./internal/capabilities/`. Expected: FAIL (no package).

- [ ] **Step 3: Implement**

```go
// Package capabilities is the answer a fuse loop server gives to
// LoopService.GetCapabilities (change 0086). Plain Go types only; the Connect
// edge maps them to proto. Every listing reads the source StartLoop enforces
// against, so an advertisement and a StartLoop refusal cannot disagree.
package capabilities

type Capabilities struct {
	Models           []Model
	Skills           []Skill
	Ceilings         []Ceiling
	SecretRefs       []string
	SpecMCPAuthKinds []harnessv1.McpAuthType
	Server           Server
}

type Model struct {
	Alias, GatewayID       string
	MaxOutputTokens        int  // 0: the gateway's own
	ContextWindow          int  // effective: the runtime default when the entry sets 0
	ContextWindowIsDefault bool // ContextWindow is that default
	Persona                string
	IsDefault              bool
	Label, Description     string
	Tags                   []string
}

type Skill struct{ Name, Description string }

// Direction is which way a spec may move a tighten-only value.
type Direction int

const (
	DirectionMax Direction = iota + 1 // a spec may only lower the value
	DirectionMin                      // a spec may only raise it (unused today)
)

// Ceiling is the operator floor one tighten-only spec field is held to.
// FieldPath is in harness.Violation.FieldPath vocabulary. OperatorSet false
// means a spec cannot set the field at all; Value is meaningful only when set.
type Ceiling struct {
	FieldPath   string
	OperatorSet bool
	Value       float64
	Direction   Direction
}

type Server struct{ Version, DigestAlgorithm string }

// Models lists, sorted by alias, the registry entries a server-mode loop can
// run: every entry except cli/ routes (a local interactive adapter a headless
// server cannot drive) and entries the operator hid. defaultContextWindow
// fills an entry whose ContextWindow is 0.
func Models(reg *model.Registry, defaultContextWindow int) []Model {
	if reg == nil {
		return nil
	}
	var out []Model
	for _, e := range reg.Entries() {
		mc := e.Config
		if mc.Hidden || strings.HasPrefix(mc.ID, "cli/") {
			continue
		}
		m := Model{Alias: e.Alias, GatewayID: mc.ID, MaxOutputTokens: max(mc.MaxTokens, 0), ContextWindow: mc.ContextWindow,
			Persona: mc.Persona, IsDefault: e.Alias == reg.Default, Label: mc.Label, Description: mc.Description, Tags: slices.Clone(mc.Tags)}
		if m.ContextWindow <= 0 {
			m.ContextWindow, m.ContextWindowIsDefault = defaultContextWindow, true
		}
		out = append(out, m)
	}
	return out
}
```

`Skills(set *skills.Set) []Skill` returns nil for a nil set. Otherwise it walks `set.All()` and
keeps the first skill of each name, because `Lookup`, which StartLoop uses, returns the first.
`ServerInfo()` returns `Server{version.Version, harness.DigestAlgorithm}`. Add doc comments on every
exported identifier.

- [ ] **Step 4:** Run `go test ./internal/capabilities/`. Expected: PASS.
- [ ] **Step 5:** `git add internal/capabilities && git commit -m "capabilities: answer types plus model and skill listings for GetCapabilities"`

---

### Task 4: Descriptor-derived tighten-only ceilings

**Tier hint:** premium. A ceiling that does not match enforcement fails silently.

**Files:** Create `internal/harnessresolve/ceilings.go` and `ceilings_test.go` (package
`harnessresolve`). Modify `resolve.go` (`tightenField`).

- [ ] **Step 1: Failing tests**

```go
// descriptorTightenPaths independently walks HarnessSpec for tighten_only
// fields, as dotted templates with map values entered keyless (the same walk
// as harness/options_test.go walkSpec).
func descriptorTightenPaths() []string { /* copy walkSpec from harness/options_test.go; collect paths whose FieldUI.GetTightenOnly() */ }

func ceilingBase() config.Config {
	c := config.Default()
	c.Agents.QueueBound = 2.5
	c.Agents.ToolTimeoutSeconds = 120
	c.Throughput.RequestsPerMinute = 6000
	c.Pipeline.Synthesis.MaxSteps = 20
	c.Workflows = map[string]config.WorkflowConfig{"triage": {Skill: "notes", Pool: config.PoolConfig{Concurrent: 4, Tokens: 50_000}}}
	return c
}

// templateOf: workflows["triage"].pool.concurrent -> workflows.pool.concurrent.
func templateOf(p string) string {
	if i := strings.Index(p, "["); i >= 0 {
		return p[:i] + p[strings.Index(p, "]")+1:]
	}
	return p
}

// The spec's coverage test: no tighten-only field may lack a ceiling.
func TestEveryTightenOnlyFieldHasACeiling(t *testing.T) {
	have := map[string]bool{}
	for _, c := range (Resolver{Base: ceilingBase()}).Ceilings() {
		have[templateOf(c.FieldPath)] = true
		if c.Direction != capabilities.DirectionMax {
			t.Errorf("%s direction %v", c.FieldPath, c.Direction)
		}
	}
	paths := descriptorTightenPaths()
	if len(paths) == 0 {
		t.Fatal("no tighten_only fields found")
	}
	for _, p := range paths {
		if !have[p] {
			t.Errorf("tighten_only field %s has no ceiling", p)
		}
	}
}

// Advertised path == refusal path: both read one accessor.
func TestCeilingPathsMatchRefusalPaths(t *testing.T) {
	r := Resolver{Base: ceilingBase()}
	for _, c := range r.Ceilings() {
		if path, _ := r.tightenField(configKeyFor(c.FieldPath)); path != c.FieldPath {
			t.Errorf("ceiling %s refuses at %s", c.FieldPath, path)
		}
	}
}
```

Add `TestCeilingsCarryTheOperatorValues`. Each of the following is present with `OperatorSet` true
and the given value: `agents.queue_bound` 2.5, `agents.tool_timeout_seconds` 120,
`throughput.requests_per_minute` 6000, `pipeline.synthesis.max_steps` 20,
`workflows["triage"].pool.concurrent` 4, `workflows["triage"].pool.tokens` 50000. Each of these is
present with `OperatorSet` false and `Value` 0: `throughput.session_tokens`,
`pipeline.synthesis.max_attempts`, `workflows["triage"].pool.total`. Also add
`TestCeilingsWithNoWorkflowsListNoWorkflowEntries`, which uses `config.Default()`.

- [ ] **Step 2:** Run `go test ./internal/harnessresolve/ -run Ceiling`. Expected: compile failure.

- [ ] **Step 3: Implement `ceilings.go`**

```go
// tightenTemplate is one tighten_only HarnessSpec field. mapField is the map
// it sits under ("" for none); suffix is the dotted path below the map value
// (or the whole path).
type tightenTemplate struct{ mapField, suffix string }

// tightenTemplates reads every tighten_only field from the
// (fuse.harness.v1.ui) descriptor options, never a hand list. One map level is
// supported (all the schema has); a nested map panics so the coverage test
// fails loudly instead of advertising nothing.
func tightenTemplates() []tightenTemplate {
	var out []tightenTemplate
	seen := map[protoreflect.FullName]bool{}
	var walk func(mapField, prefix string, md protoreflect.MessageDescriptor)
	walk = func(mapField, prefix string, md protoreflect.MessageDescriptor) {
		if seen[md.FullName()] {
			return
		}
		seen[md.FullName()] = true
		for i := 0; i < md.Fields().Len(); i++ {
			fd := md.Fields().Get(i)
			path := prefix + string(fd.Name())
			if o := fd.Options(); o != nil && proto.HasExtension(o, harnessv1.E_Ui) &&
				proto.GetExtension(o, harnessv1.E_Ui).(*harnessv1.FieldUI).GetTightenOnly() {
				out = append(out, tightenTemplate{mapField, path})
			}
			switch {
			case fd.IsMap() && fd.MapValue().Message() != nil:
				if mapField != "" {
					panic("harnessresolve: nested map under " + mapField)
				}
				walk(path, "", fd.MapValue().Message())
			case !fd.IsMap() && fd.Message() != nil:
				walk(mapField, path+".", fd.Message())
			}
		}
	}
	walk("", "", (&harnessv1.HarnessSpec{}).ProtoReflect().Descriptor())
	return out
}

// Ceilings is the tighten-only floor a spec resolved over r.Base is held to:
// one entry per tighten_only field, and per operator-defined key for a field
// under a map (a key the operator does not define is the client's, so it has
// no ceiling). Values come from operatorLimit, which tightenField words its
// refusal with.
func (r Resolver) Ceilings() []capabilities.Ceiling {
	var out []capabilities.Ceiling
	add := func(key, path string) {
		v, set, known := r.operatorLimit(key)
		if !known {
			panic("harnessresolve: no operator value for tighten_only key " + key)
		}
		c := capabilities.Ceiling{FieldPath: path, OperatorSet: set, Direction: capabilities.DirectionMax}
		if set {
			c.Value = v
		}
		out = append(out, c)
	}
	for _, t := range tightenTemplates() {
		if t.mapField == "" {
			add(t.suffix, t.suffix)
			continue
		}
		for _, k := range r.operatorMapKeys(t.mapField) {
			add(t.mapField+"."+k+"."+t.suffix, fmt.Sprintf("%s[%s].%s", t.mapField, strconv.Quote(k), t.suffix))
		}
	}
	return out
}
```

Then write these helpers.

- `operatorMapKeys(mapField string) []string`. For `"workflows"`, return the sorted keys of
  `r.Base.Workflows`. Panic on any other map.
- `operatorLimit(key string) (v float64, set, known bool)`. It covers the same keys as today's
  `tightenField` switch.
  - `workflows.<name>.pool.{concurrent,total,max_depth,tokens}`. Split at
    `strings.LastIndex(rest, ".pool.")`.
  - `agents.queue_bound`. A float, set when greater than 0.
  - `agents.tool_timeout_seconds`.
  - `throughput.{requests_per_minute,tokens_per_minute,session_tokens}`.
  - `pipeline.synthesis.{max_steps,max_fanout,max_depth,max_attempts}`.

  Integer fields are set when greater than 0. Any other key returns `known == false`.
- `configKeyFor(fieldPath string) string`. It maps `workflows["x"].pool.f` to `workflows.x.pool.f`
  with `strconv.Unquote`. Any other path is returned unchanged.

In `resolve.go`, rewrite `tightenField` so it reads the value through `operatorLimit`. Keep the
returned paths exactly as they are: `workflows[%q].pool.%s` for workflows, and
`harnessconfig.SpecPath(key)` otherwise.

```go
	if v, set, _ := r.operatorLimit(key); set {
		limit = strconv.FormatFloat(v, 'f', -1, 64) // 'f': 2000000 must not render as 2e+06
	}
```

- [ ] **Step 4:** Run `go test ./internal/harnessresolve/ ./cmd/fuse/ -run 'Ceiling|Tighten|Harness'`.
  Expected: PASS, including the existing "may only tighten this server's limit of 100" assertions.
- [ ] **Step 5:** `git add internal/harnessresolve && git commit -m "harnessresolve: derive tighten-only ceilings from the descriptor options"`

---

### Task 5: Spec MCP auth kinds and tenant-filtered secret names

**Tier hint:** premium. The secret listing is a disclosure boundary.

**Files:** Create `internal/harnessresolve/advertise.go` and `advertise_test.go`. Modify
`secrets.go`, `resolve.go` (`operatorOnlyProblems`), and `internal/harnessconfig/harnessconfig.go`.

- [ ] **Step 1: Failing tests**

```go
func TestSpecMCPAuthKindsAreTheOnesTheLayeringAccepts(t *testing.T) {
	want := []harnessv1.McpAuthType{harnessv1.McpAuthType_MCP_AUTH_TYPE_UNSPECIFIED, harnessv1.McpAuthType_MCP_AUTH_TYPE_BEARER, harnessv1.McpAuthType_MCP_AUTH_TYPE_STATIC}
	if got := SpecMCPAuthKinds(); !reflect.DeepEqual(got, want) {
		t.Fatalf("SpecMCPAuthKinds = %v", got)
	}
	for _, k := range want {
		if s, ok := harnessconfig.AuthTypeToConfig(k); !ok || !config.SpecMCPAuthTypes[s] {
			t.Errorf("advertised %v maps to %q, which the overlay drops", k, s)
		}
	}
	vals := harnessv1.McpAuthType(0).Descriptor().Values()
	for i := 0; i < vals.Len(); i++ {
		k := harnessv1.McpAuthType(vals.Get(i).Number())
		spec := &harnessv1.HarnessSpec{McpServers: []*harnessv1.McpServer{{Auth: &harnessv1.McpAuth{Type: k}}}}
		if refused := len(operatorOnlyProblems(spec)) > 0; specAuthAllowed(k) == refused {
			t.Errorf("%v: advertised=%v refused=%v", k, specAuthAllowed(k), refused)
		}
	}
}

// The listing advertises for a tenant exactly what ResolveSecret resolves
// for it (unrestricted destination).
func TestSecretRefsAgreeWithResolveSecret(t *testing.T) {
	t.Setenv("FUSE_T_A", "a")
	entries := []config.SecretSourceConfig{{Name: "a", Env: "FUSE_T_A", Tenants: []string{"acme"}}, {Name: "b", Env: "FUSE_T_A"}, {Name: "c", Env: "FUSE_T_NOPE"}}
	s := ConfigSecrets(entries)
	for _, tenant := range []string{"", "acme", "other"} {
		listed := map[string]bool{}
		for _, n := range (Resolver{Secrets: s}).SecretRefs(context.Background(), tenant) {
			listed[n] = true
		}
		for _, e := range entries {
			_, err := s.ResolveSecret(context.Background(), tenant, e.Name, "https://anywhere.example.com")
			if (err == nil) != listed[e.Name] {
				t.Errorf("tenant %q %q: resolves=%v listed=%v", tenant, e.Name, err == nil, listed[e.Name])
			}
		}
	}
}
```

Add `TestSecretRefsFilterByTenant`, using env vars `FUSE_T_DEFAULT=v1`, `FUSE_T_ACME=v2`, and
`FUSE_T_EMPTY=""`, plus a temp file containing `v3\n`. Use these entries:

| Entry | Source and rules |
|---|---|
| `zeta` | `FUSE_T_DEFAULT`, no tenants (default tenant only) |
| `acme-key` | `FUSE_T_ACME`, tenants `[acme]` |
| `both` | the file, tenants `[acme, _default]` |
| `pinned` | `FUSE_T_DEFAULT`, `Hosts: [api.example.com]` |
| `unset` | `FUSE_T_EMPTY` |
| `gone` | a missing file |

Expected results:

| Tenant | `SecretRefs` |
|---|---|
| `""` | `[both pinned zeta]` |
| `"_default"` | `[both pinned zeta]` |
| `"acme"` | `[acme-key both]` |
| `"other"` | empty |

Also add `TestSecretRefsWithoutAListerOrResolver`. `Resolver{}` lists nothing, and a resolver that
implements only `ResolveSecret` lists nothing.

- [ ] **Step 2:** Run `go test ./internal/harnessresolve/ -run 'SpecMCPAuthKinds|SecretRefs'`.
  Expected: compile failure.

- [ ] **Step 3: Implement**

In `harnessconfig.go`, next to `authToConfig`:

```go
// AuthTypeToConfig is the config auth string for an MCP auth enum value: ""
// (none) for UNSPECIFIED, the mapped name otherwise; ok false if unmapped.
func AuthTypeToConfig(t harnessv1.McpAuthType) (string, bool) {
	if t == harnessv1.McpAuthType_MCP_AUTH_TYPE_UNSPECIFIED {
		return "", true
	}
	s, ok := authToConfig[t]
	return s, ok
}
```

`advertise.go`:

```go
// specAuthAllowed is the single predicate behind operatorOnlyProblems (the
// refusal) and SpecMCPAuthKinds (the advertisement).
func specAuthAllowed(t harnessv1.McpAuthType) bool {
	s, ok := harnessconfig.AuthTypeToConfig(t)
	return ok && config.SpecMCPAuthTypes[s]
}

// SpecMCPAuthKinds lists, in enum order, the MCP auth types a spec may use.
// UNSPECIFIED is listed: it means none.
func SpecMCPAuthKinds() []harnessv1.McpAuthType { /* iterate McpAuthType(0).Descriptor().Values(), keep specAuthAllowed */ }

// SecretLister is a SecretResolver that can list the names it would resolve
// for a tenant: names only, never values or sources.
type SecretLister interface {
	SecretNames(ctx context.Context, tenant string) []string
}

// SecretRefs lists, sorted, the secret_ref names that resolve for tenant. A
// resolver that cannot list (or none) lists nothing: deny by default.
func (r Resolver) SecretRefs(ctx context.Context, tenant string) []string {
	l, ok := r.Secrets.(SecretLister)
	if !ok {
		return nil
	}
	names := l.SecretNames(ctx, tenant)
	sort.Strings(names)
	return names
}
```

In `resolve.go`, change the condition in `operatorOnlyProblems` to `if !specAuthAllowed(t)` and keep
its message, path, and rule.

In `secrets.go`, move the tenant check and the source read out of `ResolveSecret`, unchanged, into
`available`:

```go
// available is the tenant rule and source read every resolution applies.
// ResolveSecret adds the destination-host rule on top; SecretNames lists by
// this rule alone, so a host-restricted name is listed but still resolves only
// for a destination on its hosts list.
func available(e config.SecretSourceConfig, tenant string) (string, bool) {
	t := string(event.NormalizeTenant(event.TenantID(tenant)))
	allowed := e.Tenants
	if len(allowed) == 0 {
		allowed = []string{string(event.DefaultTenant)}
	}
	if !slices.Contains(allowed, t) {
		return "", false
	}
	v := os.Getenv(e.Env)
	if e.Env == "" {
		b, err := os.ReadFile(e.File)
		if err != nil {
			return "", false
		}
		v = strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	}
	return v, v != ""
}
```

After the change, `ResolveSecret` does three things in order: name lookup, then
`available(e, tenant)`, then the existing hosts check. Every failure returns `ErrSecretUnavailable`,
and no error ever carries a value. `SecretNames` iterates `byName` and keeps the names that
`available` accepts.

- [ ] **Step 4:** Run `go test ./internal/harnessresolve/ ./internal/harnessconfig/ ./cmd/fuse/ -run 'Secret|Auth|Harness|OperatorOnly'`.
  Expected: PASS.
- [ ] **Step 5:** `git add internal/harnessresolve internal/harnessconfig && git commit -m "harnessresolve: list spec MCP auth kinds and a tenant's secret names"`

---

### Task 6: `GetCapabilities` wire contract and regenerated stubs

**Tier hint:** standard

- [ ] **Step 1: Failing test.** Create `internal/loopconnect/capabilities_test.go`:

```go
// With no capability source wired, the RPC exists and answers Unimplemented,
// never an empty but plausible answer.
func TestGetCapabilitiesWithoutASourceIsUnimplemented(t *testing.T) {
	_, err := newTestServer(t, &fakeRuntime{}).GetCapabilities(context.Background(), connect.NewRequest(&loopv1.GetCapabilitiesRequest{}))
	if got := connect.CodeOf(err); got != connect.CodeUnimplemented {
		t.Fatalf("code = %v, want Unimplemented", got)
	}
}
```

- [ ] **Step 2:** Run `go test ./internal/loopconnect/ -run GetCapabilities`. Expected: compile failure.

- [ ] **Step 3: Edit `proto/fuse/loop/v1/loop.proto`.** Add
  `import "google/protobuf/struct.proto";` and the RPC
  `rpc GetCapabilities(GetCapabilitiesRequest) returns (GetCapabilitiesResponse);`. Document it:
  read-only, same auth as StartLoop, `Unimplemented` when the server has no capability source.
  Then add the messages:

```proto
message GetCapabilitiesRequest {
  // Same pass-through as StartLoopRequest.tenant: when set it must equal the
  // authenticated tenant (else PermissionDenied). The answer is always filtered
  // by the authenticated tenant, never by this field.
  string tenant = 1;
}

message GetCapabilitiesResponse {
  // Models a server-mode loop can run, by alias. cli/ routes and entries the
  // operator hid (advertise: false) are left out.
  repeated ModelInfo models = 1;
  // Skills a spec's skills[].name may reference.
  repeated SkillInfo skills = 2;
  // One per tighten-only HarnessSpec field; for a field under a map
  // (workflows), one per key the operator defines.
  repeated Ceiling ceilings = 3;
  // Secret names that resolve for the caller's tenant: names only. A name the
  // operator restricted to hosts still resolves only for those hosts.
  repeated string secret_refs = 4;
  // Auth types a spec-declared MCP server may use; UNSPECIFIED means none.
  repeated fuse.harness.v1.McpAuthType spec_mcp_auth_kinds = 5;
  ServerInfo server = 6;
}

message ModelInfo {
  string alias = 1;
  string gateway_id = 2;
  uint32 max_output_tokens = 3; // 0: left to the gateway
  // Effective window: the runtime default when the registry entry sets 0,
  // with context_window_is_default set.
  uint32 context_window = 4;
  bool context_window_is_default = 5;
  string persona = 6;
  bool is_default = 7;
  // Optional operator metadata. Well-known tags: tools, vision, reasoning,
  // long-context; other strings pass through.
  string label = 8;
  string description = 9;
  repeated string tags = 10;
}

message SkillInfo {
  string name = 1;
  string description = 2;
}

enum CeilingDirection {
  CEILING_DIRECTION_UNSPECIFIED = 0;
  CEILING_DIRECTION_MAX = 1; // a spec may only lower the value
  CEILING_DIRECTION_MIN = 2; // a spec may only raise the value
}

message Ceiling {
  // fuse.harness.v1.Violation.field_path vocabulary.
  string field_path = 1;
  // false: the operator left it unset, so a spec cannot set it at all.
  bool operator_set = 2;
  // The operator's effective value; present only when operator_set.
  google.protobuf.Value value = 3;
  CeilingDirection direction = 4;
}

message ServerInfo {
  string version = 1;
  // Algorithm id StartLoopResponse.harness_digest is prefixed with ("sha256").
  string digest_algorithm = 2;
}
```

  Also update the file's top comment to mention GetCapabilities.

- [ ] **Step 4: Regenerate.** Run `cd proto && npm ci && cd .. && PATH="$HOME/go/bin:$PATH" make proto`.
  Expected: `buf lint` is clean, and the diff covers only `internal/loopwire/v1/loop.pb.go`,
  `internal/loopwire/v1/loopv1connect/loop.connect.go`, and
  `proto/gen/ts/fuse/loop/v1/loop_pb.ts`. `proto/node_modules` must not be staged; check
  `git status`. If lint objects to a name, rename it to satisfy STANDARD and record the rename in the
  commit message.

- [ ] **Step 5: Placeholder.** In `internal/loopconnect/capabilities.go`, add
  `func (h *Handler) GetCapabilities(...)`. It returns
  `connect.NewError(connect.CodeUnimplemented, errors.New("loopconnect: capabilities are not advertised by this server"))`.
  The generated interface is then satisfied. Every `LoopServiceHandler` in the repo wraps
  `loopconnect.Handler` (`cmd/fuse`, `control_socket.go`, `sdk/ts/test/server`,
  `sdk/ts/test/statesrv`).

- [ ] **Step 6:** Run `go build ./... && go vet ./... && go test ./internal/loopconnect/ ./sdk/... ./cmd/fuse/ -run 'GetCapabilities|LoopServeNet|Remote'`.
  Expected: PASS.
- [ ] **Step 7:** `git add proto/fuse internal/loopwire proto/gen/ts internal/loopconnect/capabilities*.go && git commit -m "proto: LoopService.GetCapabilities wire contract"`

---

### Task 7: `GetCapabilities` at the Connect edge

**Tier hint:** premium. This is the auth and tenant boundary.

**Files:** `internal/loopconnect/capabilities.go`, `handler.go`, `capabilities_test.go`.

- [ ] **Step 1: Failing tests** (append)

```go
type fakeCaps struct {
	saw    event.TenantID
	calls  int
	answer capabilities.Capabilities
	err    error
}

func (f *fakeCaps) Capabilities(_ context.Context, tenant event.TenantID) (capabilities.Capabilities, error) {
	f.calls++
	f.saw = tenant
	return f.answer, f.err
}

func capsServer(t *testing.T, src CapabilitySource, v loopauth.Verifier) loopv1connect.LoopServiceClient {
	t.Helper()
	var opts []connect.HandlerOption
	if v != nil {
		opts = append(opts, connect.WithInterceptors(NewAuthInterceptor(v)))
	}
	_, h := loopv1connect.NewLoopServiceHandler(NewHandler(&fakeRuntime{}).WithCapabilities(src), opts...)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return loopv1connect.NewLoopServiceClient(srv.Client(), srv.URL)
}

func TestGetCapabilitiesTenantSpoofIsPermissionDenied(t *testing.T) {
	src := &fakeCaps{}
	_, err := capsServer(t, src, testVerifier()).GetCapabilities(context.Background(),
		bearer(connect.NewRequest(&loopv1.GetCapabilitiesRequest{Tenant: "other"}), testToken))
	if connect.CodeOf(err) != connect.CodePermissionDenied || src.calls != 0 {
		t.Fatalf("code = %v, source calls = %d; want PermissionDenied and no call", connect.CodeOf(err), src.calls)
	}
}
```

Add these tests too:

- `TestGetCapabilitiesMapsEveryField`. Feed a `fakeCaps` answer that sets every field: one model
  with all fields, one skill, an operator-set ceiling with `Value: 2.5` (MAX), an unset ceiling,
  one secret ref, two auth kinds, and server info. `proto.Equal` the response against the expected
  `loopv1.GetCapabilitiesResponse`. The set ceiling carries `structpb.NewNumberValue(2.5)`, the
  unset ceiling has `Value` nil, and the direction is `CEILING_DIRECTION_MAX`.
- `TestGetCapabilitiesFiltersByThePrincipalsTenant`. `testToken` maps to tenant `acme`. Call with no
  wire tenant and assert the source saw `acme`. Then call with `Tenant: "acme"` and assert success.
- `TestGetCapabilitiesUnauthenticated`. Missing and invalid tokens both return `Unauthenticated`,
  and the source is never called.
- `TestGetCapabilitiesSourceErrorIsInternal`.

- [ ] **Step 2:** Run `go test ./internal/loopconnect/ -run GetCapabilities`. Expected: compile failure.

- [ ] **Step 3: Implement.** Add `caps CapabilitySource` to `Handler`, with a comment: nil means
  Unimplemented, and it is injected by `WithCapabilities`. Then add:

```go
// CapabilitySource answers GetCapabilities for one tenant (change 0086). The
// tenant is always the authenticated one; implementations filter the
// tenant-scoped parts (secret refs) by it.
type CapabilitySource interface {
	Capabilities(ctx context.Context, tenant event.TenantID) (capabilities.Capabilities, error)
}

// WithCapabilities injects the source; nil keeps Unimplemented. Chainable.
func (h *Handler) WithCapabilities(s CapabilitySource) *Handler {
	if s != nil {
		h.caps = s
	}
	return h
}

// GetCapabilities: the interceptor has already refused a missing or invalid
// token (Unauthenticated); a wire tenant that mismatches the principal is a
// spoof (PermissionDenied), exactly as for StartLoop.
func (h *Handler) GetCapabilities(ctx context.Context, req *connect.Request[loopv1.GetCapabilitiesRequest]) (*connect.Response[loopv1.GetCapabilitiesResponse], error) {
	ctx, apiSpan := h.observer.Start(ctx, observe.Descriptor{Kind: observe.OperationAPIRequest, Name: "get_capabilities"})
	apiOutcome := observe.OutcomeSuccess
	defer func() { apiSpan.End(apiOutcome) }()
	fail := func(err *connect.Error) (*connect.Response[loopv1.GetCapabilitiesResponse], error) {
		apiOutcome = observeotel.OutcomeFromError(ctx, err)
		return nil, err
	}
	tenant, _, _, aerr := h.effectiveTenant(ctx, req.Msg.Tenant)
	if aerr != nil {
		return fail(aerr)
	}
	if h.caps == nil {
		return fail(connect.NewError(connect.CodeUnimplemented, errors.New("loopconnect: capabilities are not advertised by this server")))
	}
	c, err := h.caps.Capabilities(ctx, tenant)
	if err != nil {
		return fail(connect.NewError(connect.CodeInternal, err))
	}
	resp := connect.NewResponse(toProtoCapabilities(c))
	if id := observeotel.TraceID(ctx); id != "" {
		resp.Header().Set("Fuse-Trace-Id", id)
	}
	return resp, nil
}
```

`toProtoCapabilities(c capabilities.Capabilities) *loopv1.GetCapabilitiesResponse` is the only
mapping. It copies slices, so the caller's slices are never aliased. It clamps int to uint32
(negative becomes 0, and anything above max uint32 becomes max uint32). It sets
`Value: structpb.NewNumberValue(v)` only when `OperatorSet` is true. It maps
`DirectionMax/Min` to `CEILING_DIRECTION_MAX/MIN` and anything else to UNSPECIFIED. Update the
package doc to list `GetCapabilities` with the unary RPCs.

- [ ] **Step 4:** Run `go test ./internal/loopconnect/`. Expected: PASS.
- [ ] **Step 5:** `git commit -am "loopconnect: serve GetCapabilities with StartLoop's authentication"`

---

### Task 8: Production capability source and the golden consistency test

**Tier hint:** premium. This task carries the spec's central guarantee.

**Files:** Modify `cmd/fuse/loop_harness.go`. Create `cmd/fuse/loop_capabilities_test.go`.

- [ ] **Step 1: Failing tests**

```go
// Some ceilings set, some unset, an operator workflow, a hidden model, and two
// secrets: one for the default tenant and one for acme.
const capsOperator = `
models:
  default: fast
  fast: {id: cloud/fast, label: Fast, tags: [tools]}
  house: {id: cloud/house}
  quiet: {id: cloud/quiet, advertise: false}
max_turns: 2
agents: {queue_bound: 4, tool_timeout_seconds: 120}
throughput: {requests_per_minute: 6000, tokens_per_minute: 900000}
pipeline: {synthesis: {max_steps: 20, max_fanout: 8}}
workflows:
  triage: {skill: notes, pool: {concurrent: 4, total: 16}}
loop_server:
  secrets:
    - {name: search-key, env: FUSE_TEST_SEARCH_KEY}
    - {name: acme-key, env: FUSE_TEST_ACME_KEY, tenants: [acme]}
`

func capsHost(t *testing.T) (runtime.Runtime, *loopServerHost) {
	t.Helper()
	t.Setenv("FUSE_TEST_SEARCH_KEY", "search-value")
	t.Setenv("FUSE_TEST_ACME_KEY", "acme-value")
	rt, _, host := harnessServer(t, capsOperator) // production deps, scripted gateway
	return rt, host
}

// setSpecNumber sets the numeric field at a ceiling path (dotted, with
// workflows["name"] segments), creating parents.
func setSpecNumber(t *testing.T, spec *harnessv1.HarnessSpec, path string, v float64) {
	t.Helper()
	m := spec.ProtoReflect()
	segs := strings.Split(path, ".")
	for i, seg := range segs {
		name, key, isMap := seg, "", false
		if j := strings.Index(seg, "["); j >= 0 {
			k, err := strconv.Unquote(seg[j+1 : len(seg)-1])
			if err != nil {
				t.Fatal(err)
			}
			name, key, isMap = seg[:j], k, true
		}
		fd := m.Descriptor().Fields().ByName(protoreflect.Name(name))
		if fd == nil {
			t.Fatalf("path %q: no field %q", path, name)
		}
		switch {
		case isMap:
			m = m.Mutable(fd).Map().Mutable(protoreflect.ValueOfString(key).MapKey()).Message()
		case i < len(segs)-1:
			m = m.Mutable(fd).Message()
		case fd.Kind() == protoreflect.DoubleKind:
			m.Set(fd, protoreflect.ValueOfFloat64(v))
		case fd.Kind() == protoreflect.Uint32Kind:
			m.Set(fd, protoreflect.ValueOfUint32(uint32(v)))
		default:
			t.Fatalf("path %q: kind %v", path, fd.Kind())
		}
	}
}

func violationAt(t *testing.T, err error, path string) harness.Violation {
	t.Helper()
	var ve *harness.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want a violation at %s", err, path)
	}
	for _, v := range ve.Violations {
		if v.FieldPath == path {
			return v
		}
	}
	t.Fatalf("violations %v have none at %s", ve.Violations, path)
	return harness.Violation{}
}

// The golden consistency test (change 0086): the production host's
// advertisement against what the production runtime's StartLoop does.
func TestCapabilitiesAgreeWithStartLoop(t *testing.T) {
	rt, host := capsHost(t)
	ctx := context.Background()
	c, err := host.Capabilities(ctx, event.DefaultTenant)
	if err != nil || len(c.SecretRefs) == 0 {
		t.Fatalf("Capabilities = %+v, %v", c, err)
	}
	// The pre-flighted spec: every operator-set ceiling tightened, every
	// advertised skill named, an advertised secret, an advertised model.
	turns := uint32(1)
	base := &harnessv1.HarnessSpec{MaxTurns: &turns, Models: &harnessv1.Models{DefaultAlias: "house"},
		Research: &harnessv1.Research{Provider: harnessv1.ResearchProvider_RESEARCH_PROVIDER_CUSTOM,
			Custom: &harnessv1.CustomSearchProvider{Url: "https://search.example.com/search", Headers: map[string]*harnessv1.HeaderValue{
				"X-Api-Key": {Value: &harnessv1.HeaderValue_SecretRef{SecretRef: c.SecretRefs[0]}}}}}}
	for _, ce := range c.Ceilings {
		if ce.OperatorSet {
			setSpecNumber(t, base, ce.FieldPath, math.Max(1, math.Floor(ce.Value/2)))
		}
	}
	for _, s := range c.Skills {
		base.Skills = append(base.Skills, &harnessv1.SkillRef{Name: s.Name})
	}
	clone := func() *harnessv1.HarnessSpec { return proto.Clone(base).(*harnessv1.HarnessSpec) }
	start := func(spec *harnessv1.HarnessSpec) error {
		_, err := rt.StartLoop(ctx, runtime.LoopConfig{Task: "go", Harness: spec})
		return err
	}

	h, err := rt.StartLoop(ctx, runtime.LoopConfig{Task: "go", Harness: clone()})
	if err != nil {
		t.Fatalf("StartLoop refused the pre-flighted spec: %v", err)
	}
	_, _ = h.Wait()

	for _, ce := range c.Ceilings {
		spec := clone()
		over := 1.0 // unset by the operator: any positive value is refused
		if ce.OperatorSet {
			over = ce.Value + 1
		}
		setSpecNumber(t, spec, ce.FieldPath, over)
		if v := violationAt(t, start(spec), ce.FieldPath); v.RuleID != harness.RuleTightenOnly {
			t.Errorf("%s: rule %s", ce.FieldPath, v.RuleID)
		}
	}

	spec := clone()
	spec.Skills = []*harnessv1.SkillRef{{Name: "not-installed"}}
	violationAt(t, start(spec), "skills[0].name")

	spec = clone()
	spec.Research.Custom.Headers["X-Api-Key"] = &harnessv1.HeaderValue{Value: &harnessv1.HeaderValue_SecretRef{SecretRef: "acme-key"}}
	violationAt(t, start(spec), `research.custom.headers["X-Api-Key"].secret_ref`)

	advertised := map[harnessv1.McpAuthType]bool{}
	for _, k := range c.SpecMCPAuthKinds {
		advertised[k] = true
	}
	vals := harnessv1.McpAuthType(0).Descriptor().Values()
	for i := 0; i < vals.Len(); i++ {
		k := harnessv1.McpAuthType(vals.Get(i).Number())
		spec := clone()
		auth := &harnessv1.McpAuth{Type: k}
		if k == harnessv1.McpAuthType_MCP_AUTH_TYPE_BEARER || k == harnessv1.McpAuthType_MCP_AUTH_TYPE_STATIC {
			auth.ClientSecretRef = c.SecretRefs[0]
		}
		spec.McpServers = []*harnessv1.McpServer{{Name: "s", Transport: harnessv1.McpTransport_MCP_TRANSPORT_HTTP,
			Url: "https://mcp.example.com", Audience: "https://mcp.example.com", Auth: auth}}
		if advertised[k] {
			// Acceptance through the resolver StartLoop calls, without building
			// a loop that would dial the MCP server.
			if _, err := host.ResolveHarness(ctx, event.DefaultTenant, spec); err != nil {
				t.Errorf("advertised auth %v refused: %v", k, err)
			}
			continue
		}
		if v := violationAt(t, start(spec), "mcp_servers[0].auth.type"); v.RuleID != harness.RuleOperatorOnly {
			t.Errorf("%v: rule %s", k, v.RuleID)
		}
	}
}
```

Also add `TestLoopServerHostAdvertisesItsCapabilities`, against the same `capsHost`. It asserts:

- `fast` is listed with `IsDefault`, `Label: "Fast"`, and `ContextWindowIsDefault`.
- `quiet` and `claude-max` (the built-in `cli/` route) are absent.
- The skills include `notes` and `research`.
- The default tenant's `SecretRefs` is `[search-key]`, and `acme` gets `[acme-key]`.
- `Server.DigestAlgorithm == harness.DigestAlgorithm` and `Version` is not empty.

**Fixture rule:** if shape or semantic validation flags a fixture value in the "starts" case (for
example, an operator workflow that needs a worker, or `static` auth that needs `client_id`), fix the
fixture. Never loosen an assertion or skip a ceiling. A real disagreement between the advertisement
and StartLoop is a bug in Tasks 3 to 7, so fix it there. `harnessServer`'s gateway answers every
call with a `skill` tool call, and `max_turns: 1` ends the loop.

- [ ] **Step 2:** Run `go test ./cmd/fuse/ -run 'Capabilities'`. Expected: compile failure.

- [ ] **Step 3: Implement** in `loop_harness.go`:

```go
// resolver is the one Resolver this server layers specs with: ResolveHarness
// enforces through it and Capabilities advertises from it.
func (h *loopServerHost) resolver() harnessresolve.Resolver {
	r := harnessresolve.Resolver{Base: h.operator.cfg, Secrets: harnessresolve.ConfigSecrets(h.operator.cfg.LoopServer.Secrets)}
	if h.skills != nil {
		r.Skills = func(name string) bool { _, ok := h.skills.Lookup(name); return ok }
	}
	return r
}

// Capabilities implements loopconnect.CapabilitySource (change 0086). Every
// part reads what StartLoop enforces against: the operator registry, the
// installed skill set and resolver().
func (h *loopServerHost) Capabilities(ctx context.Context, tenant event.TenantID) (capabilities.Capabilities, error) {
	r := h.resolver()
	return capabilities.Capabilities{
		Models:           capabilities.Models(h.operator.reg, agent.DefaultContextWindow),
		Skills:           capabilities.Skills(h.skills),
		Ceilings:         r.Ceilings(),
		SecretRefs:       r.SecretRefs(ctx, string(tenant)),
		SpecMCPAuthKinds: harnessresolve.SpecMCPAuthKinds(),
		Server:           capabilities.ServerInfo(),
	}, nil
}

var _ loopconnect.CapabilitySource = (*loopServerHost)(nil)
```

Change `ResolveHarness` to call `h.resolver().Resolve(...)` and keep its doc comment.

- [ ] **Step 4:** Run `go test ./cmd/fuse/ -run 'Capabilities|Harness'`. Expected: PASS.
- [ ] **Step 5:** `git add cmd/fuse && git commit -m "loop server: advertise capabilities from the resolver StartLoop enforces with"`

---

### Task 9: Wire the source into `loop-serve-net`

**Tier hint:** standard

- [ ] **Step 1: Failing tests** in `cmd/fuse/loop_capabilities_test.go`:
  - `TestLoopCapabilitySourceFromProductionDeps`. `loopCapabilitySource(runtime.Deps{Harness: host})`
    returns the source, and `loopCapabilitySource(runtime.Deps{})` returns an error.
  - `TestServeNetMountsTheCapabilitySource`. Start
    `serveNetWithOptions(ctx, ln, rt, nil, nil, nil, nil, serveNetOptions{capabilities: host})` on
    `127.0.0.1:0`, poll `GetCapabilities` with a plain client for up to 2s, then assert non-empty
    `Models` and `Ceilings`. Cancel and wait in `t.Cleanup`.
- [ ] **Step 2:** Run `go test ./cmd/fuse/ -run 'CapabilitySource'`. Expected: compile failure.
- [ ] **Step 3: Implement** in `loop_serve_net.go`:
  - Add `capabilities loopconnect.CapabilitySource` to `serveNetOptions`. Comment: runLoopServeNet
    always sets it, and nil leaves the RPC Unimplemented.
  - In `serveNetWithOptions`, call `handler.WithCapabilities(sopts.capabilities)`.
  - Add a helper:

```go
// loopCapabilitySource is the GetCapabilities source for a loop server built
// from deps: its harness host. A deps without one is a wiring bug, refused at
// startup rather than served as an RPC that always answers Unimplemented.
func loopCapabilitySource(deps runtime.Deps) (loopconnect.CapabilitySource, error) {
	if src, ok := deps.Harness.(loopconnect.CapabilitySource); ok && src != nil {
		return src, nil
	}
	return nil, errors.New("loop server deps carry no capability source")
}
```

  - In `runLoopServeNet`, right after `buildLoopServerRuntimeDepsStrict` succeeds: if
    `loopCapabilitySource(deps)` fails, call `shutdownObservability()`, print
    `loop-serve-net: <err>`, and return 1. Pass `capabilities: caps` in the `serveNetOptions`
    literal.
  - Usage text, after the harness paragraph: four lines saying that GetCapabilities lists, for the
    caller's tenant, the routed models, installed skills, tighten-only ceilings, resolvable
    secret_ref names, and spec MCP auth kinds; that `advertise: false` on `models.<alias>` hides a
    model; and that `label`, `description`, and `tags` are shown.
- [ ] **Step 4:** Run `go test ./cmd/fuse/ -run 'CapabilitySource|ServeNet|LoopServeNet'`. Expected:
  PASS. `TestLoopServeNetDispatchRegistered` now passes through the startup check.
- [ ] **Step 5:** `git commit -am "loop-serve-net: serve GetCapabilities from the production harness host"`

---

### Task 10: Go SDK `Client.Capabilities`, plus the end-to-end test over h2c

**Tier hint:** standard

**Files:** Create `sdk/fuse/capabilities.go` and `sdk/fuse/capabilities_test.go`. Modify
`sdk/fuse/fuse.go`, `remote.go`, and `local.go`. Create
`cmd/fuse/loop_serve_net_capabilities_test.go`.

- [ ] **Step 1: Failing tests**

`sdk/fuse/capabilities_test.go`. Use a `sdkCaps` source that records the tenant it saw and returns a
fully populated `capabilities.Capabilities`. Mount it with
`loopconnect.NewHandler(&fakeRuntime{}).WithKeepalive(time.Hour).WithCapabilities(src)` behind
`NewAuthInterceptor(devVerifier())`. `devVerifier` maps `good-tok` to `_default` and `acme-tok` to
`acme`.

- `TestRemoteCapabilities`. With `Credentials{Token: "acme-tok", Tenant: "acme"}`, the source saw
  `acme`, and the result `reflect.DeepEqual`s the expected public `Capabilities`. Every field maps;
  a set ceiling carries `Value: 2.5`, an unset one `Value: 0`; directions are `CeilingMax`. Empty
  slices are nil on both sides.
- `TestRemoteCapabilitiesErrors`. A bad token gives `ErrUnauthenticated`. A token for `acme` with
  `Tenant: "other"` gives `ErrPermissionDenied`. A server built with `WithCapabilities(nil)` (pass
  the untyped nil) gives `ErrCapabilitiesUnsupported`.
- `TestLocalCapabilitiesUnsupported`. `NewLocal(...).Capabilities` gives
  `ErrCapabilitiesUnsupported`.

`cmd/fuse/loop_serve_net_capabilities_test.go` runs the real command, the way
`observability_acceptance_test.go` does:

```go
func h2cClient() *http.Client {
	return &http.Client{Transport: &http2.Transport{AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}}}
}
```

`TestLoopServeNetAdvertisesCapabilitiesOverH2C` sets up the environment:

- A scripted `httptest` gateway that answers `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`.
- `HOME` set to a temp dir, with `LLM_GATEWAY_URL` and `LLM_GATEWAY_KEY` pointing at the scripted
  gateway and `FUSE_TEST_CAPS_KEY=v`.
- `$HOME/.fuse/config.yml` with:
  - models `fast` (`label: Fast`) and `quiet` (`advertise: false`)
  - `throughput: {requests_per_minute: 6000}`
  - `loop_server.auth` tokens `tok-a` (tenant `tenant-a`) and `tok-b` (tenant `tenant-b`)
  - `loop_server.secrets` entry `{name: caps-key, env: FUSE_TEST_CAPS_KEY, tenants: [tenant-a]}`
- The `netListen` and `serveNetContext` seams swapped, and
  `run([]string{"loop-serve-net", "--addr", addr}, ...)` running in a goroutine. Cleanup cancels the
  context, waits up to 5s for exit, and restores the seams.

The test then asserts:

- A raw `loopv1connect` client over `h2cClient()` with bearer `tok-a`, polled for up to 3s, gets
  `fast` listed and `quiet` and `claude-max` absent, with `SecretRefs == [caps-key]`.
- `fuse.NewRemote(base, {tok-b, tenant-b}, fuse.WithHTTPClient(h2cClient())).Capabilities` gets no
  secret refs, and the same model and ceiling counts as the raw call.
- A spoof (`tok-b` with tenant `tenant-a`) gives `fuse.ErrPermissionDenied`.
- No token gives `fuse.ErrUnauthenticated`.

- [ ] **Step 2:** Run `go test ./sdk/fuse/ ./cmd/fuse/ -run Capabilities`. Expected: compile failure.

- [ ] **Step 3: Implement**

In `fuse.go`, add the sentinel. Document that it is always returned by NewLocal and, remotely, on a
connect Unimplemented.

```go
	ErrCapabilitiesUnsupported = errors.New("fuse: runtime does not advertise capabilities")
```

Add `capabilities(ctx context.Context) (Capabilities, error)` to the `backend` interface. If any
test fake implements `backend` directly, give it this method returning the sentinel.

In `capabilities.go`, define the public plain types, which mirror `internal/capabilities` without
importing it, each with doc comments:

- `Capabilities` with fields `Models []ModelInfo`, `Skills []SkillInfo`, `Ceilings []Ceiling`,
  `SecretRefs []string`, `SpecMCPAuthKinds []harnessv1.McpAuthType`, and `Server ServerInfo`
- `ModelInfo`, with the same fields as `capabilities.Model`
- `SkillInfo`, with `Name` and `Description`
- `CeilingDirection`, with the constants `CeilingMax = iota + 1` and `CeilingMin`
- `Ceiling`, with `FieldPath`, `OperatorSet`, `Value float64`, and `Direction`
- `ServerInfo`, with `Version` and `DigestAlgorithm`

The doc comment on `Capabilities` reads: "what this Client's tenant may use in a harness spec; build
a model picker from Models and pre-flight a spec against the rest before StartLoop."

Then add
`func (c *Client) Capabilities(ctx context.Context) (Capabilities, error) { return c.backend.capabilities(ctx) }`.

In `local.go`, `capabilities` returns `Capabilities{}, ErrCapabilitiesUnsupported`. An in-process
runtime has no operator to advertise.

In `remote.go`, the remote backend sends `GetCapabilitiesRequest{Tenant: b.tenant}`. Unimplemented
maps to `ErrCapabilitiesUnsupported`. Any other error goes through `mapConnectError`. On success,
map every field back from the proto, using `append([]T(nil), ...)` for slices,
`c.GetValue().GetNumberValue()`, and the direction enum to `CeilingMax/Min`.

- [ ] **Step 4:** Run `go test ./sdk/... ./cmd/fuse/ -run 'Capabilities|Remote|Local'`. Expected: PASS.
- [ ] **Step 5:** `git add sdk/fuse cmd/fuse && git commit -m "sdk: Client.Capabilities over GetCapabilities"`

---

### Task 11: Documentation

**Tier hint:** economy

- [ ] **Step 1:** In `README.md`, under "Networked loop-control", change "The service is three RPCs"
  to "four RPCs". Add a `GetCapabilities` bullet after the `StartLoop` / `Send` bullet. It is
  read-only and uses StartLoop's authentication, and for the authenticated tenant it returns:
  - models: alias, gateway id, effective context window, max output, persona, default flag, and the
    optional `label` / `description` / `tags`
  - installed skills
  - one ceiling per tighten-only field, keyed by the refusal field path (`operator_set: false`
    means a spec cannot set that field)
  - `secret_ref` names for the tenant, names only (a host-restricted secret still resolves only for
    its hosts)
  - spec MCP auth kinds
  - version and digest algorithm

  State that `cli/` routes are never listed. Include a YAML example with a model that has
  `label`/`description`/`tags` (naming the well-known tags in a comment) and a model with
  `advertise: false`.
- [ ] **Step 2:** In `sdk/README.md`, add `Capabilities(ctx)` to the API listing. Add a
  "Pre-flighting a spec" subsection: build a model picker from `Models`, use only the listed
  `Skills` and `SecretRefs`, and keep each tighten-only field at or under its `Ceiling` (leave it
  unset when `OperatorSet` is false). A local client or an older server returns
  `ErrCapabilitiesUnsupported`.
- [ ] **Step 3:** Run `git diff origin/main -U0 | grep '^+' | grep -n $'\xe2\x80\x94' || echo clean`.
  Expected: `clean`. Added lines must not contain an em dash.
- [ ] **Step 4:** `git commit -am "docs: GetCapabilities and the model advertising keys"`

---

### Task 12: Full verification

**Tier hint:** economy

- [ ] `gofmt -l $(git diff origin/main --name-only -- '*.go')`. Expected: no output.
- [ ] `go vet ./...`. Expected: clean.
- [ ] `make test`. Expected: PASS. If `observability-validate` needs a tool that is missing locally,
  run `go test ./...` and record why.
- [ ] `PATH="$HOME/go/bin:$PATH" make proto && git status --porcelain proto internal/loopwire`.
  Expected: no output, meaning there is no stub drift.
- [ ] `go test -race ./internal/loopconnect/ ./internal/harnessresolve/ ./cmd/fuse/ -run 'Capabilities|Ceiling|SecretRefs'`.
  Expected: PASS.
- [ ] Commit any fix as `fix: <what>`. If nothing needed fixing, there is nothing to commit.

---

## Spec coverage

| Requirement | Task |
|---|---|
| Unary read-only RPC, StartLoop auth, Unauthenticated / PermissionDenied | 6, 7, 10 (note 1) |
| ModelInfo fields, effective window plus default flag, tags pass-through | 1, 3, 6, 7 |
| `cli/` excluded, `advertise: false` hides | 3, 8, 10 |
| Skills from the same source StartLoop resolves against | 3, 8 |
| Ceilings from descriptor options, violation paths, coverage test, operator_set / Value / direction | 4, 6, 7 |
| Secret refs per tenant, names only, empty by default | 5, 7, 8, 10 |
| Spec MCP auth kinds = accepted minus refused | 5, 8 |
| Server version and digest algorithm | 2, 3 |
| Filtering on the principal, never a request field | 7, 10 |
| Model config keys, old configs unchanged | 1 |
| Go SDK `Capabilities(ctx)` plus regenerated TS stubs | 6, 10 |
| Golden consistency test against StartLoop | 8 |
| loop-serve-net integration over Connect h2c, raw and SDK | 10 |
| Out of scope untouched: live gateway queries, dry-run RPC, tighten-only semantics, StartLoop `model` and `static` dial bugs, TUI display | all |
