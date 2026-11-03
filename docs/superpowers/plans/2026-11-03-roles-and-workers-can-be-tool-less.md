<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0116 — Tool-less loops, roles and workers](../../changes/active/0116-roles-and-workers-can-be-tool-less.md)**
<!-- docket:backlink:end -->

# Tool-less loops, roles and workers — Implementation Plan

> **For the build role:** this plan is executed by `docket-build`, one task at a time, each task by a tier agent under the `docket-build-task` contract (focused test first, implement, verify, self-review, one commit). Steps use checkbox (`- [ ]`) syntax. The full suite runs once at the end (Task 12).

**Goal:** One `bool no_tools` field on `HarnessSpec`, `Role` and `Worker` (and `no_tools` on `Config`, `RoleConfig`, `WorkerConfig`) means exactly "no tools": a model call with no `tools` key for a tool-less loop (loop server and one-shot CLI) and for a tool-less role or worker child. That includes the internal tools and the blackboard.

**Architecture:** The child-tool resolution in `cmd/fuse/workflow.go` gains a `none` state on `childPlan`. `loopWiring.childTools` returns an empty registry for it, and `loopWiring.rootTools` does nothing for a tool-less config. A new `cmd/fuse/tool_less.go` holds the two helpers that the one-shot CLI and the loop server both use to build their base registry and MCP server list, so the two entry points cannot drift. `buildAgentCore` builds a one-line tool-less persona prompt with no tool-driven extra blocks when `cfg.NoTools` is set, and child builders pass `cp.agentConfig(cfg)`. Validation lives in `internal/config/validate.go` and reaches `harness.Validate` and `ValidateHarness` through the existing `harnessconfig` mapping. Capability discovery gains `spec_features` (`["no_tools"]`) on `GetCapabilitiesResponse`, exposed by both SDKs.

**Tech stack:** Go, protobuf/buf (protovalidate), Connect, TypeScript SDK (`node --test`).

**Spec (authoritative):** `docs/superpowers/specs/2026-11-03-roles-and-workers-can-be-tool-less-design.md`. The spec supersedes the change record's note that tool-less children keep the blackboard. **A tool-less child gets an empty registry, with no blackboard.**

---

## Ground rules (every task)

- **Worktree:** `/Users/ethanhinson/dev/fuse/.worktrees/roles-and-workers-can-be-tool-less` (branch `feat/roles-and-workers-can-be-tool-less`). All paths below are relative to it.
- **Proto regeneration:** `PATH=$HOME/go/bin:$PATH make proto`. This regenerates the Go stubs (`harness/v1`, `internal/loopwire/v1`) and the TS stubs (`proto/gen/ts`). If `buf generate` reports missing TS plugins, run `cd proto && npm ci` once, then retry. Commit the generated files along with the `.proto` change.
- **No live model traffic.** Every test uses the existing recording or fake gateways (`recordingGateway`, `routedGateway`, `rawCapture`, `scriptedGateway`, `httptest` servers). Never point a test at a real provider, and never at Anthropic.
- **Naming:** no code, comment, test name, doc or commit message may name any specific host application. Say "callers" (or "a client").
- **Cloned child builders (learning `patch-every-cloned-child-builder`):** a child-builder change must land at every site. Build the site list by grepping at the time of the change (`grep -n "wiring.childTools(" cmd/fuse/*.go`), never from this plan's list. Today there are four: `runtime_binding.go` (one-shot, research-probe, shell) and `loop_server.go` (hosted).
- **Verify at the gateway seam (learning `verify-tool-loop-at-gateway-seam`):** "what the model is offered" is asserted on recorded gateway requests, never on a registry alone.
- **Scripted tool-call args (learning `scripted-gateway-double-double-escapes-tool-args`):** build them only through the existing `toolCallsReply(call{...})` / `argsJSON(...)` helpers.
- **Focused runs:** `go test ./<pkg> -run '<Name>' -count=1`. The full suite is `make test` (Task 12 only).
- **Commit subjects** follow the repo's `<area>: <summary>` style for in-PR commits.

---

## File map

| File | Change |
|---|---|
| `proto/fuse/harness/v1/harness.proto` | `HarnessSpec.no_tools = 16`, `Role.no_tools = 5`, `Worker.no_tools = 3`; drop `Worker.tools` `min_items: 1`; three-state comments on `Role.tools`, `Worker.tools` |
| `proto/fuse/loop/v1/loop.proto` | `GetCapabilitiesResponse.spec_features = 9` |
| `harness/v1/*`, `internal/loopwire/v1/*`, `proto/gen/ts/**` | regenerated |
| `harness/testdata/shape_cases.json`, `harness/shape_parity_test.go` (if it hard-codes cases), `harness/options_test.go`, `harness/validate_test.go`, `harness/digest_test.go` | tests |
| `harness/features.go` (new) | `SpecFeatureNoTools`, `SpecFeatures()` |
| `internal/config/schema.go`, `loader.go`, `validate.go` (+ tests) | `NoTools` fields, merge, rules |
| `internal/harnessconfig/harnessconfig.go` (+ tests) | `ToConfig` / `FromConfig` mapping |
| `internal/agent/personas.go`, `personas_golden_test.go`, `testdata/compose_golden.json` | tool-less persona |
| `cmd/fuse/run.go` | `buildAgentCore` tool-less composition |
| `cmd/fuse/workflow.go` | `childPlan.none`, `agentConfig`, role/worker resolution, role option suffix |
| `cmd/fuse/loop_wiring.go` | `childTools` none branch, `rootTools` no-op |
| `cmd/fuse/tool_less.go` (new) | `loopBaseRegistry`, `loopMCPServers`, `withoutNoTools` |
| `cmd/fuse/runtime_binding.go`, `loop_server.go`, `main.go`, `shell.go`, `research_probe.go` | wiring |
| `cmd/fuse/loop_harness.go` | `SpecFeatures` in `Capabilities` |
| `internal/capabilities/capabilities.go`, `internal/loopconnect/capabilities.go` | `SpecFeatures` |
| `sdk/fuse/capabilities.go`, `sdk/fuse/remote.go` | `SpecFeatures` |
| `sdk/ts/src/index.ts`, `sdk/ts/test/server/main.go`, `sdk/ts/test/capabilities.test.ts` (new) | `specFeatures()` |
| `sdk/README.md`, `sdk/ts/README.md`, `docs/deploying.md` | docs |
| cmd/fuse tests | `loop_harness_test.go`, `instructions_golden_test.go`, `instructions_test.go`, `role_parity_test.go`, `pipeline_role_test.go`, `loop_wiring_test.go`, `workflow_test.go`, new `tool_less_test.go` |

---

## Task 1: Pin today's first request of an ordinary spec (baseline, before any production change)

**Tier:** standard. **Why first:** the acceptance says an ordinary spec's first model call is "byte-identical to today's". That can only be proven against a capture taken before any change.

**Files:** modify `cmd/fuse/instructions_golden_test.go`; create `cmd/fuse/testdata/loop_spec_first_request.golden`.

- [ ] **Step 1: Write the golden test.** Add `TestLoopSpecFirstRequestGolden` next to `TestLoopRootPromptGolden`. It reuses the same `-update-golden` flag.

