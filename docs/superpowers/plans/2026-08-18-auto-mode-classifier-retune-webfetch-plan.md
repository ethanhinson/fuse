<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0069 — Auto-mode classifier retune + web_fetch loosening — allow-bias for routine dev ops, seed becomes real auto-approve](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0069-auto-mode-classifier-retune-webfetch.md)**
<!-- docket:backlink:end -->

# Auto-mode classifier retune + web_fetch loosening — implementation plan

**Change:** #0069 · Stage C of the auto-mode overhaul (arc: #0067 → #0068 → #0069 → #0070)
**Spec:** `docs/superpowers/specs/2026-08-17-auto-mode-classifier-retune-webfetch-design.md` (on `docket`)
**Base:** `origin/main` @ `0257678`

> **Plan-role degrade:** `skills.plan` resolves to `superpowers:writing-plans`, which is not
> invocable in this environment. Per the convention's missing-skill rule the role degraded to
> `auto` and this plan file was authored directly by the implementer.

## Goal

Auto-mode currently denies routine developer work. Both LLM classifier prompts are explicitly
block-biased, and the `web_fetch` known-good seed is only a bias hint — every fetch still burns a
classifier call and 32 observed denials hit ordinary hosts. With #0068's catastrophic floor merged,
safety no longer rests on classifier pessimism, so this change flips the prompts allow-biased,
promotes the strong seed to a real auto-approve, and raises the valve's total budget.

## Invariants (must hold at the end)

1. **Config always beats the seed.** `fetch_deny` / `fetch_ask` are checked before any known-good
   promotion; a configured deny of a seeded host still denies.
2. **The SSRF floor is untouched and still first.**
3. **Input hygiene is untouched.** Tool results and assistant reasoning never reach either prompt.
4. **Dot-boundary matching stays.** `notgithub.com` and `github.com.evil.example` are never seeded.
5. **Broad TLD wildcards (`*.gov`, `*.edu`, `*.dev`) never auto-approve** — nudge-only.
6. **`valveConsecutiveLimit` stays 3.** Only the total budget moves.

## Tasks

Sequenced D1 → D2 → D3 (D1 and D2 share prompt wording; D3 is independent and last).

---

### Task 1 — Bash classifier: allow-biased system prompt (D1a)

**Files:** `internal/permissions/classifier.go`, `internal/permissions/classifier_test.go`

1. **Test first.** In `classifier_test.go`, add `TestClassifierSystemPrompt_AllowBiased`:
   - asserts `classifierSystemPrompt` names the allow-shapes — network reads, package installs,
     managing the agent's own dev-server processes, temp/scratch use;
   - asserts it names the deny-shapes — exfiltration, piping remote content into a shell,
     privilege escalation, destruction outside the workspace, credential harvesting;
   - asserts the old block-bias sentence (`"Be block-biased"`) is **gone**;
   - asserts the JSON contract line survives verbatim (`{"verdict":"allow|deny|ask"`).
   Assert on lowercased content with `strings.Contains` so wording can breathe.
2. Rewrite `classifierSystemPrompt` (classifier.go:54-59) accordingly: a permission gate for a
   coding agent working in a workspace; routine dev operations are expected and should be
   **allowed**; deny only the named dangerous shapes; ask only for genuinely ambiguous calls.
   Keep it terse and single-stage — no chain-of-thought.
3. Update the const's doc comment and the `Classifier` type comment (classifier.go:61-65) and
   `Classify`'s doc (classifier.go:153-…), which all currently say "block-biased". The
   **fail-closed** contract wording stays — a timeout still resolves to `VerdictAsk`.

**Verify:** `go test ./internal/permissions/ -run 'Classif'`.

---

### Task 2 — Workspace context in the pending-call prompt (D1b)

**Files:** `internal/permissions/classifier.go`, `cmd/fuse/run.go`,
`internal/permissions/classifier_test.go`, `internal/permissions/clone_test.go`

