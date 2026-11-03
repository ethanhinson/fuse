<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0118 — Carried skills: a harness spec may define a skill inline](../../changes/active/0118-carried-skills-a-harness-spec-may-define-a-skill-inline.md)**
<!-- docket:backlink:end -->

# Carried skills: a harness spec may define a skill inline - Implementation Plan

> Executed by `docket-build`: one task at a time. Each task is TDD: write the failing test, run it red, write the minimal code, run it green, commit. Every test uses fakes, `httptest` servers or the in-repo recording/scripted gateway doubles. No live model traffic of any kind; if a manual live check is ever wanted it goes only through the local gateway (glm/kimi at `localhost:4000`), never an Anthropic model. Code, comments, test names, commit messages and docs never name any specific host application: say "callers" (or "a caller").

**Goal:** A `HarnessSpec.skills` entry may carry its own `description` and `body`. Such a *carried skill* validates offline, is accepted by `StartLoop` without being installed on the server, shadows an installed skill of the same name for that loop only, appears in the loop's system-prompt skill listing, is returned verbatim by the `skill` tool, is recorded in `loop.harness` and changes the spec digest. Operators can refuse carried skills with `loop_server.spec_policy.carried_skills: false`, and `GetCapabilities` advertises the setting as `spec_policy.carried_skills_allowed`.

**Architecture:**
- `proto/fuse/harness/v1/harness.proto`: `SkillRef` gains `description = 2` and `body = 3` plus a message-level CEL rule; `HarnessSpec.skills` gains `max_items`, a unique-names CEL rule and a carried-count CEL rule. `proto/fuse/loop/v1/loop.proto`: `SpecPolicy` gains `bool carried_skills_allowed = 4` and nothing else. Go and TS stubs regenerated with `make proto`.
- `internal/harnessconfig`: new spec-level semantic check `SkillProblems(spec)` (rule `fuse.skills.carried_bytes`, total carried bytes at most 262144) and the new rule-id constants. `harness.Validate` and `harnessresolve.Resolver.Resolve` both call it, so the public validator, `ValidateHarness` (which is `harness.Validate`) and the server agree.
- `internal/skills`: parser-less constructor `NewCarried`, an `Entry` type (`Name`, `Carried *Skill`) and `(*Set).Compose(entries)`, which builds the loop's set in spec order, a carried entry shadowing an installed skill of the same name.
- `internal/config`: `SpecPolicyConfig.CarriedSkills *bool` (`yaml:"carried_skills"`, nil means true) with `CarriedSkillsAllowed()`, included in `isZero()` so the trusted-only merge keeps it.
- `internal/harnessresolve`: `Resolved.Skills` becomes `[]skills.Entry`; `resolveSkills` resolves carried entries without the installed check and refuses them with `fuse.skills.carried_refused` when the operator's policy says so; `SpecPolicyOf` advertises `CarriedSkillsAllowed`.
- `internal/capabilities`, `internal/loopconnect/capabilities.go`, `sdk/fuse`: the new advertised field is carried end to end.
- `cmd/fuse/loop_harness.go`: `settingsFor` builds the loop's set with `Compose`, so the `skill` tool lookup and `rootSystemBlock` both see carried skills exactly like installed ones.
- Docs: `sdk/README.md` (the harness spec reference), `README.md` (GetCapabilities list), `docs/deploying.md` (the loop server spec-policy reference), the `LoopHarnessPayload` doc comment, a new example spec.

**Tech stack:** Go, protobuf + protovalidate (buf), connect-go, protobuf-es for the TS stubs, `harness`, `internal/harnessconfig`, `internal/harnessresolve`, `internal/skills`, `internal/config`, `internal/capabilities`, `internal/loopconnect`, `cmd/fuse`, `sdk/fuse`.

**Full gate:** `make test` (runs `observability-validate` then `go test ./...`). The TS parity lane is `make sdk-ts-test` (node is on PATH); run it after Task 1 and in the final verification because `harness/testdata/shape_cases.json` also drives `sdk/ts/test/harness.test.ts`.

---

## Spec drift found while planning (verified against the feature tree at e2e92f9)