```go
const loopSpecFirstRequestGolden = "testdata/loop_spec_first_request.golden"

// TestLoopSpecFirstRequestGolden pins, byte for byte, the whole first
// chat-completions request body an ordinary harness spec (no no_tools) sends on
// the production loop-server wiring: model, messages and every tool schema.
// Change 0116 must leave it unchanged. Per-process values are normalized.
func TestLoopSpecFirstRequestGolden(t *testing.T) {
	gw := &recordingGateway{}
	raw := &rawCapture{next: gw}
	deps, _ := harnessServerWith(t, harnessOperator, raw)
	rt := runtime.New(deps)
	one := uint32(1)
	h, err := rt.StartLoop(context.Background(), runtime.LoopConfig{Task: "go", Harness: &harnessv1.HarnessSpec{MaxTurns: &one}})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	_, _ = h.Wait()
	raw.mu.Lock()
	if len(raw.bodies) == 0 {
		raw.mu.Unlock()
		t.Fatal("no model call recorded")
	}
	body := string(raw.bodies[0])
	raw.mu.Unlock()
	body = normalizeRequestBody(body)
	// ...same update/compare shape as TestLoopRootPromptGolden...
}

// normalizeRequestBody replaces per-process values in a request body.
func normalizeRequestBody(s string) string {
	if dir := sessionScratchDir(); dir != "" {
		s = strings.ReplaceAll(s, dir, "<SCRATCH>")
	}
	if home := os.Getenv("HOME"); home != "" {
		s = strings.ReplaceAll(s, home, "<HOME>")
	}
	return s
}
```

If the body has any other per-run value (for example a temp path the JSON escapes differently), normalize it in `normalizeRequestBody` and add a comment naming it. Run the test twice to confirm the result is stable.

- [ ] **Step 2: Create the golden and verify it is stable.**

Run: `go test ./cmd/fuse -run TestLoopSpecFirstRequestGolden -count=1 -update-golden`, then `go test ./cmd/fuse -run 'TestLoopSpecFirstRequestGolden|TestLoopRootPromptGolden' -count=3`.
Expected: PASS three times. `git diff --stat` shows only the new test and the new golden file.

- [ ] **Step 3: Commit.** `test: pin the first request of an ordinary harness spec on the loop server`

---

## Task 2: Proto fields, comments, regeneration and shape-layer tests

**Tier:** standard.

**Files:** `proto/fuse/harness/v1/harness.proto`, `proto/fuse/loop/v1/loop.proto`, regenerated stubs, `harness/options_test.go`, `harness/testdata/shape_cases.json`, `harness/validate_test.go` (`TestValidate_WorkerWithoutTools` only).

- [ ] **Step 1: Write the failing contract tests.**

In `harness/options_test.go`'s `TestRolesFieldContract`, after the `model` block, add:

```go
	noTools := field(role, "no_tools", 5)
	if noTools.Kind() != protoreflect.BoolKind {
		t.Errorf("Role.no_tools kind = %v", noTools.Kind())
	}
	if got := fieldUI(noTools).GetWidget(); got != harnessv1.Widget_WIDGET_TOGGLE {
		t.Errorf("Role.no_tools widget = %v, want WIDGET_TOGGLE", got)
	}
```

Add `TestNoToolsFieldContract`. It uses the same `field`/`rulesOf` helper shape; lift them to file scope if needed. It pins:
- `HarnessSpec.no_tools` is number 16, bool, `WIDGET_TOGGLE`.
- `Worker.no_tools` is number 3, bool, `WIDGET_TOGGLE`.
- `Worker.tools` has `repeated.min_items == 0` and keeps `items.string.min_len == 1`.
- `GetCapabilitiesResponse.spec_features` (from `loopv1`, `internal/loopwire/v1`) is number 9, a repeated string.

Update `harness/testdata/shape_cases.json`. The shape layer no longer refuses a worker with no tools (the semantic layer does, Task 4):

```json
  {
    "name": "worker without tools passes the shape layer",
    "spec": {"workflows": {"wf": {"workers": {"reader": {}, "ok": {"tools": ["read_file"]}}}}},
    "want": []
  },
  {
    "name": "worker with only no_tools",
    "spec": {"workflows": {"wf": {"workers": {"drafter": {"noTools": true}}}}, "noTools": true,
             "roles": {"judge": {"description": "d", "noTools": true}}},
    "want": []
  },
```

If `harness/shape_parity_test.go` keeps any Go-side copy of these cases, update it the same way. The spec says "the Go shape cases are updated together".

Update `TestValidate_WorkerWithoutTools` in `harness/validate_test.go`:
- `ValidateShape(bad)` now returns zero violations.
- `Validate(bad)` returns exactly one violation, at `` workflows["wf"].workers["reader"].tools `` with `RuleID == harness.RuleConfig`. The existing config rule already fires at that path; Task 3 rewords its message.
- Update the test's doc comment to match.

- [ ] **Step 2: Run the tests and confirm they fail.**
Run: `go test ./harness -run 'TestRolesFieldContract|TestNoToolsFieldContract|TestShape|TestValidate_WorkerWithoutTools' -count=1`
Expected: FAIL. The `no_tools` fields do not exist, `min_items` is 1, and `spec_features` does not exist.

- [ ] **Step 3: Edit the protos.**

`HarnessSpec`, after `roles = 15`:

```proto
  // Tool composition for the main agent. false (the default): all tools - the
  // platform tools, the spec's MCP tools and the runtime's internal tools,
  // minus permissions.disabled. true: none - every model call of the loop
  // carries no tools, no MCP server this spec names is started, and the main
  // agent cannot spawn helpers. Every other field stays valid beside it and is
  // simply unreachable. Check GetCapabilitiesResponse.spec_features for
  // "no_tools" first: a server that predates this field drops it and runs the
  // loop with every tool.
  bool no_tools = 16 [(fuse.harness.v1.ui) = {
    label: "No tools"
    help: "The agent answers in plain text and can call no tools at all, including the runtime's internal ones."
    widget: WIDGET_TOGGLE
  }];
```

`Role.tools`: replace the comment with:

```proto
  // A role's tool set has three states. Inherit: tools empty and no_tools
  // false - the helper gets the main agent's tool set. Narrowed: tools names
  // the helper's whole tool set; it can only narrow, the five blackboard tools
  // are always added, and the helper can nest exactly when the list names
  // spawn_agent. None: no_tools true (tools must then be empty) - see no_tools.
  // Names are not checked against the runtime's tools at validation time (tool
  // sets are dynamic).
```

`Role`, new field:

```proto
  // None: the helper is offered no tools at all, not even the blackboard; it
  // answers in plain text through its spawn result. A spawn that names tools
  // for it is refused; a model override still applies. Setting no_tools beside
  // a non-empty tools list is refused.
  bool no_tools = 5 [(fuse.harness.v1.ui) = {
    label: "No tools"
    help: "The helper only thinks and writes: it gets no tools, not even the blackboard."
    widget: WIDGET_TOGGLE
  }];
```

`Worker.tools`: remove `min_items: 1` (keep `items: {string: {min_len: 1}}`), and replace the comment with:

```proto
  // A worker's tool set has two states. Named: tools is the worker's whole
  // allowlist; it inherits nothing from the parent, the five blackboard tools
  // are always added, and the worker can nest exactly when the list names
  // spawn_agent. None: no_tools true (tools must then be empty) - see
  // no_tools. A worker must be one or the other: empty tools without no_tools
  // is refused.
```

`Worker`, new field:

```proto
  // None: the worker is offered no tools at all, not even the blackboard; it
  // answers in plain text through its spawn result. A spawn that names tools
  // for it is refused. Setting no_tools beside a non-empty tools list is
  // refused.
  bool no_tools = 3 [(fuse.harness.v1.ui) = {
    label: "No tools"
    widget: WIDGET_TOGGLE
  }];
```

`loop.proto`, `GetCapabilitiesResponse`, after `limits = 8`:

```proto
  // HarnessSpec features this server implements beyond the fields every
  // server accepts, by name (today: "no_tools"). proto3 drops a field a server
  // does not know, so a server that predates a feature would run the loop
  // without it; check here before StartLoop.
  repeated string spec_features = 9;
```

- [ ] **Step 4: Regenerate.** Run `PATH=$HOME/go/bin:$PATH make proto` (it runs `buf lint` first). Expected: exits 0, and `git status` shows changes under `harness/v1`, `internal/loopwire/v1` and `proto/gen/ts`.

- [ ] **Step 5: Run the tests and confirm they pass.**
Run: `go build ./... && go test ./harness ./internal/loopconnect ./sdk/fuse -count=1`
Expected: PASS. If the TS toolchain is available, also run `make sdk-ts-test` (its `harness.test.ts` reads the same `shape_cases.json`). If node is absent the target fails loudly; record that in the task report and do not skip silently.

