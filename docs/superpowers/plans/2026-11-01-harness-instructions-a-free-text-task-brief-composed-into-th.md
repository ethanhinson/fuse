<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0112 — Harness instructions: a free-text task brief composed into the system prompt](../../changes/active/0112-harness-instructions-a-free-text-task-brief-composed-into-th.md)**
<!-- docket:backlink:end -->

# Harness instructions: a free-text task brief composed into the system prompt - Implementation Plan

> Executed by `docket-build`: one task at a time. Each task is TDD: write the failing test, run it red, write the minimal code, run it green, commit. Every test uses fakes, `httptest` servers, or the in-repo scripted/recording gateway (`recordingGateway`, `routedGateway`) with fake model ids such as `cloud/fast`. No live model traffic, and never an Anthropic/Claude model for verification.

**Goal:** A `HarnessSpec` can say what the agent is *for*. A new free-text `HarnessSpec.instructions` (max 16384 characters) is composed into the **main agent's** system prompt after the persona prompt and before the existing extra block (skill listing, spawn block, scratch block). Workers and subagents keep their own prompts. Empty instructions leave today's system prompt byte-for-byte unchanged.

**Architecture:**
- `internal/agent/personas.go`: `ComposeSystemPrompt(persona, prefix, instructions, extra string)`. Order: `prefix` + `"\n"` + persona prompt, then `"\n\n"` + trimmed instructions (only when non-blank), then `"\n\n"` + extra (only when non-empty). Plus a read-only `(*Agent).SystemPrompt()` getter for tests.
- `proto/fuse/harness/v1/harness.proto`: `string instructions = 14` on `HarnessSpec`, `buf.validate` `string.max_len = 16384`, UI label "Instructions", `WIDGET_TEXTAREA`. Go and TS stubs regenerated with `make proto`.
- `internal/config`: `Config.Instructions` and top-level YAML `instructions:`, merged at every layer (home file, `.fuse.local.yml`, harness-spec overlay) as "set when non-empty". `config.MaxInstructionsChars = 16384` and a structural `Problems()` check at path `instructions` (character count, matching protovalidate's `max_len`). `configcheck.Problems` inherits it.
- `internal/harnessconfig`: `toDocument` renders `instructions` (the `root.set` export path) and `FromConfig` carries it back, so spec -> config -> spec and config -> spec -> config round trips keep it.
- Digest: `harness.Digest` is computed over the deterministic proto encoding, so the new field is covered automatically. A test pins that changing only `instructions` changes the digest, and that an unset/empty `instructions` does not change any existing digest (proto3 default is not encoded).
- `cmd/fuse`: `buildAgentCore` and `buildAgentWithRendererAndTrace` gain an explicit `instructions string` parameter. Every **root** build site passes `cfg.Instructions`. Every **child** build site passes `""`. Both `ComposeSystemPrompt` call sites in `buildAgentCore` (CLI-adapter path and gateway path) pass it. The loop server's spec-driven loops already build their root through `buildAgentCore` with the loop's resolved config, so a spec's instructions reach the root with no extra plumbing.

**Tech stack:** Go, protobuf/buf (+ protovalidate), connect-go, `internal/agent`, `internal/config`, `internal/configcheck`, `internal/harnessconfig`, `harness`, `cmd/fuse`.

**Repository rules that apply to every task:**
- Public MIT repository. Commit messages, code comments, test names and docs describe this feature on its own terms. Never mention downstream products, hosted services, SaaS, control planes or customers.
- Regenerate generated files only with the repo's own target (`make proto`, which runs `proto/generate.sh`). Never hand-edit `harness/v1/*.pb.go` or `proto/gen/ts/**`.
- No new system tools. `buf`, `protoc-gen-go` and `protoc-gen-connect-go` are already in `~/go/bin` (put it on `PATH`). The TS plugin comes from the npm workspace (`npm ci` at the repo root of the worktree; `node_modules/` is gitignored). If buf cannot reach the BSR for the protovalidate dep, stop and report BLOCKED.
- No GitHub settings changes.
- Run gating commands so their real exit status is seen. Never pipe a gating command through `tail`/`head`/`grep`. Use `cmd; echo "exit=$?"` or redirect to a file and read it afterwards.

---

## Spec drift and code facts found while planning (verified against the feature tree at adc3046)

1. **`buildAgentCore` builds roots AND children.** The change says "both `ComposeSystemPrompt` call sites in `cmd/fuse/run.go`". Both are inside `buildAgentCore` (`run.go:997` CLI-adapter path, `run.go:1013` gateway path), and `buildAgentCore` is reached from nine production sites, four of them child builders. Today (`grep -n "buildAgentCore(\|buildAgentWithRendererAndTrace(" cmd/fuse/*.go | grep -v _test`):
   - Root: `runtime_binding.go:247` (one-shot root), `runtime_binding.go:447` (research-probe root), `runtime_binding.go:708` (shell runtime `BuildAgent`, via wrapper), `shell.go:262` (interactive shell root `build` closure, via wrapper), `loop_server.go:465` (loop-server root, operator and spec loops).
   - Child: `runtime_binding.go:164` (one-shot child, via wrapper), `runtime_binding.go:364` (research-probe child), `runtime_binding.go:593` (shell child, via wrapper), `loop_server.go:427` (loop-server child).
   - `buildChildAgent` (`run.go:439`, used when `spawn_agent` sets `system_prompt`) bypasses persona composition entirely and is untouched.
   **Decision:** an explicit `instructions string` parameter on `buildAgentCore` and `buildAgentWithRendererAndTrace`, not a value read from `cfg` inside the builder. Every call site must then decide at compile time, and a child site cannot inherit the brief by default. Re-derive the site list by grep at build time; do not trust the list above (learning `patch-every-cloned-child-builder`).
2. **The loop-server child builder receives the root's `systemBlock` as its extra** (`loop_server.go:427`). That is existing behavior and stays. Only the new instructions are withheld from children.
3. **Orchestrator loops** (`LoopConfig.Orchestrate`) replace the root's system prompt with `orchestrator.SystemPrompt()` in `internal/runtime/inproc.go:695`. An orchestrator watches and steers; it is not the main working agent. **Decision:** unchanged, so an orchestrator loop does not carry the instructions. Note it in the proto comment.
4. **Digest** is `harness.Digest` over `proto.MarshalOptions{Deterministic: true}` (`harness/digest.go`), recorded in `loop.harness` and returned from `StartLoop`. No code change is needed for "changing instructions changes the digest"; tests pin it.
5. **Shape vs semantic duplicate.** A >16384 instructions value fails protovalidate at `instructions` (`string.max_len`) and would also fail the new config `Problems()` check at the same path. `harnessconfig.Combine` drops a later problem whose field path is already flagged, so the spec reports exactly one violation (shape wins). A test pins it.
6. **`.fuse.local.yml`.** The loader honors only tightening keys from the untrusted, repo-plantable file (ADR-0006). `instructions` is prompt text, not a permission or a limit, and it plays the same role a repository's own instruction file does. **Decision:** honored at every layer (home file, `.fuse.local.yml`, spec overlay), "set when non-empty", with no warning. A spec's instructions replace the operator's; a spec without instructions keeps the operator's.
7. **Research probe root** counts as a main agent: it is the root of its tree. It gets the instructions like every other root.

## Decisions made in this plan (record them in the PR body; any ADR is recorded separately by the parent)

1. **Composition order** (documented on `ComposeSystemPrompt` and in the proto comment): `system_prefix` line, persona prompt, harness instructions, extra block. Separator: `"\n"` after the prefix (unchanged), `"\n\n"` between blocks (unchanged for extra).
2. **Blank handling:** `strings.TrimSpace(instructions)`. When the result is empty, nothing is added, so empty or whitespace-only instructions produce exactly today's prompt. When non-empty, the trimmed text is inserted (a YAML `|` block's trailing newline does not create a triple newline before the extra block). The stored value is never trimmed; only composition trims.
3. **Length rule:** 16384 *characters* (Unicode code points), same as protovalidate's `string.max_len`. `config.MaxInstructionsChars = 16384`; a test pins the proto `max_len` to that constant.
4. **UI help text** (spec text plus the main-agent-only note the change asks for): `What this agent is for and how it should work. Added to the system prompt after the persona. Applies to the main agent only; subagents keep their own prompts.`
5. **Explicit parameter, not config inheritance**, for `cmd/fuse` builders (code fact 1).

