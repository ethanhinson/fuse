<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0113 — Roles: named helper definitions in the harness spec](../../changes/archive/2026-11-02-0113-roles-named-helper-definitions-in-the-harness-spec.md)**
<!-- docket:backlink:end -->

# Roles: named helper definitions in the harness spec - Implementation Plan

> Executed by `docket-build`: one task at a time. Each task is TDD: write the failing test, run it red, write the minimal code, run it green, commit. Tests are pure Go/TS unit tests over generated types, the config loader and the validator. No model traffic of any kind.

**Goal:** A harness spec can define reusable, named helpers. `HarnessSpec.roles` is a map of `Role` entries (description, instructions, optional tool set, optional model). `Workflow.roles` names the roles that may take part in a workflow. Both are carried through the YAML config and `internal/harnessconfig`, so they round-trip, and both are validated (shape rules in the proto, plus one semantic rule: every `Workflow.roles` name must resolve to a defined role). The digest covers roles automatically; a spec without roles keeps its digest. **No runtime behavior changes**: nothing in `cmd/fuse`, `internal/agent` or `internal/runtime` reads roles in this change.

**Architecture:**
- `proto/fuse/harness/v1/harness.proto`: new `message Role { description = 1; instructions = 2; tools = 3; model = 4; }`, `map<string, Role> roles = 15` on `HarnessSpec`, `repeated string roles = 4` on `Workflow`. Go and TS stubs regenerated with `make proto` (`proto/generate.sh`: `buf lint && buf generate`).
- `internal/config`: `RoleConfig`, `Config.Roles map[string]RoleConfig`, `WorkflowConfig.Roles []string`, `rawConfig.Roles`, constants `MaxRoles = 32` and `MaxRoleDescriptionChars = 512` (role instructions reuse `MaxInstructionsChars`). Loader merge per trust layer, `clone()` deep-copies `Roles`, and `Problems()` gains the role rules.
- `internal/harnessconfig`: `toDocument` renders `roles:` and `workflows.<wf>.roles`; `FromConfig` carries them back. `SpecPath` learns list indexes (`workflows.wf.roles[1]` -> `workflows["wf"].roles[1]`), so the unresolved-role problem lands on a spec field path.
- `harness`: no production code change. `harness.Validate` already runs shape (protovalidate) then semantic (`config.Problems` through `harnessconfig.SemanticProblems`) and `Combine` reports a field once, shape first. `harness.Digest` already hashes the deterministic proto encoding.
- Docs: README "Roles" section next to the `instructions` section; a two-role example spec under `examples/harness/` asserted by `harness/example_spec_test.go`.

**Tech stack:** Go, protobuf/buf + protovalidate, protoc-gen-es (TS stubs), `internal/config`, `internal/harnessconfig`, `harness`, `harness/testdata/shape_cases.json` (shared with the TS SDK test `sdk/ts/test/harness.test.ts`).

**Repository rules that apply to every task:**
- Public MIT repository. Commit messages, code comments, test names and docs describe this feature on its own terms. Never mention downstream products, consumer applications, hosted services, control planes or customers.
- Regenerate generated files only with the repo's own target: `make proto` (runs `proto/generate.sh`). Never hand-edit `harness/v1/*.pb.go` or `proto/gen/ts/**`.
- No new system tools. `buf`, `protoc-gen-go` and `protoc-gen-connect-go` are in `~/go/bin` (not on the default `PATH`; prefix commands with `PATH="$HOME/go/bin:$PATH"`). The TS plugin `protoc-gen-es` comes from the npm workspace: run `npm ci` once at the worktree root (`node_modules/` is gitignored; the worktree currently has none). If `buf` cannot reach the BSR for the pinned protovalidate dep, stop and report BLOCKED.
- Run gating commands so their real exit status is seen: `cmd; echo "exit=$?"` or redirect to a file and read it afterwards. Never pipe a gating command through `tail`/`head`/`grep`.
- Plain prose in docs and comments: no em dashes.

---

## Code facts verified against the feature tree at 634fb8b