- [ ] **Step 6: Commit.** `harness: no_tools on HarnessSpec, Role and Worker; spec_features on GetCapabilities`

---

## Task 3: Config fields, merge and semantic rules

**Tier:** standard.

**Files:** `internal/config/schema.go`, `internal/config/loader.go`, `internal/config/validate.go`; tests in `internal/config/roles_test.go` and/or `validate_test.go` and `loader_test.go`.

- [ ] **Step 1: Write the failing tests.**

Validator (`config.Validate` / its problem list; follow how `roles_test.go` asserts `Problem.Path`):
- `roles: {x: {description: d, tools: [read_file], no_tools: true}}`: one problem at `roles.x.no_tools`, message `names tools and no_tools; choose one`.
- `workflows.wf.workers.w: {tools: [read_file], no_tools: true}`: one problem at `workflows.wf.workers.w.no_tools` with the same message.
- `workflows.wf.workers.w: {no_tools: true}`: no problem.
- `workflows.wf.workers.w: {}`: one problem at `workflows.wf.workers.w.tools`, message containing `must name at least one tool or set no_tools`.
- Top-level `no_tools: true` beside `permissions.disabled`, `mcp_servers`, `roles` (valid), `workflows` (valid) and `pipeline`: no problem.

Loader (`config.FromYAML` and the layered loader, as `loader_test.go` exercises them):
- `no_tools: true` in the home file gives `Config.NoTools == true`. Role `no_tools` and worker `no_tools` are carried into `RoleConfig.NoTools` and `WorkerConfig.NoTools`.
- `no_tools: true` in `.fuse.local.yml` is honored. It only ever removes tools, so it is a tightening, like `instructions`. A later layer that omits it does not clear an earlier `true`.
- Roles from `.fuse.local.yml` stay inert, as today. This is existing behavior; one assertion is enough.

- [ ] **Step 2: Run the tests and confirm they fail.** `go test ./internal/config -run 'NoTools' -count=1`. Expected: FAIL (compile errors on the unknown fields).

- [ ] **Step 3: Implement.**

`schema.go`:
- `RoleConfig` gains `NoTools bool \`yaml:"no_tools"\``.
- `WorkerConfig` gains `NoTools bool \`yaml:"no_tools"\``.
- `Config` gains a `NoTools bool` field, placed after `Instructions`. Its comment: "NoTools composes the main agent with no tools at all (change 0116): no platform, MCP or internal tools, no spawn. Honored from every layer: it only removes tools."
- `rawConfig` gains `NoTools bool \`yaml:"no_tools"\``.

`loader.go`: in the merge, after the `Instructions` block:

```go
	// no_tools only ever removes tools, so it is honored from every layer, and
	// an omitted key never clears a true set by an earlier layer (change 0116).
	if raw.NoTools {
		c.NoTools = true
	}
```

Also check `overlay.go`. If it copies top-level scalars field by field, carry `NoTools` there with the same rule.

`validate.go`: replace the worker loop with:

```go
	for _, wf := range sortedKeys(c.Workflows) {
		workers := c.Workflows[wf].Workers
		for _, w := range sortedKeys(workers) {
			wk, at := workers[w], "workflows."+wf+".workers."+w
			switch {
			case wk.NoTools && len(wk.Tools) > 0:
				add(at+".no_tools", "names tools and no_tools; choose one")
			case !wk.NoTools && len(wk.Tools) == 0:
				add(at+".tools", "must name at least one tool or set no_tools (a worker inherits nothing; omit spawn_agent to keep it from nesting)")
			}
		}
	}
```

In the roles loop, after the tool-name check:

```go
		if r.NoTools && len(r.Tools) > 0 {
			add("roles."+name+".no_tools", "names tools and no_tools; choose one")
		}
```

- [ ] **Step 4: Run the tests and confirm they pass.** `go test ./internal/config -count=1`. Expected: PASS.

- [ ] **Step 5: Commit.** `config: no_tools on config, roles and workers, with conflict rules`

---

## Task 4: Spec mapping, spec validation and digest

**Tier:** standard.

**Files:** `internal/harnessconfig/harnessconfig.go` (+ `harnessconfig_test.go`), `harness/validate_test.go`, `harness/digest_test.go`.

- [ ] **Step 1: Write the failing tests.**

`internal/harnessconfig/harnessconfig_test.go`:
- `TestToConfigMapsNoTools`: a spec with `NoTools: true`, role `judge{Description:"d", NoTools:true}` and worker `wf.drafter{NoTools:true}` maps to `cfg.NoTools`, `cfg.Roles["judge"].NoTools` and `cfg.Workflows["wf"].Workers["drafter"].NoTools`, all true.
- `TestFromConfigCarriesNoTools`: the reverse direction. Extend the existing round-trip tests (`TestRoundTripSpecConfigSpec`, `TestRoundTripConfigSpecConfig`) with these values if they are table-driven.
- `TestSpecPath` cases: `roles.x.no_tools` maps to `` roles["x"].no_tools ``, and `workflows.wf.workers.w.no_tools` maps to `` workflows["wf"].workers["w"].no_tools ``.

`harness/validate_test.go`, new `TestValidateNoTools`. For both `harness.Validate` and `harness.Service{}.ValidateHarness` (the Connect handler, as `TestValidateHarness*` already calls it), assert:
- role with tools and no_tools: one violation at `` roles["x"].no_tools ``, `RuleConfig`, message contains `names tools and no_tools; choose one`.
- worker with tools and no_tools: one violation at `` workflows["wf"].workers["w"].no_tools ``, same message.
- worker with neither: one violation at `` workflows["wf"].workers["w"].tools ``, `RuleConfig`.
- worker with only `no_tools`: valid.
- `HarnessSpec{NoTools: true}` beside `Permissions{Disabled: [...]}`, one remote `McpServers` entry (`MCP_TRANSPORT_HTTP`, `https://mcp.example.com`), `Skills: [{Name: "research"}]`, a role, a workflow with a named worker, and a `Pipeline` block: no violations.

`harness/digest_test.go`, new `TestDigestCoversNoTools`:

```go
func TestDigestCoversNoTools(t *testing.T) {
	// An unset flag leaves the pinned digest unchanged.
	if got := Digest(rolelessSpec()); got != rolelessDigest {
		t.Fatalf("Digest(rolelessSpec()) = %s, want %s", got, rolelessDigest)
	}
	base := Digest(rolesSpec())
	spec := rolesSpec()
	spec.NoTools = true
	if Digest(spec) == base {
		t.Error("flipping HarnessSpec.no_tools leaves the digest unchanged")
	}
	role := rolesSpec()
	for _, r := range role.Roles {
		r.NoTools = true
		break
	}
	if Digest(role) == base {
		t.Error("flipping Role.no_tools leaves the digest unchanged")
	}
	worker := rolesSpec()
	// Give the spec's workflow one worker with tools, digest it, then flip
	// that worker's no_tools and compare.
	...
}
```

Write the worker case concretely. If `rolesSpec()` has no worker, add `Workers: {"w": {Tools: []string{"read_file"}}}` to a copy, take that digest as the base, then set `NoTools` (keep `Tools`; digest coverage does not depend on validity) and assert the digest differs.

- [ ] **Step 2: Run the tests and confirm they fail.** `go test ./internal/harnessconfig ./harness -run 'NoTools|SpecPath' -count=1`. Expected: FAIL (mapping missing; validation reports nothing at `no_tools`).

- [ ] **Step 3: Implement.** In `toDocument`:
- `root.set("no_tools", s.GetNoTools())`
- inside the workers loop, `wd.set("no_tools", wk.GetNoTools())`
- inside the roles loop, `rd.set("no_tools", r.GetNoTools())`