## Learnings applied

- `patch-every-cloned-child-builder`: derive the root/child site list by grep when building Task 6; the verification step greps again.
- `security-knob-inert-at-composition-root`: unit tests on `ComposeSystemPrompt` prove nothing about the binary. Task 7 asserts the system message the model actually receives on the production loop-server deps and on the one-shot, shell and research-probe deps.
- `runtime-deps-field-overwrites-builder-injection`: the only runtime writer of a system prompt is the orchestrator branch (code fact 3). Task 7 still drives the loop server through `runtime.New(deps).StartLoop`, not `Deps.BuildAgent` alone.
- `verify-tool-loop-at-gateway-seam`: assert on `calls[i].Messages[0].Content` from the recording gateway, which is the exact model-facing prompt.
- `yaml-plain-scalar-colon-space`: the round-trip fixtures use instructions containing `": "`, a leading `#`, `- ` list markers, quotes, tabs and multiple lines, so the YAML rendering in `toDocument` is proven to quote correctly.
- `scripted-gateway-double-double-escapes-tool-args`: script `spawn_agent` calls with the existing `toolCallsReply(call{...})` / `argsJSON` helpers (plain maps), never pre-escaped JSON.

---

## Task 1: Pin today's system prompt byte for byte (golden, before any change)

**Tier:** standard