1. **Field numbers are free.** `HarnessSpec` ends at `instructions = 14`; `Workflow` ends at `workers = 3`. No `Role` type exists in `fuse.harness.v1` (no generated-name collision).
2. **Workflow map keys** use `(buf.validate.field).map.keys.string = {min_len: 1, max_len: 64, pattern: "^[a-z0-9][a-z0-9_-]*$"}` with no `max_pairs`. Roles copy the key rule and add `max_pairs: 32`.
3. **`Worker` precedent** (`harness.proto` ~line 615): `tools` has `repeated = {min_items: 1, items: {string: {min_len: 1}}}`, `model` has `string.max_len = 128` and `WIDGET_MODEL_ALIAS`. A role's `tools` is optional (no `min_items`) but keeps the non-empty item rule.
4. **Every reachable field needs a `ui.label`** (`harness/options_test.go` `TestEveryFieldHasALabel` walks the descriptor). `TestTightenOnlyMarkers` pins the tighten-only set; roles add none, so that test must pass unchanged.
5. **`SpecPath`** (`internal/harnessconfig/harnessconfig.go` ~line 757) splits a config path on `.` and walks the descriptor. It handles map keys but not list indexes: `workflows.wf.roles[0]` currently maps to `""` (unattributed). It must learn `name[N]`. Role keys cannot contain `.` (key pattern), so the split stays safe.
6. **`Combine`** (`internal/harnessconfig/semantic.go`) drops a later problem whose field path is already flagged, and always keeps a problem with an empty path. So config-side checks on role fields must produce the same spec path as the shape rule, or they would be reported twice.
7. **Loader trust layers** (`internal/config/loader.go` `mergeLayer`): `layerTrusted` (home file), `layerUntrusted` (`.fuse.local.yml`, loosening keys inert + aggregated `warning: ... ignores permission-loosening keys (...)`), `layerSpec` (`OverlaySpec`, client-settable keys honored). Workflow `workers`/`skill` from the untrusted file are already treated as loosening surface (`mergeWorkflows`); the spec layer replaces them (`mergeSpecWorkflows` in `overlay.go`).
8. **`Config.clone()`** (`internal/config/overlay.go`) deep-copies only the maps the merge writes into, so `OverlaySpec` never mutates the operator's config. A new `Roles` map written by the merge must be cloned there too.
9. **Digest** is `harness.Digest` over `proto.MarshalOptions{Deterministic: true}` (`harness/digest.go`). Proto3 does not encode an empty map or empty repeated field, so a spec without roles keeps its digest with no code change.
10. **Shared shape fixture**: `harness/testdata/shape_cases.json` drives both `harness.ValidateShape` (`harness/shape_parity_test.go`) and the TS SDK's `validateHarnessShape` (`sdk/ts/test/harness.test.ts`, run by `make sdk-ts-test`). A new case there is checked in both languages, which is why the TS stubs must be regenerated.
11. **`harness/v1/drift_test.go` `TestGeneratedStubsHaveNoDrift`** regenerates into a temp dir and diffs; it skips unless `buf`, `protoc-gen-go` and `protoc-gen-connect-go` are on `PATH`. Always run it with `~/go/bin` on `PATH` and confirm PASS, not SKIP.

## Decisions made in this plan (record them in the PR body)

1. **Proto shape (exact):**
   - `HarnessSpec.roles = 15`: `map<string, Role>`, `(buf.validate.field).map = {max_pairs: 32, keys: {string: {min_len: 1, max_len: 64, pattern: "^[a-z0-9][a-z0-9_-]*$"}}}`, ui label `"Roles"`, help `"Named helpers the main agent can hand work to. A role says who a helper is; a workflow says how a big job is divided up."`.
   - `Role.description = 1`: `string = {min_len: 1, max_len: 512}`, label `"Description"`, help exactly `"What the main agent reads when deciding whom to hand work to."`, `WIDGET_TEXT`.
   - `Role.instructions = 2`: `string.max_len = 16384`, label `"Instructions"`, help `"The helper's brief, added to its system prompt."`, `WIDGET_TEXTAREA`.
   - `Role.tools = 3`: `repeated = {items: {string: {min_len: 1}}}` (no `min_items`), label `"Tools"`, help `"Leave empty to inherit the main agent's tools. Otherwise this is the helper's whole tool set: it can only narrow, and the helper can nest only when the list names spawn_agent."`, `WIDGET_LIST`.
   - `Role.model = 4`: `string.max_len = 128`, label `"Model"`, help `"Model alias. Leave empty to use the main agent's model."`, `WIDGET_MODEL_ALIAS`.
   - `Workflow.roles = 4`: `repeated string`, no shape rules (resolution is semantic), label `"Roles"`, help `"Roles that may take part in this workflow. Each must be defined under roles."`, `WIDGET_LIST`.
2. **Length rules count characters**, as protovalidate's `string.max_len` does. Config-side checks use `utf8.RuneCountInString`, matching `instructions`.
3. **Config-side checks mirror the shape rules** so a YAML config file is held to the same contract as a spec: `roles` count <= 32 (path `roles`), description non-blank (path `roles.<name>.description`, `strings.TrimSpace(...) == ""`), description <= 512 characters, role instructions <= 16384 characters (path `roles.<name>.instructions`), and the resolution rule (path `workflows.<wf>.roles[i]`). Each path maps through `SpecPath` to exactly the shape violation's path, so `Combine` reports each field once (shape wins). The role key pattern is checked by shape only: a config key that breaks the pattern could contain `.`, and `SpecPath` would then return `""`, which `Combine` never deduplicates.
4. **Trust layers:**
   - Home file (`~/.fuse/config.yml`): `roles:` honored; an entry replaces any earlier entry with the same name (whole entry, not per field). `workflows.<wf>.roles` honored when non-empty (replaces the list), like `workers`.
   - `.fuse.local.yml` (untrusted): `roles:` is inert and the key `roles` is named in the existing aggregated permission-loosening warning. A role carries a tool set and a model pin, and a repo-plantable file must not be able to redefine a role the operator narrowed. A non-empty `workflows.<wf>.roles` on an existing workflow is treated as loosening surface exactly like `workers` (the workflow is named in the workflow warning, the list is not applied).
   - Spec overlay (`OverlaySpec`): `roles:` honored per name like the home file (a spec role replaces an operator role of the same name; operator roles the spec does not name are kept). `workflows.<wf>.roles` replaces the list when non-empty, like `workers` in `mergeSpecWorkflows`. Roles are not tighten-only, so no `refused` paths and no `tighten_only` markers.
5. **No resolution of role tool names or model aliases** (out of scope, consistent with `Worker`). A test pins that an unknown tool and an unknown model alias on a role produce no violation, so a later change cannot add it silently.
6. **Semantic rule id** for the unresolved workflow role is the existing `fuse.config` (`RuleConfig`); message `"<name>" is not a defined role (define it under roles)`.
7. **Example spec** is a new JSON file `examples/harness/review-team.json`; `harness/example_spec_test.go` already validates every file in that directory and gains an assertion that this file defines two roles and a workflow that lists one.