`doc.set` already drops a false bool. In `FromConfig`:
- `s.NoTools = cfg.NoTools`
- `&harnessv1.Worker{Tools: wk.Tools, Model: wk.Model, NoTools: wk.NoTools}`
- `&harnessv1.Role{..., NoTools: r.NoTools}`

`harness.Digest` needs no change (deterministic proto encoding).

- [ ] **Step 4: Run the tests and confirm they pass.** `go test ./internal/harnessconfig ./harness ./internal/harnessresolve -count=1`. Expected: PASS, and `TestDigestOfRolelessSpecIsPinned` is unchanged.

- [ ] **Step 5: Commit.** `harnessconfig: map no_tools; validate and digest cover it`

---

## Task 5: Tool-less persona prompt

**Tier:** economy.

**Files:** `internal/agent/personas.go`, `internal/agent/personas_golden_test.go`, `internal/agent/personas_test.go`, `internal/agent/testdata/compose_golden.json`.

- [ ] **Step 1: Write the failing tests.**

`personas_test.go`:

```go
func TestToolLessPersonaNeverClaimsTools(t *testing.T) {
	for _, p := range []string{"coding", "research", "reasoning", "general", "nope"} {
		got := ToolLessPersonaPrompt(p)
		if strings.Contains(got, "access to") || strings.Contains(got, "file and shell") {
			t.Errorf("%s: tool-less persona claims tools: %q", p, got)
		}
		if !strings.Contains(got, "no tools") || strings.Contains(got, "\n") {
			t.Errorf("%s: want one line stating no tools, got %q", p, got)
		}
	}
	if ToolLessPersonaPrompt("nope") != ToolLessPersonaPrompt("general") {
		t.Error("unknown persona does not fall back to general")
	}
}

func TestComposeToolLessSystemPrompt(t *testing.T) {
	got := ComposeToolLessSystemPrompt("coding", "/no_think", "  BRIEF  ")
	want := "/no_think\n" + ToolLessPersonaPrompt("coding") + "\n\n  BRIEF  "
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if ComposeToolLessSystemPrompt("coding", "", "") != ToolLessPersonaPrompt("coding") {
		t.Error("blank prefix and instructions must add nothing")
	}
}
```

`want` must match how `ComposeSystemPrompt` appends instructions. It checks `strings.TrimSpace(instructions) != ""` but appends `instructions` untrimmed, so keep the same behavior.

`personas_golden_test.go`: extend `composeGoldenCases()` so each persona × prefix also yields a key `p+"|"+pre+"|tool-less"` with the value `ComposeToolLessSystemPrompt(p, pre, "")`.

- [ ] **Step 2: Run the tests and confirm they fail.** `go test ./internal/agent -run 'ToolLess|ComposeSystemPromptGolden' -count=1`. Expected: FAIL (undefined functions).

- [ ] **Step 3: Implement** in `personas.go`:

```go
// toolLessPersonaPrompts is each persona's one-line form for an agent offered
// no tools at all (change 0116): it states that and asks for plain text.
var toolLessPersonaPrompts = map[string]string{
	"coding":    "You are a coding agent. You have no tools: answer directly in plain text.",
	"research":  "You are a research agent. You have no tools: answer directly in plain text.",
	"reasoning": "You are a reasoning agent. You have no tools: think step-by-step and answer directly in plain text.",
	"general":   "You are a general-purpose agent. You have no tools: answer directly in plain text.",
}

// ToolLessPersonaPrompt returns the one-line persona prompt for a tool-less
// agent, falling back to the general one for unknown names.
func ToolLessPersonaPrompt(persona string) string {
	if p, ok := toolLessPersonaPrompts[strings.ToLower(persona)]; ok {
		return p
	}
	return toolLessPersonaPrompts["general"]
}

// ComposeToolLessSystemPrompt is ComposeSystemPrompt for an agent offered no
// tools: the tool-less persona line in place of the persona prompt, and no
// extra block, since every extra block describes a tool.
func ComposeToolLessSystemPrompt(persona, prefix, instructions string) string {
	base := ToolLessPersonaPrompt(persona)
	if prefix != "" {
		base = prefix + "\n" + base
	}
	if strings.TrimSpace(instructions) != "" {
		base += "\n\n" + instructions
	}
	return base
}
```

- [ ] **Step 4: Regenerate the golden; only additions are allowed.** Run `go test ./internal/agent -run TestComposeSystemPromptGolden -update -count=1`, then `git diff internal/agent/testdata/compose_golden.json`. Expected: the only changes are added `...|tool-less` keys, with no existing value changed. Then `go test ./internal/agent -count=1` passes.

- [ ] **Step 5: Commit.** `agent: one-line tool-less persona prompt`

---

## Task 6: `buildAgentCore` composes the tool-less prompt

**Tier:** standard.

**Files:** `cmd/fuse/run.go`, `cmd/fuse/instructions_test.go` (or a new `cmd/fuse/tool_less_test.go`).

- [ ] **Step 1: Write the failing test.** Reuse the setup of `instructionsTestBuild` with `cfg.NoTools = true`. Factor a variant `instructionsTestBuildCfg(t, cfg, mc, extra, instructions)` and have `instructionsTestBuild` call it.

```go
// TestBuildAgentCoreToolLessPrompt: a tool-less config composes the one-line
// tool-less persona plus the brief, and drops every tool-driven extra block,
// on the gateway and the CLI paths alike.
func TestBuildAgentCoreToolLessPrompt(t *testing.T) {
	t.Setenv("FUSE_HITL_MODE", "")
	for _, tc := range []struct{ id, prefix string }{{"cloud/fast", ""}, {"cli/claude", "/no_think"}} {
		cfg := config.Default()
		cfg.NoTools = true // permissions mode left at its default, so the scratch block would otherwise be appended
		a := instructionsTestBuildCfg(t, cfg, model.ModelConfig{ID: tc.id, Persona: "research", SystemPrefix: tc.prefix}, spawnAgentBlock+"EXTRA-BLOCK", "BRIEF")
		want := agent.ComposeToolLessSystemPrompt("research", tc.prefix, "BRIEF")
		if got := a.SystemPrompt(); got != want {
			t.Errorf("%s:\n got: %q\nwant: %q", tc.id, got, want)
		}
	}
}
```

The existing `TestBuildAgentCoreComposesInstructionsOnGatewayPath` and `TestRoleBriefCompositionByteForByte` must stay green unchanged. They are the ordinary-path guard.

- [ ] **Step 2: Run the test and confirm it fails.** `go test ./cmd/fuse -run 'TestBuildAgentCoreToolLessPrompt' -count=1`. Expected: FAIL (it gets the full persona and the extra blocks).

- [ ] **Step 3: Implement.** In `buildAgentCore`, replace the `extra` adjustments and the two identical `ComposeSystemPrompt` calls with one composition before the `cli/` branch, then use `systemPrompt` in both branches:

```go
	// A tool-less agent (change 0116: no_tools on the loop, or a tool-less role
	// or worker child, whose builder passes childPlan.agentConfig) is offered
	// nothing. Every block extra can carry (skills listing, spawn guidance,
	// scratch directory) describes a tool, so it is dropped, and the persona is
	// its one-line tool-less form. The caller's instructions are unchanged.
	var systemPrompt string
	if cfg.NoTools {
		systemPrompt = agent.ComposeToolLessSystemPrompt(mc.Persona, mc.SystemPrefix, instructions)
	} else {
		extra = withoutUnavailableSpawnBlock(extra, cfg, toolReg)
		if scratchBlockWanted(cfg) {
			extra = appendScratchBlock(extra)
		}
		systemPrompt = agent.ComposeSystemPrompt(mc.Persona, mc.SystemPrefix, instructions, extra)
	}
```

Do not change `buildChildAgent`. It takes a caller-supplied system prompt verbatim.

- [ ] **Step 4: Run the tests and confirm they pass.** `go test ./cmd/fuse -run 'Instructions|RoleBrief|ToolLess|LoopRootPromptGolden|LoopSpecFirstRequestGolden|PromptBlocks' -count=1`. Expected: PASS, with both goldens unchanged.