**Files:**
- Create: `internal/agent/personas_golden_test.go`
- Create: `internal/agent/testdata/compose_golden.json`
- Create: `cmd/fuse/instructions_golden_test.go`
- Create: `cmd/fuse/testdata/loop_root_prompt.golden`

**Step 1: Write the golden tests against the CURRENT 3-argument `ComposeSystemPrompt`.**
- `internal/agent/personas_golden_test.go`: `var updateGolden = flag.Bool("update", false, "rewrite golden files")`. A table of cases: every persona key (`coding`, `research`, `reasoning`, `general`, plus an unknown name `nope`) x prefix (`""`, `"/no_think"`) x extra (`""`, `"## Skills\n- /foo"`). Name each case `persona|prefix|extra-flag`. `TestComposeSystemPromptGolden` computes each output and compares it with `testdata/compose_golden.json` (a JSON object case-name -> exact string); with `-update` it writes the file instead. Call the function through a tiny local helper `composeNoInstructions(persona, prefix, extra string) string` so Task 2 changes exactly one line in this test.
- `cmd/fuse/instructions_golden_test.go`: `TestLoopRootPromptGolden` builds `harnessServer(t, harnessOperator)` (from `loop_harness_test.go`), starts a loop with no spec and `Task: "go"`, waits, and takes `gw.snapshot()[0].Messages[0].Content`. Normalize the per-process scratch path: `strings.ReplaceAll(sys, sessionScratchDir(), "<SCRATCH>")` (skip the replace when `sessionScratchDir()` is `""`). Compare with `testdata/loop_root_prompt.golden`; `-update` writes it (own `flag.Bool("update-golden", ...)` name to avoid colliding with any other flag in package main; check with grep first).

**Step 2: Generate the goldens on unmodified code:** `go test ./internal/agent -run TestComposeSystemPromptGolden -update -count=1` and `go test ./cmd/fuse -run TestLoopRootPromptGolden -update-golden -count=1`. Inspect both files: they must contain the persona text, the skills listing and the scratch placeholder, and no absolute temp path.

**Step 3: Run green without the flag:** `go test ./internal/agent ./cmd/fuse -run 'TestComposeSystemPromptGolden|TestLoopRootPromptGolden' -count=3` (three runs prove the goldens are deterministic).

**Step 4: Commit:** `test: pin today's system prompt composition byte for byte`

---

## Task 2: `ComposeSystemPrompt` takes harness instructions

**Tier:** economy

**Files:**
- Modify: `internal/agent/personas.go`
- Modify: `internal/agent/personas_test.go`
- Modify: `internal/agent/personas_golden_test.go` (the helper only)
- Modify: `internal/agent/agent.go` (getter)
- Modify: `cmd/fuse/run.go` (the two call sites, passing `""` for now)

**Step 1: Write failing tests** in `personas_test.go`:
- `TestComposeSystemPromptInstructionsOrder`: `ComposeSystemPrompt("coding", "/no_think", "You are a release-notes writer.", "## Skills\n- /foo")` equals exactly `"/no_think\n" + PersonaPrompt("coding") + "\n\nYou are a release-notes writer.\n\n## Skills\n- /foo"`.
- `TestComposeSystemPromptInstructionsWithoutExtra`: no extra -> `PersonaPrompt("research") + "\n\nBRIEF"`.
- `TestComposeSystemPromptBlankInstructionsAreAbsent`: for instructions `""`, `"   "`, `"\n\t\n"`, the result equals `ComposeSystemPrompt(p, prefix, "", extra)` for every combination used in the golden table.
- `TestComposeSystemPromptTrimsInstructions`: `"\n  BRIEF\n\n"` composes as `"BRIEF"` between the blank-line separators.
- In `agent_test.go` (or a new small test): `New(nil, nil, nil, "m", "SYS", 1, 0).SystemPrompt() == "SYS"`.
- Update the existing `TestComposeSystemPrompt` calls to the 4-argument form with `""` instructions, and change the golden helper to `ComposeSystemPrompt(persona, prefix, "", extra)`.

**Step 2: Run red:** `go test ./internal/agent -count=1` (compile failure).

