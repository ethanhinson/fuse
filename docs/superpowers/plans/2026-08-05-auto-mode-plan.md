<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0017 — Auto mode — layered safe/unsafe classification for autonomous tool approval](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0017-auto-mode.md)**
<!-- docket:backlink:end -->

# Plan — Auto mode: layered safe/unsafe classification for autonomous tool approval (change 0017)

> **Plan authored inline (auto fallback).** The configured plan skill
> `superpowers:writing-plans` was not invocable in this harness, so per the
> docket Skill-layer missing-skill rule the plan role degraded to `auto` and
> this plan was authored by the implementer directly. Noted in the PR body.

Spec: `docs/superpowers/specs/2026-08-05-auto-mode-design.md` (on the `docket`
branch). Read D1–D9 there for the full design rationale; this plan is the
task breakdown.

## Context anchors (verified against `origin/main` at reconcile)

- `internal/permissions/` — `gate.go` (`PermissionGate`, `resolve`, `Execute`,
  `AlwaysApprove`, `CloneForChild`), `policy.go` (`PermissionMode`,
  `ParseMode`, `ModeSmart|Off|PromptAll`, `safeList`, `onSafeList`),
  `patterns.go` (`matchesAny`, `subjects`, `firstToken`), `cache.go`.
- `internal/config/schema.go` — `PermissionsConfig` (Mode, SessionAllow,
  AutoApprove, AlwaysPrompt, Disabled).
- `internal/config/loader.go` — `Load()` → `Default()` then `mergeFile(&c,
  "~/.fuse/config.yml")` then `mergeFile(&c, ".fuse.local.yml")`. `mergeFile`
  takes only a path today; it does **not** distinguish trusted vs.
  repo-plantable source.
- `internal/model/adapter.go` — `Adapter.Complete(ctx, CompletionReq)` at
  ~L212, bounded (RequestTimeout 5m/attempt, response-header timeout, retries,
  labeled traces). `NewAdapter`, `WithTraceLabel`. This is the classifier's
  gateway path.
- `internal/model/registry.go` — `DefaultRegistry()`, `Resolve(alias)`,
  `Names()`; default alias `deepseek-flash`. **No `haiku` alias** (spec fixed
  to use `deepseek-flash` in its example).
- `AlwaysApprove` construction sites: `cmd/fuse/main.go:144,146,179`,
  `cmd/fuse/shell.go:150`.
- Tests: table-driven, `stubTool` / `newTestRegistry` helpers in
  `internal/permissions/*_test.go`. Module `github.com/ethanhinson/fuse`,
  Go 1.26.5.

## Learnings folded in (from `docs/changes/learnings/`)

- **bound-every-model-call** — the classifier (Task 6) MUST route through the
  existing bounded `Adapter`, never a fresh `http.DefaultClient`. Per-attempt
  timeout, response-header timeout, bounded retries, cancel-aware, and a
  labeled trace entry. Timeout/error ⇒ fail closed to `ask`.
- **dirent-isdir-skips-symlinks** — the path-scoping heuristic (Task 5) must
  resolve symlinks (`filepath.EvalSymlinks` / `os.Stat`) before deciding a
  path is inside the workspace; an `IsDir`-style check on the entry alone
  misses symlinked escapes.
- **yaml-plain-scalar-colon-space** — pattern lists (`auto_approve`, `deny`,
  `ask`) hold values like `bash:git *` containing `: `; these are YAML
  sequence *items*, not mapping values, so `yaml.Unmarshal` of a sequence is
  fine — but any single-quoting guidance in docs/examples must keep colon-
  bearing scalars quoted where they'd be read as map keys.

## Ordering rationale

Deterministic layers are the floor and ceiling; the classifier is the
probabilistic middle. Build inside-out: parser → segmentation → static
allow/ask/deny + safe list → heuristics → classifier → mode wiring →
fallback/`--approve-all` → trust boundary. Each task is TDD (focused failing
test first), and the bypass corpus (Task 3) grows across every static task.

---

## Task 1 — Add the `auto` mode enum + config surface (no behavior yet)

**Files:** `internal/permissions/policy.go`, `internal/config/schema.go`.