- [ ] **Step 5: Commit.** `fuse: compose the tool-less prompt for a tool-less agent`

---

## Task 7: Child resolution — `childPlan.none`, refusals, empty child registry

**Tier:** premium. Named risk: the shared child path for every binding. A wrong branch either leaks the full parent tool set (`childToolRegistry(base, nil)` is a full clone) or strips tools from ordinary children.

**Files:** `cmd/fuse/workflow.go`, `cmd/fuse/loop_wiring.go`, `cmd/fuse/runtime_binding.go`, `cmd/fuse/loop_server.go`; tests in `cmd/fuse/loop_wiring_test.go` and `cmd/fuse/workflow_test.go`.

- [ ] **Step 1: Write the failing unit tests.**

`workflow_test.go`:
- `resolveWorkerTools(wf with drafter{NoTools:true}, "drafter", nil)` returns `(nil, nil)`.
- The same call with `[]string{"read_file"}` returns an error containing `` worker "drafter" permits no tools ``.
- `resolveRoleTools("judge", RoleConfig{NoTools:true}, nil)` returns `(nil, false, nil)`.
- `resolveRoleTools` with `[]string{"read_file"}` returns an error containing `` role "judge" permits no tools ``.
- `TestResolveWorkerTools_EmptyAllowlistErrors` stays as is ("declares no tools").

`loop_wiring_test.go`: extend the harness fixtures. Add a `judge` role `{Description: "Judges.", NoTools: true}` to the roles that `scopeRoles()` returns, and a `drafter` worker `{NoTools: true}` to the `wf` workflow in `scopeWF()`. If other tests depend on those fixtures' exact contents, add the entries only inside `newWiringHarnessN` instead. New tests:
- `TestChildToolsToolLessRole`: `h.spawn(h.root, agent.SpawnOpts{Label: "j", Task: "t", Role: "judge"})` returns `len(b.reg.Tools()) == 0` and `b.plan.none`, with no blackboard and no `spawn_agent` or `pipeline_run`.
- `TestChildToolsToolLessWorker`: `lead := h.lead()`, then `h.spawn(<lead node>, SpawnOpts{Worker: "drafter", ...})` returns zero tools. In the same workflow, a `reader` worker still gets `read_file` plus the five blackboard tools (`readerSchema`-style assertion on `b.reg`).
- `TestToolLessRoleSpawnRefusesTools`: the root's `spawn_agent` (built through `h.w.rootTools` on a registry; see how existing tests in this file get a spawn tool) with `{"role":"judge","tools":["read_file"]}` returns an error result containing `` role "judge" permits no tools ``. Assert that no node was created (tree node count unchanged), since `checkRequest` refuses before any node exists.
- `TestToolLessRoleOptionDescription`: `h.w.scope.roleOptions(h.root.ID)` gives `judge` a description ending ` (no tools: answers in plain text)`. Roles that are not tool-less keep their description unchanged.

- [ ] **Step 2: Run the tests and confirm they fail.** `go test ./cmd/fuse -run 'ResolveWorkerTools|ResolveRoleTools|ChildToolsToolLess|ToolLessRole' -count=1`. Expected: FAIL.

- [ ] **Step 3: Implement.**

`workflow.go`, `childPlan`:

```go
type childPlan struct {
	tools        []string // effective tool list (nil = inherit all, only when unbound and not none: a bound child always has a non-empty set)
	model        string   // worker or role model pin, "" when none
	bound        bool     // a worker or a role with a non-empty tool set was selected
	none         bool     // a tool-less role or worker (change 0116): the child gets an empty registry, not even the blackboard
	instructions string   // the role brief for the child's instructions slot, "" when none
}

// agentConfig is cfg as this child's agent is built from it: a tool-less
// child composes the tool-less system prompt (change 0116). cfg is a value,
// so the loop's own config is untouched.
func (p childPlan) agentConfig(cfg config.Config) config.Config {
	if p.none {
		cfg.NoTools = true
	}
	return cfg
}
```

`resolveRoleTools`, first branch (it is checked before the inherit branch; validation forbids both being set, and at run time none wins):

```go
	if role.NoTools {
		if len(requested) > 0 {
			return nil, false, fmt.Errorf("role %q permits no tools", name)
		}
		return nil, false, nil
	}
```

Update its doc comment to describe the three states. `resolveWorkerTools`, after the unknown-worker check and before the `len(w.Tools) == 0` check:

```go
	if w.NoTools {
		if len(requested) > 0 {
			return nil, fmt.Errorf("worker %q permits no tools", worker)
		}
		return nil, nil
	}
```

`prepareChild`: in the role branch, return `childPlan{tools: rt, bound: bound, none: role.NoTools, model: role.Model, instructions: role.Instructions}`. In the worker branch:

```go
	if plan.bound {
		wk := act.cfg.Workers[opts.Worker]
		plan.model = wk.Model
		plan.none = wk.NoTools
	}
```

`roleOptions`:

```go
	for _, n := range names {
		desc := s.roles[n].Description
		if s.roles[n].NoTools {
			desc += " (no tools: answers in plain text)" // change 0116: the spawn and pipeline schemas show a text-only helper
		}
		opts = append(opts, tools.RoleOption{Name: n, Description: desc})
	}
```

`loop_wiring.go`, `childTools`, right after `prepareChild`:

```go
	if plan.none {
		// A tool-less role or worker (change 0116): the child is offered
		// nothing, not even the blackboard; it answers through its spawn
		// result. No spawn, blackboard or pipeline wiring runs.
		return tools.NewRegistry(), plan, nil
	}
```

Update `childTools`'s doc comment ("decision 3" paragraph): bound children get the blackboard; a tool-less child gets nothing.

- [ ] **Step 4: Pass `cp.agentConfig(cfg)` at every child builder.** Re-derive the sites: `grep -n "wiring.childTools(" cmd/fuse/*.go`. Expect four: three in `runtime_binding.go` and one in `loop_server.go`. If the count differs, patch every site found and say so in the commit message. In each closure, right after the `childTools` error check, add `childCfg := cp.agentConfig(cfg)` and pass `childCfg` instead of `cfg` to both the `buildChildAgent(...)` and the `buildAgentCore(...)` / `buildAgentWithRendererAndTrace(...)` calls of that closure. Then confirm that `grep -n "childCfg" cmd/fuse/*.go` shows one declaration and two uses per site.

- [ ] **Step 5: Run the tests and confirm they pass.** `go test ./cmd/fuse -run 'Workflow|Wiring|Role|Pipeline|ChildTools|ToolLess|Blackboard' -count=1`. Expected: PASS, including the guard tests `TestEveryChildBuilderUsesSharedWorkflowWiring` and `TestRoleParityTableCoversEveryChildBuilder`.

- [ ] **Step 6: Commit.** `fuse: tool-less roles and workers spawn children with an empty registry`

---

## Task 8: Spawn parity — tool-less role and worker at every child-builder site, and a pipeline step

**Tier:** premium. Gateway-seam proof across five rows; the fixtures are shared with other tests.

**Files:** `cmd/fuse/role_parity_test.go`, `cmd/fuse/pipeline_role_test.go`, new worker-parity test in `cmd/fuse/tool_less_test.go`.

- [ ] **Step 1: Extend the role parity table.**
- `roleParityRoles` gains:
  ```
    drafter:
      description: Drafts text.
      instructions: DRAFTER-BRIEF
      no_tools: true
  ```
- `roleParitySpec()` gains `"drafter": {Description: "Drafts text.", Instructions: "DRAFTER-BRIEF", NoTools: true}`.
- `roleParitySpawns` gains `{"label": "dft", "task": "DFT-TASK", "role": "drafter"}` and `{"label": "df2", "task": "DF2-TASK", "role": "drafter", "model": "house"}`.