**Step 3: Implement.**
```go
// ComposeSystemPrompt builds the final system prompt. The order is:
//
//	prefix (model-specific directive, e.g. "/no_think")
//	persona base prompt
//	instructions (the harness's task brief; main agent only)
//	extra block (e.g. skill listing from shell mode)
//
// Blocks are separated by a blank line. Blank instructions add nothing, so the
// result is then identical to a prompt composed without them.
func ComposeSystemPrompt(persona, prefix, instructions, extra string) string {
	base := PersonaPrompt(persona)
	if prefix != "" {
		base = prefix + "\n" + base
	}
	if ins := strings.TrimSpace(instructions); ins != "" {
		base += "\n\n" + ins
	}
	if extra == "" {
		return base
	}
	return base + "\n\n" + extra
}
```
Add `func (a *Agent) SystemPrompt() string { return a.systemPrompt }` next to `SetSystemPrompt` with a one-line doc comment. In `cmd/fuse/run.go` change both calls to `agent.ComposeSystemPrompt(mc.Persona, mc.SystemPrefix, "", extra)` (Task 6 replaces `""`).

**Step 4: Run green:** `go test ./internal/agent ./cmd/fuse -count=1` (the Task 1 goldens must still pass untouched) and `go build ./...`.

**Step 5: Commit:** `agent: compose harness instructions between the persona and the extra block`

---

## Task 3: Wire contract - `HarnessSpec.instructions`

**Tier:** standard

**Files:**
- Modify: `proto/fuse/harness/v1/harness.proto`
- Regenerate: `harness/v1/harness.pb.go`, `proto/gen/ts/fuse/harness/v1/harness_pb.ts` (whatever `make proto` rewrites)
- Modify: `harness/options_test.go`, `harness/validate_test.go`, `harness/digest_test.go`

**Step 1: Write failing tests.**
- `harness/options_test.go` `TestInstructionsFieldContract`: on `(&harnessv1.HarnessSpec{}).ProtoReflect().Descriptor().Fields().ByName("instructions")` assert number `14`, kind string, `fieldUI(fd)` label `"Instructions"`, widget `harnessv1.Widget_WIDGET_TEXTAREA` (check the generated enum name in `options.pb.go`), help exactly the text in Decision 4, and the protovalidate rule `proto.GetExtension(fd.Options(), validate.E_Field).(*validate.FieldRules).GetString().GetMaxLen() == 16384`.
- `harness/validate_test.go` `TestValidateInstructionsLength`: `strings.Repeat("a", 16384)` -> no violations; `strings.Repeat("é", 16384)` (16384 characters, 32768 bytes) -> no violations; `strings.Repeat("a", 16385)` -> exactly one violation (use `byPath`), at `"instructions"`, rule `"string.max_len"`, non-empty message.
- `harness/digest_test.go` `TestDigestCoversInstructions`: two specs equal except `Instructions` ("a" vs "b") have different digests; `Digest(&harnessv1.HarnessSpec{Instructions: ""}) == Digest(&harnessv1.HarnessSpec{})` and both equal `"sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"` (the empty encoding: adding the field changes no existing digest).

**Step 2: Run red:** `go test ./harness/... -count=1` (compile failure on `Instructions`).

**Step 3: Implement.** In `HarnessSpec` after `research = 13`:
```proto
  // Free-text task brief: what this harness's agent is for and how it should
  // work. Composed into the main agent's system prompt after the persona
  // prompt and before the runtime's own blocks (skills, spawn guidance). It
  // applies to the root agent only: subagents and workers keep their own
  // prompts, and an orchestrator loop keeps the orchestrator prompt. Empty
  // leaves the system prompt unchanged.
  string instructions = 14 [
    (buf.validate.field).string.max_len = 16384,
    (fuse.harness.v1.ui) = {
      label: "Instructions"
      help: "What this agent is for and how it should work. Added to the system prompt after the persona. Applies to the main agent only; subagents keep their own prompts."
      widget: WIDGET_TEXTAREA
    }
  ];
```
Regenerate:
```
cd <worktree> && npm ci
PATH="$HOME/go/bin:$PATH" make proto; echo "exit=$?"
```
Do not hand-edit generated files. `git status` must show only `harness.proto`, `harness/v1/harness.pb.go` and `proto/gen/ts/fuse/harness/v1/harness_pb.ts` changed (plus your test files).

**Step 4: Run green:** `PATH="$HOME/go/bin:$PATH" go test ./harness/... -count=1 -v -run 'TestInstructionsFieldContract|TestValidateInstructionsLength|TestDigestCoversInstructions|TestGeneratedStubsHaveNoDrift|TestEveryFieldHasALabel'` and confirm in the `-v` output that `TestGeneratedStubsHaveNoDrift` PASSED (not SKIPPED). Then `go build ./...`.

**Step 5: Commit:** `proto: HarnessSpec.instructions, a free-text task brief for the main agent`

