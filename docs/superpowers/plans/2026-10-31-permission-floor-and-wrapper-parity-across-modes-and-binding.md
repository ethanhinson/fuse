<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0093 — Permission floor and wrapper parity across modes and bindings](../../changes/active/0093-permission-floor-and-wrapper-parity-across-modes-and-binding.md)**
<!-- docket:backlink:end -->

# Permission floor and wrapper parity across modes and bindings - Implementation Plan

> For the executor: this plan is run by `docket-build`, one commit per task, TDD inside each task (write the test, watch it fail, implement, watch it pass). The ADR is recorded afterwards by the parent through `docket-adr`; it is not a task here.

Note: `superpowers:writing-plans` was unavailable in this environment; the plan writer authored this plan directly in the same shape.

**Goal:** Close the three remaining gaps between the catastrophic floor and protective matcher in auto mode and what they do in smart mode and in the headless mcp-server binding:

1. Smart mode's legacy first-token allow (`auto_approve: ["bash:rm"]`) and the headless smart policy stand-in can approve a catastrophic command. Fix: the floor is a rules-layer decision in smart mode too.
2. Protective rules naming a peeled wrapper (`bash:timeout*`, `bash:nice *`, `bash:env *`, `bash:bash *`) never fire in auto mode. Fix: the raw, pre-peel command is an extra protective subject.
3. The mcp-server gate has no workspace root or write roots, so `rm -rf ..` is not caught. Fix: bind both.

**Architecture:** One shared helper per decision, so modes cannot drift:
- `catastrophicSegment(name, args, workspaceRoot)` in `internal/permissions/patterns.go`, called by `matchesPermissive` and by the smart branch of `PermissionGate.resolve`.
- `protectiveSegmentSubjects(seg)` / `rawSubjects(seg)` in `internal/permissions/rules.go`, called by `protectiveHitPattern` (the one protective subject rule both modes use) and by the protective branch of `matchesSegment`. The permissive consumer never sees raw subjects.
- `buildMCPServerGate(cfg, toolReg, approve)` in `cmd/fuse/mcp_server.go`, called by `runMCPServer` and its test.

**Tech stack:** Go 1.26 (`t.Chdir` available), `mvdan.cc/sh/v3/syntax` parser already in use, existing test helpers.

**Spec:** `docs/superpowers/specs/2026-10-31-permission-floor-and-wrapper-parity-across-modes-and-binding-design.md` (committed in the feature worktree).

**Writing rules for everything this plan prescribes (code comments, test names, commit messages):** no em dashes (use a colon, comma, parentheses, or a plain hyphen); reference the change as "change 0093" in comments the way neighbouring comments reference 0088/0092; never mention downstream applications or products built on fuse.

**Commands:** run from the feature worktree root `/Users/ethanhinson/dev/fuse/.worktrees/permission-floor-and-wrapper-parity-across-modes-and-binding`.
- Focused: `go test ./internal/permissions -run '<names>' -count=1` or `go test ./cmd/fuse -run '<names>' -count=1`
- Hygiene per task: `gofmt -l internal cmd` (must print nothing), `go vet ./internal/permissions ./cmd/fuse`
- The full suite (`go test ./...`) is docket-build's single end gate, not a per-task step.

## Deviations from the spec (deliberate, each a correction found while reading the code)

1. **env parity row uses `env TZ=UTC make`, not `env FOO=1 make`.** `peelEnv` only peels names on the inert allowlist (`envAssignNameIsInert`, ADR-0049); `FOO` is not on it, so `env FOO=1 make` is `ErrUnparseable` and never reaches the segment matchers in auto mode (it asks at `LayerParse`, and the `bash:make` witness can never approve it). `TZ` is on the list.
2. **dash-c parity row uses `bash -c "make"` (witness `bash:make`), not `bash -c "ls"`.** The `protectiveParityRows` doc comment promises every cmd is off the read-only safelist; `ls` is on it. `make` keeps the invariant. The parser test (Task 1) still uses the spec's `bash -c "rm x && ls"`.
3. **The smart-mode end-to-end run uses `always_prompt: ["bash:timeout *"]` (with a space), not `bash:timeout*`.** `bash:timeout*` already matches smart mode's legacy `bash:<firstToken>` subject `bash:timeout`, so that run would pass before this change and prove nothing. `bash:timeout *` misses the legacy head (no argument text) and is reachable only through the new raw-text subject.
4. **`TestHostedBindingDeniesCatastrophicInSmartMode` sets `cfg.Gateway.URL` and passes `registryFromConfig(cfg)` to `buildGate`.** `buildGate` binds `WithWorkspaceRoot` only inside `autoModeOptions`, which returns nil unless a gateway is configured (`classifierConstructible`). Every loop-server binding has a gateway, so the test mirrors that. A nil `*model.Registry` would panic in `registryDefault`, hence the real registry. Smart mode never consults the classifier, so the URL is never dialled.
5. **The raw-subject change lands in a shared helper used by both `protectiveHitPattern` and `matchesSegment`.** The spec names `matchesSegment`, but the production protective path for both modes is `protectiveHitPattern` (it inlines its subjects; `matchesSegment` has no production protective caller today). Routing both through one helper keeps them identical.

---

## Task 1: bash -c inner segments carry the outer call as Raw

**Tier:** economy (mechanical, fully specified)

**Files:**
- Modify: `internal/permissions/shellparse.go` (the `if name == "bash" || name == "sh"` branch of the call collector, around lines 945-958)
- Test: `internal/permissions/shellparse_test.go` (add after `TestSplitSegments_Raw`, around line 460)

**Step 1: Write the failing test.** Add to `shellparse_test.go`:

```go
// TestSplitSegments_DashCInnerRawIsOuterCall: every segment a peeled `bash -c`
// script yields carries the OUTER call's source text as Raw (change 0093), so a
// protective rule naming the wrapper ("bash:bash *") can reach the command as
// written. Name and Args stay the inner command's own.
func TestSplitSegments_DashCInnerRawIsOuterCall(t *testing.T) {
	cases := []struct {
		cmd   string
		names []string
	}{
		{`bash -c "rm x && ls"`, []string{"rm", "ls"}},
		{`sh -c "make"`, []string{"make"}},
		{`timeout 5 bash -c "make"`, []string{"make"}},
	}
	for _, tc := range cases {
		t.Run(tc.cmd, func(t *testing.T) {
			got := segsFor(t, tc.cmd)
			if len(got) != len(tc.names) {
				t.Fatalf("splitSegments(%q) = %d segments, want %d: %+v", tc.cmd, len(got), len(tc.names), got)
			}
			for i, seg := range got {
				if seg.Name != tc.names[i] {
					t.Errorf("segment %d Name = %q, want %q", i, seg.Name, tc.names[i])
				}
				if seg.Raw != tc.cmd {
					t.Errorf("segment %d Raw = %q, want the outer call %q", i, seg.Raw, tc.cmd)
				}
			}
		})
	}
}
```