In `TestRoleParityAcrossChildBuilders`, per row:
- `DFT-TASK` turn 0 has `len(c.Tools) == 0`.
- Its system prompt has `agent.ToolLessPersonaPrompt(<persona of fast>)` then `DRAFTER-BRIEF` (`assertInOrder`), and contains neither `file and shell tools` nor `blackboard`.
- Its model is `cloud/fast` (no pin).
- `DF2-TASK` model is `cloud/house` (the model override is honored) and it also has zero tools.
- The existing reviewer assertion (role tools plus five blackboard tools) stays. That covers "a narrowed role beside it still gets its tools plus the blackboard".
- The node `dft` has `Role == "drafter"`.

Add a helper `toolLessPersonaFor(t, operatorYAML, alias)` next to `personaPromptFor`.

- [ ] **Step 2: Add a test for a refused tools request on a tool-less role.** In `TestToolLessRoleRefusesToolsAcrossBindings`, use the one-shot local row (`roleParityLocal`-style setup without running the parity spawns) and call the root's `spawn_agent` with `{"role":"drafter","tools":["read_file"]}`. Assert `res.IsError` and that the output contains `` role "drafter" permits no tools ``. For the hosted spec row, drive one root turn that issues that call (a route function like `roleChildRoute`) and assert that the root's turn-1 tool results contain the same text (`toolResults(...)`). Both rows go through `workflowScope.checkRequest`, so two rows are enough.

- [ ] **Step 3: Write the worker parity test** in `cmd/fuse/tool_less_test.go`. Name it `TestToolLessWorkerParityAcrossChildBuilders`. Use a YAML of its own:

  ```
  <wfOperatorYAML models/max_turns/throughput>
  workflows:
    wf:
      pool: {total: 4}
      workers:
        reader: {tools: [read_file]}
        drafter: {no_tools: true}
  ```

  The matching spec has `Workflows: {"wf": {Pool: {Total: 4}, Workers: {"reader": {Tools: ["read_file"]}, "drafter": {NoTools: true}}}}`. From the root, spawn `{"label":"drf","task":"DRF-TASK","workflow":"wf","worker":"drafter"}` and `{"label":"rdr","task":"RDR-TASK","workflow":"wf","worker":"reader"}`. A child that roots `wf` resolves the worker against it in `prepareChild`. Use rows for one-shot, shell, loop-server/operator and loop-server/spec, built exactly as `roleParityRows()` builds them (factor a shared row builder if that keeps both tables in step). Research-probe row: its root is already the `research` workflow root, so build its config with that workflow's workers set to `{drafter: {no_tools: true}, reader: {tools: [read_file]}}`, activate the root as in `TestResearchProbeRefusesNestedRootSpawn`, and spawn `{"worker":"drafter"}` and `{"worker":"reader"}` without `workflow`.

  Per row:
  - `DRF-TASK` turn 0 has zero tools and the tool-less persona line.
  - `RDR-TASK` has `readerSchema` (`read_file` plus the five blackboard tools).

  Pin the table size the way `TestRoleParityTableCoversEveryChildBuilder` does: worker rows == role rows. A new child-builder site then fails here too.

- [ ] **Step 4: Pipeline step.** In `TestPipelineRoleStepAtGateway`, add a step `- name: dft\n    role: drafter\n    prompt: DFT-PIPE-TASK\n` and assert:
  - its turn-0 request has zero tools;
  - it has the tool-less persona line then `DRAFTER-BRIEF`;
  - `pipeline_run` still completes.

- [ ] **Step 5: Run the tests and confirm they pass.** `go test ./cmd/fuse -run 'RoleParity|ToolLess|PipelineRoleStep|NoRolesSpawnSchema' -count=1 -race`. Expected: PASS. These are test-only additions over Task 7's production change. If one fails, fix production code under Task 7's design, not the assertion.

- [ ] **Step 6: Commit.** `test: tool-less role and worker children get zero tools at every child builder`

---

## Task 9: Tool-less loop — loop server and one-shot CLI

**Tier:** premium. Composition-root wiring with a sandbox and MCP lifecycle. A mistake starts MCP servers or a bash pool for a loop that must have none.

**Files:** new `cmd/fuse/tool_less.go`; `cmd/fuse/loop_wiring.go`, `cmd/fuse/loop_server.go`, `cmd/fuse/main.go`, `cmd/fuse/runtime_binding.go`, `cmd/fuse/shell.go`, `cmd/fuse/research_probe.go`; tests in `cmd/fuse/loop_harness_test.go` and `cmd/fuse/tool_less_test.go`.

- [ ] **Step 1: Write the failing loop-server test** in `cmd/fuse/loop_harness_test.go`:

```go
// TestLoopServerRunsAToolLessSpec (change 0116): on the production loop-server
// wiring, a spec with no_tools makes its one model call with no tools key at
// all (no platform, MCP or internal tools), carries the tool-less persona line,
// and starts no MCP server it names; the same server still runs an ordinary
// spec with its full tool set.
func TestLoopServerRunsAToolLessSpec(t *testing.T) {
	var mcpHits atomic.Int32
	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mcpHits.Add(1)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	t.Cleanup(mcpSrv.Close)

	gw := &recordingGateway{}
	raw := &rawCapture{next: gw}
	deps, _ := harnessServerWith(t, harnessOperator, raw)
	rt := runtime.New(deps)
	one := uint32(1)
	spec := &harnessv1.HarnessSpec{
		NoTools:  true,
		MaxTurns: &one,
		McpServers: []*harnessv1.McpServer{{
			Name: "probe", Transport: harnessv1.McpTransport_MCP_TRANSPORT_HTTP, Url: mcpSrv.URL,
		}},
	}
	h, err := rt.StartLoop(context.Background(), runtime.LoopConfig{Task: "go", Harness: spec})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	_, _ = h.Wait()

	// Exactly one call, with no tools / tool_choice key on the wire.
	// (read raw.bodies under raw.mu)
	// - len(bodies) == 1
	// - json.Unmarshal(bodies[0], &map[string]json.RawMessage{}): no "tools", no "tool_choice"
	// - system message == agent.ComposeToolLessSystemPrompt(<persona of cloud/fast>, "", "")
	//   and contains neither "file and shell tools" nor "## Skills" nor "Scratch directory"
	// - mcpHits.Load() == 0

	// The same server, an ordinary spec: the full set, internal tools included.
	// StartLoop(Task: "go", Harness: &HarnessSpec{MaxTurns: &one}); its first
	// request's tool names include bash, read_file, segment_read,
	// codeindex_impact, codeindex_callers, the five blackboard_* tools,
	// spawn_agent and pipeline_run.
}
```

Write the commented assertions as real code. Get the persona for `cloud/fast` the way `personaPromptFor(t, harnessOperator, "fast")` does, but through `agent.ToolLessPersonaPrompt`. The spec MCP URL is loopback over http, which `harnessServerWith` already allows (`AllowHTTPHosts: 127.0.0.1`, private networks not denied). If the resolver still refuses it, look at how `loop_server_mcp_test.go` declares a spec MCP server and copy that.

- [ ] **Step 2: Write the failing CLI test** in `cmd/fuse/tool_less_test.go`:

```go
// TestOneShotNoToolsSendsNoTools (change 0116): `fuse "<task>"` with
// no_tools: true in config makes its first model call with no tools key, the
// tool-less persona line, and starts no configured MCP server.
func TestOneShotNoToolsSendsNoTools(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// count hits on an http MCP server named in config
	// write ~/.fuse/config.yml:
	//   no_tools: true
	//   mcp_servers:
	//     - {name: probe, transport: http, url: <mcpSrv.URL>}
	// gateway: an httptest server recording raw bodies and answering
	//   {"choices":[{"message":{"role":"assistant","content":"DONE"}}]};
	//   t.Setenv("LLM_GATEWAY_URL", srv.URL); t.Setenv("LLM_GATEWAY_KEY", "tkn")
	var out, errb bytes.Buffer
	if code := run([]string{"--model", "deepseek-flash", "say hi"}, &out, &errb); code != 0 {
		t.Fatalf("run exit=%d stderr=%s", code, errb.String())
	}
	// first body: no "tools" key, no "tool_choice" key; system message starts with
	// agent.ToolLessPersonaPrompt(<persona of deepseek-flash in config.Default()'s registry>)
	// and does not contain "file and shell tools"; MCP hits == 0.
}
```