---

## Task 4: Config - top-level `instructions:` with a length rule

**Tier:** economy

**Files:**
- Modify: `internal/config/schema.go` (`Config.Instructions`, `rawConfig.Instructions`, `MaxInstructionsChars`)
- Modify: `internal/config/loader.go` (`mergeLayer`)
- Modify: `internal/config/validate.go` (`Problems`)
- Modify/Create: tests under `internal/config/` (follow the existing loader/overlay/validate test files; grep for `OverlaySpec(` and `func TestProblems` to find them)
- Modify: `internal/configcheck/configcheck_test.go`

**Step 1: Write failing tests.**
- `FromYAML([]byte("instructions: |\n  You are a release-notes writer.\n  Keep it short: one line per change.\n"), "t")` -> `cfg.Instructions == "You are a release-notes writer.\nKeep it short: one line per change.\n"`.
- `config.Default().Instructions == ""`.
- Untrusted layer: with a temp HOME whose `~/.fuse/config.yml` sets `instructions: home` and a working-dir `.fuse.local.yml` setting `instructions: local`, the loaded config has `"local"`, and nothing about `instructions` is written to the loader's warning stream (find how existing loader tests capture `warnw` and reuse it). A `.fuse.local.yml` without the key keeps `"home"`.
- `OverlaySpec(base{Instructions: "operator"}, []byte("instructions: spec\n"), "s")` -> `"spec"`, refused empty; `OverlaySpec(base{Instructions: "operator"}, []byte("max_tokens: 5\n"), "s")` -> `"operator"`; `base` is not mutated.
- `Problems()`: `Config{Instructions: strings.Repeat("a", MaxInstructionsChars+1)}` (start from `Default()`) yields exactly one problem with `Path == "instructions"` and a message naming the limit; `strings.Repeat("é", MaxInstructionsChars)` yields none.
- `configcheck_test.go`: `configcheck.Problems(cfg)` with over-long instructions includes a problem at `instructions`.

**Step 2: Run red:** `go test ./internal/config/... ./internal/configcheck/... -count=1`.