**Step 2: Run it, expect FAIL** (Raw is the inner script text, e.g. `rm x`):
`go test ./internal/permissions -run TestSplitSegments_DashCInnerRawIsOuterCall -count=1`

**Step 3: Implement.** In `shellparse.go`, replace the tail of the bash/sh branch

```go
		return collectStmts(script, file.Stmts, out)
	}
```

with

```go
		// Every segment the script yields carries the OUTER call's source text
		// as Raw (change 0093), so a protective rule naming the wrapper
		// ("bash:bash *") can reach the command as written in every mode. Name
		// and Args stay the inner command's own. A nested `bash -c` is
		// overwritten again here by its own caller, so Raw always ends up as the
		// outermost call.
		first := len(*out)
		if err := collectStmts(script, file.Stmts, out); err != nil {
			return err
		}
		outer := rawSegment(src, call)
		for i := first; i < len(*out); i++ {
			(*out)[i].Raw = outer
		}
		return nil
	}
```

(`src` and `call` are already in scope: the same function builds the ordinary segment with `Raw: rawSegment(src, call)` a few lines below.)

**Step 4: Run, expect PASS**, plus the neighbouring parser tests:
`go test ./internal/permissions -run 'TestSplitSegments' -count=1`

**Step 5: Commit.** `permissions: bash -c inner segments carry the outer call as Raw`

---

## Task 2: raw pre-peel text is a protective subject

**Tier:** standard

**Files:**
- Modify: `internal/permissions/rules.go` (`matchesSegment` around line 584, `protectiveHitPattern` around line 607; add two helpers next to them)
- Modify: `internal/permissions/patterns.go` (doc comment of `protectivePattern`, lines 8-16)
- Test: `internal/permissions/patternparity_test.go` (extend `protectiveParityRows`)
- Test: `internal/permissions/patterns_test.go` (add `TestProtectiveRawSubjects`)

**Step 1: Write the failing tests.**

(a) Append these rows to `protectiveParityRows` in `patternparity_test.go` (before the `"unrelated pattern misses"` row). Witness allows name the peeled segment:

```go
	// Raw pre-peel subjects (change 0093): a protective rule naming a peeled
	// wrapper fires in both modes; the peeled subject still matches too.
	{"wrapper glob matches the command as written", "bash:timeout*", "timeout 5 rm x", []string{"bash:rm x"}, true},
	{"wrapper with argument text matches", "bash:nice *", "nice -n 5 make", []string{"bash:make"}, true},
	{"env wrapper with an inert assignment matches", "bash:env *", "env TZ=UTC make", []string{"bash:make"}, true},
	{"dash-c wrapper matches the outer call", "bash:bash *", `bash -c "make"`, []string{"bash:make"}, true},
	{"wrapper rule reaches a later segment", "bash:timeout*", "git status && timeout 5 rm x", []string{"bash:git status", "bash:rm x"}, true},
	{"peeled subject still matches under a wrapper", "bash:rm *", "timeout 5 rm x", []string{"bash:rm x"}, true},
```

`TestProtectivePatterns_SameMatchInSmartAndAuto` and `TestProtectivePatterns_SameVerdictInSmartAndAuto` run these unchanged; both modes must agree.

(b) Add to `patterns_test.go`:

```go
// TestProtectiveRawSubjects: the protective matcher also sees the command as
// written before wrappers are peeled (change 0093), through both entry points,
// and the permissive consumer never does: a bare "bash:timeout" allow must not
// approve "timeout 5 rm -rf a/b".
func TestProtectiveRawSubjects(t *testing.T) {
	hits := []struct{ pattern, cmd string }{
		{"bash:timeout*", "timeout 5 rm x"},
		{"bash:nice *", "nice -n 5 make"},
		{"bash:env *", "env TZ=UTC make"},
		{"bash:bash *", `bash -c "make"`},
		{"bash:timeout*", "git status && timeout 5 rm x"},
		{"bash:rm *", "timeout 5 rm x"},
	}
	for _, h := range hits {
		t.Run(h.pattern+" on "+h.cmd, func(t *testing.T) {
			segs := segsFor(t, h.cmd)
			if pat, ok := protectiveHitPattern([]string{h.pattern}, segs); !ok || pat != h.pattern {
				t.Errorf("protectiveHitPattern = %q %v, want %q true", pat, ok, h.pattern)
			}
			var hit bool
			for _, seg := range segs {
				hit = hit || matchesSegment(consumerProtective, []string{h.pattern}, seg)
			}
			if !hit {
				t.Errorf("matchesSegment(protective, %q) missed every segment of %q", h.pattern, h.cmd)
			}
		})
	}

	// The permissive consumer never sees raw subjects.
	for _, pat := range []string{"bash:timeout", "bash:timeout*", "bash:timeout *", "bash:timeout 5 rm x"} {
		for _, seg := range segsFor(t, "timeout 5 rm x") {
			if matchesSegment(consumerPermissive, []string{pat}, seg) {
				t.Errorf("permissive %q matched peeled segment %+v through a raw subject", pat, seg)
			}
		}
	}

	// Nothing peeled: no raw subject is added (rawText equals the parsed text).
	if got := rawSubjects(segsFor(t, "rm x")[0]); got != nil {
		t.Errorf("rawSubjects(rm x) = %v, want nil", got)
	}
	if got := rawSubjects(Segment{Name: "rm", Args: []string{"x"}}); got != nil {
		t.Errorf("rawSubjects with empty Raw = %v, want nil", got)
	}
}
```

Every row in `hits` matches on a per-segment subject (not only the whole-command subject, which `matchesSegment` does not test), so the `matchesSegment(consumerProtective, ...)` assertion holds for each row.