1. **`Resolver.CarriedSkills bool` would default to refusing.** The spec adds a `CarriedSkills bool` field to `harnessresolve.Resolver` with default true. A Go `bool` field defaults to `false`, so every `Resolver{...}` built without it (all existing tests, and any future caller) would silently refuse carried skills. Every other `spec_policy` knob is read from `r.Base.LoopServer.SpecPolicy` (`specPolicyProblems` in `internal/harnessresolve/policy.go`), and `SpecPolicyOf` advertises from the same config, so what is advertised is what is enforced. **Decision:** no new `Resolver` field. The resolver reads `r.Base.LoopServer.SpecPolicy.CarriedSkillsAllowed()` (nil `*bool` means true, like `DenyPrivate()`).
2. **The shared semantic layer is config-based.** `harnessconfig.SemanticProblems(cfg config.Config)` runs on the mapped config, and `SkillRef` has no `config.Config` counterpart (`ToConfig` doc: "SkillRefs ... are not mapped"). The carried-bytes rule therefore cannot ride `SemanticProblems`. **Decision:** add `harnessconfig.SkillProblems(spec *harnessv1.HarnessSpec) []Problem`, called by `harness.Validate` (with the semantic list) and by `Resolver.Resolve` (with the skill problems). `harness.Service.ValidateHarness` calls `harness.Validate`, so all three agree.
3. **The message-level CEL rule reports at the entry, not at `description`.** A protovalidate field rule cannot see a sibling field, so `fuse.skill.carried_description` is a message rule on `SkillRef` (`option (buf.validate.message).cel`). Its violation path is `skills[i]`. Task 1's shared shape case pins whatever path protovalidate-go reports (expected `skills[0]`), and the TS lane must report the same.
4. **`Combine` drops a second problem at an already-flagged path.** The carried-bytes rule is reported at `skills`. When a shape rule has already flagged `skills` (`repeated.max_items`, `fuse.skills.unique_names`, `fuse.skills.carried_max`), the semantic one is dropped. This matches the existing "a field is reported once" contract, and the test for the semantic rule uses a shape-valid spec (9 bodies of 30000 bytes: 270000 > 262144, 9 <= 16, each <= 32768).
5. **There is no separate "harness spec reference" or "loop server config reference" file.** The harness spec reference is `sdk/README.md` "Start a loop from a harness spec". The loop server spec-policy reference is `docs/deploying.md` "Spec policy: what a client's harness spec may point at". The README's `GetCapabilities` bullet list is in "Networked loop-control". Docs go there.
6. **Only one binding resolves specs.** `grep -rn "func (.*) ResolveHarness" --include='*.go' . | grep -v _test` gives only `cmd/fuse/loop_harness.go`, and `settingsFor` is the only `.Only(` caller. Both bindings (`loop-server`, `loop-serve-net`) go through `assembleLoopServerRuntimeDeps` and this host. Re-run the grep in Task 6 before editing (learning `patch-every-cloned-child-builder`).
7. **The test fixture overwrites `spec_policy` wholesale.** `harnessServerWith` (`cmd/fuse/loop_harness_test.go`) does `cfg.LoopServer.SpecPolicy = config.SpecPolicyConfig{...}`, which would discard `carried_skills: false` from a test's operator YAML. Task 6 changes it to set only the two fields it needs (`DenyPrivateNetworks`, `AllowHTTPHosts`).
8. **The proto toolchain is under `~/go/bin`, not on PATH, and `proto/node_modules` is absent.** Regenerate with `(cd proto && npm ci) && PATH="$HOME/go/bin:$PATH" make proto`. `buf` will fetch protovalidate from the BSR on first use.

## Decisions made in this plan (record them in the PR body; the ADR is recorded by the docket ADR role, not by a build task)

ADR content: skills travel in the spec, not beside it. A harness spec may carry a skill's name, description and body. The carried skill is part of the spec, so it is validated offline, digested (`harness.Digest`), recorded in `loop.harness` and pinnable with a digest-pinned token, which a file mounted on the server could never be. A carried skill shadows an installed skill of the same name for that loop only. It grants no tools and changes no permission. The operator floor is a policy switch (`loop_server.spec_policy.carried_skills`, default true), not a mount: an operator who refuses carried skills still serves references to installed skills.