## Learnings applied

- `yaml-plain-scalar-colon-space`: role descriptions and instructions are free text that now travels through YAML (`toDocument` renders with `yaml.Marshal`, the loader parses with `yaml.Unmarshal`). The round-trip fixtures use text with `": "`, a leading `#`, `- ` list markers, mixed quotes, tabs and newlines to prove quoting is correct in both directions. Reuse the existing `tricky` constant in `harnessconfig_test.go`.
- `reconcile-verify-claims-against-origin-not-working-tree`: the code facts above were checked against the feature tree, which equals the reconciled base plus the spec commit. Re-check field numbers in `harness.proto` before editing.
- `spec-asserted-error-code-verify-emitter-first`: the spec asserts violation paths and rule ids. Do not trust the expected strings in this plan blindly: run each new validation test and confirm the real `FieldPath`/`RuleID`. If protovalidate reports a different path shape than `roles["x"].description` (it reports `workflows["Bad Key"]` for map keys today, see `shape_cases.json`), stop and reconcile the config-side paths so `Combine` still deduplicates, rather than editing expectations to match.

---

## Task 1: Pin the digest of a spec without roles (before any proto change)

**Tier:** economy

**Files:**
- Modify: `harness/digest_test.go`

**Step 1: Write the test against the current code.** Add a helper `rolelessSpec() *harnessv1.HarnessSpec` returning a fixed, realistic spec that uses only existing fields: `Models{DefaultAlias: "fast", Entries: {"fast": {Id: "cloud/fast-1"}}}`, `Instructions: "Review the change."`, `MaxTokens: 4096`, and `Workflows: {"review": {Skill: "review", Pool: {Concurrent: 2, Total: 4}, Workers: {"reader": {Tools: []string{"read_file"}}}}}`. Add:
```go
// TestDigestOfRolelessSpecIsPinned pins today's digest of a spec without
// roles. Adding roles to the schema must not move it.
func TestDigestOfRolelessSpecIsPinned(t *testing.T) {
	const want = "sha256:<fill in>"
	if got := Digest(rolelessSpec()); got != want {
		t.Fatalf("Digest(rolelessSpec()) = %s, want %s", got, want)
	}
}
```
**Step 2: Capture the value on unmodified code.** Run `go test ./harness -run TestDigestOfRolelessSpecIsPinned -count=1`; it fails and prints the real digest. Paste that value into `want`. This must happen before Task 2 touches the proto.

**Step 3: Run green:** `go test ./harness -run 'TestDigest' -count=3; echo "exit=$?"`.

**Step 4: Commit:** `test: pin the digest of a harness spec without roles`

---

## Task 2: Wire contract - `Role`, `HarnessSpec.roles`, `Workflow.roles`

**Tier:** standard

**Files:**
- Modify: `proto/fuse/harness/v1/harness.proto`
- Regenerate (via `make proto` only): `harness/v1/harness.pb.go`, `proto/gen/ts/fuse/harness/v1/harness_pb.ts`
- Modify: `harness/options_test.go`, `harness/validate_test.go`, `harness/digest_test.go`, `harness/testdata/shape_cases.json`

**Step 1: Write failing tests.**
- `harness/options_test.go` `TestRolesFieldContract`: using `fieldUI` and `proto.GetExtension(fd.Options(), validate.E_Field).(*validate.FieldRules)` (pattern of `TestInstructionsFieldContract`), assert:
  - `HarnessSpec.roles`: number 15, `IsMap()`, map value message full name `fuse.harness.v1.Role`, label `"Roles"`, `GetMap().GetMaxPairs() == 32`, key rules `min_len 1`, `max_len 64`, pattern `^[a-z0-9][a-z0-9_-]*$`.
  - `Role.description` (1): string, `min_len 1`, `max_len 512`, label `"Description"`, help exactly `"What the main agent reads when deciding whom to hand work to."`.
  - `Role.instructions` (2): string, `max_len 16384`, widget `WIDGET_TEXTAREA`.
  - `Role.tools` (3): repeated string, `GetRepeated().GetMinItems() == 0`, item `min_len 1`, widget `WIDGET_LIST`.
  - `Role.model` (4): string, `max_len 128`, widget `WIDGET_MODEL_ALIAS`.
  - `Workflow.roles` (4): repeated string, label `"Roles"`, widget `WIDGET_LIST`.
- `harness/validate_test.go` `TestValidateShapeRoles` (table over `harness.ValidateShape`, use `byPath`), each case a spec with otherwise-valid roles:
  - missing description: `{"a": {}}` -> one violation at `roles["a"].description`, rule `string.min_len`;
  - description of 513 `a`s -> `roles["a"].description`, `string.max_len`; 512 `é` (512 characters) -> none;
  - instructions of 16385 `a`s -> `roles["a"].instructions`, `string.max_len`; 16384 `é` -> none;
  - 33 roles (`fmt.Sprintf("r%02d", i)`, each with a description) -> one violation at `roles`, rule `map.max_pairs`; 32 roles -> none;
  - bad key `"Bad Key"` -> `roles["Bad Key"]`, `string.pattern`;
  - empty tool name `Tools: []string{""}` -> `roles["a"].tools[0]`, `string.min_len`;
  - empty tools `nil` -> none (inherit is valid).