**Step 3: Implement.**
- `schema.go`: `const MaxInstructionsChars = 16384` with a doc comment (same limit as `HarnessSpec.instructions`' `max_len`; counted in characters). `Config.Instructions string` with a doc comment: the main agent's task brief, composed after the persona prompt; empty leaves the prompt unchanged; not a permission, so honored from every layer. `rawConfig.Instructions string \`yaml:"instructions"\``.
- `loader.go` `mergeLayer`, next to `max_tokens`: `if raw.Instructions != "" { c.Instructions = raw.Instructions }` (all layers, no `ignored` entry). `clone()` needs no change (string).
- `validate.go` `Problems()`: `if n := utf8.RuneCountInString(c.Instructions); n > MaxInstructionsChars { add("instructions", "must be at most %d characters, got %d", MaxInstructionsChars, n) }`.

**Step 4: Run green:** `go test ./internal/config/... ./internal/configcheck/... -count=1` and `go build ./...`.

**Step 5: Commit:** `config: top-level instructions key with a 16384-character limit`

---

## Task 5: harnessconfig - map `instructions` both ways

**Tier:** economy

**Files:**
- Modify: `internal/harnessconfig/harnessconfig.go` (`toDocument`, `FromConfig`)
- Modify: `internal/harnessconfig/harnessconfig_test.go`, `internal/harnessconfig/overlay_test.go`
- Modify: `harness/validate_test.go`

**Step 1: Write failing tests.**
- `fullSpec()` gains `Instructions: tricky`, where `const tricky = "You are a release-notes writer.\nKey: value pairs stay literal.\n# not a comment\n- not a list\n  indented 'single' and \"double\" quotes\ttab"`. `TestRoundTripSpecConfigSpec` must then still pass (it will fail red first because the field is dropped).
- `TestToConfigMapsInstructions`: `ToConfig(&harnessv1.HarnessSpec{Instructions: tricky}, nil)` -> `cfg.Instructions == tricky`; and `ToConfig(&harnessv1.HarnessSpec{}, nil)` still deep-equals `config.Default()` (existing `TestToConfigEmptySpecIsDefault` covers it).
- `TestRoundTripConfigSpecConfig`: add to its YAML fixture
  ```yaml
  instructions: |
    You are a code reviewer.
    Flag: correctness first.
  ```
  (the existing deep-equal then covers it).
- `TestFromConfigCarriesInstructions`: `cfg := config.Default(); cfg.Instructions = tricky; spec, om := FromConfig(cfg)` -> `spec.GetInstructions() == tricky`, `len(om) == 0`; with `""` the spec's field is empty.
- `overlay_test.go` `TestOverlayInstructions`: `Overlay(base{Instructions: "operator"}, &harnessv1.HarnessSpec{Instructions: "spec"}, nil)` -> `"spec"`; with an empty spec -> `"operator"`.
- `TestInstructionsLimitMatchesProto` (pattern of `TestCredentialHeaderPatternMatchesProto`): the proto `max_len` on `HarnessSpec.instructions` equals `uint64(config.MaxInstructionsChars)`.
- `harness/validate_test.go` `TestValidateInstructionsReportedOnce`: `harness.Validate(&harnessv1.HarnessSpec{Instructions: strings.Repeat("a", 16385)})` returns exactly one violation, at `instructions`, rule `string.max_len` (the config rule at the same path is deduplicated by `Combine`).

**Step 2: Run red:** `go test ./internal/harnessconfig/... ./harness/... -count=1`.

**Step 3: Implement.**
- `toDocument`: after `root.set("max_tokens", ...)`, `root.set("instructions", s.GetInstructions())` (the `set` helper skips `""`).
- `FromConfig`: after the `max_tokens` block, `if cfg.Instructions != def.Instructions { s.Instructions = cfg.Instructions }`.

**Step 4: Run green:** `go test ./internal/harnessconfig/... ./internal/harnessresolve/... ./harness/... -count=1` and `go build ./...`.

**Step 5: Commit:** `harnessconfig: carry instructions between a harness spec and config`

---

## Task 6: cmd/fuse - root agents get the instructions, children never do

**Tier:** premium (cross-cutting signature change over cloned builders; a missed or wrong site silently leaks the brief into workers or drops it from a root)

**Files:**
- Modify: `cmd/fuse/run.go` (`buildAgentCore`, `buildAgentWithRendererAndTrace`)
- Modify: `cmd/fuse/runtime_binding.go`, `cmd/fuse/shell.go`, `cmd/fuse/loop_server.go` (every call site)
- Modify: existing tests that call these builders (today `cli_adapter_mode_test.go`, `observe_wiring_test.go`, `shell_test.go`; re-grep)
- Create: `cmd/fuse/instructions_test.go`

**Step 1: Enumerate sites by grep, now:** `grep -n "buildAgentCore(\|buildAgentWithRendererAndTrace(" cmd/fuse/*.go`. Classify each non-test site as root (label `"root"`, builds the tree's root node) or child (inside a `childBuilder`/spawn closure, label `opts.Label`). Compare with code fact 1; if they differ, follow the code and note the difference in your report.

**Step 2: Write failing unit tests** in `instructions_test.go` (pattern: `shell_test.go:47` and `cli_adapter_mode_test.go:127` for building config/registry):
- `TestBuildAgentCoreComposesInstructionsOnGatewayPath`: config with model `fast: {id: cloud/fast, persona: research}`, `permissions: {mode: off}` (so no scratch block), `extra = "EXTRA-BLOCK"`, `instructions = "You are a release-notes writer."`. `a.SystemPrompt()` must equal `agent.ComposeSystemPrompt("research", "", "You are a release-notes writer.", "EXTRA-BLOCK")`, and `strings.Index(persona) < strings.Index(brief) < strings.Index("EXTRA-BLOCK")`.
- `TestBuildAgentCoreComposesInstructionsOnCLIPath`: same with a `cli/`-prefixed model id (copy how `cli_adapter_mode_test.go` sets one up; building does not spawn a subprocess).
- `TestBuildAgentCoreEmptyInstructionsUnchanged`: with `instructions = ""` the prompt equals `agent.ComposeSystemPrompt(persona, prefix, "", extra)` on both paths.

**Step 3: Run red:** `go test ./cmd/fuse -run 'TestBuildAgentCore' -count=1` (compile failure).

**Step 4: Implement.**
- `buildAgentCore(cfg, reg, alias, r, extra, instructions string, traceW, ...)`: add the parameter directly after `extra`, and pass it as the third argument of both `ComposeSystemPrompt` calls. Update the doc comment: `instructions` is the harness's task brief; root builds pass `cfg.Instructions`, child builds pass `""` (workers keep their own prompts).
- `buildAgentWithRendererAndTrace`: same parameter after `extra`, forwarded.
- Root sites: pass `cfg.Instructions` (the `cfg` in scope at that site, which for the loop server is the loop's resolved config).
- Child sites: pass `""` with a short comment at each, e.g. `"" /* subagents never get the harness instructions */`.
- Existing tests: pass `""`.

**Step 5: Run green:** `go test ./cmd/fuse -count=1` (includes the Task 1 golden, which must pass untouched) and `go build ./...`.

**Step 6: Commit:** `fuse: compose harness instructions into every root agent and no subagent`

---

## Task 7: Composition-root proof - the model sees the brief on the root only

**Tier:** standard

**Files:**
- Modify: `cmd/fuse/instructions_test.go`

These tests may pass on first run because Task 6 already wired the sites. Prove each one can fail: temporarily change the matching root site to pass `""` (and, for the child tests, a child site to pass `cfg.Instructions`), run the test red, then revert. Mention in your report that you did this for the loop-server root and loop-server child.

**Step 1: Write the tests.**
- `TestLoopServerSpecInstructionsReachRootPrompt` (pattern: `TestLoopServerRunsALoopFromItsHarnessSpec`): `rt, gw, _ := harnessServer(t, harnessOperator)`; `turns := uint32(1)`, spec `{MaxTurns: &turns, Instructions: "You are a release-notes writer."}`; `rt.StartLoop(ctx, runtime.LoopConfig{Task: "go", Harness: spec})`, `h.Wait()`. On `calls[0].Messages[0]` (role `system`): it contains the default model's persona prompt (`agent.PersonaPrompt(<persona of the default alias>)`, resolve via `registryFromConfig`), then the brief, then the skills listing (`"- research"`), in that order by `strings.Index`.
- `TestLoopServerOperatorInstructionsAndSpecOverride`: operator YAML = `harnessOperator + "instructions: OPERATOR-BRIEF\n"`. A loop with no spec has `OPERATOR-BRIEF` in its root system message. A spec loop with `Instructions: "SPEC-BRIEF"` has `SPEC-BRIEF` and not `OPERATOR-BRIEF`. A spec loop with `Skills` set but no instructions keeps `OPERATOR-BRIEF`.
- `TestLoopServerBlankInstructionsMatchGolden`: a spec loop whose spec sets only `MaxTurns` and `Instructions: "   "` (no `Skills`, so `settingsFor` keeps the full installed skill set and the prompt matches the operator loop's) produces a root system message equal, after the same scratch normalization, to `testdata/loop_root_prompt.golden` from Task 1. If it differs for a reason unrelated to instructions, compare the blank-instructions spec loop with the same spec minus the field instead, and say so in your report.
- `TestLoopServerSubagentsNeverGetInstructions` (pattern: `runHostedSpec` + `toolCallsReply`): spec = `wfSpec()` with `Instructions: "ROOT-ONLY-BRIEF"`; route: `ROOT-TASK` turn 0 -> `toolCallsReply(call{name: "spawn_agent", args: map[string]any{"label": "helper", "task": "CHILD-TASK"}})`, `ROOT-TASK` later -> `textReply("root done")`, `CHILD-TASK` -> `textReply("child done")`. Also spawn one workflow worker (`"workflow": "wf"` with the existing `leadRoute` shape, or a `"worker": "reader"` spawn) so a worker build is covered. Assert the root's first request system message contains `ROOT-ONLY-BRIEF`, and every request in `gw.callsFor("CHILD-TASK")` (and the worker's marker) has a system message that does not.
- `TestLocalRootsGetInstructionsChildrenDoNot`: table over one-shot (`buildOneShotRuntimeDeps`), shell (`buildShellRuntimeDeps`) and research probe (`buildResearchProbeRuntimeDeps`), built exactly as `TestLocalBindingsActivateWorkflowPerSpawn` / `TestResearchProbeRefusesNestedRootSpawn` do, with `cfg.Instructions = "LOCAL-BRIEF"` and a `routedGateway` that answers `textReply("ok")`.
  - Root: call `deps.BuildAgent(<store>, tree, reg.Default, toolReg)`, where `<store>` is whatever no-op/in-memory `event.EventStore` the existing `cmd/fuse` tests pass to `BuildAgent` (grep `BuildAgent(` in `cmd/fuse/*_test.go`), and assert `a.SystemPrompt()` contains `LOCAL-BRIEF` after the persona prompt.
  - Child: `toolReg.Execute(ctx, "spawn_agent", argsJSON(map[string]any{"label": "c", "task": "LOCAL-CHILD"}))` (for the research probe, use a spawn the probe allows; if none is reachable without a workflow, spawn through `"workflow": "research"` after `scope.activateRoot` as the existing test does, and pick a task the routed gateway answers). Every `gw.callsFor("LOCAL-CHILD")` system message lacks `LOCAL-BRIEF`.
  - The interactive shell's TUI `build` closure (`shell.go:262`) cannot be driven headless; it is covered by the Task 6 grep classification and the verification grep.
- `TestSpecDigestChangesWithInstructions`: resolve two specs that differ only in `instructions` through the production host (`host.resolver().Resolve(ctx, "", spec)`, or read `StartLoop`'s recorded `loop.harness` digest via the runtime's history if that is what other tests use); the two `Digest`s differ, and a spec with `Instructions: ""` has the same digest as the same spec without the field.

**Step 2: Run:** `go test ./cmd/fuse -run 'Instructions|Subagents|LocalRoots|Digest' -count=1 -v`, then the sanity mutations described above, then the full package `go test ./cmd/fuse -count=1`.

**Step 3: Commit:** `test: harness instructions reach the root prompt and never a subagent`

---

## Task 8: Docs and an example spec

**Tier:** economy

**Files:**
- Create: `examples/harness/release-notes.json`
- Create: `harness/example_spec_test.go`
- Modify: `sdk/README.md` ("Start a loop from a harness spec")
- Modify: `README.md` (new "### The `instructions` config key" section next to "### The `[agents]` config block")

**Step 1: Write a failing test** `harness/example_spec_test.go` `TestExampleSpecsAreValid`: for every `../examples/harness/*.json` (use `filepath.Glob` relative to the package dir; fail if none match), `protojson.Unmarshal` into a `HarnessSpec`, assert `harness.Validate(spec)` is empty, and for `release-notes.json` assert `spec.GetInstructions() != ""`.

**Step 2: Run red:** `go test ./harness -run TestExampleSpecsAreValid -count=1` (no files).

**Step 3: Write.**
- `examples/harness/release-notes.json` (protojson field names):
  ```json
  {
    "models": {"defaultAlias": "fast", "entries": {"fast": {"id": "cloud/deepseek-v4-flash", "persona": "general"}}},
    "maxTurns": 20,
    "instructions": "You are a release-notes writer.\nRead the merged changes since the last tag and write one line per user-visible change, grouped under Added, Changed and Fixed.\nSkip internal refactors and test-only changes.\nKeep each line under 100 characters."
  }
  ```
- `sdk/README.md`: a short "### Instructions" subsection: what the field is for, the composition order (system prefix, persona prompt, instructions, runtime blocks), main agent only (subagents and workers keep their own prompts), 16384-character limit (`string.max_len` violation at `instructions`), included in the spec digest, a spec's instructions replace the server config's, and a Go snippet adding `Instructions: "You are a release-notes writer."` to the existing `StartLoop` example. Link `examples/harness/release-notes.json`.
- `README.md`: the YAML key:
  ```yaml
  instructions: |
    You are a code reviewer. Flag correctness problems first, then style.
  ```
  with two or three sentences: added to the main agent's system prompt after the persona; subagents keep their own prompts; honored from `~/.fuse/config.yml` and `.fuse.local.yml` (the local file wins); at most 16384 characters; a harness spec's `instructions` replaces it for that loop.
- Plain style, no em dashes, describe the feature on its own terms.

**Step 4: Run green:** `go test ./harness -run TestExampleSpecsAreValid -count=1`.

**Step 5: Commit:** `docs: harness instructions reference and an example spec`

---

## Final verification (docket-build's suite gate)

Run from the feature worktree. Read each exit status; never pipe these through `tail`/`head`.

1. Generated files in sync:
   ```
   PATH="$HOME/go/bin:$PATH" make proto; echo "exit=$?"
   git status --porcelain; git diff --stat
   ```
   `make proto` exits 0 and leaves no diff.
2. `make lint; echo "exit=$?"` (the repo's lint target is `go vet ./...`) exits 0.
3. `PATH="$HOME/go/bin:$PATH" make test > /tmp/fuse-0112-test.log 2>&1; echo "exit=$?"` exits 0 (with `~/go/bin` on `PATH` so `TestGeneratedStubsHaveNoDrift` runs instead of skipping; confirm with `go test ./harness/v1 -run TestGeneratedStubsHaveNoDrift -v -count=1`).
4. `make test-race > /tmp/fuse-0112-race.log 2>&1; echo "exit=$?"` exits 0 (CI runs `go test -race ./...`).
5. Vulncheck: the repo has no vulncheck target and `govulncheck` is not installed. Do not install it. Record "no vulncheck target in this repo" in the evidence.
6. Site audit (learning `patch-every-cloned-child-builder`): `grep -n "buildAgentCore(\|buildAgentWithRendererAndTrace(" cmd/fuse/*.go | grep -v _test` shows every root site passing `cfg.Instructions` and every child site passing `""`; `grep -rn "ComposeSystemPrompt(" --include='*.go' . | grep -v _test` shows exactly the definition and the two `buildAgentCore` calls, both passing `instructions`.
7. `git diff adc3046 -- internal/agent/testdata cmd/fuse/testdata` shows only the files added in Task 1 (the goldens were never regenerated after the code change).
8. Writing check: `git log adc3046..HEAD --format=%B` and the diff contain no mention of downstream products, hosted services, SaaS, control planes or customers.