- Add `ModeAuto` to the `PermissionMode` iota and map `"auto"` in `ParseMode`.
- Extend `PermissionsConfig` with an `Auto` sub-struct:
  `ClassifierModel string` (`yaml:"classifier_model"`), `Deny []string`
  (`yaml:"deny"`), `Ask []string` (`yaml:"ask"`), nested under
  `Auto AutoConfig \`yaml:"auto"\``.
- Wire the new `auto:` block through `rawConfig` / `mergeFile` in
  `loader.go` (slice + string fields), following the existing permission-field
  merge pattern.

**Tests (write first):** `ParseMode("auto") == ModeAuto`; loader test that a
config with a `permissions.auto` block populates `ClassifierModel`/`Deny`/`Ask`.

**Done when:** compiles, mode + config parse, no gate wiring yet.

---

## Task 2 — Shell parsing + per-segment splitting (deterministic floor)

**Dependency:** add `mvdan.cc/sh/v3` (bash AST in Go) — `go get`, commit the
`go.mod`/`go.sum` bump in this task.

**Files:** new `internal/permissions/shellparse.go` + test.

- `splitSegments(cmd string) ([]Segment, error)` — parse the command to an AST
  and enumerate every simple-command segment across `&&`, `||`, `;`, `|`, and
  newlines, **including inside `bash -c "…"` / `sh -c "…"`** script bodies.
- **Fail-closed** (`return …, ErrUnparseable`) on: parse error; command
  substitution (`$( )`, backticks); process substitution; oversized input
  (fixed byte cap); a path-qualified `argv[0]` (contains `/` — the Codex
  basename-collapse bug: `./sed` must NOT match the safe list); an
  arbitrary-arg wrapper as argv[0] (`bash`/`sh` without a parseable `-c`,
  `xargs`, `env` with assignments, `npx`, `timeout`-then-unknown, `sudo`).
- Each `Segment` exposes `Name` (argv[0] basename), `Args []string`, and the
  raw segment text.

**Tests (write first):** a first slice of the **bypass corpus** — `git status
&& rm -rf ~` → 2 segments; `git diff $(curl …)` → ErrUnparseable; `git
status# $(id)` → ErrUnparseable; `pwd\nrm -rf .` → 2 segments; `URL=evil curl
$URL` → wrapper/assignment fail-closed; `./sed` → path-qualified fail-closed;
`bash -c "ls && rm x"` → 2 inner segments; word-boundary `lsof` vs `ls`.

**Done when:** every corpus row parses to the expected segment set or the
expected `ErrUnparseable`.

---

## Task 3 — Per-segment allow/ask/deny rule evaluation (deny-first, default-deny)

**Files:** `internal/permissions/rules.go` + test; extend `patterns.go` only if
a per-segment matcher is cleaner as a new function (keep `matchesAny` for the
legacy smart path).

- `evalRules(segments []Segment, cfg AutoConfig, autoApprove, alwaysPrompt
  []string) Verdict` where `Verdict ∈ {allow, ask, deny}`.
- Precedence, evaluated against **every segment and the whole string**:
  **deny wins globally** (any segment matching a deny/dangerous pattern ⇒
  deny, regardless of any allow), then **ask**, then **allow**; **default
  action = deny** (CWE-1188 hardening) — an action no rule admits is **ask**,
  never allow (fail toward the human).
- Allow rules are matched **per-segment** against the parsed argv, never the
  raw string (the one deliberate deviation from Grok Build — closes the
  `git status && rm -rf /` hole).
- Built-in **dangerous-command list** (`rm`, `chmod`, `kill`, `git push`,
  `curl`/`wget` with network egress, `dd`, `mkfs`, …) that forces ask/deny
  even over an allow rule and over session grants.

**Tests (write first):** grow the bypass corpus — an `auto_approve: ["bash:git
*"]` rule does NOT approve `git status && rm -rf ~` (deny wins on segment 2);
a whole-string allow never overrides a segment deny; unmatched command ⇒ ask;
denylist-wrapping (`sh -c "rm x"`) still denies (Task 2 already surfaces the
inner segment).

**Done when:** corpus green; deny-first + default-deny proven.

---

## Task 4 — Built-in read-only safe list, evaluated per-segment

**Files:** `internal/permissions/policy.go` (extend), `rules.go`.