- `harness/digest_test.go` `TestDigestCoversRoles`:
  - `Digest` of `rolelessSpec()` with `Roles: map[string]*harnessv1.Role{}` (empty, non-nil) and with `Workflows["review"].Roles = []string{}` equals the pinned Task 1 constant (hoist it to a package-level `const rolelessDigest` so both tests share it);
  - two specs with two roles each (`reviewer`, `researcher`) that differ only in `Roles["reviewer"].Instructions` have different digests;
  - two specs that differ only in `Workflows["review"].Roles` (`["reviewer"]` vs `["researcher"]`) have different digests.
- `harness/testdata/shape_cases.json`: append a case `"roles shapes"`:
  ```json
  {
    "name": "roles shapes",
    "spec": {
      "roles": {
        "Bad Key": {"description": "d"},
        "blanktool": {"description": "d", "tools": [""]},
        "nodesc": {"instructions": "x"}
      },
      "workflows": {"wf": {"roles": ["ghost"]}}
    },
    "want": [
      {"fieldPath": "roles[\"Bad Key\"]", "ruleId": "string.pattern"},
      {"fieldPath": "roles[\"blanktool\"].tools[0]", "ruleId": "string.min_len"},
      {"fieldPath": "roles[\"nodesc\"].description", "ruleId": "string.min_len"}
    ]
  }
  ```
  (`ghost` is unresolved but that is semantic, so the shape layer reports nothing for it. Check the JSON field casing the other cases use, protojson camelCase, and keep it consistent.)

**Step 2: Run red:** `go test ./harness/... -count=1; echo "exit=$?"` (compile failure on `Roles` / `harnessv1.Role`).

**Step 3: Implement the proto.** After `string instructions = 14 [...]` in `HarnessSpec`:
```proto
  // Named helper definitions. A role says who a helper is: a description the
  // main agent reads when choosing whom to hand work to, a brief for the
  // helper's system prompt, and optionally a tool set and a model. A workflow
  // says how a big job is divided up, and lists the roles that may take part
  // in it. Defining roles does not change how a loop runs yet: the runtime
  // validates and records them, and later changes hand them to subagents.
  map<string, Role> roles = 15 [
    (buf.validate.field).map = {
      max_pairs: 32
      keys: {
        string: {
          min_len: 1
          max_len: 64
          pattern: "^[a-z0-9][a-z0-9_-]*$"
        }
      }
    },
    (fuse.harness.v1.ui) = {
      label: "Roles"
      help: "Named helpers the main agent can hand work to. A role says who a helper is; a workflow says how a big job is divided up."
    }
  ];
```
In `Workflow` after `workers = 3`:
```proto
  // Names of the top-level roles that may take part in this workflow. Every
  // name must be defined under HarnessSpec.roles.
  repeated string roles = 4 [(fuse.harness.v1.ui) = {
    label: "Roles"
    help: "Roles that may take part in this workflow. Each must be defined under roles."
    widget: WIDGET_LIST
  }];
```
New message, placed after `Worker`:
```proto
// Role is a named, reusable helper definition.
message Role {
  // What the main agent reads when deciding whom to hand work to.
  string description = 1 [
    (buf.validate.field).string = {min_len: 1, max_len: 512},
    (fuse.harness.v1.ui) = {
      label: "Description"
      help: "What the main agent reads when deciding whom to hand work to."
      widget: WIDGET_TEXT
    }
  ];
  // The helper's brief, composed into its system prompt.
  string instructions = 2 [
    (buf.validate.field).string.max_len = 16384,
    (fuse.harness.v1.ui) = {
      label: "Instructions"
      help: "The helper's brief, added to its system prompt."
      widget: WIDGET_TEXTAREA
    }
  ];
  // Empty: the helper inherits the main agent's tool set. Non-empty: the
  // helper's whole tool set, under the same rules as Worker.tools: it can only
  // narrow, the blackboard tools are always added, and the helper can nest
  // exactly when the list names spawn_agent. Names are not checked against the
  // runtime's tools at validation time (tool sets are dynamic).
  repeated string tools = 3 [
    (buf.validate.field).repeated = {
      items: {string: {min_len: 1}}
    },
    (fuse.harness.v1.ui) = {
      label: "Tools"
      help: "Leave empty to inherit the main agent's tools. Otherwise this is the helper's whole tool set: it can only narrow, and the helper can nest only when the list names spawn_agent."
      widget: WIDGET_LIST
    }
  ];
  // Model alias; empty inherits the main agent's model.
  string model = 4 [
    (buf.validate.field).string.max_len = 128,
    (fuse.harness.v1.ui) = {
      label: "Model"
      help: "Model alias. Leave empty to use the main agent's model."
      widget: WIDGET_MODEL_ALIAS
    }
  ];
}
```
If `buf lint` objects to any option syntax, use the equivalent split form (`(buf.validate.field).map.max_pairs = 32, (buf.validate.field).map.keys.string = {...}`) and keep the same rules.

Regenerate:
```
cd /Users/ethanhinson/dev/fuse/.worktrees/roles-named-helper-definitions-in-the-harness-spec
npm ci; echo "exit=$?"
PATH="$HOME/go/bin:$PATH" make proto; echo "exit=$?"
git status --porcelain
```
`git status` must show only `proto/fuse/harness/v1/harness.proto`, `harness/v1/harness.pb.go`, `proto/gen/ts/fuse/harness/v1/harness_pb.ts` and the test files changed (`harnessv1connect` must be unchanged; no service changed).

**Step 4: Run green:**
`PATH="$HOME/go/bin:$PATH" go test ./harness/... -count=1 -v -run 'TestRolesFieldContract|TestValidateShapeRoles|TestDigest|TestValidateShapeSharedCases|TestEveryFieldHasALabel|TestTightenOnlyMarkers|TestSecretRefMarkers|TestGeneratedStubsHaveNoDrift' > /tmp/fuse-0113-t2.log 2>&1; echo "exit=$?"`, then read the log and confirm `TestGeneratedStubsHaveNoDrift` PASSED (not SKIPPED). Then `go build ./...; echo "exit=$?"`.