This follows the existing `TestOneShotEntryPointObservesTheRootTurn` pattern (HOME temp dir, `LLM_GATEWAY_URL`, `run(...)`). If an http MCP server in the home config needs extra fields to load, check `internal/config` MCP tests for the minimal valid form.

- [ ] **Step 3: Write a failing test for the helpers.** In `tool_less_test.go`:
- `loopBaseRegistry(cfg{NoTools:true}, build)` returns a registry with zero tools and never calls `build` (use a flag).
- `loopBaseRegistry(cfg{}, build)` returns `build()`.
- `loopMCPServers` returns nil for `NoTools`, and `cfg.MCPServers` otherwise.
- `withoutNoTools(cfg{NoTools:true}, &buf, "shell")` returns `NoTools == false` and writes one warning line naming `shell`. With `NoTools` false it writes nothing.
- `loopWiring{cfg: cfg{NoTools:true}, ...}.rootTools(reg, root)` leaves `reg` empty. Reuse `newWiringHarness` and set `h.w.cfg.NoTools = true`.

- [ ] **Step 4: Run the tests and confirm they fail.** `go test ./cmd/fuse -run 'ToolLessSpec|OneShotNoTools|LoopBaseRegistry|LoopMCPServers|WithoutNoTools|RootToolsToolLess' -count=1`. Expected: FAIL.

- [ ] **Step 5: Implement.**

New `cmd/fuse/tool_less.go`:

```go
package main

import (
	"fmt"
	"io"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/tools"
)

// loopBaseRegistry returns the base registry a loop's root is built on
// (change 0116). A tool-less config (no_tools) yields an empty registry and
// build never runs; otherwise it is build(). The one-shot CLI and the loop
// server both compose through here, so the two entry points cannot drift.
func loopBaseRegistry(cfg config.Config, build func() *tools.Registry) *tools.Registry {
	if cfg.NoTools {
		return tools.NewRegistry()
	}
	return build()
}

// loopMCPServers is the MCP servers a loop starts: none for a tool-less
// config, whose model is offered no tools to call them with.
func loopMCPServers(cfg config.Config) []config.MCPServerConfig {
	if cfg.NoTools {
		return nil
	}
	return cfg.MCPServers
}

// withoutNoTools clears no_tools for an entry point that always runs with
// its tools (the interactive shell, the research probe), saying so once on w,
// so its registry and its system prompt agree.
func withoutNoTools(cfg config.Config, w io.Writer, entry string) config.Config {
	if !cfg.NoTools {
		return cfg
	}
	fmt.Fprintf(w, "warning: no_tools applies to one-shot runs and the loop server; %s runs with its tools\n", entry)
	cfg.NoTools = false
	return cfg
}
```

`loop_wiring.go`, `rootTools`, first lines:

```go
	if w.cfg.NoTools {
		return // a tool-less loop (change 0116): no spawn_agent, blackboard or pipeline_run
	}
```

`loop_server.go`, `factory`:
- `NewToolRegistry` becomes `return loopBaseRegistry(cfg, func() *tools.Registry { <today's body, with mcp.NewManager(loopMCPServers(cfg), loopReg, mcpOpts...)> })`.
- In `BuildAgent`, inside `if loopToolReg != nil { ... }`, keep `mintAudit.bind` and `emitMCPServerEvents`. Wrap only the bash rebind and `installSandboxLoopHooks` in `if !cfg.NoTools { ... }`, with the comment "a tool-less loop has no bash and no sandbox pool (change 0116)".
- `teardown` is already safe on an empty registry: no manager, unbind tolerates a missing binding, and `ReleaseSandboxes` finds no bash.

`main.go` (one-shot): `toolReg := loopBaseRegistry(cfg, func() *tools.Registry { return defaultToolRegistry(sb, cfg.Research, skillSet.Lookup) })`.

`runtime_binding.go`, `buildOneShotRuntimeDeps`: `mcp.NewManager(loopMCPServers(cfg), toolReg, mcpOpts...)`.

`shell.go` and `research_probe.go`: right after their config is resolved and before any registry or agent is built, `cfg = withoutNoTools(cfg, stderr, "the shell")` and `cfg = withoutNoTools(cfg, stderr, "the research probe")`. Use each entry point's existing stderr writer.

Then re-grep the composition roots: `grep -n "defaultToolRegistry(\|mcp.NewManager(" cmd/fuse/*.go | grep -v _test`. Confirm every agent-loop entry point either goes through `loopBaseRegistry`/`loopMCPServers` (one-shot, loop server) or clears `NoTools` (shell, research probe). Also check `loop_serve_net.go`: it builds the operator base registry that the factory clones, so the factory's `loopBaseRegistry` already covers it. Leave `mcp_server.go` alone: it serves tools and builds no agent loop.

- [ ] **Step 6: Run the tests and confirm they pass.** `go test ./cmd/fuse -run 'ToolLess|OneShotNoTools|LoopBaseRegistry|LoopMCPServers|WithoutNoTools|LoopServer|LoopHarness|LoopRootPromptGolden|LoopSpecFirstRequestGolden|Teardown|MCP' -count=1`. Expected: PASS. `TestLoopSpecFirstRequestGolden` (Task 1) passing unchanged is the proof that an ordinary spec's first request is byte-identical.

- [ ] **Step 7: Commit.** `fuse: a tool-less loop offers no tools and starts no MCP server`

---

## Task 10: Capabilities advertise `spec_features`; both SDKs expose it

**Tier:** standard.

**Files:** new `harness/features.go` (+ test); `internal/capabilities/capabilities.go`; `internal/loopconnect/capabilities.go` (+ `capabilities_test.go`); `cmd/fuse/loop_harness.go` (+ `loop_capabilities_test.go`); `sdk/fuse/capabilities.go`, `sdk/fuse/remote.go` (+ `capabilities_test.go`); `sdk/ts/src/index.ts`, `sdk/ts/test/server/main.go`, new `sdk/ts/test/capabilities.test.ts`.

- [ ] **Step 1: Write the failing tests.**
- `harness`: `TestSpecFeatures`. `SpecFeatures()` equals `[]string{SpecFeatureNoTools}`, `SpecFeatureNoTools == "no_tools"`, and the result is a fresh slice (mutating it does not change the next call).
- `internal/loopconnect/capabilities_test.go`: in `TestGetCapabilitiesMapsEveryField`, set `SpecFeatures: []string{"no_tools"}` on the source and assert `resp.Msg.GetSpecFeatures()`. In `TestToProtoCapabilitiesClampsAndCopies`, assert that the slice is copied.
- `cmd/fuse/loop_capabilities_test.go`: `TestLoopServerHostAdvertisesItsCapabilities` asserts `SpecFeatures` contains `no_tools`. Add one assertion to `TestServeNetMountsTheCapabilitySource` (it goes over the real `loop-serve-net` wire) that `spec_features` lists `no_tools`.
- `sdk/fuse/capabilities_test.go`: `TestRemoteCapabilities` asserts `caps.SpecFeatures` equals `[]string{"no_tools"}` from a server that advertises it.
- TS, `sdk/ts/test/capabilities.test.ts`:

```ts
import { test } from "node:test";
import assert from "node:assert/strict";
import { createConnectTransport } from "@connectrpc/connect-node";
import { createClient } from "../src/index.js";
import { spawnGoServer } from "./helpers.js";

test("specFeatures lists no_tools from a server that advertises it", async () => {
  const srv = await spawnGoServer("./test/server");
  try {
    const client = createClient({ baseUrl: srv.url, credentials: { token: "", tenant: "" },
      transport: createConnectTransport({ baseUrl: srv.url, httpVersion: "1.1" }) });
    assert.deepEqual(await client.specFeatures(), ["no_tools"]);
  } finally { srv.stop(); }
});

test("specFeatures is empty from a server that does not answer GetCapabilities", async () => {
  const srv = await spawnGoServer("./test/server", { FUSE_TEST_PREDATING: "1" });
  // same client; expect []
});
```