- Keep the tool-level `onSafeList` for non-bash tools (`read_file`,
  `list_directory`, `grep`, `codeindex_*`).
- Add a **bash argv[0] read-only allowlist** with flag-inspecting
  conditionals (Codex `is_safe_command` shape): `find` unsafe with
  `-exec`/`-delete`; `git` only read-only subcommands (`status`, `log`,
  `diff`, `show`, `branch --list`…); `sed` only the `-n …p` form; `ls`, `cat`,
  `pwd`, `wc`, `head`, `tail`, `grep`, `rg`, `echo`. **Every** segment must be
  independently safe for the whole command to be safe.
- Strip a fixed set of side-effect-free wrappers (`time`, `nice`) before the
  argv[0] check — never `bash -c`/`xargs`/`env`.

**Tests (write first):** `find . -delete` unsafe; `git log` safe, `git push`
unsafe; `./sed` never matches (path-qualified, from Task 2); one safe + one
unsafe segment ⇒ whole command unsafe.

**Done when:** safe list is per-segment and flag-aware; corpus green.

---

## Task 5 — Heuristic layer: read-only vs mutating + workspace path scoping + egress

**Files:** `internal/permissions/heuristics.go` + test.

- Classify each segment read-only vs mutating from the safe-list verdict.
- **Path scoping (symlink-aware):** for mutating file operations, resolve
  every path argument with `filepath.EvalSymlinks` (per the
  *dirent-isdir-skips-symlinks* learning — never trust a lexical prefix), then
  require it to remain within the workspace root; an escape ⇒ ask/deny.
- **Network egress** (`curl`, `wget`, `nc`, `ssh`, `scp`, host-qualified `git`
  remotes) is a hard risk boundary ⇒ ask (never silent allow).

**Tests (write first):** a symlink pointing outside the workspace is caught;
in-workspace relative path passes; `curl https://x` ⇒ ask.

**Done when:** symlink-escape and egress rows green.

---

## Task 6 — LLM classifier layer (probabilistic middle; bounded, hardened)

**Files:** new `internal/permissions/classifier.go` + test with a stub model.

- `Classifier` wraps the existing `*model.Adapter` (constructor injects it —
  **do not build a new HTTP client**, *bound-every-model-call*). Resolve
  `cfg.Auto.ClassifierModel` via `registry.Resolve`; unset ⇒ session default
  model + a one-time startup warning.
- One **block-biased structured verdict** call (`allow|deny|ask` + one-line
  reason). Single-stage (two-stage CoT is a documented future upgrade).
- **Input hygiene (non-negotiable):** the classifier prompt includes the
  user's messages + the pending tool call **only** — never tool results,
  never actor reasoning. The verdict is enforced by the gate and never
  surfaced to the actor model as advisory text.
- **Bounded:** rely on the Adapter's per-attempt + response-header timeouts,
  one retry, labeled trace entry. **Timeout/error ⇒ `ask`** (fail closed to
  the fallback surface).
- **Verdict cache:** identical `(tool, normalized-command)` verdicts cached
  per session (reuse/extend `cache.go`).

**Tests (write first):** stub adapter returns `deny` ⇒ gate denies; stub
errors/times out ⇒ verdict is `ask`; classifier prompt assembly excludes tool
results (assert on the built request); identical call hits the cache (adapter
invoked once).

**Done when:** classifier verdict enforced, fail-closed, cached, traced;
never sees tool results.

---

## Task 7 — Wire auto mode into `PermissionGate.resolve` (the pipeline)

**Files:** `internal/permissions/gate.go`.

- In `resolve`, add the `ModeAuto` branch, ordered:
  disabled → session cache → **static: split segments (Task 2) → rules
  (Task 3) → safe list (Task 4) → heuristics (Task 5)** → if still unresolved,
  **classifier (Task 6)** → verdict.
- Map the final `Verdict`: `allow` ⇒ `AutoApprove: true`; `ask` ⇒ the existing
  human-approval path (`g.approve`); `deny` ⇒ denial `tools.Result` whose
  error **names the layer that blocked it** (deny-and-continue, so the model
  can retry a safer path).
- `CloneForChild` carries the mode + classifier to child gates.

**Tests (write first):** end-to-end gate tests in auto mode using `stubTool` /
`newTestRegistry`: safe read-only auto-approves; compound with a deny segment
denies with a layer-named error; gray-area routes to the classifier stub.