**Step 5: Commit:** `proto: named roles on HarnessSpec and Workflow.roles` with a body: adds `Role` (description, instructions, tools, model), `HarnessSpec.roles = 15` (at most 32, workflow key pattern) and `Workflow.roles = 4`; Go and TS stubs regenerated with `make proto`; tests pin the field contract, the shape rules and that a spec without roles keeps its digest.

---

## Task 3: Config - `roles:` and `workflows.<wf>.roles` with merge and problems

**Tier:** standard

**Files:**
- Modify: `internal/config/schema.go` (types, constants, `rawConfig.Roles`)
- Modify: `internal/config/loader.go` (`mergeLayer`, `mergeWorkflows`)
- Modify: `internal/config/overlay.go` (`clone`, `mergeSpecWorkflows`)
- Modify: `internal/config/validate.go` (`Problems`)
- Create: `internal/config/roles_test.go`

**Step 1: Write failing tests** in `internal/config/roles_test.go` (reuse `chdirTemp`, `captureWarnings` from `loader_test.go`; write a local `writeRoleLayers(t, home, local string)` modeled on `writeInstructionsLayers` in `instructions_test.go`):
- `TestRolesFromYAML`: `FromYAML` of
  ```yaml
  roles:
    reviewer:
      description: "Reviews a diff: correctness first."
      instructions: |
        You review code.
        Flag: correctness, then style.
      tools: [read_file, grep]
      model: fast
    researcher:
      description: Finds sources.
  workflows:
    review: {skill: review, roles: [reviewer]}
  ```
  yields `Roles["reviewer"]` with all four fields exact, `Roles["researcher"].Tools == nil` and `.Model == ""`, and `Workflows["review"].Roles == []string{"reviewer"}`. `Default().Roles` is nil/empty and `Default().Workflows["research"].Roles` is empty.