**Step 2: Run, expect FAIL** (auto mode misses the wrapper rows; `rawSubjects` is undefined so the package will not compile first):
`go test ./internal/permissions -run 'TestProtectiveRawSubjects|TestProtectivePatterns_' -count=1`

**Step 3: Implement in `rules.go`.**

Add next to `matchesSegment`:

```go
// protectiveSegmentSubjects returns every subject a protective pattern is
// matched against for one segment, in match order: the parsed subject
// ("bash:<argv0> <args>"), the head ("bash:<argv0>"), then the command as
// written before wrappers were peeled (rawSubjects). Only protective
// consumers use this list (change 0093).
func protectiveSegmentSubjects(seg Segment) []string {
	return append([]string{segmentSubject(seg), "bash:" + seg.Name}, rawSubjects(seg)...)
}

// rawSubjects returns the pre-peel subjects for a segment (change 0093):
// "bash:<rawHead>" and "bash:<rawText>", where rawText is seg.Raw with its
// whitespace collapsed and rawHead is its first word. The parser peels
// timeout, nice, stdbuf, nohup, time, env and bash -c before naming the
// segment, so without these a rule naming the wrapper ("bash:timeout*",
// "bash:nice *") could never fire in auto mode. Nothing is returned when Raw
// is empty or rawText equals the parsed subject's text (nothing was peeled);
// rawHead is left out when it equals the head already matched.
//
// These subjects are PROTECTIVE ONLY. A permissive (auto_approve) match must
// never see them: "bash:timeout" would otherwise approve "timeout 5 rm -rf
// a/b", and "bash:timeout *" would admit what the strict per-segment rule
// rejects (ADR-0005, ADR-0065). Raw keeps quoting as written, so it can
// differ from the parsed text even when nothing was peeled; that only ever
// gives a protective rule more text to match, which is the direction
// protective rules err.
func rawSubjects(seg Segment) []string {
	fields := strings.Fields(seg.Raw)
	if len(fields) == 0 {
		return nil
	}
	rawText := strings.Join(fields, " ")
	if "bash:"+rawText == segmentSubject(seg) {
		return nil
	}
	var out []string
	if fields[0] != seg.Name {
		out = append(out, "bash:"+fields[0])
	}
	return append(out, "bash:"+rawText)
}
```

Replace the body of `matchesSegment`:

```go
func matchesSegment(kind patternConsumer, patterns []string, seg Segment) bool {
	if kind != consumerProtective {
		return matchesSubjects(kind, patterns, segmentSubject(seg))
	}
	for _, subj := range protectiveSegmentSubjects(seg) {
		if matchesSubjects(kind, patterns, subj) {
			return true
		}
	}
	return false
}
```

and extend its doc comment with one sentence: "The protective consumer also sees the raw, pre-peel subjects (rawSubjects, change 0093); the permissive consumer sees only the parsed subject."

Replace the per-segment loop in `protectiveHitPattern`:

```go
	for _, seg := range segments {
		for _, subj := range protectiveSegmentSubjects(seg) {
			if pat, ok := firstProtectiveMatch(patterns, subj); ok {
				return pat, true
			}
		}
	}
	return firstProtectiveMatch(patterns, wholeSubject(segments))
```

and update its doc comment so the subject list reads "each segment's subject, its "bash:<argv0>" head, and its raw pre-peel subjects (rawSubjects), then the whole command". Do the same for the `evalRules` doc comment line that lists protective subjects (rules.go around lines 85-89) and for `protectivePattern`'s doc comment in `patterns.go` ("each parsed segment, its head, its raw pre-peel text, and the whole command").

Do not touch `allSegmentsAllowed`, `segmentSubject`, `wholeSubject`, or the legacy `subjects` helper. Smart mode's legacy `bash:<firstToken>` subject stays (it is now redundant with `rawHead` for the first segment, as change 0092 promised it would stay).

**Step 4: Run, expect PASS**, then the existing allow and rule fixtures must be unchanged:
`go test ./internal/permissions -run 'TestProtectiveRawSubjects|TestProtectivePatterns_|TestMatchesProtective|TestMatchesPermissive|TestSmartMode_AllowPatternsStayStrict|TestEvalRules_|TestProtectiveMatch_' -count=1`

**Step 5: Commit.** `permissions: protective patterns also match the command as written before wrapper peeling`

---

## Task 3: the catastrophic floor is a rules-layer decision in smart mode

**Tier:** premium (security floor in the gate; correctable but consequential)

**Files:**
- Modify: `internal/permissions/patterns.go` (add `catastrophicSegment`, rework `matchesPermissive`)
- Modify: `internal/permissions/gate.go` (smart branch of `resolve`, around lines 568-591; add a reason-prefix constant next to `explicitAskDenyPrefix`, around line 619)
- Test: `internal/permissions/patterns_test.go` (new rows in `TestMatchesPermissive`; new `TestSmartLegacyAllow_KeepsCatastrophicFloor`, `TestPermissiveIgnoresRawSubjects`)
- Test: `internal/permissions/gate_test.go` (new `TestSmartMode_PolicyStandIn_DeniesCatastrophic`)

**Step 1: Write the failing tests.**

(a) Add rows to the `cases` table in `TestMatchesPermissive` (before `"no patterns never match"`):

```go
		{"legacy first-token allow keeps the catastrophic floor", []string{"bash:rm"}, "rm -rf ~", false},
		{"legacy first-token allow keeps the floor on the root", []string{"bash:rm"}, "rm -rf /", false},
		{"legacy first-token allow keeps the floor on mkfs", []string{"bash:mkfs.ext4"}, "mkfs.ext4 /dev/sda1", false},
		{"legacy bare wrapper allow does not approve a catastrophic inner command", []string{"bash:timeout"}, "timeout 5 rm -rf ~", false},
		{"parser refusal keeps the legacy allow (documented boundary)", []string{"bash:sudo"}, "sudo ls", true},
```

Every existing row stays byte-identical.

(b) Add to `patterns_test.go`:

```go
// TestSmartLegacyAllow_KeepsCatastrophicFloor: smart mode's legacy first-token
// allow never approves a catastrophic segment (change 0093). With a human
// bound the call prompts and the ask names the rules floor; a benign command
// under the same rule still allows without a prompt. A command the parser
// refuses keeps the legacy allow: there is no segment to judge, and the legacy
// subject only ever saw the first word (pinned as the documented boundary).
func TestSmartLegacyAllow_KeepsCatastrophicFloor(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(base, "ws")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	defer chdir(t, ws)()

	cases := []struct {
		allow      []string
		cmd        string
		wantPrompt bool
		wantLayer  string
	}{
		{[]string{"bash:rm"}, "rm -rf ~", true, LayerRules},
		{[]string{"bash:rm"}, "rm -rf /", true, LayerRules},
		{[]string{"bash:rm"}, "rm -rf ..", true, LayerRules},
		{[]string{"bash:rm"}, "rm x", false, LayerSmartConfig},
		{[]string{"bash:mkfs.ext4"}, "mkfs.ext4 /dev/sda1", true, LayerRules},
		{[]string{"bash:sudo"}, "sudo ls", false, LayerSmartConfig},
	}
	for _, tc := range cases {
		t.Run(tc.cmd, func(t *testing.T) {
			approve, called := newApproveRecorder(false)
			cfg := config.PermissionsConfig{Mode: "smart", AutoApprove: tc.allow}
			g := New(cfg, newTestRegistry("bash"), approve,
				WithWorkspaceRoot(ws), WithApprovalProvenance(DecidedByHuman))
			ctx, rec := recordDecisions()
			g.Execute(ctx, "bash", bashArgs(tc.cmd))
			if *called != tc.wantPrompt {
				t.Fatalf("smart %v on %q: prompted=%v, want %v", tc.allow, tc.cmd, *called, tc.wantPrompt)
			}
			if len(*rec) == 0 {
				t.Fatalf("no decision recorded for %q", tc.cmd)
			}
			first := (*rec)[0]
			wantVerdict := "allow"
			if tc.wantPrompt {
				wantVerdict = "ask"
			}
			if first.Verdict != wantVerdict || first.Layer != tc.wantLayer {
				t.Fatalf("first decision for %q = %s/%s, want %s/%s (%+v)", tc.cmd, first.Verdict, first.Layer, wantVerdict, tc.wantLayer, first)
			}
			if tc.wantPrompt && first.Reason != "catastrophic command: "+tc.cmd {
				t.Errorf("ask reason = %q, want %q", first.Reason, "catastrophic command: "+tc.cmd)
			}
		})
	}
}

// TestPermissiveIgnoresRawSubjects: raw pre-peel subjects never widen an allow
// (change 0093). "bash:timeout *" as an allow does not approve a wrapped rm in
// either mode, and a bare "bash:timeout" never approves a catastrophic inner
// command: smart mode prompts (the floor), auto mode denies (as it always did).
func TestPermissiveIgnoresRawSubjects(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := func(cfg config.PermissionsConfig, cmd string) (prompted, denied bool) {
		approve, called := newApproveRecorder(false)
		g := New(cfg, newTestRegistry("bash"), approve, WithWorkspaceRoot(root))
		res := g.Execute(context.Background(), "bash", bashArgs(cmd))
		return *called, res.Denied && !*called
	}
	smart := func(allow ...string) config.PermissionsConfig {
		return config.PermissionsConfig{Mode: "smart", AutoApprove: allow}
	}

	// a/b is relative, so it resolves against the package directory, outside
	// root: no auto-mode heuristic can allow it on its own evidence.
	if p, _ := run(smart("bash:timeout *"), "timeout 5 rm -rf a/b"); !p {
		t.Error("smart: bash:timeout * must not approve timeout 5 rm -rf a/b")
	}
	if p, _ := run(autoCfg(config.AutoConfig{}, []string{"bash:timeout *"}, nil), "timeout 5 rm -rf a/b"); !p {
		t.Error("auto: bash:timeout * must not approve timeout 5 rm -rf a/b")
	}
	if p, _ := run(smart("bash:timeout"), "timeout 5 rm -rf ~"); !p {
		t.Error("smart: bash:timeout must not approve timeout 5 rm -rf ~ (floor)")
	}
	if p, d := run(autoCfg(config.AutoConfig{}, []string{"bash:timeout"}, nil), "timeout 5 rm -rf ~"); p || !d {
		t.Errorf("auto: bash:timeout on timeout 5 rm -rf ~: prompted=%v denied=%v, want a deny with no prompt", p, d)
	}
}
```

(c) Add to `gate_test.go` (near `TestSmartMode_AlwaysPrompt_HumanPrompts`; add `os` and `path/filepath` imports if missing):

```go
// TestSmartMode_PolicyStandIn_DeniesCatastrophic: with only a policy stand-in
// bound, a catastrophic command in smart mode is denied at the rules layer
// whether or not an allow rule names it (change 0093). The stand-in is never
// consulted. Benign commands keep their existing outcome.
func TestSmartMode_PolicyStandIn_DeniesCatastrophic(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(base, "ws")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	defer chdir(t, ws)()

	newGate := func(allow []string) (*PermissionGate, *bool) {
		approve, called := newApproveRecorder(true)
		g := New(config.PermissionsConfig{Mode: "smart", AutoApprove: allow}, newTestRegistry("bash"), approve,
			WithWorkspaceRoot(ws), WithApprovalProvenance(DecidedByPolicy))
		return g, called
	}

	for _, allow := range [][]string{nil, {"bash:rm"}} {
		for _, cmd := range []string{"rm -rf ~", "rm -rf /", "rm -rf ..", "mkfs.ext4 /dev/sda1"} {
			t.Run(fmt.Sprintf("%v/%s", allow, cmd), func(t *testing.T) {
				g, called := newGate(allow)
				ctx, rec := recordDecisions()
				res := g.Execute(ctx, "bash", bashArgs(cmd))
				if !res.Denied {
					t.Fatalf("want a deny; res=%+v decisions=%+v", res, *rec)
				}
				if *called {
					t.Fatal("the policy stand-in must never be consulted for a catastrophic command")
				}
				if len(*rec) != 1 {
					t.Fatalf("want exactly one decision; got %+v", *rec)
				}
				d := (*rec)[0]
				if d.Verdict != "deny" || d.Layer != LayerRules || d.DecidedBy != DecidedByPolicy || d.Mode != "smart" ||
					!strings.HasPrefix(d.Reason, "catastrophic command denied by rules floor:") {
					t.Fatalf("want deny/rules/policy/smart with the floor reason; got %+v", d)
				}
			})
		}
	}

	t.Run("benign with allow", func(t *testing.T) {
		g, called := newGate([]string{"bash:rm"})
		ctx, rec := recordDecisions()
		g.Execute(ctx, "bash", bashArgs("rm x"))
		if *called || len(*rec) != 1 || (*rec)[0].Verdict != "allow" || (*rec)[0].Layer != LayerSmartConfig {
			t.Fatalf("rm x under bash:rm must allow at smart_config without the stand-in; called=%v decisions=%+v", *called, *rec)
		}
	})
	t.Run("benign without allow", func(t *testing.T) {
		g, called := newGate(nil)
		ctx, rec := recordDecisions()
		g.Execute(ctx, "bash", bashArgs("rm x"))
		last := (*rec)[len(*rec)-1]
		if !*called || last.Verdict != "allow" || last.Layer != LayerHuman {
			t.Fatalf("rm x with no allow must be answered by the stand-in at human; called=%v decisions=%+v", *called, *rec)
		}
	})
}
```