**Done when:** the full pipeline is exercised through `Execute` in auto mode.

---

## Task 8 — Escalation valve

**Files:** `internal/permissions/gate.go` (+ session counter).

- Track consecutive and total classifier blocks per gate/session. **3
  consecutive or 20 total** classifier blocks pauses auto mode: fall back to
  prompting when interactive; abort with a summary when not (thresholds from
  the spec/Claude Code).

**Tests (write first):** 3 consecutive blocks flips to prompting; a non-block
verdict resets the consecutive counter.

**Done when:** valve thresholds proven.

---

## Task 9 — Fallback surface + remove `AlwaysApprove` + `--approve-all` (subsumes 0016)

**Files:** `cmd/fuse/main.go`, `cmd/fuse/shell.go`,
`internal/permissions/gate.go`.

- Remove `permissions.AlwaysApprove` from the three `main.go` sites and the
  `shell.go` child site. Every entry point enforces the configured mode.
- **Interactive TTY one-shot:** pending approvals surface as a terminal y/N
  prompt with the existing preview line, honoring `session_allow`; child /
  subagent approvals route to the same parent channel (reuse
  `prefixedApprove`).
- **Non-TTY (piped stdin / CI):** deny-by-default with a clear structured
  error.
- **`--approve-all` flag** on `fuse run`: explicit, visible opt-in that
  restores approve-all for scripted use (equivalent to `mode: off` for that
  run) — the deliberate documented footgun.
- Detect TTY via the existing terminal check (or `golang.org/x/term`'s
  `IsTerminal` on the stdin fd).

**Tests (write first):** non-TTY one-shot without `--approve-all` denies;
`--approve-all` restores auto-approve; TTY path invokes the prompt fn.
`AlwaysApprove` no longer referenced at any construction site (grep-clean).

**Done when:** one configured policy means the same thing at every entry point;
0016's one-shot bypass is gone.

---

## Task 10 — Trust boundary: `.fuse.local.yml` cannot loosen policy (D9)

**Files:** `internal/config/loader.go`, `internal/config/schema.go`.

- Thread a **trust flag** through the merge: `mergeFile(&c, path, trusted
  bool)` (or a small `mergeSource` enum). `~/.fuse/config.yml` = trusted;
  `.fuse.local.yml` = untrusted (repo-plantable).
- From an **untrusted** source, **ignore permission-loosening keys with a
  startup warning**: `permissions.mode`, `permissions.auto_approve`,
  `permissions.session_allow`, and the whole `permissions.auto.*` block.
  **Tightening keys stay honored** from either file:
  `permissions.always_prompt`, `permissions.disabled`.
- Clean field-level merge policy — no legacy flag, no dead path (per human
  direction). Emit one aggregated warning line listing the ignored keys.

**Tests (write first):** a `.fuse.local.yml` setting `mode: off` /
`auto_approve` / `auto.classifier_model` is ignored (config keeps the trusted
value) and warns; the same keys from `~/.fuse/config.yml` are honored;
`always_prompt`/`disabled` from `.fuse.local.yml` ARE honored.

**Done when:** loosening keys inert from the repo file, tightening keys live,
warning emitted.

---

## Task 11 — Docs + config example + full-suite gate

- Update any user-facing config docs / example `~/.fuse/config.yml` to show
  the `permissions.auto` block using the real alias `deepseek-flash` (never
  `haiku`), the per-segment `auto_approve` semantics, and the trust-boundary
  note.
- Run the full suite (`go test ./...`) and `go vet ./...`; the bypass corpus
  is the acceptance heart — every documented pitfall (compound-command prefix
  bypass, substitution/comments/newlines, env-prefix, basename collapse,
  arbitrary-arg wrappers, denylist wrapping, symlink escape, egress) has a
  green red-team row.

**Done when:** `go test ./...` green, `go vet` clean, corpus complete.

---

## Out of scope (do not build here)

OS-level sandboxing (Seatbelt/Landlock/bubblewrap); the MCP server HITL relay
and `fuse mcp-server`'s no-socket `AlwaysApprove` fallback (follow-up); a
two-stage classifier; Ed25519 signed policy envelopes; a hook system.