- `TestRolesHomeLayerReplacesByName`: home defines `reviewer` (description `home`, tools `[read_file]`); a second trusted merge (use `FromYAML` twice is not layered, so use `Load()` with home + an empty-ish local, or call `mergeData` directly with `trusted=true` twice) that defines `reviewer` with only a description replaces the whole entry (tools now nil) and keeps other names.
- `TestRolesIgnoredFromLocalLayer`: home defines `reviewer`; `.fuse.local.yml` defines `reviewer` (different description) and a new `sneaky`. After `Load()`: `Roles` equals the home value exactly, `sneaky` absent, and the captured warning contains `ignores permission-loosening keys` and `roles`.
- `TestWorkflowRolesFromLocalLayerAreInert`: home defines workflow `review` with `roles: [reviewer]` and role `reviewer`; local sets `workflows: {review: {roles: [other]}}`. `Workflows["review"].Roles` stays `[reviewer]` and the workflow warning names `review`.
- `TestOverlaySpecRoles`: base (Default plus operator role `ops` and `reviewer`, and workflow `review` with `Roles: [ops]`). `OverlaySpec` with a document setting `roles: {reviewer: {description: spec}}` and `workflows: {review: {roles: [reviewer]}}` gives `Roles["reviewer"].Description == "spec"`, `Roles["ops"]` kept, `Workflows["review"].Roles == [reviewer]`, `refused` empty. Afterwards `base` is unchanged (proves `clone()` copies `Roles` and the workflow's `Roles` slice is not written through).
- `TestProblemsRoles` (table over `Config.Problems()`, starting from `Default()`; assert exact `Path` and that the message mentions the limit):
  - role with description `""` -> path `roles.a.description`; description `"   "` -> same path;
  - description 513 characters -> `roles.a.description` (message contains `512`); 512 `é` -> no problem;
  - instructions `MaxInstructionsChars+1` characters -> `roles.a.instructions` (message contains `16384`);
  - 33 roles -> path `roles` (message contains `32`); 32 -> none;
  - workflow `wf` with `Roles: [a, ghost]`, role `a` defined -> exactly one problem, path `workflows.wf.roles[1]`, message contains `"ghost"`;
  - a fully valid two-role config with a workflow listing one -> no problems;
  - a role with `Tools: [no_such_tool]` and `Model: "no-such-alias"` -> no problems from `Problems()` (decision 5).
- `TestRoleLimitsAreConstants`: `MaxRoles == 32`, `MaxRoleDescriptionChars == 512`.

**Step 2: Run red:** `go test ./internal/config -count=1; echo "exit=$?"`.

**Step 3: Implement.**
- `schema.go`:
  ```go
  // MaxRoles caps the number of top-level roles. It matches the max_pairs rule
  // on HarnessSpec.roles.
  const MaxRoles = 32

  // MaxRoleDescriptionChars caps a role description, counted in characters.
  // It matches the max_len on Role.description.
  const MaxRoleDescriptionChars = 512

  // RoleConfig is a named, reusable helper definition: a description the main
  // agent reads when choosing whom to hand work to, a brief for the helper's
  // system prompt, and optionally a tool set (empty: inherit the parent's) and
  // a model alias (empty: inherit the parent's). The runtime does not use roles
  // yet; they are parsed, validated and carried through harness specs.
  type RoleConfig struct {
  	Description  string   `yaml:"description"`
  	Instructions string   `yaml:"instructions"`
  	Tools        []string `yaml:"tools"`
  	Model        string   `yaml:"model"`
  }
  ```
  Add `Roles map[string]RoleConfig` to `Config` (doc comment: keyed by role name; honored from the home file and a harness spec, inert from `.fuse.local.yml`), `Roles []string \`yaml:"roles"\`` to `WorkflowConfig` (doc: names of top-level roles that may take part), and `Roles map[string]RoleConfig \`yaml:"roles"\`` to `rawConfig`. Keep `Default()` without roles.
- `loader.go` `mergeLayer`: after the orchestrator block and before the aggregated permission warning is printed:
  ```go
  // Roles carry a tool set and a model pin, so a repo-plantable file must not
  // redefine them: honored from the home file and a spec, inert (and named in
  // the warning) from .fuse.local.yml.
  if len(raw.Roles) > 0 {
  	if lvl == layerUntrusted {
  		ignored = append(ignored, "roles")
  	} else {
  		if c.Roles == nil {
  			c.Roles = map[string]RoleConfig{}
  		}
  		for name, r := range raw.Roles {
  			c.Roles[name] = r
  		}
  	}
  }
  ```
  `mergeWorkflows`: trusted branch `if len(rw.Roles) > 0 { cur.Roles = rw.Roles }`; untrusted branch extends the loosening condition to `|| len(rw.Roles) > 0` (and update the comment: skill / workers / roles).
- `overlay.go`: `clone()` adds `out.Roles = maps.Clone(c.Roles)` and, per workflow, `w.Roles = slices.Clone(w.Roles)` (update the doc comment listing the cloned maps). `mergeSpecWorkflows`: `if len(rw.Roles) > 0 { cur.Roles = rw.Roles }`.
- `validate.go` `Problems()`, after the workers loop:
  ```go
  // Roles: the same limits the harness spec's shape rules enforce, so a YAML
  // config is held to the same contract. Role tool names and model aliases are
  // not resolved here (tool sets are dynamic, as for workers).
  if n := len(c.Roles); n > MaxRoles {
  	add("roles", "must define at most %d roles, got %d", MaxRoles, n)
  }
  for _, name := range sortedKeys(c.Roles) {
  	r := c.Roles[name]
  	if strings.TrimSpace(r.Description) == "" {
  		add("roles."+name+".description", "is required: what the main agent reads when deciding whom to hand work to")
  	} else if n := utf8.RuneCountInString(r.Description); n > MaxRoleDescriptionChars {
  		add("roles."+name+".description", "must be at most %d characters, got %d", MaxRoleDescriptionChars, n)
  	}
  	if n := utf8.RuneCountInString(r.Instructions); n > MaxInstructionsChars {
  		add("roles."+name+".instructions", "must be at most %d characters, got %d", MaxInstructionsChars, n)
  	}
  }
  for _, wf := range sortedKeys(c.Workflows) {
  	for i, name := range c.Workflows[wf].Roles {
  		if _, ok := c.Roles[name]; !ok {
  			add(fmt.Sprintf("workflows.%s.roles[%d]", wf, i), "%q is not a defined role (define it under roles)", name)
  		}
  	}
  }
  ```

**Step 4: Run green:** `go test ./internal/config ./internal/configcheck -count=1; echo "exit=$?"` then `go build ./...; echo "exit=$?"`. Existing loader tests must still pass (the warning text for other keys is unchanged).

**Step 5: Commit:** `config: roles and workflow roles with trust-layer merge and validation`

---

## Task 4: harnessconfig - carry roles both ways and map list-index paths

**Tier:** standard

**Files:**
- Modify: `internal/harnessconfig/harnessconfig.go` (`toDocument`, `FromConfig`, `SpecPath`)
- Modify: `internal/harnessconfig/harnessconfig_test.go`
- Modify: `internal/harnessconfig/overlay_test.go`

**Step 1: Write failing tests.**
- `fullSpec()`: add two roles, `"reviewer": {Description: "Reviews a diff: correctness first.", Instructions: tricky, Tools: []string{"read_file", "grep"}, Model: "fast"}` and `"researcher": {Description: "Finds sources."}`, and set `Roles: []string{"reviewer"}` on the existing `"review"` workflow. `TestRoundTripSpecConfigSpec` then covers roles spec -> config -> spec with `proto.Equal`. Also add `Roles: []string{"researcher"}` to the built-in `"research"` override entry so the built-in diff path is exercised.
- `TestRoundTripConfigSpecConfig`: extend the YAML fixture with
  ```yaml
  roles:
    reviewer:
      description: "Reviews a diff: correctness first."
      instructions: |
        You review code.
        # not a comment
        - not a list
      tools: [read_file]
      model: glm
    researcher: {description: Finds sources.}
  ```
  and `roles: [reviewer]` on the `research` workflow entry (keeping its `pool: {concurrent: 2}`). `reflect.DeepEqual(cfg, back)` must hold.
- `TestToConfigMapsRoles`: a spec with one role (all four fields) and a workflow `wf` listing it maps to `cfg.Roles["reviewer"]` exactly and `cfg.Workflows["wf"].Roles == []string{"reviewer"}`; a role with nil tools maps to `Tools == nil` (inherit stays inherit).
- `TestFromConfigCarriesRoles`: `config.Default()` plus `Roles` and a workflow role list -> spec carries them, no omissions; `config.Default()` alone -> `spec.Roles` empty and `spec.Workflows["research"]` absent (unchanged behavior).
- `TestRoleLimitsMatchProto`: `config.MaxRoles` equals `HarnessSpec.roles` `GetMap().GetMaxPairs()`, `config.MaxRoleDescriptionChars` equals `Role.description` `GetString().GetMaxLen()`, and `config.MaxInstructionsChars` equals `Role.instructions` `GetString().GetMaxLen()` (pattern of `TestInstructionsLimitMatchesProto`).
- `TestSpecPath_ListIndexes` (new table; keep `TestSpecPath` and `TestSpecPath_WalksMaps` unchanged and passing):
  - `workflows.wf.roles[1]` -> `workflows["wf"].roles[1]`
  - `roles.reviewer.tools[0]` -> `roles["reviewer"].tools[0]`
  - `roles.reviewer.description` -> `roles["reviewer"].description`
  - `roles` -> `roles`
  - `mcp_servers[2].url` -> `mcp_servers[2].url` (a list of messages continues into the element)
  - `max_tokens[0]` -> `""` (index on a non-list)
  - `workflows.wf.roles[x]` -> `""` and `workflows.wf.roles[1].extra` -> `""` (bad index; scalar list element has no fields)
  - `loop_server.auth[0].scopes` -> `""` (operator-only stays unattributed)
- `overlay_test.go` `TestSemanticProblemsRoles`: `SemanticProblems` of a config with workflow `wf` listing `[a, ghost]` and role `a` defined returns a problem with `FieldPath == "workflows[\"wf\"].roles[1]"` and `RuleID == RuleConfig`. And `TestOverlayRoles`: `Overlay` of a spec defining role `reviewer` and workflow `review` listing it over a base with operator role `ops` resolves both roles and the workflow list, with no refused paths.

**Step 2: Run red:** `go test ./internal/harnessconfig -count=1; echo "exit=$?"`.

**Step 3: Implement.**
- `toDocument`, after the workflows block:
  ```go
  roles := doc{}
  for name, r := range s.GetRoles() {
  	rd := doc{}
  	rd.set("description", r.GetDescription())
  	rd.set("instructions", r.GetInstructions())
  	rd.set("tools", r.GetTools())
  	rd.set("model", r.GetModel())
  	roles[name] = rd
  }
  root.set("roles", roles)
  ```
  and inside the workflow loop `d.set("roles", w.GetRoles())`.
- `FromConfig`: in the workflow loop `if !builtin || !slices.Equal(w.Roles, dw.Roles) { out.Roles = w.Roles }` (placed before the `isZero(out)` check). After the workflows loop, carry every role (there are no built-in roles):
  ```go
  for name, r := range cfg.Roles {
  	if s.Roles == nil {
  		s.Roles = map[string]*harnessv1.Role{}
  	}
  	s.Roles[name] = &harnessv1.Role{Description: r.Description, Instructions: r.Instructions, Tools: r.Tools, Model: r.Model}
  }
  ```
- `SpecPath`: before the descriptor lookup, split a trailing index off the part with a package-level `var indexedPart = regexp.MustCompile(`^([a-z_]+)\[(\d+)\]$`)`. When an index is present the field must be a list (`fd.IsList()`, else return `""`); write `name[N]`; if it is the last part return; if the list element is a message continue into `fd.Message()`; otherwise return `""`. Unchanged behavior for parts without an index. Update the doc comment with the list-index example.

**Step 4: Run green:** `go test ./internal/harnessconfig ./internal/config ./harness/... -count=1; echo "exit=$?"`.

**Step 5: Commit:** `harnessconfig: carry roles between a harness spec and config` with a body: `toDocument` renders `roles` and workflow `roles`, `FromConfig` carries them back, so both round trips keep them (free-text fixtures prove YAML quoting); `SpecPath` maps list indexes so an unresolved workflow role is reported on its spec field.

---

## Task 5: End-to-end validation acceptance through `harness.Validate`

**Tier:** economy

**Files:**
- Modify: `harness/validate_test.go`

**Step 1: Write the tests** (all against `harness.Validate`, the full verdict; use `byPath`, which fails on duplicate paths and so proves each field is reported once):
- `TestValidateRolesAccepted`: a spec with roles `reviewer` (description, instructions, tools `[read_file]`, model `fast` where `fast` is defined under `Models.Entries`) and `researcher` (description only), and workflow `review` (`Skill: "review"`, `Roles: ["reviewer"]`) -> no violations.
- `TestValidateRolesRejected` (table; each case asserts exactly one violation at the stated path with the stated rule):
  - missing description -> `roles["a"].description`, `string.min_len` (the config duplicate is dropped by `Combine`);
  - description 513 characters -> `roles["a"].description`, `string.max_len`;
  - instructions 16385 characters -> `roles["a"].instructions`, `string.max_len`;
  - 33 roles -> `roles`, `map.max_pairs`;
  - bad key `"Bad Key"` (with a description) -> `roles["Bad Key"]`, `string.pattern`;
  - workflow `wf` with `Roles: ["a", "ghost"]`, role `a` defined -> `workflows["wf"].roles[1]`, rule `harness.RuleConfig`, message contains `"ghost"`.
- `TestValidateRolesDoNotResolveToolsOrModels`: a role with `Tools: ["no_such_tool"]` and `Model: "no-such-alias"` -> no violations (decision 5). `configcheck.ModelRefProblems` checks only the default, context, auto-classifier and orchestrator aliases, not `Worker.model`; do not add role models to it.

**Step 2: Run:** `go test ./harness -run 'TestValidateRoles' -count=1 -v; echo "exit=$?"`. These should pass already (Tasks 2 to 4 implemented the behavior); if one fails, fix the cause in the layer that owns it rather than loosening the test, and note it in the commit body.

**Step 3: Commit:** `test: harness roles validate end to end with field-scoped violations`

---

## Task 6: Docs and example spec

**Tier:** economy

**Files:**
- Create: `examples/harness/review-team.json`
- Modify: `harness/example_spec_test.go`
- Modify: `README.md`

**Step 1: Write the failing test.** In `TestExampleSpecsAreValid`, next to the `release-notes.json` assertion:
```go
if filepath.Base(p) == "review-team.json" {
	if len(spec.GetRoles()) != 2 {
		t.Fatalf("review-team.json must define two roles, got %d", len(spec.GetRoles()))
	}
	wf := spec.GetWorkflows()["review"]
	if len(wf.GetRoles()) != 1 || spec.GetRoles()[wf.GetRoles()[0]] == nil {
		t.Fatal("review-team.json must have a review workflow listing one defined role")
	}
}
```
Also fail the test when `review-team.json` is not among the globbed `paths`, so the assertion cannot be skipped by a missing file.

**Step 2: Run red:** `go test ./harness -run TestExampleSpecsAreValid -count=1; echo "exit=$?"` (fails: `review-team.json` is missing).

**Step 3: Write the example and docs.**
- `examples/harness/review-team.json` (protojson field names):
  ```json
  {
    "models": {"defaultAlias": "fast", "entries": {"fast": {"id": "cloud/deepseek-v4-flash", "persona": "coding"}}},
    "instructions": "You lead code reviews. Split a large change into parts and hand each part to a reviewer.",
    "roles": {
      "reviewer": {
        "description": "Reviews one part of a change for correctness, then style.",
        "instructions": "You review the files you are given. Report correctness problems first, each with a file and line. Then list style issues briefly.",
        "tools": ["read_file", "grep", "list_directory"]
      },
      "researcher": {
        "description": "Looks up library documentation and prior decisions.",
        "instructions": "Find the documentation or design note that answers the question. Quote the relevant passage and give its source.",
        "tools": ["web_fetch", "read_file"]
      }
    },
    "workflows": {
      "review": {"skill": "review", "roles": ["reviewer"]}
    }
  }
  ```
  These tool names exist today (`read_file`, `grep`, `list_directory`, `web_fetch` in `internal/tools`); tool names are not validated, but the example should stay accurate.
- `README.md`: a `### The \`roles\` config key` section directly after the `instructions` section:
  ```yaml
  roles:
    reviewer:
      description: Reviews one part of a change for correctness, then style.
      instructions: |
        You review the files you are given. Report correctness problems first.
      tools: [read_file, grep]   # empty or omitted: inherit the main agent's tools
      model: fast                # empty or omitted: inherit the main agent's model
  workflows:
    review:
      skill: review
      roles: [reviewer]
  ```
  Then a few plain sentences: a role says who a helper is; a workflow says how a big job is divided up and lists the roles that may take part. The description is what the main agent reads when choosing whom to hand work to (required, at most 512 characters); instructions are at most 16384 characters; at most 32 roles; names use lowercase letters, digits, `-` and `_`. A non-empty `tools` list is the helper's whole tool set and can only narrow it; the helper can nest only when the list names `spawn_agent`. Tool names and model aliases are not checked when the config is loaded. Every name in a workflow's `roles` must be defined under `roles`. Roles are read from `~/.fuse/config.yml` and from a harness spec, not from `.fuse.local.yml`. Defining roles does not change how an agent runs yet; this release validates and carries them. Link `examples/harness/review-team.json`.

**Step 4: Run green:** `go test ./harness -run TestExampleSpecsAreValid -count=1 -v; echo "exit=$?"` (both example files listed and passing).

**Step 5: Commit:** `docs: harness roles reference and an example spec`

---

## Final verification (docket-build's suite gate)

Run from the feature worktree. Read each exit status; never pipe these through `tail`/`head`.

1. Generated files in sync:
   ```
   PATH="$HOME/go/bin:$PATH" make proto; echo "exit=$?"
   git status --porcelain; git diff --stat
   ```
   `make proto` exits 0 and leaves no diff.
2. `make lint; echo "exit=$?"` (`go vet ./...`) exits 0.
3. `PATH="$HOME/go/bin:$PATH" make test > /tmp/fuse-0113-test.log 2>&1; echo "exit=$?"` exits 0. Confirm the drift guard ran: `PATH="$HOME/go/bin:$PATH" go test ./harness/v1 -run TestGeneratedStubsHaveNoDrift -v -count=1` shows PASS, not SKIP.
4. `make test-race > /tmp/fuse-0113-race.log 2>&1; echo "exit=$?"` exits 0.
5. TS lane (the shared shape fixture gained a case): `make sdk-ts-test > /tmp/fuse-0113-ts.log 2>&1; echo "exit=$?"` exits 0. If it fails only because the npm registry is unreachable, record that in the evidence as an environment limit; any assertion failure is a real failure.
6. Vulncheck: the repo has no vulncheck target and `govulncheck` is not installed. Do not install it. Record "no vulncheck target in this repo" in the evidence.
7. Scope audit: `git diff 634fb8b --stat` touches only `proto/`, `harness/`, `internal/config/`, `internal/harnessconfig/`, `examples/harness/`, `README.md` (plus generated TS under `proto/gen/ts/`). Nothing under `cmd/`, `internal/agent/`, `internal/runtime/` or `internal/tools/` changes: runtime use of roles is a follow-up.
8. Writing check: `git log 634fb8b..HEAD --format=%B` and the diff contain no mention of downstream products, consumer applications, hosted services, SaaS, control planes or customers, and no em dashes in new prose.