1. **Test first.**
   - `classifier_test.go`: extend/add a test proving the pending prompt carries the workspace root
     and scratch dir when set, and omits the context line entirely when both are empty (a zero
     `Classifier` must not emit `workspace: , scratch: `).
   - `clone_test.go`: assert `cloneForChild` **propagates** the two new fields.
2. Add two unexported fields to `Classifier` — `workspaceRoot`, `scratchDir`.
3. Add an exported chainable setter `func (c *Classifier) WithWorkspaceContext(workspaceRoot,
   scratchDir string) *Classifier` returning the receiver (nil-safe). **Do not change
   `NewClassifier`'s signature** — `classifier_export_test.go` pins it, and a new parameter would
   be gratuitous churn.
4. Thread the fields into `pendingCallPrompt`. Change it from a package function to a method on
   `*Classifier` (or pass the two strings) so it can render
   `workspace: <root>, scratch: <dir>` as a context line. Suppress the line when both are empty.
5. Copy both fields in `cloneForChild` (classifier.go:137-151) alongside `client`/`modelID`.
6. Wire it in `cmd/fuse/run.go`'s `autoModeOptions` (run.go:387): after constructing `cls`, call
   `cls.WithWorkspaceContext(workspaceRoot(), sessionScratchDir())`.
   **Reconcile note:** the spec said the scratch dir arrives "from `autoModeOptions`", but scratch
   is actually resolved one level up in `buildGate` via `gateWriteRoots`. `sessionScratchDir()`
   (`cmd/fuse/scratch.go`) is directly callable here, so call it in place — no signature churn
   through `buildGate`, same value.

**Verify:** `go test ./internal/permissions/ ./cmd/fuse/`.

---

### Task 3 — Split the seed into strong vs. nudge-only (D2a)

**Files:** `internal/permissions/fetchhost.go`, `internal/permissions/fetchhost_test.go`

1. **Test first.** In `fetchhost_test.go`:
   - `strongSeedMatch("github.com")` is true; `strongSeedMatch("agency.gov")` is **false**
     (TLD wildcards are nudge-only);
   - `knownGoodSeedMatch` still covers **both** sets, so the existing nudge behavior for `*.gov`
     survives;
   - keep the existing spoof/dot-boundary regressions green unchanged
     (`TestClassifyFetchHost_SpoofNoNudge`, `TestHostMatchesSuffix`).
2. Split `knownGoodSeed` (fetchhost.go:26-60) into two vars: `strongKnownGoodSeed` (the exact/suffix
   host entries — code/dev hosting + official docs/references) and `nudgeOnlyKnownGoodSeed`
   (`*.gov`, `*.edu`, `*.dev`). Document *why* the TLD wildcards cannot auto-approve.
3. Add `strongSeedMatch(host)`; redefine `knownGoodSeedMatch(host)` as the union of both sets so
   the `AllowNudge` surface is behaviour-identical.

**Verify:** `go test ./internal/permissions/ -run FetchHost`.

---

### Task 4 — Known-good becomes a real auto-approve (D2b)

**Files:** `internal/permissions/fetchhost.go`, `internal/permissions/gate.go`,
`internal/permissions/fetchhost_test.go`, `internal/permissions/gate_test.go`

1. **Test first.**
   - `fetchhost_test.go`: a strong-seed host returns `Verdict: VerdictAllow` with
     `DecidedBy: "known-good"`; a `reputation.KnownGood` top-site (e.g. `google.com`) likewise;
     an unrecognized host still returns `DecidedBy: "fallthrough"`; **"config `fetch_deny` beats
     known-good"** — `classifyFetchHost("https://github.com/x", []string{"github.com"}, nil)`
     denies with `config-deny`; and the same for `fetch_ask`.
   - `gate_test.go`: extend `TestResolveAuto_WebFetchStaticFloor` (line 302) with a known-good arm
     asserting `VerdictAllow` and **`stub.calls == 0`** — the classifier is never invoked.