1. **Carried means non-empty `body`.** An entry with a non-empty `body` is carried; an entry without one is a reference. A reference entry that sets `description` is accepted and the description is ignored (the installed skill's own description is listed). No new rule for that case.
2. **Limits.** `description` at most 1024 characters (`string.max_len`), one line (`string.pattern` `^[^\r\n]*$`); `body` at most 32768 bytes (`string.max_bytes`); at most 64 entries (`repeated.max_items`); names unique (`fuse.skills.unique_names`); at most 16 carried entries (`fuse.skills.carried_max`); carried bodies together at most 262144 bytes (`fuse.skills.carried_bytes`, semantic, counted with Go `len`, i.e. bytes).
3. **Carried skill values.** `skills.NewCarried(name, description, body)` returns `Skill{Name, Description, Body, Path: skills.CarriedPath}` with `CarriedPath = "spec"`. `SlashCommand`, `Context` and `Agent` stay empty. The per-loop composed set is only used by the loop's `skill` tool and system block, never by the TUI or slash-command registration, so carried skills get neither.
4. **Compose semantics.** `(*Set).Compose(entries []Entry) *Set` walks entries in order; a carried entry contributes `*Carried`; a reference contributes the installed skill (skipped if not installed, like `Only`); the first entry of a name wins (shape validation already forbids duplicates). A nil receiver composes from carried entries only. The receiver is never mutated.
5. **Refusal.** With `carried_skills: false`, each carried entry gets one violation at `skills[i].body`, rule `fuse.skills.carried_refused`, message `this server does not accept skills carried in a spec`. A refused carried entry is not also checked against the installed set.
6. **Resolved shape.** `Resolved.Skills` becomes `[]skills.Entry` (nil when the spec lists none, so the server's full set still applies). `settingsFor` calls `Compose` when it is non-empty.
7. **`GetCapabilities`** reports `spec_policy.carried_skills_allowed` from the same `SpecPolicyConfig.CarriedSkillsAllowed()` the resolver enforces. The installed `skills` list is unchanged: a carried skill is not installed.
8. **`loop.harness`** already records the spec as sent, so carried bodies are recorded with no code change; only the payload doc comment changes (Task 9). A resume re-resolves the recorded spec, so a server whose operator later sets `carried_skills: false` refuses to resume such a loop; that is the policy working as intended, not a bug to work around.
9. **Change 0117 overlap.** Change 0117 adds a `CancelLoop` RPC to `loop.proto` in parallel. This change touches `loop.proto` only to add `SpecPolicy` field 4. When rebasing over 0117, resolve `loop.proto` by hand and regenerate stubs with `make proto`; never hand-merge generated files.

## Learnings applied

- `security-knob-inert-at-composition-root`: the operator switch is proven through the production composition root (Task 6 drives `harnessServer`, the real `loopServerHost.resolver()`, and Task 7 drives the real Connect edge), not only by building a `Resolver` by hand. A grep in the final verification confirms a non-test caller of `Compose` exists in `cmd/fuse`.
- `patch-every-cloned-child-builder`: derive the list of spec-resolving bindings and skill-set consumers by grep at build time (Task 6 Step 0), never from this plan's list.
- `parity-test-feeds-each-side-its-own-production-source` / `smoke-over-fake-backend-proves-wire-not-system`: the new shape rules go into the shared `harness/testdata/shape_cases.json`, which Go and the TS SDK each evaluate from their own generated descriptors; `make sdk-ts-test` fails loud when node is absent, so the TS side is genuinely exercised.
- `verify-tool-loop-at-gateway-seam`: the end-to-end proof that the `skill` tool returns the carried body uses the recording gateway and inspects what the model was actually sent (system prompt listing and the tool result message), not a faked completer.
- `yaml-plain-scalar-colon-space`: carried skills never go through the SKILL.md frontmatter parser; `NewCarried` builds the value directly, so a description containing `: ` is fine. Task 3 asserts that.

---

## Task 1: Proto - carried `SkillRef` fields, `skills` list rules, `SpecPolicy.carried_skills_allowed`; regenerate

**Tier:** standard

**Files:**
- Modify: `proto/fuse/harness/v1/harness.proto` (`HarnessSpec.skills`, `SkillRef`)
- Modify: `proto/fuse/loop/v1/loop.proto` (`SpecPolicy` only)
- Regenerate: `harness/v1/*.pb.go`, `internal/loopwire/v1/*.pb.go`, `proto/gen/ts/**` (via `make proto`)
- Modify: `harness/options_test.go`
- Modify: `harness/testdata/shape_cases.json`
- Modify: `harness/validate_test.go`

**Step 1: Write failing tests.**
- `harness/options_test.go`: add `TestSkillRefFieldContract`, modelled on `TestInstructionsFieldContract`. On the `SkillRef` descriptor assert:
  - `name` is field 1 (unchanged).
  - `description` is field 2, string, ui label `"Description"`, widget `WIDGET_TEXT`, `string.max_len == 1024`, `string.pattern == "^[^\\r\\n]*$"` (the Go string literal for the proto pattern `^[^\r\n]*$`; match what the proto file holds).
  - `body` is field 3, string, ui label `"Instructions"`, widget `WIDGET_TEXTAREA`, `string.max_bytes == 32768`.
  - The `SkillRef` message options carry a `buf.validate.message` CEL rule with id `fuse.skill.carried_description`.
  - On `HarnessSpec.skills`: `repeated.max_items == 64` and CEL ids `fuse.skills.unique_names` and `fuse.skills.carried_max` are present.
  - `TestEveryFieldHasALabel` already covers the new fields; nothing to add there.
- Add a loop-wire assertion (in `internal/loopconnect/capabilities_test.go` or a small new test there): the `fuse.loop.v1.SpecPolicy` descriptor field `carried_skills_allowed` exists with number 4 and kind bool.
- `harness/testdata/shape_cases.json`: append these cases (fixture keys are protojson camelCase):
  - `"carried skill is valid"`: `{"skills": [{"name": "triage", "description": "Sort incoming issues: label, size, owner", "body": "# Triage\n\n1. Read the issue.\n2. Label it."}, {"name": "research"}]}` wants `[]`.
  - `"carried skill shapes"`: skills `[{"name": "a", "body": "x"}, {"name": "b", "description": "one\ntwo", "body": "x"}]` wants `[{"fieldPath": "skills[0]", "ruleId": "fuse.skill.carried_description"}, {"fieldPath": "skills[1].description", "ruleId": "string.pattern"}]` (the first path is whatever protovalidate-go reports for a message rule on a list item; expected `skills[0]`. If it differs, record the real path here and in the PR body: the TS lane must then report the same).
  - `"skill names must be unique"`: skills `[{"name": "a"}, {"name": "a"}]` wants `[{"fieldPath": "skills", "ruleId": "fuse.skills.unique_names"}]`.
  - `"at most 16 carried skills"`: 17 entries `{"name": "s1".."s17", "description": "d", "body": "b"}` wants `[{"fieldPath": "skills", "ruleId": "fuse.skills.carried_max"}]`.
  Keep the cases sorted the way `ValidateShape` sorts (field path, then rule id).
- `harness/validate_test.go`: add `TestValidateCarriedSkillSizeLimits`, built with `strings.Repeat` (too large for the JSON fixture; these are standard protovalidate rules the TS side shares by construction):
  - body of 32769 bytes: `skills[0].body` `string.max_bytes`; body of exactly 32768 bytes: valid.
  - description of 1025 ASCII characters: `skills[0].description` `string.max_len`; 1024: valid.
  - 65 reference entries with distinct names: `skills` `repeated.max_items`.

**Step 2: Run red:** `go test ./harness/... -run 'TestSkillRefFieldContract|TestValidateShapeSharedCases|TestValidateCarriedSkillSizeLimits' -count=1` (compile failure on the new fields is the expected red) and `go test ./internal/loopconnect -run CarriedSkillsAllowed -count=1`.

**Step 3: Implement.**
- `harness.proto`, `HarnessSpec.skills` (field 2): keep the field number; add
  ```proto
  repeated SkillRef skills = 2 [
    (buf.validate.field).repeated.max_items = 64,
    (buf.validate.field).cel = {
      id: "fuse.skills.unique_names"
      message: "skill names must be unique"
      expression: "this.map(s, s.name).unique()"
    },
    (buf.validate.field).cel = {
      id: "fuse.skills.carried_max"
      message: "at most 16 skills may be carried in a spec"
      expression: "this.filter(s, s.body != '').size() <= 16"
    },
    (fuse.harness.v1.ui) = {
      label: "Skills"
      help: "Skills by name, resolved against the skills the runtime has installed, or carried in the spec with a description and instructions."
    }
  ];
  ```
- `SkillRef`: rewrite the message comment: an entry with only `name` is a reference the runtime resolves against its installed skills; an entry with `body` is a carried skill the spec defines, run exactly as carried, which shadows an installed skill of the same name for that loop only and grants no tools. Add the message rule and fields:
  ```proto
  option (buf.validate.message).cel = {
    id: "fuse.skill.carried_description"
    message: "a carried skill (one with a body) needs a description"
    expression: "this.body == '' || this.description != ''"
  };
  // One line the model reads when choosing a skill. Required when body is set.
  string description = 2 [
    (buf.validate.field).string = {max_len: 1024, pattern: "^[^\\r\\n]*$"},
    (fuse.harness.v1.ui) = {label: "Description", widget: WIDGET_TEXT}
  ];
  // The skill's instructions as markdown. Set: this entry is a carried skill.
  string body = 3 [
    (buf.validate.field).string.max_bytes = 32768,
    (fuse.harness.v1.ui) = {label: "Instructions", widget: WIDGET_TEXTAREA}
  ];
  ```
  Keep `name` exactly as it is (field 1, same rules) so the wire and generated names are compatible.
- `loop.proto`, `SpecPolicy` only:
  ```proto
  // Skills carried in a spec (SkillRef.body) are accepted
  // (loop_server.spec_policy.carried_skills).
  bool carried_skills_allowed = 4;
  ```
  Touch nothing else in `loop.proto` (change 0117 is editing the service block in parallel).
- Regenerate: `(cd proto && npm ci) && PATH="$HOME/go/bin:$PATH" make proto`. `buf lint` runs first and must pass. Commit the regenerated Go and TS stubs together with the proto edits. If the generated diff touches anything beyond the new fields/rules and their descriptors, stop and report (a toolchain version mismatch against the pins in `proto/generate.sh`).

**Step 4: Run green:** the Step 2 commands, then `go build ./... && go test ./harness/... ./internal/loopconnect/... -count=1`, then `make sdk-ts-test` (the TS SDK evaluates the same `shape_cases.json` from the regenerated TS descriptors; it must pass with the same paths and ids).

**Step 5: Commit:** `harness: carried skill fields and skills list rules; SpecPolicy.carried_skills_allowed`

---

## Task 2: Semantic rule `fuse.skills.carried_bytes`, shared by the validator and the resolver

**Tier:** standard

**Files:**
- Modify: `internal/harnessconfig/semantic.go`
- Create: `internal/harnessconfig/skills_test.go`
- Modify: `harness/validate.go`
- Modify: `harness/validate_test.go`

**Step 1: Write failing tests.**
- `internal/harnessconfig/skills_test.go`:
  - `TestSkillProblemsCarriedBytes`: 9 carried entries with 30000-byte bodies (270000 total) gives exactly one `Problem{FieldPath: "skills", RuleID: RuleSkillsCarriedBytes}` whose message contains `270000` and `262144`. 8 such entries (240000) gives none. Reference entries (no body) and a nil spec give none.
  - `TestSkillProblemsCountsBytesNotRunes`: bodies made of a multi-byte rune (e.g. `strings.Repeat("é", n)`) are counted by `len` (bytes).
- `harness/validate_test.go`: `TestValidateReportsCarriedBytes`: the 9 x 30000 spec through `harness.Validate` reports `skills` `fuse.skills.carried_bytes` and nothing else; `harness.RuleSkillsCarriedBytes == "fuse.skills.carried_bytes"` and `harness.RuleSkillsCarriedRefused == "fuse.skills.carried_refused"`.

**Step 2: Run red:** `go test ./internal/harnessconfig ./harness -run 'TestSkillProblems|TestValidateReportsCarriedBytes' -count=1`.

**Step 3: Implement.**
- `semantic.go`: add rule ids with doc comments:
  - `RuleSkillsCarriedBytes = "fuse.skills.carried_bytes"`: the carried skills' bodies together exceed `MaxCarriedSkillBytes` (change 0118).
  - `RuleSkillsCarriedRefused = "fuse.skills.carried_refused"`: the server's operator does not accept carried skills (change 0118).
  - `const MaxCarriedSkillBytes = 262144`.
  - `func SkillProblems(spec *harnessv1.HarnessSpec) []Problem`: sums `len(ref.GetBody())` over `spec.GetSkills()`; over the limit returns one problem at `skills`, message `carried skills total <n> bytes; at most 262144 bytes may be carried in one spec`. Doc comment: a spec-level semantic rule (skills have no config counterpart), the single place the total is checked, called by both the public validator and a server resolving a spec.
- `harness/validate.go`: re-export `RuleSkillsCarriedBytes` and `RuleSkillsCarriedRefused` with doc comments in the existing const block. In `Validate`, append `harnessconfig.SkillProblems(spec)` to the semantic list in both branches (after `SemanticProblems(cfg)`, and alongside the mapping problem), before `Combine`.

**Step 4: Run green**, then `go test ./harness/... ./internal/harnessconfig/... -count=1`.

**Step 5: Commit:** `harness: cap the total size of skills carried in one spec`

---

## Task 3: `skills` - parser-less carried skills and `Set.Compose`

**Tier:** economy

**Files:**
- Create: `internal/skills/compose.go`
- Create: `internal/skills/compose_test.go`

**Step 1: Write failing tests** in `compose_test.go` (package `skills`, like `only_test.go`):
- `TestNewCarriedBuildsWithoutParsing`: `NewCarried("triage", "Sort issues: label and size", "# Triage\nbody")` equals `Skill{Name: "triage", Description: "Sort issues: label and size", Body: "# Triage\nbody", Path: CarriedPath}`, `CarriedPath == "spec"`, and `SlashCommand`, `Context`, `Agent` are empty. The description with `: ` survives verbatim.
- `TestComposeKeepsSpecOrderAndShadows`: installed set `{a(body "A"), b(body "B"), c}`; entries `[{Name: "c"}, {Name: "x", Carried: &x}, {Name: "a", Carried: &aCarried(body "carried A")}, {Name: "missing"}]`. Result `All()` names are `c, x, a`; `Lookup("a")` returns body `carried A`; `Lookup("b")` is not found; the source set's `Lookup("a")` still returns `A` (not mutated); `len(src.All()) == 3`.
- `TestComposeFirstEntryOfANameWins`: two entries named `a` (carried then reference): one `a`, the carried one.
- `TestComposeOnNilSetUsesCarriedOnly`: `(*Set)(nil).Compose([]Entry{{Name: "x", Carried: &x}, {Name: "a"}})` holds exactly `x`.
- `TestComposedSetListsCarriedSkillsInThePromptBlock`: `SystemPromptBlock()` of a composed set contains `- x: <description>`.

**Step 2: Run red:** `go test ./internal/skills -run 'TestNewCarried|TestCompose' -count=1`.

**Step 3: Implement** `compose.go`:
```go
// CarriedPath is the Path of a skill carried in a harness spec (change 0118):
// it has no file on the server.
const CarriedPath = "spec"

// NewCarried returns a skill a harness spec carries: name, description and
// body exactly as sent, with no frontmatter, slash command or dispatch hints.
func NewCarried(name, description, body string) Skill

// Entry is one skill a loop's spec lists: a reference to an installed skill by
// Name, or, when Carried is set, a skill the spec defines itself.
type Entry struct {
	Name    string
	Carried *Skill
}

// Compose returns a new Set holding the entries' skills in spec order. A
// carried entry contributes its own skill and shadows an installed skill of
// the same name for this set only; a reference contributes the installed skill
// and is skipped when this set does not hold it. The first entry of a name
// wins. A nil set composes from carried entries only. s is never modified.
func (s *Set) Compose(entries []Entry) *Set
```
Leave `Only` in place (other callers and tests may use it; re-check with grep).

**Step 4: Run green**, then `go test ./internal/skills/... -count=1`.

**Step 5: Commit:** `skills: carried skills and Set.Compose for a loop's skill set`

---

## Task 4: Config - `loop_server.spec_policy.carried_skills`

**Tier:** economy

**Files:**
- Modify: `internal/config/schema.go`
- Modify: `internal/config/issuers_test.go` (next to `TestLoopServerSpecPolicyIgnoredFromUntrustedLocal`) or `internal/config/validate_test.go`

**Step 1: Write failing tests:**
- `TestSpecPolicyCarriedSkillsDefaultsTrue`: `SpecPolicyConfig{}.CarriedSkillsAllowed()` is true; a config loaded with no `spec_policy` block allows; `config.FromYAML("loop_server:\n  spec_policy: {carried_skills: false}\n", ...)` gives `CarriedSkillsAllowed() == false`; `{carried_skills: true}` gives true.
- `TestSpecPolicyCarriedSkillsOnlyKeyIsKept`: a trusted home file whose only `spec_policy` key is `carried_skills: false` keeps it (this fails today if `isZero()` ignores the new field, because the merge would drop the block). Follow the home-file/`.fuse.local.yml` setup `TestLoopServerSpecPolicyIgnoredFromUntrustedLocal` uses.
- `TestSpecPolicyCarriedSkillsIgnoredFromUntrustedLocal`: `carried_skills: false` in `.fuse.local.yml` is dropped (still allowed), like the other spec-policy keys.
- `TestSpecPolicyCarriedSkillsMustBeABool`: `carried_skills: maybe` is a load error naming the key (the loader's typed decode; assert on whatever error path its siblings use, e.g. `deny_private_networks: maybe`, so the two behave alike).

**Step 2: Run red:** `go test ./internal/config -run 'TestSpecPolicyCarriedSkills' -count=1`.

**Step 3: Implement** in `schema.go`:
- `SpecPolicyConfig` gains `CarriedSkills *bool \`yaml:"carried_skills"\``. Extend the type's doc list: `CarriedSkills accepts skills carried in a spec (SkillRef.body); nil means true` (change 0118).
- `func (p SpecPolicyConfig) CarriedSkillsAllowed() bool { return p.CarriedSkills == nil || *p.CarriedSkills }`.
- `isZero()` also requires `p.CarriedSkills == nil`. (Also delete the stray blank line before its closing brace.)
- No `validate.go` change is needed beyond what the typed decode gives, unless the bool test shows sibling keys get a dedicated message; then mirror it.

**Step 4: Run green**, then `go test ./internal/config/... -count=1`.

**Step 5: Commit:** `config: loop_server.spec_policy.carried_skills (default true)`

---

## Task 5: Resolver - carried entries, operator refusal, advertisement

**Tier:** standard

**Files:**
- Modify: `internal/harnessresolve/resolve.go`
- Modify: `internal/harnessresolve/advertise.go`
- Modify: `internal/capabilities/capabilities.go`
- Modify: `internal/harnessresolve/resolve_test.go`
- Modify: `internal/harnessresolve/policy_test.go` (or the advertise test file, wherever `SpecPolicyOf` is tested; create `advertise_test.go` cases if none)

**Step 1: Write failing tests** in `resolve_test.go`:
- Update `TestResolveLayersAValidSpec`: `got.Skills` equals `[]skills.Entry{{Name: "research"}}`.
- `TestResolveCarriedSkillNeedsNoInstall`: `Resolver{Base: operator(t)}` (nil `Skills`: nothing installed) resolves a spec with `{Name: "triage", Description: "d", Body: "b"}` with no error; `got.Skills == []skills.Entry{{Name: "triage", Carried: &want}}` where `want = skills.NewCarried("triage", "d", "b")`.
- `TestResolveCarriedAndReferenceInSpecOrder`: `installed("research")`, spec skills `[{Name: "triage", carried}, {Name: "research"}]`: entries come back in that order, the reference with `Carried == nil`.
- `TestResolveCarriedShadowsNoInstallCheck`: `installed("research")` and a carried `research`: resolves, entry is carried.
- `TestResolveUninstalledReferenceStillRefused`: a reference `{Name: "nope"}` beside a carried skill: one violation `skills[1].name` `fuse.skill_ref` (index as placed).
- `TestResolveRefusesCarriedSkillsWhenPolicySaysSo`: base with `LoopServer.SpecPolicy.CarriedSkills = &false`; spec `[carried a, ref research, carried b]` with `installed("research")`: violations exactly `skills[0].body fuse.skills.carried_refused` and `skills[2].body fuse.skills.carried_refused`, each with message `this server does not accept skills carried in a spec`; the reference is accepted.
- `TestResolveReportsCarriedBytes`: the 9 x 30000 spec gives `skills fuse.skills.carried_bytes` from `Resolve` too.
- `TestResolveDigestCoversTheBody`: two specs differing only in a carried body resolve with different `Digest`s.
- `TestResolveNilAndEmptySpecIsTheBase`: unchanged assertion `got.Skills != nil` still compiles against the new type.
- Advertisement: `SpecPolicyOf(config.SpecPolicyConfig{}).CarriedSkillsAllowed` is true; with `CarriedSkills: &false` it is false.

**Step 2: Run red:** `go test ./internal/harnessresolve -count=1`.

**Step 3: Implement.**
- `resolve.go`:
  - Import `internal/skills` (check: `go list -deps ./internal/skills` must not include `harnessresolve`; it imports only stdlib and yaml today).
  - `Resolved.Skills` becomes `[]skills.Entry` with the doc: the skills the spec lists, in spec order, each a reference to an installed skill or a carried skill; nil when the spec lists none (the server's full set applies).
  - `resolveSkills(spec)` returns `([]skills.Entry, []harnessconfig.Problem)`. Read `allowed := r.Base.LoopServer.SpecPolicy.CarriedSkillsAllowed()`. For each `ref` at index `i`: if `ref.GetBody() != ""`, then if `!allowed` append the refusal problem at `skills[i].body` with `harness.RuleSkillsCarriedRefused`, else append `skills.Entry{Name, Carried: &sk}` with `sk := skills.NewCarried(ref.GetName(), ref.GetDescription(), ref.GetBody())`; `continue` either way (never the installed check). Otherwise the existing installed check and `skills.Entry{Name: name}`. Update the function doc and the package doc paragraph ("skill names against the skills the server has installed") to mention carried skills and the policy.
  - In `Resolve`, add `harnessconfig.SkillProblems(spec)` to the skill problems.
- `internal/capabilities/capabilities.go`: `SpecPolicy` gains `CarriedSkillsAllowed bool` (`// CarriedSkillsAllowed accepts skills carried in a spec.`).
- `advertise.go`: `SpecPolicyOf` sets `CarriedSkillsAllowed: p.CarriedSkillsAllowed()`.
- `cmd/fuse/loop_harness.go` will not compile against the new `Resolved.Skills` type until Task 6. Make the minimal compile fix here so the tree stays green: replace `skillSet.Only(res.Skills)` with `skillSet.Compose(res.Skills)` and adjust the `lookup` guard as in Task 6 Step 3. Task 6 then proves it end to end.

**Step 4: Run green**, then `go build ./... && go test ./internal/harnessresolve/... ./internal/capabilities/... ./cmd/fuse/... -count=1`.

**Step 5: Commit:** `harnessresolve: resolve carried skills; operator may refuse them`

---

## Task 6: Loop server - carried skills reach the skill tool and the system prompt; operator switch end to end

**Tier:** standard

**Files:**
- Modify: `cmd/fuse/loop_harness.go` (if Task 5's minimal fix needs finishing)
- Modify: `cmd/fuse/loop_harness_test.go`
- Modify: `cmd/fuse/loop_capabilities_test.go`
- Modify: `internal/runtime/harness_test.go` (only if the `loop.harness` assertion fits better there; the cmd/fuse test below is the required one)

**Step 0: Re-derive sites by grep** (do not trust this plan's list): `grep -rn "func (.*) ResolveHarness\|\.Only(\|\.Compose(\|rootSystemBlock(\|defaultToolRegistry(" --include='*.go' cmd internal sdk | grep -v _test`. Every place that builds a spec loop's skill set must use `Compose`. Report any new site in the commit body.

**Step 1: Write failing tests** in `loop_harness_test.go`:
- Fixture change first: in `harnessServerWith`, replace `cfg.LoopServer.SpecPolicy = config.SpecPolicyConfig{DenyPrivateNetworks: &noDeny, AllowHTTPHosts: []string{"127.0.0.1"}}` with assignments to just those two fields, so a test's `spec_policy.carried_skills` survives. Keep the explanatory comment.
- `TestLoopServerRunsACarriedSkill`: `rt, gw, _ := harnessServer(t, harnessOperator)`. Set `gw.script` (under `gw.mu`) to two replies: first a `skill` tool call with `{"name":"triage"}`, then `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`. Spec: `MaxTurns: 2`, skills `[{Name: "triage", Description: "Sort incoming issues", Body: "CARRIED-TRIAGE-BODY"}, {Name: "notes"}]`. After `h.Wait()`:
  - the first call's system message contains `- triage: Sort incoming issues` and `- notes`, and does not contain `- research`;
  - the second call's messages contain a tool-role message whose content is exactly `CARRIED-TRIAGE-BODY` (the `skill` tool returned the body verbatim);
  - `rt.Attach(..., h.ID(), 0)` history starts with `loop.harness` and that payload contains `CARRIED-TRIAGE-BODY` (the record holds the spec as sent).
- `TestLoopServerCarriedSkillShadowsInstalled`: same scripted gateway calling `skill` with `{"name":"notes"}`; spec skills `[{Name: "notes", Description: "carried notes", Body: "CARRIED-NOTES"}]`. The tool result is `CARRIED-NOTES`, not the installed `Take notes.`, and the system listing shows `- notes: carried notes`. Then start a loop on the same runtime with no spec: the installed `notes` is untouched (its listing shows `- notes: take notes`).
- `TestLoopServerRefusesCarriedSkillsWhenOperatorDisables`: `harnessServer(t, harnessOperator+"loop_server:\n  spec_policy:\n    carried_skills: false\n")`. A spec with a carried skill fails `StartLoop` with a `*harness.ValidationError` holding exactly `skills[0].body fuse.skills.carried_refused`, and the gateway saw no request. A spec with only `{Name: "research"}` still starts.
- `TestLoopServerAcceptsCarriedSkillWithoutInstalledMatch` is covered by `TestLoopServerRunsACarriedSkill` (`triage` is not installed); say so in that test's doc comment rather than adding a duplicate.
- `loop_capabilities_test.go`: in the existing fixture assertion, `c.SpecPolicy.CarriedSkillsAllowed` is true by default. Add `TestLoopCapabilitiesAdvertiseCarriedSkillsDisabled`: a host built with `carried_skills: false` reports `CarriedSkillsAllowed == false` through `host.Capabilities` (the same `SpecPolicyConfig` the resolver enforces).

**Step 2: Run red:** `go test ./cmd/fuse -run 'TestLoopServerRunsACarriedSkill|TestLoopServerCarriedSkillShadowsInstalled|TestLoopServerRefusesCarriedSkills|TestLoopCapabilities' -count=1`.

**Step 3: Implement** `settingsFor` in `cmd/fuse/loop_harness.go`:
```go
// The loop's skills: exactly the ones the spec lists, in spec order (an
// installed skill by name, or a skill the spec carries, which shadows an
// installed one of the same name for this loop only), or every installed
// skill when it lists none. Both the skill tool's lookup and the system
// prompt's skill listing use this set.
skillSet := h.skills
if len(res.Skills) > 0 {
	skillSet = skillSet.Compose(res.Skills)
}
```
Keep the `if skillSet != nil { lookup = skillSet.Lookup }` guard (Compose on a nil installed set returns a non-nil set, so carried skills still get a lookup). Update the `ResolveHarness` doc comment ("skill names resolve against the skills installed here") to mention carried skills. `resolver()` needs no change: the policy is read from `Base` (drift item 1).

**Step 4: Run green**, then `go test ./cmd/fuse/... -count=1`.

**Step 5: Commit:** `loop server: run skills carried in a harness spec`

---

## Task 7: Capabilities on the wire and in the Go SDK; digest pin over a carried skill

**Tier:** standard

**Files:**
- Modify: `internal/loopconnect/capabilities.go`
- Modify: `internal/loopconnect/capabilities_test.go`
- Modify: `sdk/fuse/capabilities.go`
- Modify: `sdk/fuse/remote.go`
- Modify: `sdk/fuse/capabilities_test.go`
- Modify: `cmd/fuse/loop_serve_net_scopes_test.go`

**Step 1: Write failing tests.**
- `internal/loopconnect/capabilities_test.go`: in `fullAnswer()` set `SpecPolicy.CarriedSkillsAllowed: true`, and in `TestGetCapabilitiesMapsEveryField`'s `want` set `CarriedSkillsAllowed: true` on the proto `SpecPolicy`. Add a case (or extend the per-token loop at ~line 255) asserting a `false` answer maps to `false`.
- `sdk/fuse/capabilities_test.go`: `fullInternalCaps()` sets `CarriedSkillsAllowed: true`; `TestRemoteCapabilities`'s `want.SpecPolicy` has `CarriedSkillsAllowed: true`.
- `cmd/fuse/loop_serve_net_scopes_test.go`: `TestDigestPinCoversACarriedSkill`, modelled on `TestDigestPinEndToEnd`: `spec` has `MaxTurns: 1` and one carried skill with body `v1`; `edited` is a clone with body `v2`. Assert `harness.Digest(spec) != harness.Digest(edited)`. A token pinned to `harness.Digest(spec)` is `PermissionDenied` for `edited` with no registry record left behind, and starts `spec` with `HarnessDigest == pin`; wait for the turn end with `waitForTurnEndByObserve`. This test passes as soon as Task 1 lands (it is a regression proof of the AC through the real Connect edge); note that in its doc comment.

**Step 2: Run red:** `go test ./internal/loopconnect ./sdk/fuse -run 'Capabilities' -count=1`.

**Step 3: Implement.**
- `toProtoCapabilities`: `CarriedSkillsAllowed: c.SpecPolicy.CarriedSkillsAllowed`.
- `sdk/fuse/capabilities.go`: `SpecPolicy` gains `CarriedSkillsAllowed bool` (`// CarriedSkillsAllowed reports that a spec may carry its own skills (SkillRef.body).`).
- `sdk/fuse/remote.go` `fromProtoCapabilities`: `CarriedSkillsAllowed: m.GetSpecPolicy().GetCarriedSkillsAllowed()`.

**Step 4: Run green**, then `go test ./internal/loopconnect/... ./sdk/... ./cmd/fuse -run 'Capabilities|DigestPin' -count=1`.

**Step 5: Commit:** `capabilities: advertise carried_skills_allowed; SDK maps it`

---

## Task 8: Example spec with a carried skill

**Tier:** economy

**Files:**
- Create: `examples/harness/carried-skill.json`
- Modify: `harness/example_spec_test.go`

**Step 1: Write failing test:** in `TestExampleSpecsAreValid`, require `carried-skill.json` to exist (same pattern as `review-team.json`) and, for it, assert at least one `skills` entry has a non-empty `body` and `description`, and at least one entry is a reference (no body).

**Step 2: Run red:** `go test ./harness -run TestExampleSpecsAreValid -count=1`.

**Step 3: Implement** `examples/harness/carried-skill.json` (protojson camelCase): a small spec with `models.defaultAlias` (match what the other examples use), `maxTurns`, `instructions`, and `skills`: one carried skill (e.g. `"name": "changelog-entry"`, a one-line description, a short markdown body with numbered steps) and one reference `{"name": "research"}`. Generic content only; no product or host names.

**Step 4: Run green**, then `go test ./harness/... -count=1`.

**Step 5: Commit:** `examples: harness spec carrying a skill`

---

## Task 9: Docs

**Tier:** economy

**Files:**
- Modify: `sdk/README.md` ("Start a loop from a harness spec")
- Modify: `README.md` (`GetCapabilities` bullet list in "Networked loop-control")
- Modify: `docs/deploying.md` ("Spec policy: what a client's harness spec may point at")
- Modify: `internal/event/event.go` (`LoopHarnessPayload` doc comment)

**Step 1: Edit `sdk/README.md`:**
- In the intro paragraph, change "Skill names resolve against the skills installed on the server" to say a skills entry is either a reference resolved against installed skills or a carried skill.
- Add a `### Skills` subsection after `### Instructions`: an entry with only `name` references an installed skill (`fuse.skill_ref` when it is not installed); an entry with `description` and `body` is a carried skill the loop runs as sent, listed in the system prompt and returned by the `skill` tool like an installed one, with no slash command. Carried skills shadow an installed skill of the same name for that loop only. They grant no tools and change no permissions. Limits: description one line, at most 1024 characters; body at most 32768 bytes; at most 16 carried and 64 entries in all; names unique; carried bodies together at most 262144 bytes (`fuse.skills.carried_bytes`). Carried skills are part of the digest and the `loop.harness` record, so a digest-pinned token pins their content. A server may refuse them (`fuse.skills.carried_refused` at `skills[i].body`); check `Capabilities(ctx)`'s `SpecPolicy.CarriedSkillsAllowed` first. Include a short Go example with `[]*harnessv1.SkillRef{{Name: "changelog-entry", Description: "...", Body: "..."}, {Name: "research"}}` and point at `examples/harness/carried-skill.json`.
- In "Pre-flighting a spec", extend "Reference only the listed `Skills`" with: or carry a skill when `SpecPolicy.CarriedSkillsAllowed` is true.

**Step 2: Edit `README.md`:** in the `GetCapabilities` list, add a bullet: the spec policy (`mcp_hosts`, `deny_private_networks`, `models_registry_only`, `carried_skills_allowed`); and make "installed skills" read "installed skills (a spec may also carry its own; see `sdk/README.md`)".

**Step 3: Edit `docs/deploying.md`:** add `carried_skills: true  # default true; false refuses skills carried in a spec` to the YAML block; add `carried_skills_allowed` to the advertised list in the first paragraph; add a bullet: `carried_skills: false` refuses every spec skill entry with a `body` (`fuse.skills.carried_refused` at `skills[i].body`) while references to installed skills still work, and a loop started earlier with a carried skill cannot be resumed after the switch is turned off.

**Step 4: Edit `internal/event/event.go`:** in the `LoopHarnessPayload` comment, add that the spec includes the bodies of any skills it carries (at most 262144 bytes in total, change 0118).

**Step 5: Verify:** `go build ./... && make test`. Then re-read every changed doc and comment line in `git diff origin/main` and confirm it names no specific host application (only "callers").

**Step 6: Commit:** `docs: carried skills in harness specs and the carried_skills policy`

---

## Final verification (docket-build's suite gate)

- `make test` green.
- `make sdk-ts-test` green (the TS SDK holds the same shape rule ids and field paths for the new cases).
- `go vet ./...` clean.
- `PATH="$HOME/go/bin:$PATH" make proto && git status --porcelain` shows no diff (committed stubs match the protos).
- `git diff origin/main -- proto/fuse/loop/v1/loop.proto` adds only `carried_skills_allowed = 4` and its comment.
- `grep -rn '\.Compose(' cmd/fuse/*.go | grep -v _test` finds the `settingsFor` call (the carried-skill path is wired at the composition root), and `grep -rn 'CarriedSkillsAllowed()' --include='*.go' internal | grep -v _test` finds both the resolver and `SpecPolicyOf` (advertised equals enforced).
- `git diff origin/main` names no specific host application in code, comments, tests, docs or commit messages.