Check the imports `fmt` and `strings` are present in `gate_test.go`; add them if not.

**Step 2: Run, expect FAIL:**
`go test ./internal/permissions -run 'TestMatchesPermissive|TestSmartLegacyAllow_KeepsCatastrophicFloor|TestPermissiveIgnoresRawSubjects|TestSmartMode_PolicyStandIn_DeniesCatastrophic' -count=1`

**Step 3: Implement.**

(a) `patterns.go`: add

```go
// catastrophicSegment reports whether a bash call parses into at least one
// catastrophic segment (isDangerous against workspaceRoot) and returns the
// command text for the decision reason (change 0093). It is the one floor
// check smart mode's allow step (matchesPermissive) and the smart branch of
// resolve share, so the two cannot disagree. A non-bash tool, or a command the
// parser refuses (sudo, npx, xargs, a path-qualified argv0, an opaque command
// word), is never catastrophic here: there is no segment to judge.
func catastrophicSegment(toolName, args, workspaceRoot string) (string, bool) {
	if toolName != "bash" {
		return "", false
	}
	command := bashCommand(args)
	segments, err := splitSegments(command)
	if err != nil {
		return "", false
	}
	for _, seg := range segments {
		if isDangerous(seg, workspaceRoot) {
			return command, true
		}
	}
	return "", false
}
```

and rework `matchesPermissive` so the floor runs before the legacy subjects, then drop its now-redundant inner `isDangerous` loop:

```go
func matchesPermissive(patterns []string, toolName, args, workspaceRoot string) bool {
	if len(patterns) == 0 {
		return false
	}
	if _, catastrophic := catastrophicSegment(toolName, args, workspaceRoot); catastrophic {
		return false
	}
	for _, subj := range subjects(toolName, args) {
		if matchesSubjects(consumerPermissive, patterns, subj) {
			return true
		}
	}
	if toolName != "bash" {
		return false
	}
	segments, err := splitSegments(bashCommand(args))
	if err != nil || len(segments) == 0 {
		return false
	}
	return allSegmentsAllowed(segments, patterns, false)
}
```

Rewrite its doc comment: the legacy `{tool, "bash:<firstToken>"}` match is kept for compatibility except that, since change 0093, no allow (legacy or per-segment) admits a command with a catastrophic segment; a command the parser refuses keeps the legacy match, because there is no segment to judge and `bash:sudo` in `auto_approve` is the operator's explicit choice.

(b) `gate.go`: next to `explicitAskDenyPrefix` add

```go
// catastrophicFloorDenyPrefix opens the reason of a smart-mode catastrophic
// deny under a policy stand-in (change 0093). Auto mode's floor keeps its own
// "denied by auto-mode rules layer: " reason.
const catastrophicFloorDenyPrefix = "catastrophic command denied by rules floor: "

// catastrophicAskPrefix opens the ask reason when a human is prompted for a
// catastrophic command in smart mode (change 0093).
const catastrophicAskPrefix = "catastrophic command: "
```

and restructure the smart branch of `resolve` so the floor runs first and the existing always_prompt/allow switch only runs when it does not fire:

```go
	if mode == ModeSmart {
		// The catastrophic floor is a rules-layer decision in every mode
		// (change 0093). Auto mode denies these in evalRules before any rule is
		// read; smart mode checks them here, before always_prompt and before any
		// allow, so no auto_approve rule (legacy first-token or per-segment) can
		// approve one. A policy stand-in never approves a catastrophic command:
		// it is denied at the rules layer. With a human bound, the human keeps
		// the last word: the call prompts and the ask event names the floor.
		if command, catastrophic := catastrophicSegment(name, args, g.workspaceRoot); catastrophic {
			if g.approvalProvenance == DecidedByPolicy {
				reason := catastrophicFloorDenyPrefix + command
				g.emitDecision(ctx, mode, name, args, "deny", LayerRules, reason, g.approvalProvenance, nil)
				return ToolPolicy{Enabled: true, AutoApprove: false, DenyReason: reason, DenyLayer: LayerRules}, nil
			}
			askLayer = LayerRules
			askReason = catastrophicAskPrefix + command
		} else {
			// <the existing always_prompt comment and switch, moved here unchanged>
		}
	}
```

Auto mode's floor (`evalRulesDetail` step 1) and its reason string are untouched. Prompt-all and off are untouched.

**Step 4: Run, expect PASS**, then the whole package (the smart allow, parity and always_prompt fixtures must not move):
`go test ./internal/permissions -count=1`

**Step 5: Commit.** `permissions: apply the catastrophic floor to smart mode's allow step and policy stand-in`

---

## Task 4: the mcp-server gate binds the workspace root and write roots

**Tier:** standard

**Files:**
- Modify: `cmd/fuse/mcp_server.go` (factor the `permissions.New` call at lines 59-61 into `buildMCPServerGate`)
- Test: create `cmd/fuse/mcp_server_gate_test.go`

**Step 1: Write the failing test** (`package main`):

