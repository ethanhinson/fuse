<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0115 — Pipeline steps name a role; roles in the synthesis palette](../../changes/archive/2026-11-02-0115-pipeline-steps-name-a-role-roles-in-the-synthesis-palette.md)**
<!-- docket:backlink:end -->

# Pipeline steps name a role; roles in the synthesis palette: implementation plan

> **For agentic workers:** this plan is executed by `docket-build`, one task at a time, each task ending in one commit. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A pipeline step can name a top-level `role` (instead of a workflow `worker`). A role step's child is built through the same role resolution that `spawn_agent` uses (change 0114). Pipeline synthesis and the `pipeline_run` tool description learn about roles, and nothing changes when no roles are defined.

**Architecture:** `internal/pipeline` gains `Step.Role`. A step that names both `role` and `worker` is a parse error in `fromWire`. A new `pipeline.CheckRoles` refuses an unknown role. The engine forwards `Role` in `agent.SpawnOpts`. From there the existing `workflowScope.prepareChild` in `cmd/fuse/workflow.go` already resolves a role child (`checkRole`, `resolveRoleTools`, the role's model pin and brief). It already says it covers "paths that call Spawner.Spawn directly (pipeline steps)", so **no role resolution is duplicated**. In `cmd/fuse`, the authored seam runs `CheckRoles` against the defined roles. The synthesis seam builds a per-call role palette (`name — description`) next to its per-call worker palette. `pipeline_run` gets a `WithRoles` that adds a role paragraph to its description. `refuseUnboundWorkerSteps` keeps refusing only `worker` steps, because roles are global.

**Tech stack:** Go. Test suite: `make test` (runs `observability-validate` then `go test ./...`).

**Spec:** `docs/superpowers/specs/2026-11-02-pipeline-steps-name-a-role-roles-in-the-synthesis-palette-design.md`

**Constraints (apply to every task):**
- Keep the change self-contained. No commit message, comment, or test may mention any downstream consumer application.
- With no roles defined, these must stay byte-identical to today: the synthesis system prompt, the synthesis Expects schema, the `pipeline_run` description and parameters, and the `spawn_agent` schema. Goldens captured **before** the code change prove this (Tasks 3 and 4).
- Any live check uses a cheap gateway model (glm or kimi via `localhost:4000`), never an Anthropic model.
- Learnings applied:
  - `patch-every-cloned-child-builder`: every child-builder site already reaches pipeline wiring through `loopWiring.wirePipeline`, so the wiring change lands once (Task 8). Confirm by grep at build time.
  - `verify-tool-loop-at-gateway-seam` and `security-knob-inert-at-composition-root`: the acceptance test (Task 9) drives a real binding's composition root against a scripted gateway double and asserts what the model actually receives: system prompt, model, and tools.
  - `yaml-plain-scalar-colon-space`: test YAML in this plan uses no unquoted `: ` inside scalars.

---

## File map

| File | Change |
|---|---|
| `internal/pipeline/pipeline.go` | `Step.Role` field |
| `internal/pipeline/parse.go` | `wireStep.Role`, role+worker parse error, `CheckRoles` |
| `internal/pipeline/engine.go` | `spawnOnce` forwards `Role` |
| `internal/pipeline/synthesize.go` | `SynthRole`, `SynthContext.Roles`, role palette in prompt, role field in schema description and Expects schema (only with roles), `CheckRoles` in the re-ask loop |
| `internal/pipeline/testdata/*.golden` | new no-roles synthesis goldens |
| `internal/tools/spawn_agent.go` | shared `normalizeRoleOptions`; role param note "with `workflow`, the role must be one that workflow offers" |
| `internal/tools/pipeline_run.go` | `roles` field, `WithRoles`, role paragraph in `Description()` |
| `internal/tools/testdata/pipeline_run_schema*.golden` | new no-roles goldens |
| `cmd/fuse/workflow.go` | extract `sortedRoleNames` / `workflowRoleNames` package funcs (pure refactor; scope methods delegate) |
| `cmd/fuse/run.go` | `makePipelineRunFn` gains `roles []string` and calls `CheckRoles`; `refuseUnboundWorkerSteps` doc; `synthRolePalette`; `makePipelineSynthFn` passes the role palette |
| `cmd/fuse/loop_wiring.go` | `wirePipeline` passes role names and `WithRoles(scope.roleOptions(node.ID))` |
| `cmd/fuse/loop_wiring_test.go`, `cmd/fuse/pipeline_gateway_e2e_test.go` | call-site updates plus new seam tests |
| `cmd/fuse/pipeline_role_test.go` | new gateway acceptance test |

---

### Task 1: `role` on the pipeline step; role+worker and unknown role are parse errors

Suggested tier: economy

**Files:**
- Modify: `internal/pipeline/pipeline.go`, `internal/pipeline/parse.go`
- Test: `internal/pipeline/parse_test.go`

- [ ] **Step 1: Write failing tests** (append to `internal/pipeline/parse_test.go`; add the `encoding/json` and `strings` imports if missing)

```go
// TestParseRoleField (change 0115): a step's optional role decodes from YAML
// and JSON alike.
func TestParseRoleField(t *testing.T) {
	for _, def := range []string{
		"name: p\nsteps:\n  - name: rev\n    role: reviewer\n    prompt: check it\n",
		`{"name":"p","steps":[{"name":"rev","role":"reviewer","prompt":"check it"}]}`,
	} {
		p, err := Parse([]byte(def))
		if err != nil {
			t.Fatalf("Parse(%q): %v", def, err)
		}
		if got := p.Steps[0].Role; got != "reviewer" {
			t.Fatalf("Parse(%q) role = %q, want reviewer", def, got)
		}
	}
}

// TestParseRoleAndWorkerIsError: naming both role and worker on one step is a
// parse error naming the step.
func TestParseRoleAndWorkerIsError(t *testing.T) {
	_, err := Parse([]byte("name: p\nsteps:\n  - name: s\n    role: reviewer\n    worker: reader\n"))
	if err == nil {
		t.Fatal("role+worker step parsed without error")
	}
	want := `pipeline: step "s": role "reviewer" and worker "reader" cannot be combined; name one`
	if err.Error() != want {
		t.Fatalf("err = %q, want %q", err, want)
	}
}

// TestCheckRoles: a role step must name a known role; steps without a role
// always pass.
func TestCheckRoles(t *testing.T) {
	ok := &Pipeline{Name: "p", Steps: []Step{{Name: "a"}, {Name: "b", Role: "reviewer"}}}
	if err := CheckRoles(ok, []string{"reviewer", "writer"}); err != nil {
		t.Fatalf("known role refused: %v", err)
	}
	if err := CheckRoles(&Pipeline{Name: "p", Steps: []Step{{Name: "a"}}}, nil); err != nil {
		t.Fatalf("role-free pipeline refused with no roles: %v", err)
	}
	bad := &Pipeline{Name: "p", Steps: []Step{{Name: "a"}, {Name: "b", Role: "nope"}}}
	err := CheckRoles(bad, []string{"reviewer", "writer"})
	if want := `pipeline: step "b": unknown role "nope" (available: reviewer, writer)`; err == nil || err.Error() != want {
		t.Fatalf("unknown role err = %v, want %q", err, want)
	}
	err = CheckRoles(bad, nil)
	if want := `pipeline: step "b": unknown role "nope" (available: none configured)`; err == nil || err.Error() != want {
		t.Fatalf("no-roles err = %v, want %q", err, want)
	}
}

// TestStepRoleOmittedWhenEmpty: a role-free step marshals with no role key, so
// a published pipeline.plan is unchanged when roles are unused.
func TestStepRoleOmittedWhenEmpty(t *testing.T) {
	b, err := json.Marshal(Step{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "role") {
		t.Fatalf("role-free step marshals a role key: %s", b)
	}
}
```

- [ ] **Step 2: Run and confirm failure**

Run: `go test ./internal/pipeline -run 'TestParseRole|TestCheckRoles|TestStepRoleOmitted' -count=1`
Expected: compile failure (`Step.Role` and `CheckRoles` are undefined).

- [ ] **Step 3: Implement**

`internal/pipeline/pipeline.go`: add to `Step`, directly after `Worker`:

```go
	// Role names a top-level role (change 0115): the step's child is built from
	// the role's brief, tool set and model exactly as a spawn_agent role child.
	// Mutually exclusive with Worker. Unlike a worker, a role needs no workflow.
	Role string `yaml:"role,omitempty" json:"role,omitempty"`
```

`internal/pipeline/parse.go`:
- add `Role string \`yaml:"role" json:"role"\`` to `wireStep` after `Worker`;
- in `fromWire`, before `parseOnError`:

```go
		if ws.Role != "" && ws.Worker != "" {
			return nil, fmt.Errorf("pipeline: step %q: role %q and worker %q cannot be combined; name one", ws.Name, ws.Role, ws.Worker)
		}
```

- set `Role: ws.Role,` in the `Step{...}` literal;
- add:

```go
// CheckRoles refuses a pipeline whose step names a role outside known (change
// 0115). known lists the defined role names in the order the error should show
// them; empty means no role is defined, so any role step is refused. A step
// with no role always passes. Roles are global, so this needs no workflow.
func CheckRoles(p *Pipeline, known []string) error {
	set := make(map[string]bool, len(known))
	for _, k := range known {
		set[k] = true
	}
	for _, s := range p.Steps {
		if s.Role == "" || set[s.Role] {
			continue
		}
		list := "none configured"
		if len(known) > 0 {
			list = strings.Join(known, ", ")
		}
		return fmt.Errorf("pipeline: step %q: unknown role %q (available: %s)", s.Name, s.Role, list)
	}
	return nil
}
```

- [ ] **Step 4: Run and confirm pass**

Run: `go test ./internal/pipeline -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/pipeline/pipeline.go internal/pipeline/parse.go internal/pipeline/parse_test.go
git commit -m "feat(pipeline): steps may name a role; role+worker and unknown roles are parse errors"
```

---

### Task 2: The engine forwards a step's role to the spawn

Suggested tier: economy

**Files:**
- Modify: `internal/pipeline/engine.go` (`spawnOnce`)
- Test: `internal/pipeline/engine_test.go`

- [ ] **Step 1: Write the failing test** (append; add the `reflect` import if missing)

```go
// TestEngineRoleStepForwardsRole (change 0115): a role step spawns with
// SpawnOpts.Role set (every fanout instance alike); a plain step spawns with
// none. Role resolution itself happens in the child builder (cmd/fuse).
func TestEngineRoleStepForwardsRole(t *testing.T) {
	tree := agent.NewAgentTree("root", "m")
	bb := agent.NewBlackboard(tree)
	var mu sync.Mutex
	got := map[string]string{}
	sp := fakeSpawner(t, tree, func(opts agent.SpawnOpts) (string, error) {
		mu.Lock()
		got[opts.Label] = opts.Role
		mu.Unlock()
		return "ok", nil
	})
	p := &Pipeline{Name: "p", Steps: []Step{
		{Name: "rev", Role: "reviewer", Prompt: "x"},
		{Name: "plain", Prompt: "y"},
		{Name: "fan", Role: "writer", Prompt: "z", Fanout: 2},
	}}
	if _, err := Run(context.Background(), p, sp, bb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := map[string]string{"rev": "reviewer", "plain": "", "fan-0": "writer", "fan-1": "writer"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("spawned roles = %v, want %v", got, want)
	}
}
```

- [ ] **Step 2: Run and confirm failure**

Run: `go test ./internal/pipeline -run TestEngineRoleStepForwardsRole -count=1`
Expected: FAIL. `rev`, `fan-0` and `fan-1` show role `""`.

- [ ] **Step 3: Implement.** In `spawnOnce`, add `Role: s.Role,` to the `agent.SpawnOpts` literal after `Worker: s.Worker,`.

- [ ] **Step 4: Run and confirm pass**

Run: `go test ./internal/pipeline -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/pipeline/engine.go internal/pipeline/engine_test.go
git commit -m "feat(pipeline): role steps spawn with their role"
```

---

### Task 3: Roles in the synthesis palette (byte-identical without roles)

Suggested tier: standard

**Files:**
- Modify: `internal/pipeline/synthesize.go`
- Create: `internal/pipeline/testdata/synth_prompt_noroles.golden`, `internal/pipeline/testdata/synth_schema_noroles.golden`
- Test: `internal/pipeline/synthesize_test.go`

- [ ] **Step 1: Capture the no-roles goldens from the UNCHANGED code.** Append to `synthesize_test.go` (add the `encoding/json`, `flag`, `os` and `path/filepath` imports as needed):

```go
var updateSynthGolden = flag.Bool("update-synth-golden", false, "rewrite the no-roles synthesis goldens")

// synthNoRolesContext is the palette the no-roles goldens are captured with.
var synthNoRolesContext = SynthContext{Workers: []string{"searcher", "ranker"}, Tools: []string{"read_file", "web_search"}}

func checkSynthGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateSynthGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update-synth-golden to create it): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("%s changed:\n got: %q\nwant: %q", name, got, want)
	}
}

// TestSynthesisNoRolesGolden (change 0115): with no roles, the synthesis
// system prompt and the Expects schema stay byte-identical to before roles.
func TestSynthesisNoRolesGolden(t *testing.T) {
	checkSynthGolden(t, "synth_prompt_noroles.golden", []byte(synthesisSystemPrompt(synthNoRolesContext)))
	b, err := json.MarshalIndent(pipelineJSONSchema(nil), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	checkSynthGolden(t, "synth_schema_noroles.golden", append(b, '\n'))
}
```

The golden must reflect today's code, so make it compile against the current signature first. Temporarily write `pipelineJSONSchema()` (no argument), then run:

`go test ./internal/pipeline -run TestSynthesisNoRolesGolden -update-synth-golden -count=1`

Confirm that both files exist under `internal/pipeline/testdata/`, then change the call back to `pipelineJSONSchema(nil)`. Step 3 introduces that signature, so from here the test fails to compile until Step 3. Commit the goldens together with this task.

- [ ] **Step 2: Write the failing role tests** (append)

```go
// TestSynthesisPromptListsRoles: with roles, the prompt lists each role as
// "name — description" (description on one line) and the schema description
// and Expects schema gain the role field.
func TestSynthesisPromptListsRoles(t *testing.T) {
	sc := synthNoRolesContext
	sc.Roles = []SynthRole{{Name: "reviewer", Description: "Reviews code\nfor bugs."}, {Name: "writer", Description: "Writes prose."}}
	got := synthesisSystemPrompt(sc)
	for _, want := range []string{
		"Available roles (set a step's role to one of these instead of worker):\n- reviewer — Reviews code for bugs.\n- writer — Writes prose.\n",
		`"role": string (one of the available roles; never together with worker),`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q:\n%s", want, got)
		}
	}
	props := pipelineJSONSchema([]string{"reviewer", "writer"})["properties"].(map[string]any)["steps"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	if _, ok := props["role"]; !ok {
		t.Fatal("Expects schema lacks the role field with roles defined")
	}
}

// TestSynthesizeReasksOnUnknownRole: a synthesized step naming a role outside
// the palette is rejected and fed back; a corrected reply is accepted.
func TestSynthesizeReasksOnUnknownRole(t *testing.T) {
	bad := `{"name":"p","workflow":"w","steps":[{"name":"a","role":"ghost","prompt":"go"}]}`
	good := `{"name":"p","workflow":"w","steps":[{"name":"a","role":"reviewer","prompt":"go"}]}`
	var calls int32
	var saw string
	sp := scriptedSpawner(t, []string{bad, good}, &saw, &calls)
	sc := SynthContext{Roles: []SynthRole{{Name: "reviewer", Description: "Reviews."}}}
	p, err := Synthesize(context.Background(), "goal", sc, sp, Caps{})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if p.Steps[0].Role != "reviewer" || atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("got role %q after %d calls, want reviewer after 2", p.Steps[0].Role, calls)
	}
	if !strings.Contains(saw, `unknown role "ghost" (available: reviewer)`) {
		t.Fatalf("re-ask did not feed back the unknown role: %q", saw)
	}
}

// TestSynthesizeNoRolesRefusesRoleStep: with no roles defined, a synthesized
// role step is an unknown role (re-asked, then a loud failure at the cap).
func TestSynthesizeNoRolesRefusesRoleStep(t *testing.T) {
	bad := `{"name":"p","workflow":"w","steps":[{"name":"a","role":"ghost","prompt":"go"}]}`
	sp := scriptedSpawner(t, []string{bad}, nil, nil)
	_, err := Synthesize(context.Background(), "goal", SynthContext{}, sp, Caps{MaxAttempts: 2})
	if err == nil || !strings.Contains(err.Error(), `unknown role "ghost" (available: none configured)`) {
		t.Fatalf("err = %v, want the unknown-role failure", err)
	}
}
```

- [ ] **Step 3: Implement** in `internal/pipeline/synthesize.go`:

```go
// SynthRole is one role a synthesized pipeline may assign a step to (change
// 0115): its name and the description the synthesizer chooses by.
type SynthRole struct {
	Name        string
	Description string
}

// SynthContext carries the palette a synthesized pipeline may draw from: the
// available worker types, roles and tool names. All are advisory context
// injected into the synthesis prompt; a step naming a role outside Roles is
// rejected and re-asked.
type SynthContext struct {
	Workers []string
	Roles   []SynthRole
	Tools   []string
}
```

- In `Synthesize`: compute `roleNames := synthRoleNames(sc.Roles)` and `schema := pipelineJSONSchema(roleNames)`. After `parseSynth` succeeds and before `Validate`, add:

```go
		if rerr := CheckRoles(p, roleNames); rerr != nil {
			lastErr = rerr
			continue
		}
```

- Add:

```go
// synthRoleNames returns the palette's role names in palette order.
func synthRoleNames(roles []SynthRole) []string {
	if len(roles) == 0 {
		return nil
	}
	out := make([]string, len(roles))
	for i, r := range roles {
		out[i] = r.Name
	}
	return out
}
```

- `synthesisSystemPrompt`: write `pipelineSchemaDescription(len(sc.Roles) > 0)`. After the workers block and before the tools block, add:

```go
	if len(sc.Roles) > 0 {
		b.WriteString("Available roles (set a step's role to one of these instead of worker):\n")
		for _, r := range sc.Roles {
			b.WriteString("- " + r.Name + " — " + strings.Join(strings.Fields(r.Description), " ") + "\n")
		}
	}
```

- `pipelineSchemaDescription(withRoles bool) string`: move the existing raw literal **unchanged** into `const pipelineSchemaText = ...`. When `withRoles` is set, splice the role line in after the worker line:

```go
const (
	schemaWorkerLine = `      "worker": string (one of the available worker types),` + "\n"
	schemaRoleLine   = `      "role": string (one of the available roles; never together with worker),` + "\n"
)

func pipelineSchemaDescription(withRoles bool) string {
	if !withRoles {
		return pipelineSchemaText
	}
	return strings.Replace(pipelineSchemaText, schemaWorkerLine, schemaWorkerLine+schemaRoleLine, 1)
}
```

  Check that `schemaWorkerLine` matches the literal's worker line byte for byte (six-space indent). The role test in Step 2 catches a mismatch.
- `pipelineJSONSchema(roles []string) map[string]any`: after building `stepProps`, add `if len(roles) > 0 { stepProps["role"] = map[string]any{"type": "string"} }`. Leave everything else unchanged.

- [ ] **Step 4: Run and confirm pass.** The no-roles goldens must pass unchanged. Never regenerate them in this step.

Run: `go test ./internal/pipeline -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/pipeline/synthesize.go internal/pipeline/synthesize_test.go internal/pipeline/testdata
git commit -m "feat(pipeline): roles join the synthesis palette; unknown synthesized roles are re-asked"
```

---

### Task 4: `pipeline_run` mentions roles; the `spawn_agent` role note covers `workflow`

Suggested tier: economy

**Files:**
- Modify: `internal/tools/spawn_agent.go`, `internal/tools/pipeline_run.go`
- Create: `internal/tools/testdata/pipeline_run_schema.golden`, `internal/tools/testdata/pipeline_run_schema_workflows.golden`
- Test: `internal/tools/pipeline_run_test.go`, `internal/tools/spawn_agent_test.go`

- [ ] **Step 1: Capture the no-roles `pipeline_run` goldens from the UNCHANGED code** (append to `pipeline_run_test.go`; add imports as needed):

```go
var updatePipelineGolden = flag.Bool("update-pipeline-golden", false, "rewrite the pipeline_run goldens")

func pipelineRunGolden(t *testing.T, tool *PipelineRunTool) []byte {
	t.Helper()
	b, err := json.MarshalIndent(map[string]any{"description": tool.Description(), "parameters": tool.Parameters()}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

// TestPipelineRunSchemaGolden (change 0115): with no roles the pipeline_run
// description and schema stay byte-identical, with and without workflows.
func TestPipelineRunSchemaGolden(t *testing.T) {
	for _, tc := range []struct {
		file string
		tool *PipelineRunTool
	}{
		{"testdata/pipeline_run_schema.golden", NewPipelineRunTool(nil, nil)},
		{"testdata/pipeline_run_schema_workflows.golden", NewPipelineRunTool(nil, nil).WithWorkflows([]string{"wf"})},
	} {
		got := pipelineRunGolden(t, tc.tool)
		if *updatePipelineGolden {
			if err := os.WriteFile(tc.file, got, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatalf("read golden (run with -update-pipeline-golden to create it): %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s changed:\n got: %s\nwant: %s", tc.file, got, want)
		}
	}
}
```

Run `go test ./internal/tools -run TestPipelineRunSchemaGolden -update-pipeline-golden -count=1` on the unchanged code to create both goldens.

- [ ] **Step 2: Write the failing tests**

In `pipeline_run_test.go`:

```go
// TestPipelineRunNoRolesUnchanged: WithRoles(nil) and WithRoles(empty) leave
// every no-roles golden byte-identical.
func TestPipelineRunNoRolesUnchanged(t *testing.T) {
	for _, tc := range []struct {
		file string
		tool *PipelineRunTool
	}{
		{"testdata/pipeline_run_schema.golden", NewPipelineRunTool(nil, nil).WithRoles(nil)},
		{"testdata/pipeline_run_schema.golden", NewPipelineRunTool(nil, nil).WithRoles([]RoleOption{})},
		{"testdata/pipeline_run_schema_workflows.golden", NewPipelineRunTool(nil, nil).WithWorkflows([]string{"wf"}).WithRoles(nil)},
	} {
		want, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatal(err)
		}
		if got := pipelineRunGolden(t, tc.tool); !bytes.Equal(got, want) {
			t.Errorf("WithRoles(none) changed %s:\n got: %s\nwant: %s", tc.file, got, want)
		}
	}
}

// TestPipelineRunWithRolesMentionsRole: with roles the description names the
// step `role` field and lists each role on one line, sorted; params are unchanged.
func TestPipelineRunWithRolesMentionsRole(t *testing.T) {
	plain := NewPipelineRunTool(nil, nil)
	tool := plain.WithRoles([]RoleOption{{"writer", "Writes prose."}, {"reviewer", "Reviews code\nfor bugs."}})
	desc := tool.Description()
	if !strings.HasPrefix(desc, plain.Description()) {
		t.Fatalf("role paragraph must extend, not replace, the description: %q", desc)
	}
	for _, want := range []string{"`role`", "`worker`", "With `workflow`, a step's role must be one that workflow offers."} {
		if !strings.Contains(desc, want) {
			t.Errorf("description lacks %q: %q", want, desc)
		}
	}
	rev := strings.Index(desc, "\n- reviewer — Reviews code for bugs.")
	wri := strings.Index(desc, "\n- writer — Writes prose.")
	if rev < 0 || wri < 0 || rev > wri {
		t.Errorf("role lines missing or out of order: %q", desc)
	}
	if !reflect.DeepEqual(tool.Parameters(), plain.Parameters()) {
		t.Error("WithRoles changed the parameters")
	}
	if plain.Description() != NewPipelineRunTool(nil, nil).Description() {
		t.Error("WithRoles must not mutate the receiver")
	}
}
```

In `spawn_agent_test.go` (0114 reviewer nit):

```go
// TestSpawnAgentRoleNoteForWorkflow (change 0115): where the tool offers both
// workflows and roles, the role param says a role must be one the chosen
// workflow offers; with roles but no workflows the note is absent.
func TestSpawnAgentRoleNoteForWorkflow(t *testing.T) {
	const note = "With `workflow`, the role must be one that workflow offers."
	roles := []RoleOption{{"reviewer", "Reviews."}}
	desc := func(tool *SpawnAgentTool) string {
		r, _ := tool.Parameters()["properties"].(map[string]any)["role"].(map[string]any)
		s, _ := r["description"].(string)
		return s
	}
	if d := desc(NewSpawnAgentTool(okSpawn("x")).WithWorkflows([]string{"wf"}).WithRoles(roles)); !strings.Contains(d, note) {
		t.Errorf("role description with workflows lacks the note: %q", d)
	}
	if d := desc(NewSpawnAgentTool(okSpawn("x")).WithRoles(roles)); strings.Contains(d, note) {
		t.Errorf("role description without workflows carries the note: %q", d)
	}
}
```

- [ ] **Step 3: Run and confirm failure**

Run: `go test ./internal/tools -run 'TestPipelineRun|TestSpawnAgentRoleNote' -count=1`
Expected: compile failure (`WithRoles` is undefined on `PipelineRunTool`).

- [ ] **Step 4: Implement**

`internal/tools/spawn_agent.go`. Extract the normalization `WithRoles` already performs so both tools share it:

```go
// normalizeRoleOptions returns opts sorted by name with each description
// collapsed to a single line; nil for an empty list.
func normalizeRoleOptions(opts []RoleOption) []RoleOption {
	var out []RoleOption
	for _, o := range opts {
		out = append(out, RoleOption{Name: o.Name, Description: strings.Join(strings.Fields(o.Description), " ")})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
```

`SpawnAgentTool.WithRoles` becomes `cp := *t; cp.roles = normalizeRoleOptions(opts); return &cp`. In `Parameters()`, the role description becomes:

```go
		note := ""
		if len(t.workflows) > 0 {
			note = " With `workflow`, the role must be one that workflow offers."
		}
		... "model overrides its model). Do not pass system_prompt or worker with a role." + note + " Roles:" + lines.String(),
```

The existing role tests check substrings only, and every `spawn_agent` golden is a no-roles golden, so none changes. Run the spawn tests to confirm.

`internal/tools/pipeline_run.go`:
- add the field `roles []RoleOption` (doc: "roles, when non-empty, are described in the tool description (change 0115)");
- add:

```go
// WithRoles returns a copy of the tool whose description tells the model a
// pipeline step may name one of these roles instead of a worker (change 0115),
// sorted by name with one description line each. An empty/nil list leaves the
// description unchanged.
func (t *PipelineRunTool) WithRoles(opts []RoleOption) *PipelineRunTool {
	cp := *t
	cp.roles = normalizeRoleOptions(opts)
	return &cp
}
```

- `Description()`: keep the existing string as `base`. When `len(t.roles) > 0`, append:

```go
" A step may name a `role` instead of a `worker`: the role supplies the step's system prompt, " +
	"and its tool set and model when it defines them; a role needs no workflow. " +
	"With `workflow`, a step's role must be one that workflow offers. Roles:" + lines
```

  `lines` is `"\n- " + name + " — " + description` per role, the same shape the `spawn_agent` role param uses.

- [ ] **Step 5: Run and confirm pass**

Run: `go test ./internal/tools -count=1`
Expected: PASS. Every golden is unchanged.

- [ ] **Step 6: Commit**

```bash
git add internal/tools/spawn_agent.go internal/tools/pipeline_run.go internal/tools/pipeline_run_test.go internal/tools/spawn_agent_test.go internal/tools/testdata/pipeline_run_schema.golden internal/tools/testdata/pipeline_run_schema_workflows.golden
git commit -m "feat(tools): pipeline_run describes role steps; spawn_agent role note covers workflow"
```

---

### Task 5: Extract role-name helpers from the workflow scope (pure refactor)

Suggested tier: economy

**Files:**
- Modify: `cmd/fuse/workflow.go`

The synthesis seam (Task 7) needs the same "defined roles" and "roles a workflow offers" sets that `workflowScope.roleNames` and `workflowScope.workflowRoles` compute, but from `config.Config` instead of a scope. Extract the logic so it exists once.

- [ ] **Step 1: Implement.** Add these package-level funcs, with bodies moved verbatim from the methods:

```go
// sortedRoleNames returns every defined role name, sorted.
func sortedRoleNames(roles map[string]config.RoleConfig) []string { /* body of roleNames over roles */ }

// workflowRoleNames returns the role names offered inside a workflow
// configured as cfg: its listed roles that are defined, or every defined role
// when it lists none. Sorted.
func workflowRoleNames(roles map[string]config.RoleConfig, cfg config.WorkflowConfig) []string { /* body of workflowRoles over roles */ }
```

Then `func (s *workflowScope) roleNames() []string { return sortedRoleNames(s.roles) }` and `func (s *workflowScope) workflowRoles(cfg config.WorkflowConfig) []string { return workflowRoleNames(s.roles, cfg) }`. Keep the method doc comments. This is not a behavior change.

- [ ] **Step 2: Run the existing role and scope tests**

Run: `go test ./cmd/fuse -run 'Role|Scope|CheckRequest' -count=1`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add cmd/fuse/workflow.go
git commit -m "refactor(fuse): role-name sets as package helpers shared by the scope"
```

---

### Task 6: The authored pipeline seam checks roles; role steps run outside a workflow

Suggested tier: standard

**Files:**
- Modify: `cmd/fuse/run.go` (`makePipelineRunFn`, `refuseUnboundWorkerSteps` doc), `cmd/fuse/loop_wiring.go` (`wirePipeline` call)
- Modify call sites: `cmd/fuse/loop_wiring_test.go`, `cmd/fuse/pipeline_gateway_e2e_test.go`
- Test: `cmd/fuse/loop_wiring_test.go`

- [ ] **Step 1: Write the failing tests** (append to `loop_wiring_test.go`)

```go
// TestPipelineRunFnRoleStepOutsideWorkflow (change 0115): a role step is not
// refused outside a workflow (roles are global); its child is built through
// prepareChild's role resolution, so it gets the reviewer's tools, model pin
// and brief, and the node records the role.
func TestPipelineRunFnRoleStepOutsideWorkflow(t *testing.T) {
	ctx := context.Background()
	h := newWiringHarness(t)
	runFn := makePipelineRunFn(h.makeSpawner(h.root, 0), h.w.bb, h.tree.Scheduler(), h.root,
		h.w.scope.runner(h.makeSpawner, h.root), h.w.scope.governing, h.w.scope.roleNames())
	const def = "name: rp\nsteps:\n  - name: rev\n    role: reviewer\n    prompt: check\n"
	line, err := runFn(ctx, []byte(def), "")
	if err != nil || line != "pipeline rp: completed" {
		t.Fatalf("role pipeline = %q, %v; want completed", line, err)
	}
	steps := nodesLabelled(h.tree, "rev")
	if len(steps) != 1 || steps[0].Role != "reviewer" {
		t.Fatalf("rev steps = %+v, want one reviewer node", steps)
	}
	b := h.observed(steps[0].ID)
	want := append(append([]string(nil), blackboardNames...), "grep", "read_file")
	sort.Strings(want)
	if got := schemaNames(b.reg); !reflect.DeepEqual(got, want) {
		t.Fatalf("role step tools = %v, want %v", got, want)
	}
	if b.plan.model != "rmodel" || b.plan.instructions != "REVIEWER-BRIEF" {
		t.Fatalf("role step plan = %+v, want rmodel pin and REVIEWER-BRIEF", b.plan)
	}
}

// TestPipelineRunFnRoleStepUnderWorkflow: inside a workflow run the role step
// is a child of the coordinator and resolves against the workflow's offered
// roles (wf lists none, so every role is offered).
func TestPipelineRunFnRoleStepUnderWorkflow(t *testing.T) {
	ctx := context.Background()
	h := newWiringHarness(t)
	runFn := makePipelineRunFn(h.makeSpawner(h.root, 0), h.w.bb, h.tree.Scheduler(), h.root,
		h.w.scope.runner(h.makeSpawner, h.root), h.w.scope.governing, h.w.scope.roleNames())
	const def = "name: rp\nsteps:\n  - name: wri\n    role: writer\n    prompt: draft\n"
	if line, err := runFn(ctx, []byte(def), "wf"); err != nil || line != "pipeline rp: completed" {
		t.Fatalf("workflow role pipeline = %q, %v; want completed", line, err)
	}
	steps := nodesLabelled(h.tree, "wri")
	if len(steps) != 1 || steps[0].Role != "writer" {
		t.Fatalf("wri steps = %+v, want one writer node", steps)
	}
	if got := h.observed(steps[0].ID).plan.instructions; got != "WRITER-BRIEF" {
		t.Fatalf("writer step instructions = %q, want WRITER-BRIEF", got)
	}
}

// TestPipelineRunFnRefusesBadRoleSteps: an unknown role and a role+worker step
// are parse errors, refused before any node exists.
func TestPipelineRunFnRefusesBadRoleSteps(t *testing.T) {
	ctx := context.Background()
	h := newWiringHarness(t)
	runFn := makePipelineRunFn(h.makeSpawner(h.root, 0), h.w.bb, h.tree.Scheduler(), h.root,
		h.w.scope.runner(h.makeSpawner, h.root), h.w.scope.governing, h.w.scope.roleNames())
	before := len(h.tree.SnapshotAll())
	for def, want := range map[string]string{
		"name: p\nsteps:\n  - name: s1\n    role: nope\n    prompt: x\n":                 `parse: pipeline: step "s1": unknown role "nope" (available: reviewer, writer)`,
		"name: p\nsteps:\n  - name: s1\n    role: reviewer\n    worker: reader\n    prompt: x\n": `parse: pipeline: step "s1": role "reviewer" and worker "reader" cannot be combined; name one`,
	} {
		if _, err := runFn(ctx, []byte(def), "wf"); err == nil || err.Error() != want {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
	if n := len(h.tree.SnapshotAll()); n != before {
		t.Fatalf("refused pipelines created %d nodes, want 0", n-before)
	}
	// With no roles defined, any role step is an unknown role.
	noRoles := makePipelineRunFn(h.makeSpawner(h.root, 0), h.w.bb, h.tree.Scheduler(), h.root, nil, nil, nil)
	if _, err := noRoles(ctx, []byte("name: p\nsteps:\n  - name: s1\n    role: reviewer\n    prompt: x\n"), ""); err == nil ||
		!strings.Contains(err.Error(), `unknown role "reviewer" (available: none configured)`) {
		t.Fatalf("no-roles err = %v, want unknown role", err)
	}
}
```

- [ ] **Step 2: Run and confirm failure**

Run: `go test ./cmd/fuse -run 'TestPipelineRunFn' -count=1`
Expected: compile failure (too many arguments to `makePipelineRunFn`).

- [ ] **Step 3: Implement**
- `makePipelineRunFn(..., governing workflowLookup, roles []string)`. Document `roles`: "every defined role name, sorted; a step naming any other role is refused as a parse error (change 0115). Nil means no roles are defined." Right after `pipeline.Parse`, add:

```go
		if err := pipeline.CheckRoles(p, roles); err != nil {
			return "", fmt.Errorf("parse: %w", err)
		}
```

- Extend the `refuseUnboundWorkerSteps` doc comment: role steps are not refused here, because roles are global (change 0115). Whether a workflow offers a role is enforced when the step spawns, by `workflowScope.prepareChild` (`checkRole`), the same as for any direct `Spawner.Spawn`. Leave the body unchanged.
- `loop_wiring.go` `wirePipeline`: pass `w.scope.roleNames()` as the new last argument.
- Update every other call site to pass a trailing `nil`: `loop_wiring_test.go` (the existing `makePipelineRunFn(` calls) and `pipeline_gateway_e2e_test.go` (three calls). Find them all with `grep -rn 'makePipelineRunFn(' cmd/fuse`.

- [ ] **Step 4: Run and confirm pass**

Run: `go test ./cmd/fuse -run 'Pipeline|Role|Wiring' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/fuse/run.go cmd/fuse/loop_wiring.go cmd/fuse/loop_wiring_test.go cmd/fuse/pipeline_gateway_e2e_test.go
git commit -m "feat(fuse): authored pipelines check step roles; role steps run outside workflows"
```

---

### Task 7: The synthesis seam offers the role palette

Suggested tier: standard

**Files:**
- Modify: `cmd/fuse/run.go` (`makePipelineSynthFn`, new `synthRolePalette`, `pipelineSynthPalette` doc)
- Test: `cmd/fuse/loop_wiring_test.go`

- [ ] **Step 1: Write the failing test** (append)

```go
// TestPipelineSynthRolePalette (change 0115): the synthesis prompt lists the
// roles a step may take: every defined role outside a workflow, the
// workflow's offered roles with one, and none at all when no roles are defined.
func TestPipelineSynthRolePalette(t *testing.T) {
	ctx := context.Background()
	h := newWiringHarness(t)
	var prompt string
	h.reply = func(opts agent.SpawnOpts) string {
		if opts.Label != "synthesize" {
			return ""
		}
		prompt = opts.SystemPrompt
		// Root synthesis requires a pool (RequirePool), so the plan names wf.
		return `{"name":"p","workflow":"wf","steps":[{"name":"s1","role":"writer","prompt":"hi"}]}`
	}
	sched := h.tree.Scheduler()
	run := h.w.scope.runner(h.makeSpawner, h.root)
	synth := makePipelineSynthFn(h.makeSpawner(h.root, 0), h.w.bb, sched, h.root, h.cfg, nil, nil, run, h.w.scope.governing)

	if line, err := synth(ctx, "goal", false, ""); err != nil || line != "pipeline p: completed" {
		t.Fatalf("root role synthesis = %q, %v; want completed", line, err)
	}
	if !strings.Contains(prompt, "- reviewer — Reviews code.\n- writer — Writes prose.\n") {
		t.Fatalf("root palette lacks both roles: %q", prompt)
	}

	wf := h.cfg.Workflows["wf"]
	wf.Roles = []string{"writer"}
	h.cfg.Workflows["wf"] = wf
	synth = makePipelineSynthFn(h.makeSpawner(h.root, 0), h.w.bb, sched, h.root, h.cfg, nil, nil, run, h.w.scope.governing)
	prompt = ""
	if _, err := synth(ctx, "goal", false, "wf"); err != nil {
		t.Fatalf("wf role synthesis: %v", err)
	}
	if !strings.Contains(prompt, "- writer — Writes prose.\n") || strings.Contains(prompt, "- reviewer") {
		t.Fatalf("wf palette = %q, want only writer", prompt)
	}

	none := h.cfg
	none.Roles = nil
	h.reply = func(opts agent.SpawnOpts) string {
		if opts.Label != "synthesize" {
			return ""
		}
		prompt = opts.SystemPrompt
		return `{"name":"p","workflow":"wf","steps":[{"name":"s1","prompt":"hi"}]}`
	}
	prompt = ""
	if _, err := makePipelineSynthFn(h.makeSpawner(h.root, 0), h.w.bb, sched, h.root, none, nil, nil, run, h.w.scope.governing)(ctx, "goal", false, ""); err != nil {
		t.Fatalf("no-roles synthesis: %v", err)
	}
	if strings.Contains(prompt, "Available roles") || strings.Contains(prompt, `"role"`) {
		t.Fatalf("no-roles synthesis prompt mentions roles: %q", prompt)
	}
}
```

(`h.cfg.Workflows` is the same map the scope holds, so the `wf.Roles` edit is visible to the step's spawn-time `checkRole`. The writer step is offered there, so it completes.)

- [ ] **Step 2: Run and confirm failure**

Run: `go test ./cmd/fuse -run TestPipelineSynthRolePalette -count=1`
Expected: FAIL. The synthesis prompt has no role palette, and the role step is re-asked as an unknown role until the attempt cap.

- [ ] **Step 3: Implement** in `cmd/fuse/run.go`:

```go
// synthRolePalette is the synthesis role palette (change 0115): the roles a
// step may take, as name and description, sorted. Outside a workflow (wf nil)
// it is every defined role; with one, the roles that workflow offers. Nil when
// none, so the synthesis prompt is unchanged without roles.
func synthRolePalette(roles map[string]config.RoleConfig, wf *config.WorkflowConfig) []pipeline.SynthRole {
	names := sortedRoleNames(roles)
	if wf != nil {
		names = workflowRoleNames(roles, *wf)
	}
	var out []pipeline.SynthRole
	for _, n := range names {
		out = append(out, pipeline.SynthRole{Name: n, Description: roles[n].Description})
	}
	return out
}
```

In `makePipelineSynthFn`, track the workflow config that the worker palette already comes from. Declare `var wfCfg *config.WorkflowConfig`, then set `wfCfg = &wf` in both the `governing` branch and the `workflow != ""` branch, next to `palette = sortedWorkerNames(wf)`. Build the context as:

```go
pipeline.SynthContext{Workers: palette, Roles: synthRolePalette(cfg.Roles, wfCfg), Tools: toolNames}
```

Add to the `makePipelineSynthFn` doc: "The role palette is computed per call the same way (change 0115): the argument's or governing workflow's offered roles, else every defined role." Add a sentence to `pipelineSynthPalette`'s doc: its worker list is unused by the shared wiring, and the seam computes both the worker and role palettes per call. The spec says the role palette "joins `pipelineSynthPalette`". Since change 0091, the synthesis palette is actually assembled per call in `makePipelineSynthFn`, so the role palette lives there, next to the workers.

- [ ] **Step 4: Run and confirm pass**

Run: `go test ./cmd/fuse -run 'Pipeline|Synth' -count=1`
Expected: PASS. The existing palette tests are unchanged.

- [ ] **Step 5: Commit**

```bash
git add cmd/fuse/run.go cmd/fuse/loop_wiring_test.go
git commit -m "feat(fuse): pipeline synthesis offers the role palette"
```

---

### Task 8: `wirePipeline` advertises roles on `pipeline_run`

Suggested tier: economy

**Files:**
- Modify: `cmd/fuse/loop_wiring.go` (`wirePipeline`)
- Test: `cmd/fuse/loop_wiring_test.go`

- [ ] **Step 1: Write the failing test** (append)

```go
// pipelineRunDescription returns the description of reg's pipeline_run.
func pipelineRunDescription(t *testing.T, reg *tools.Registry) string {
	t.Helper()
	for _, tl := range reg.Tools() {
		if tl.Name() == "pipeline_run" {
			return tl.Description()
		}
	}
	t.Fatal("registry has no pipeline_run")
	return ""
}

// TestWirePipelineAdvertisesRoles (change 0115): pipeline_run's description
// lists the roles offered at the node, and is byte-identical to the role-free
// tool when no roles are defined.
func TestWirePipelineAdvertisesRoles(t *testing.T) {
	h := newWiringHarness(t)
	reg := tools.NewRegistry()
	h.w.wirePipeline(reg, h.root, nil)
	if d := pipelineRunDescription(t, reg); !strings.Contains(d, "\n- reviewer — Reviews code.") || !strings.Contains(d, "\n- writer — Writes prose.") {
		t.Fatalf("root pipeline_run description lacks the roles: %q", d)
	}

	plain := newWiringHarness(t)
	plain.w.scope = newWorkflowScope(plain.tree, plain.cfg.Workflows, nil)
	reg = tools.NewRegistry()
	plain.w.wirePipeline(reg, plain.root, nil)
	if got, want := pipelineRunDescription(t, reg), tools.NewPipelineRunTool(nil, nil).Description(); got != want {
		t.Fatalf("no-roles pipeline_run description changed:\n got %q\nwant %q", got, want)
	}
}
```

- [ ] **Step 2: Run and confirm failure**

Run: `go test ./cmd/fuse -run TestWirePipelineAdvertisesRoles -count=1`
Expected: FAIL (the description has no role lines).

- [ ] **Step 3: Implement.** In `wirePipeline`, after constructing `t`, add `t = t.WithRoles(w.scope.roleOptions(node.ID))`. This is the same offered set that `spawn_agent` advertises at that node. Then run `grep -rn 'NewPipelineRunTool(' cmd/fuse --include='*.go' | grep -v _test` and confirm that `loop_wiring.go` is the only non-test construction site (learning `patch-every-cloned-child-builder`).

- [ ] **Step 4: Run and confirm pass**

Run: `go test ./cmd/fuse -count=1`
Expected: PASS. That includes `workflow_wiring_guard_test.go` and the role parity table guard.

- [ ] **Step 5: Commit**

```bash
git add cmd/fuse/loop_wiring.go cmd/fuse/loop_wiring_test.go
git commit -m "feat(fuse): pipeline_run advertises the roles offered at the node"
```

---

### Task 9: Acceptance at the gateway seam: a role step gets the role's composed prompt, tools and model

Suggested tier: standard

**Files:**
- Create: `cmd/fuse/pipeline_role_test.go`

This test proves the acceptance criterion through a real binding's composition root (the one-shot binding, as in `role_parity_test.go`), against the scripted gateway double. It reuses the existing helpers: `roleOperatorYAML`, `routedGateway`, `turnRequest`, `personaPromptFor`, `assertInOrder`, `sortedToolNames`, `nodeLabelled`, `argsJSON`, `textReply`.

- [ ] **Step 1: Write the test**

```go
package main

import (
	"context"
	"io"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/permissions"
)

// TestPipelineRoleStepAtGateway (change 0115): an authored pipeline run from a
// real binding's root builds each role step exactly like a spawn_agent role
// child. The reviewer step gets the reviewer brief after the persona, the
// role's model pin and the role's tools plus the blackboard tools. The writer
// step gets its brief and the plain step's tools. The plain step gets no
// brief. An unknown role is refused before any step spawns.
func TestPipelineRoleStepAtGateway(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	gw := &routedGateway{route: func(string, int, gatewayCall) string { return textReply("done") }}
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)
	cfg, err := config.FromYAML([]byte(roleOperatorYAML), "test")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Gateway.URL, cfg.Gateway.Key = srv.URL, "tkn"
	reg := registryFromConfig(cfg)
	toolReg := defaultToolRegistry(nil, cfg.Research, nil)
	tree := agent.NewAgentTreeWithConcurrency(reg.Default, reg.Default, cfg.Agents.MaxConcurrent)
	deps, closeMCP := buildOneShotRuntimeDeps(cfg, reg, reg.Default, toolReg, tree, io.Discard, false, nil,
		permissions.AlwaysApprove, "block", false, nil, nil)
	t.Cleanup(closeMCP)
	if _, _, _, err := deps.BuildAgent(nil, tree, reg.Default, toolReg); err != nil {
		t.Fatalf("BuildAgent: %v", err)
	}

	const def = "name: rp\nsteps:\n" +
		"  - name: rev\n    role: reviewer\n    prompt: REV-PIPE-TASK\n" +
		"  - name: wri\n    role: writer\n    prompt: WRI-PIPE-TASK\n" +
		"  - name: pln\n    prompt: PLN-PIPE-TASK\n"
	res := toolReg.Execute(context.Background(), "pipeline_run", argsJSON(map[string]any{"definition": def}))
	if res.IsError || res.Output != "pipeline rp: completed" {
		t.Fatalf("pipeline_run = %+v, want completed", res)
	}

	house := personaPromptFor(t, roleOperatorYAML, "house")
	fast := personaPromptFor(t, roleOperatorYAML, "fast")
	system := func(marker string) (gatewayCall, string) {
		t.Helper()
		c := turnRequest(t, gw, marker, 0)
		if len(c.Messages) == 0 || c.Messages[0].Role != "system" {
			t.Fatalf("%s: first message is not the system prompt", marker)
		}
		return c, c.Messages[0].Content
	}

	rev, revSys := system("REV-PIPE-TASK")
	assertInOrder(t, revSys, house, "REVIEWER-BRIEF")
	if rev.Model != "cloud/house" {
		t.Errorf("reviewer step model = %q, want the role's pin cloud/house", rev.Model)
	}
	reviewerTools := []string{"blackboard_delete", "blackboard_keys", "blackboard_read", "blackboard_wait", "blackboard_write", "grep", "read_file"}
	if got := sortedToolNames(rev); !reflect.DeepEqual(got, reviewerTools) {
		t.Errorf("reviewer step tools = %v, want %v", got, reviewerTools)
	}

	wri, wriSys := system("WRI-PIPE-TASK")
	assertInOrder(t, wriSys, fast, "WRITER-BRIEF")
	pln, plnSys := system("PLN-PIPE-TASK")
	if got, want := sortedToolNames(wri), sortedToolNames(pln); !reflect.DeepEqual(got, want) {
		t.Errorf("writer step tools = %v, want the plain step's %v", got, want)
	}
	if strings.Contains(plnSys, "REVIEWER-BRIEF") || strings.Contains(plnSys, "WRITER-BRIEF") {
		t.Errorf("plain step carries a role brief:\n%s", plnSys)
	}
	for label, want := range map[string]string{"rev": "reviewer", "wri": "writer", "pln": ""} {
		if got := nodeLabelled(t, tree, label).Role; got != want {
			t.Errorf("step %q Role = %q, want %q", label, got, want)
		}
	}

	before := len(tree.SnapshotAll())
	bad := toolReg.Execute(context.Background(), "pipeline_run",
		argsJSON(map[string]any{"definition": "name: bad\nsteps:\n  - name: x\n    role: nope\n    prompt: X-PIPE-TASK\n"}))
	if !bad.IsError || !strings.Contains(bad.Output, `unknown role "nope" (available: reviewer, writer)`) {
		t.Fatalf("unknown-role pipeline_run = %+v, want the unknown-role refusal", bad)
	}
	if n := len(tree.SnapshotAll()); n != before {
		t.Fatalf("refused pipeline created %d nodes, want 0", n-before)
	}

	var desc string
	for _, tl := range toolReg.Tools() {
		if tl.Name() == "pipeline_run" {
			desc = tl.Description()
		}
	}
	if !strings.Contains(desc, "`role`") || !strings.Contains(desc, "\n- reviewer — ") {
		t.Errorf("root pipeline_run description does not mention roles: %q", desc)
	}
}
```

Notes for the builder:
- The writer step's model is the parent's, so the test expects `fast` as the writer persona, matching `TestRoleParityAcrossChildBuilders`. If `personaPromptFor` or the default alias differ in practice, mirror whatever the parity test asserts for `WRI-TASK`.

- [ ] **Step 2: Run**

Run: `go test ./cmd/fuse -run TestPipelineRoleStepAtGateway -count=1 -v`
Expected: PASS, because Tasks 1–8 are already in place. If it fails, the failure is a real wiring gap at the composition root: fix the wiring, never the assertion.

- [ ] **Step 3: Commit**

```bash
git add cmd/fuse/pipeline_role_test.go
git commit -m "test(fuse): pipeline role steps match spawn_agent role children at the gateway seam"
```

---

### Task 10: Full suite and optional live check

Suggested tier: economy

- [ ] **Step 1: Full suite.** Run `make test` from the feature worktree. Expected: PASS. If a no-roles golden fails, the byte-identity promise is broken: fix the code, never regenerate the golden.
- [ ] **Step 2: Vet and format.** Run `gofmt -l ./cmd ./internal` (expect no output) and `go vet ./cmd/fuse ./internal/pipeline ./internal/tools`.
- [ ] **Step 3 (optional, non-blocking): live smoke against a cheap model.** Build the binary from **the feature worktree** (learning `verify-from-feature-worktree-binary`). Use a temporary `HOME` whose `.fuse/config.yml` copies the operator config and adds a `roles:` block. Define a `reviewer` role with `tools: [read_file, grep]` and **no Anthropic model pin**. Route the root model to glm or kimi through the gateway at `localhost:4000`. Ask the agent to call `pipeline_run` with a one-step definition (`role: reviewer`), and confirm from the trace that the step ran with the reviewer's tools. Skip this step if the gateway is unreachable. The Task 9 gateway test is the authoritative evidence. Never use an Anthropic model for this check.
- [ ] **Step 4:** No commit unless Steps 1–2 forced a fix. If they did, commit it with a message naming the fix.

---

## Acceptance criteria to evidence map

| Spec criterion | Evidence |
|---|---|
| A `role: reviewer` step runs with the reviewer's composed prompt and tool set | Task 9 `TestPipelineRoleStepAtGateway` (gateway seam: system prompt order, model pin, tools); Task 6 `TestPipelineRunFnRoleStepOutsideWorkflow` (plan); Task 2 engine forwarding |
| Parse error: both `role` and `worker` | Task 1 `TestParseRoleAndWorkerIsError`; Task 6 `TestPipelineRunFnRefusesBadRoleSteps` |
| Parse error: unknown role | Task 1 `TestCheckRoles`; Task 6 `TestPipelineRunFnRefusesBadRoleSteps`; Task 9 refusal; Task 3 synthesis re-ask |
| `refuseUnboundWorkerSteps` does not refuse role steps | Task 6 `TestPipelineRunFnRoleStepOutsideWorkflow` |
| Synthesis input includes the role palette when roles exist | Task 3 `TestSynthesisPromptListsRoles`; Task 7 `TestPipelineSynthRolePalette` |
| Without roles: synthesis prompt and `pipeline_run` description byte-identical | Task 3 `TestSynthesisNoRolesGolden` (captured before the change); Task 4 `TestPipelineRunSchemaGolden` / `TestPipelineRunNoRolesUnchanged`; Task 8 `TestWirePipelineAdvertisesRoles`; Task 7 no-roles branch |
| `pipeline_run` description mentions `role` when roles exist | Task 4 `TestPipelineRunWithRolesMentionsRole`; Task 8; Task 9 |
| 0114 reviewer nit: the `spawn_agent` role param notes that with `workflow` the role must be one the workflow offers | Task 4 `TestSpawnAgentRoleNoteForWorkflow` |

## Out of scope

- Pipeline scheduling or limit changes.
- Making roles mandatory in pipelines.
- An up-front "workflow offers this role" check in the authored seam. Offered-ness stays enforced at spawn by `prepareChild` → `checkRole`, the existing path for direct `Spawner.Spawn` callers.