2. `classifyFetchHost` (fetchhost.go:130-159): after the blocklist check, add
   `if strongSeedMatch(host) || reputation.KnownGood(host)` ⇒ return
   `{Verdict: VerdictAllow, DecidedBy: "known-good", Host: host, AllowNudge: true}`.
   **Order is load-bearing** — this sits strictly after SSRF, `config-deny`, `config-ask`, and the
   blocklist. Update the layer-order doc comment.
3. `gate.go` web_fetch routing (**now at gate.go:592-607**, the spec's 449-459): in the `default:`
   arm, handle `r.DecidedBy == "known-good"` by returning `VerdictAllow, LayerFetchFloor, ""`
   *before* falling through to `g.classifyWebFetch`. Note in a comment that a known-good allow does
   **not** touch the valve — it is a floor decision, not a classifier non-block.
4. Because `DecidedBy` gained a value, update the `fetchFloorResult.DecidedBy` doc enum
   (fetchhost.go:15).

**Verify:** `go test ./internal/permissions/`.

---

### Task 5 — Allow-biased web_fetch pending prompt (D2c)

**Files:** `internal/permissions/classifier.go`, `internal/permissions/classifier_test.go`

1. **Test first.** Update `TestClassifyWebFetch_PromptNamesHostAndReputation` and
   `TestClassifyWebFetch_KnownGoodHintNotABypass`: the prompt must still name the host and still
   state that the known-good hint is not an absolute bypass, **and** must now name the read-only
   GET framing plus the deny shapes (credential-bearing URLs, webhook endpoints, paste/upload
   services, raw-IP URLs, URLs encoding workspace data). Relax the bare `"reputation"` assertion if
   the rewrite drops that literal word — assert on the *shapes*, which is what the spec fixes.
2. Rewrite `webFetchPendingPrompt` (classifier.go:240-253) allow-biased per the spec's D2 wording.
   `web_fetch` is verified GET-only (`internal/tools/web_fetch.go:52-60`), so the "read-only GET
   returning page text" framing is factually true — re-verify that before asserting it.

**Verify:** `go test ./internal/permissions/`.

---

### Task 6 — Valve total budget 20 → 50, tests driven off the constant (D3)

**Files:** `internal/permissions/gate.go`, `internal/permissions/valve_test.go`

1. **Test first.** Rework `TestValve_TwentyTotalBlocks_TripsIndependentOfConsecutive`
   (valve_test.go:243-269) to drive every literal off `valveTotalLimit` — loop counts, the
   expected-calls arithmetic, and the failure message. Rename it
   `TestValve_TotalBlocks_TripsIndependentOfConsecutive`. Sweep the file for any other bare `20`.
2. `gate.go:120-126`: `valveTotalLimit` 20 → 50; `valveConsecutiveLimit` stays 3. Update the
   const doc comment and the `escalationValve` type comment (gate.go:104-110), both of which say
   "20 total", to name the constants and the #0069 rationale rather than fresh literals.

**Verify:** `go test ./internal/permissions/ -run Valve`.

---

### Task 7 — Full-suite gate

Run the repo suite (`make test`) on the branch. Green is the precondition for review.
Grep the whole `internal/permissions` tree plus `docs/` for now-stale "block-biased" / "20 total"
prose and fix any that is load-bearing documentation.

## Out of scope

Rules/heuristic layer changes (#0068, merged). Shell parsing (#0070). Reputation DB expansion
beyond what #0045 shipped.

## Risks

- **Prompt assertions are brittle.** Assert on lowercased substrings for concepts, never on whole
  sentences.
- **`reputation.KnownGood` breadth is now load-bearing.** It was a bias hint; it is now an
  auto-approve. Its source is the bundled popularity CSV top-sites set — the exact-match
  `normalize` lookup (reputation.go:162-170) means no subdomain widening, which is the containment
  argument. Confirm that before Task 4 lands.
- **Seed poisoning by subdomain** is already mitigated by dot-boundary matching; Task 3 pins it.