```go
package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// TestMCPServerGateBindsWorkspaceRoot builds the headless mcp-server gate
// exactly as runMCPServer does (buildMCPServerGate, policy stand-in) and runs
// `rm -rf ..` from inside a workspace (change 0093). With the workspace root
// bound, the floor recognises the workspace ancestor and denies it at the
// rules layer in auto and smart mode alike; the workspace survives.
func TestMCPServerGateBindsWorkspaceRoot(t *testing.T) {
	for _, mode := range []string{"auto", "smart"} {
		t.Run(mode, func(t *testing.T) {
			prev := gateApprovalProvenance
			t.Cleanup(func() { gateApprovalProvenance = prev })
			markPolicyApproval()

			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ws := filepath.Join(base, "ws")
			if err := os.Mkdir(ws, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Chdir(ws)

			cfg := config.Config{Permissions: config.PermissionsConfig{Mode: mode}}
			svc, err := sandbox.NewService(sandbox.Config{Contained: false, Handler: sandbox.HandlerHost})
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			toolReg := defaultToolRegistry(svc, cfg.Research, nil)
			gate := buildMCPServerGate(cfg, toolReg, permissions.AlwaysApprove)

			var decisions []permissions.Decision
			ctx := permissions.WithDecisionSink(context.Background(), func(d permissions.Decision) {
				decisions = append(decisions, d)
			})
			res := gate.Execute(ctx, "bash", `{"command":"rm -rf .."}`)
			if !res.Denied {
				t.Fatalf("rm -rf .. under the mcp-server gate must deny; res=%+v decisions=%+v", res, decisions)
			}
			if _, err := os.Stat(ws); err != nil {
				t.Fatalf("workspace ancestor deletion ran: %v", err)
			}
			var ok bool
			for _, d := range decisions {
				if d.Verdict == "allow" {
					t.Fatalf("no allow decision expected; got %+v", decisions)
				}
				if d.Verdict == "deny" && d.Layer == permissions.LayerRules {
					ok = true
				}
			}
			if !ok {
				t.Fatalf("want a deny at the rules layer; got %+v", decisions)
			}
		})
	}
}
```

**Step 2: Run, expect FAIL** (compile error: `buildMCPServerGate` undefined):
`go test ./cmd/fuse -run TestMCPServerGateBindsWorkspaceRoot -count=1`

**Step 3: Implement** in `mcp_server.go`. Add

```go
// buildMCPServerGate constructs the headless mcp-server's permission gate,
// scoped like every other binding's gate (change 0093): the workspace root is
// the server's cwd and the write roots are the session scratch dir plus the
// trusted permissions.auto.write_roots. The root is bound once for the
// process, which is correct: the config-reload path rebuilds tools in place
// and never the gate, and the server's cwd does not move. The binding has no
// model registry, so it gets no classifier (unchanged). runMCPServer and its
// test share this function so the test proves the binding's own construction.
func buildMCPServerGate(cfg config.Config, toolReg *tools.Registry, approve permissions.ApprovalFunc) *permissions.PermissionGate {
	return permissions.New(cfg.Permissions, toolReg, approve,
		permissions.WithApprovalProvenance(gateApprovalProvenance),
		permissions.WithWorkspaceRoot(workspaceRoot()),
		permissions.WithWriteRoots(gateWriteRoots(cfg)))
}
```

and replace the two-line `gate := permissions.New(...)` in `runMCPServer` with `gate := buildMCPServerGate(cfg, toolReg, approve)`. The approve selection and `markPolicyApproval()` stay before it, unchanged.

**Step 4: Run, expect PASS:**
`go test ./cmd/fuse -run 'TestMCPServerGateBindsWorkspaceRoot|TestMCPServer' -count=1`

**Step 5: Commit.** `mcp-server: bind the workspace root and write roots on the permission gate`

---

## Task 5: composition-root and end-to-end loop-server proofs

**Tier:** standard

These tests prove Tasks 2 and 3 through the production wirings. They pass once Tasks 2 and 3 are in; there is no new production code here. To confirm each test is not vacuous, run it once with the relevant Task 2 or Task 3 hunk temporarily disabled in the working tree (comment out the floor block or the `rawSubjects` append locally), observe the failure, restore the file, and never commit the disabled form. Record the observed red in the task report.

**Files:**
- Test: create `cmd/fuse/permission_floor_binding_test.go` (`package main`)

Safety rule for every test in this file: `t.Chdir` into a fresh, symlink-resolved `<tmp>/ws` BEFORE building any gate or harness, so the bound workspace root and the cwd agree and any accidental execution of `rm -rf ..` can only reach the test's own temp directory.

**Step 1: Write the tests.**

Shared helpers in the file:

```go
// floorWorkspace chdirs into a fresh, symlink-resolved <tmp>/ws and returns
// (base, ws).
func floorWorkspace(t *testing.T) (string, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(base, "ws")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(ws)
	return base, ws
}

// scriptedBashCall renders one chat-completions reply asking for a bash call.
// Arguments are marshalled from plain values exactly once per layer, never
// hand-escaped (see the scripted-gateway double-escaping finding).
func scriptedBashCall(command string) string {
	args, _ := json.Marshal(map[string]string{"command": command})
	callArgs, _ := json.Marshal(string(args))
	return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"bash","arguments":` + string(callArgs) + `}}]}}]}`
}

const scriptedFinal = `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`

// runScriptedLoop starts one loop on rt, drains Observe, waits, and returns
// the permission.decision payloads from the durable history only (the live
// tail can overlap it, so folding both would double-count).
func runScriptedLoop(t *testing.T, rt runtime.Runtime) []event.PermissionDecisionPayload {
	t.Helper()
	h, err := rt.StartLoop(context.Background(), runtime.LoopConfig{Task: "go"})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	ch, cancel, err := rt.Observe(context.Background(), event.DefaultTenant, h.ID())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	defer cancel()
	for range ch {
	}
	_, _ = h.Wait()
	hist, err := rt.Attach(context.Background(), event.DefaultTenant, h.ID(), 0)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	var out []event.PermissionDecisionPayload
	for _, ev := range hist {
		if ev.Kind != event.KindPermissionDecision {
			continue
		}
		var d event.PermissionDecisionPayload
		if json.Unmarshal(ev.Payload, &d) == nil {
			out = append(out, d)
		}
	}
	return out
}