Match the credentials and transport construction to what `remote.test.ts` / `startharness.test.ts` already do.

- [ ] **Step 2: Run the tests and confirm they fail.** `go test ./harness ./internal/loopconnect ./sdk/fuse ./cmd/fuse -run 'SpecFeatures|Capabilit' -count=1`. Expected: FAIL.

- [ ] **Step 3: Implement.**

`harness/features.go`:

```go
package harness

// SpecFeatureNoTools names HarnessSpec.no_tools (and Role.no_tools,
// Worker.no_tools) in GetCapabilitiesResponse.spec_features. proto3 drops a
// field a server does not know, so a caller checks for it before StartLoop.
const SpecFeatureNoTools = "no_tools"

// SpecFeatures lists the HarnessSpec features this runtime implements beyond
// the fields every server accepts. It returns a fresh slice.
func SpecFeatures() []string { return []string{SpecFeatureNoTools} }
```

- `internal/capabilities.Capabilities` gains a `SpecFeatures []string` field with the comment "SpecFeatures are the HarnessSpec features this server implements (harness.SpecFeatures)".
- `cmd/fuse/loop_harness.go`, `Capabilities`: `SpecFeatures: harness.SpecFeatures()`.
- `internal/loopconnect/capabilities.go`, `toProtoCapabilities`: `SpecFeatures: slices.Clone(c.SpecFeatures)`.
- `sdk/fuse/capabilities.go`: `Capabilities` gains a `SpecFeatures []string` field with the comment "SpecFeatures are the HarnessSpec features the server implements, by name (harness.SpecFeatureNoTools). A server that predates a feature drops the field and runs the loop without it, so check here before StartLoop".
- `sdk/fuse/remote.go`, `fromProtoCapabilities`: `SpecFeatures: append([]string(nil), m.GetSpecFeatures()...)`.

TS `sdk/ts/src/index.ts`: add to `Client`:

```ts
  /**
   * specFeatures lists the HarnessSpec features the server implements beyond the fields every
   * server accepts (today "no_tools"). A server that predates a feature drops the field and runs
   * the loop without it, so check before startLoop. A server that does not answer
   * GetCapabilities resolves to [] (assume no such feature).
   */
  specFeatures(): Promise<string[]>;
```

Implement it in `createClient` with the existing Connect client and tenant: call `getCapabilities({ tenant })` and return `[...resp.specFeatures]`. On `ConnectError` with `Code.Unimplemented`, return `[]`. Rethrow any other error the way the other methods map errors.

TS test server `sdk/ts/test/server/main.go`:
- `handler := loopconnect.NewHandler(f).WithKeepalive(time.Hour).WithCapabilities(staticCaps{})`, where `staticCaps.Capabilities` returns `capabilities.Capabilities{SpecFeatures: harness.SpecFeatures()}`.
- `predatingHandler` gains a `GetCapabilities` that returns `connect.NewError(connect.CodeUnimplemented, ...)`.

- [ ] **Step 4: Run the tests and confirm they pass.** `go test ./harness ./internal/capabilities ./internal/loopconnect ./sdk/fuse ./cmd/fuse -run 'SpecFeatures|Capabilit' -count=1`, then `make sdk-ts-test`. Expected: PASS. If node is not on PATH, `make sdk-ts-test` fails loudly; report that, do not mark it green.

- [ ] **Step 5: Commit.** `capabilities: advertise spec_features (no_tools); Go and TS SDKs expose it`

---

## Task 11: Docs

**Tier:** economy.

**Files:** `sdk/README.md`, `sdk/ts/README.md`, `docs/deploying.md` (only where it lists what `GetCapabilities` advertises).

- [ ] **Step 1: `sdk/README.md`.** In the `Capabilities(ctx)` section (around the `CarriedSkillsAllowed` text), add a bullet: `SpecFeatures` lists the HarnessSpec features the server implements. Check `slices.Contains(caps.SpecFeatures, harness.SpecFeatureNoTools)` before sending a spec with `NoTools`; an older server would drop the field and run the loop with every tool. Add a short paragraph on `no_tools` explaining:
  - the three role states (inherit, narrowed, none) and the two worker states (named, none);
  - that a tool-less loop or child sends no tools at all, blackboard included;
  - that a spawn naming tools for a tool-less role or worker is refused.
- [ ] **Step 2: `sdk/ts/README.md`.** Document `specFeatures()` and its `[]` result from a server that does not answer `GetCapabilities`.
- [ ] **Step 3: `docs/deploying.md`.** Where it lists what `GetCapabilities` advertises, add `spec_features`.
- [ ] **Step 4: Review the wording.** No named host application anywhere; caller-facing text says "callers" or "a client". Check with `git diff -- '*.md'` read-through.
- [ ] **Step 5: Commit.** `docs: no_tools and spec_features`

---

## Task 12: Full gate

**Tier:** standard.

- [ ] **Step 1:** `PATH=$HOME/go/bin:$PATH make proto && git status --porcelain`. Expected: nothing changes; the committed stubs match the protos.
- [ ] **Step 2:** `make test`. Expected: PASS.
- [ ] **Step 3:** `make sdk-ts-test`. Expected: PASS, or a loud "node required" failure that is reported as an environment gap, not hidden.
- [ ] **Step 4:** `go test -race ./cmd/fuse -run 'RoleParity|ToolLess|PipelineRoleStep|LoopServerRunsAToolLessSpec' -count=1`. Expected: PASS.
- [ ] **Step 5: Acceptance walk.** Tick each spec acceptance criterion against a named test:
  - no tools key on the loop server and in the CLI: `TestLoopServerRunsAToolLessSpec`, `TestOneShotNoToolsSendsNoTools`
  - no MCP server started: both tests above, MCP hit counters
  - digest unchanged and ordinary first call byte-identical: `TestDigestCoversNoTools`, `TestLoopSpecFirstRequestGolden`
  - role and worker zero tools at every site, plus the pipeline: `TestRoleParityAcrossChildBuilders`, `TestToolLessWorkerParityAcrossChildBuilders`, `TestPipelineRoleStepAtGateway`
  - refusals and model override: `TestToolLessRoleRefusesToolsAcrossBindings`, `TestChildToolsToolLessWorker`, `DF2-TASK`
  - narrowed role and named worker unchanged: reviewer and reader assertions
  - validation paths and TS shape parity: `TestValidateNoTools`, `shape_cases.json`
  - spec-level `no_tools` beside other fields validates: `TestValidateNoTools`
  - digest flips: `TestDigestCoversNoTools`
  - `spec_features`: Task 10 tests
  - persona: Tasks 5, 6, 8 and 9 assertions
  - proto comments: Task 2 text
- [ ] **Step 6:** Read the full branch diff and every commit message once more. No named host application, no live-provider endpoints in tests.

---

## Notes for the builder

- **Structured return (`expects`):** a spawn that sets `expects` gets the agent-level `return_result` tool, which is the structured-return channel and not a registry tool. That stays true for a tool-less child that a caller explicitly spawns with `expects`. The spec's "zero tools" tests spawn without `expects`, and this plan does not change `SetExpects`.
- **`cli/` models:** the tool-less system prompt is composed on the `cli/` path too (Task 6 covers both). The CLI adapter's own subprocess tool surface is outside this change.
- **Unreachable but valid:** roles with tools under a tool-less loop, `permissions.disabled`, `skills`, `workflows` and `pipeline` beside `no_tools` all validate and are simply never reached, because the loop is never offered `spawn_agent`, `skill` or `pipeline_run`.
- **Empty `Role.tools` still means inherit.** Do not change that.