// modelWasTold reports whether the second gateway request carried a tool-role
// message containing want.
func modelWasTold(gw *recordingGateway, want string) bool {
	calls := gw.snapshot()
	if len(calls) < 2 {
		return false
	}
	for _, m := range calls[1].Messages {
		if m.Role == "tool" && strings.Contains(m.Content, want) {
			return true
		}
	}
	return false
}
```

Test 1, `TestHostedBindingDeniesCatastrophicInSmartMode`:

```go
// TestHostedBindingDeniesCatastrophicInSmartMode proves, at the composition
// root the loop servers share (buildGate under markPolicyApproval +
// AlwaysApprove), that a smart-mode legacy allow cannot approve a workspace
// ancestor deletion (change 0093): it is denied at the rules layer and the
// sentinel survives.
func TestHostedBindingDeniesCatastrophicInSmartMode(t *testing.T) {
	prev := gateApprovalProvenance
	t.Cleanup(func() { gateApprovalProvenance = prev })
	markPolicyApproval()

	_, ws := floorWorkspace(t)
	keep := filepath.Join(ws, "keep")
	if err := os.Mkdir(keep, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{Permissions: config.PermissionsConfig{
		Mode:        "smart",
		AutoApprove: []string{"bash:rm"},
	}}
	// buildGate binds the workspace root through autoModeOptions, which only
	// applies when a gateway is configured, as it is in every loop-server
	// binding. Smart mode never consults the classifier, so this is never dialled.
	cfg.Gateway.URL = "http://127.0.0.1:1"
	svc, err := sandbox.NewService(sandbox.Config{Contained: false, Handler: sandbox.HandlerHost})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	toolReg := defaultToolRegistry(svc, cfg.Research, nil)
	gate := buildGate(cfg, toolReg, permissions.AlwaysApprove, registryFromConfig(cfg), nil, nil)

	var decisions []permissions.Decision
	ctx := permissions.WithDecisionSink(context.Background(), func(d permissions.Decision) {
		decisions = append(decisions, d)
	})
	res := gate.Execute(ctx, "bash", `{"command":"rm -rf .."}`)
	if !res.IsError || !res.Denied {
		t.Fatalf("rm -rf .. under the hosted smart binding must deny; res=%+v decisions=%+v", res, decisions)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("denied command was executed: %v", err)
	}
	var ok bool
	for _, d := range decisions {
		if d.Verdict == "allow" {
			t.Fatalf("no allow decision expected; got %+v", decisions)
		}
		if d.Verdict == "deny" && d.Layer == permissions.LayerRules && d.DecidedBy == permissions.DecidedByPolicy &&
			d.Mode == "smart" && strings.HasPrefix(d.Reason, "catastrophic command denied by rules floor:") {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("want deny/rules/policy/smart with the floor reason; got %+v", decisions)
	}
}
```

If `registryFromConfig` needs a `models:` block to produce a usable default, give `cfg` the same minimal models block the harness uses (parse `harnessOperator` plus the permissions block with `config.FromYAML(..., "operator")` and then set `cfg.Gateway.URL`), rather than passing a nil registry (nil panics in `registryDefault`).

Test 2, `TestLoopServerDeniesWrapperRuleEndToEnd`, two subtests on the production loop-server wiring (`harnessServer` + scripted `recordingGateway`):

```go
// TestLoopServerDeniesWrapperRuleEndToEnd runs the production loop-server
// runtime against a scripted gateway whose model asks for a wrapped command
// (change 0093). A protective rule naming the wrapper must fire: in auto mode
// an auto.deny rule denies at the rules layer, and in smart mode (the default)
// an always_prompt rule with argument text, reachable only through the raw
// pre-peel subject, is denied by policy. The harness binds no sandbox, so bash
// could not run here either way; the decision events are the proof.
func TestLoopServerDeniesWrapperRuleEndToEnd(t *testing.T) {
	t.Run("auto deny", func(t *testing.T) {
		prev := gateApprovalProvenance
		t.Cleanup(func() { gateApprovalProvenance = prev })
		markPolicyApproval()
		floorWorkspace(t)

		rt, gw, _ := harnessServer(t, harnessOperator+`
permissions:
  mode: auto
  auto:
    deny: ["bash:timeout*"]
`)
		gw.script = []string{scriptedBashCall("timeout 5 touch ran"), scriptedFinal}
		ds := runScriptedLoop(t, rt)

		var denies []event.PermissionDecisionPayload
		for _, d := range ds {
			if d.Tool == "bash" && d.Verdict == "allow" {
				t.Fatalf("the wrapped call must not be allowed; got %+v", d)
			}
			if d.Verdict == "deny" {
				denies = append(denies, d)
			}
		}
		if len(denies) != 1 {
			t.Fatalf("want exactly one deny; got %+v", denies)
		}
		d := denies[0]
		if d.Layer != permissions.LayerRules || d.Mode != "auto" ||
			!strings.HasPrefix(d.Reason, "denied by auto-mode rules layer:") || !strings.Contains(d.Reason, "timeout 5 touch ran") {
			t.Fatalf("want deny/rules/auto naming the command; got %+v", d)
		}
		if !modelWasTold(gw, d.Reason) {
			t.Fatalf("model was not told the deny reason %q", d.Reason)
		}
	})

	t.Run("smart always_prompt", func(t *testing.T) {
		prev := gateApprovalProvenance
		t.Cleanup(func() { gateApprovalProvenance = prev })
		markPolicyApproval()
		floorWorkspace(t)

		// "bash:timeout *" (with a space) misses smart mode's legacy
		// "bash:timeout" head, so only the raw pre-peel subject reaches it.
		rt, gw, _ := harnessServer(t, harnessOperator+`
permissions:
  always_prompt: ["bash:timeout *"]
`)
		gw.script = []string{scriptedBashCall("timeout 5 touch ran"), scriptedFinal}
		ds := runScriptedLoop(t, rt)

		var denies []event.PermissionDecisionPayload
		for _, d := range ds {
			if d.Tool == "bash" && d.Verdict == "allow" {
				t.Fatalf("the wrapped call must not be allowed; got %+v", d)
			}
			if d.Verdict == "deny" {
				denies = append(denies, d)
			}
		}
		if len(denies) != 1 {
			t.Fatalf("want exactly one deny; got %+v", denies)
		}
		d := denies[0]
		if d.Mode != "smart" || d.Layer != permissions.LayerRules || d.DecidedBy != permissions.DecidedByPolicy ||
			!strings.Contains(d.Reason, "bash:timeout *") {
			t.Fatalf("want deny/rules/policy/smart naming bash:timeout *; got %+v", d)
		}
		if !modelWasTold(gw, "matched ask rule bash:timeout *") {
			t.Fatal("model was not told the deny reason")
		}
	})
}
```

Test 3, `TestLoopServerDeniesCatastrophicInSmartModeEndToEnd`:

```go
// TestLoopServerDeniesCatastrophicInSmartModeEndToEnd runs the production
// loop-server runtime in its default smart mode with a legacy allow naming rm,
// against a scripted model that asks for a workspace ancestor deletion (change
// 0093). With no human bound it must be denied at the rules layer by the
// floor. The harness binds no sandbox; the decision event plus
// TestHostedBindingDeniesCatastrophicInSmartMode are the proof of
// non-execution.
func TestLoopServerDeniesCatastrophicInSmartModeEndToEnd(t *testing.T) {
	prev := gateApprovalProvenance
	t.Cleanup(func() { gateApprovalProvenance = prev })
	markPolicyApproval()
	floorWorkspace(t)

	rt, gw, _ := harnessServer(t, harnessOperator+`
permissions:
  auto_approve: ["bash:rm"]
`)
	gw.script = []string{scriptedBashCall("rm -rf .."), scriptedFinal}
	ds := runScriptedLoop(t, rt)

	var denies []event.PermissionDecisionPayload
	for _, d := range ds {
		if d.Tool == "bash" && d.Verdict == "allow" {
			t.Fatalf("rm -rf .. must not be allowed; got %+v", d)
		}
		if d.Verdict == "deny" {
			denies = append(denies, d)
		}
	}
	if len(denies) != 1 {
		t.Fatalf("want exactly one deny; got %+v", denies)
	}
	d := denies[0]
	if d.Mode != "smart" || d.Layer != permissions.LayerRules || d.DecidedBy != permissions.DecidedByPolicy ||
		!strings.HasPrefix(d.Reason, "catastrophic command denied by rules floor:") {
		t.Fatalf("want deny/rules/policy/smart with the floor reason; got %+v", d)
	}
	if !modelWasTold(gw, "catastrophic command denied by rules floor:") {
		t.Fatal("model was not told the deny reason")
	}
}
```

Imports for the file: `context`, `encoding/json`, `os`, `path/filepath`, `strings`, `testing`, and `internal/config`, `internal/event`, `internal/permissions`, `internal/runtime`, `internal/tools/sandbox`.

If `harnessServer` or the loop server builds its gate before `StartLoop` and reads `workspaceRoot()` at that point, calling `floorWorkspace` before `harnessServer` (as above) already covers it. Do not call `floorWorkspace` after `harnessServer`.

**Step 2: Run, expect PASS** (Tasks 2 and 3 are in):
`go test ./cmd/fuse -run 'TestHostedBindingDeniesCatastrophicInSmartMode|TestLoopServerDeniesWrapperRuleEndToEnd|TestLoopServerDeniesCatastrophicInSmartModeEndToEnd|TestHostedBindingDenies|TestLoopServerDenies' -count=1`

**Step 3: Non-vacuity check** as described at the top of this task: with the floor block in `gate.go` locally disabled, `TestHostedBindingDeniesCatastrophicInSmartMode` and `TestLoopServerDeniesCatastrophicInSmartModeEndToEnd` fail; with the `rawSubjects(seg)...` append in `protectiveSegmentSubjects` locally disabled, both `TestLoopServerDeniesWrapperRuleEndToEnd` subtests fail. Restore before committing; `git diff` must show only the new test file.

**Step 4: Commit.** `cmd/fuse: prove the smart floor and wrapper rules at the loop-server composition root`

---

## Task 6: README sentence

**Tier:** economy

**Files:**
- Modify: `README.md`, the "How patterns match (every mode alike)" paragraph (around lines 249-269)

**Step 1: Edit.** Immediately after the sentence ending ``matches `npm publish ./pkg/dir`.`` insert:

```
Protective patterns also match the command as written, before wrappers such as
`timeout`, `nice`, `env` and `bash -c` are peeled, so a rule naming the wrapper
fires in every mode.
```

so the paragraph reads "... and `ask: ["bash:npm publish*"]` matches `npm publish ./pkg/dir`. Protective patterns also match the command as written, ... fires in every mode. A matching `always_prompt` or ...". No other README edits. No em dashes.

**Step 2: Verify** the file still renders (no broken backticks): `grep -n "fires in every mode" README.md` prints one line.

**Step 3: Commit.** `docs: protective patterns match the command as written before wrapper peeling`

---

## After the tasks (parent-owned, not build tasks)

- **ADR** (parent, via `docket-adr`): one ADR, Accepted, relating to ADR-0005, ADR-0049 and ADR-0065: the catastrophic floor is a rules-layer decision in every mode and binding that evaluates rules, a policy stand-in never approves a catastrophic command, and protective patterns match the command as written as well as as parsed, while permissive patterns match only the parsed, strict form.
- **Release note / PR body compatibility bullets** (there is no CHANGELOG file; release notes come from PR bodies), all tightenings in the safe direction:
  - Smart-mode `auto_approve` first-token rules (`bash:rm`, `bash:dd`, `bash:mkfs*`, `bash:shutdown`, `bash:timeout`) no longer approve a catastrophic segment: interactive users get a prompt naming the floor, headless smart loops get a rules-layer deny with a reason. Benign commands under the same rules are unaffected. Commands the parser refuses (`sudo`, `npx`, `xargs`, path-qualified argv0) keep their legacy first-token allow.
  - Headless smart gates (loop servers, the mcp-server without a HITL socket) deny catastrophic commands they previously policy-approved.
  - Protective rules naming a wrapper (`bash:timeout*`, `bash:nice *`, `bash:env *`, `bash:bash *`) start firing in auto mode and, for the argument-bearing forms, in smart mode.
  - The mcp-server gate scopes against its cwd; `rm -rf ..` and other workspace-ancestor deletions are denied there as everywhere else.
  - No config keys change. Allow matching is not widened anywhere.

## Known residuals (out of scope, for the parent to note or file)

- `buildGate` binds `WithWorkspaceRoot` only when a gateway is configured (`autoModeOptions`), so a gate built with no gateway still sees an empty root and cannot recognise workspace-ancestor deletions. Every loop-server binding has a gateway; binding the root unconditionally would also enable deterministic in-workspace allows in degraded auto mode, which is a loosening this change does not make.
- Prompt-all mode under a policy stand-in still approves every call, catastrophic ones included; this change scopes the floor to smart mode as the spec does.
- `denyHint(LayerRules)` says "blocked by auto-mode policy as catastrophic" for every rules-layer deny, including smart-mode floor and explicit-ask denies. Pre-existing wording.
