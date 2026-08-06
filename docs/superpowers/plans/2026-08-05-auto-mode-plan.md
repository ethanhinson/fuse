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

## D10 — In-session mode switching (subsumes 0032; added 2026-08-05 post-first-use)

First real use showed config-file activation is the wrong primary surface: the
permission mode must be a **session** surface (Claude-Code-style), with the
config file only the startup default. Change 0032 is killed as subsumed. These
tasks are appended after the D1–D9 build (Tasks 1–11, already merged-ready on
this branch through commit `5046a8d`, review C1 closed) and follow the same
TDD-per-task, one-commit-each contract. Spec section D10 is authoritative.

**D10 context anchors (verified against this branch at append time):**

- `PermissionGate` (`internal/permissions/gate.go`) reads `g.mode` unguarded in
  `resolve`/`resolveAuto`/`CloneForChild`; there is no `SetMode` and no mutex on
  `mode`. `PermissionMode` (`policy.go`) has no `String()`.
- The shell builds a **fresh gate per turn/per child** inside
  `buildAgentCore` → `permissions.New(cfg.Permissions, …, autoModeOptions(…)…)`
  (`cmd/fuse/run.go`), reading `cfg.Permissions.Mode` each time. There is **no**
  long-lived root gate the TUI holds. `autoModeOptions` returns `nil` unless the
  *configured* mode is auto — so a mid-session switch into auto would today get
  no classifier (D10 item 5 fixes this).
- `ShellModel` (`internal/tui/shell_model.go`) holds no gate/mode reference; its
  `View()` status line shows `m.alias`; `handleKey` switches on `tea.KeyTab`;
  `handleSlash` has the builtin `/exit /verbose /model /agents /approvals`
  switch; `BuiltinProvider` (`builtin_provider.go`) lists them for the
  completer. bubbletea v1.3.10 maps CSI `Z` (`\x1b[Z`) → `tea.KeyShiftTab`
  (`msg.String() == "shift+tab"`) — **verified in the vendored `key.go`**.

**Learnings folded in (D10-relevant):**

- **mutex-test-double-concurrent-provider** — `SetMode`/mode reads cross the
  TUI goroutine and per-turn gate construction: guard **both** the getter and
  the setter of the shared session-mode source with one mutex; a lock on one
  side only is still a race. Tests must exercise concurrent get/set.
- **completer-entry-bypass-dispatch** — `/mode` is a `KindBuiltin` entry:
  dispatch it through the entry object / `handleSlash` builtin switch, never by
  re-parsing the completer expansion as free text.
- **sanitize-untrusted-bytes-fixed-width-tui** — the new mode indicator is a
  fixed-width status-line cell; keep its text a fixed, known-safe token set
  (`smart`/`auto`/`prompt-all`/`off` + a static degraded marker), never
  interpolating model/tool bytes.

---

## Task 12 — Session mode source + thread-safe `SetMode` on the gate

**Files:** `internal/permissions/policy.go`, `internal/permissions/gate.go`
(+ new session-mode holder; a small `internal/permissions/sessionmode.go` or an
addition to an existing file), tests alongside.

- Add `func (m PermissionMode) String() string` returning the canonical token
  (`smart`/`auto`/`prompt-all`/`off`) — the single source of the indicator/
  `/mode` names, so no string literals are duplicated across packages.
- Add a **thread-safe session-mode holder** owned by the session (not per-gate):
  a small `*SessionMode` value with `Get() PermissionMode` and
  `Set(PermissionMode)` under one `sync.Mutex` (or `atomic`). This is the single
  source both the TUI (indicator, Shift+Tab, `/mode`) and per-turn gate
  construction read, so a switch is picked up by the *next* built gate.
- Add thread-safe `func (g *PermissionGate) SetMode(PermissionMode)` and make
  every `g.mode` read go through a guarded accessor (`g.currentMode()`), so a
  live root gate switches immediately and concurrently-running `resolve` calls
  never race the write. **Semantics per D10:** the root gate switches
  immediately; `CloneForChild` snapshots the parent's mode **at spawn** (already
  captured — the child reads its own copied `mode`, so running children keep
  their mode); **session-cache grants survive a switch** (do not touch
  `g.cache` in `SetMode`); the **escalation valve resets when leaving auto**
  (`SetMode` to a non-auto mode zeroes the valve counters via a valve reset
  method under the valve mutex; entering auto leaves counters as-is).

**Tests (write first):** `ModeAuto.String() == "auto"` (+ the other three);
`SetMode` changes the mode a subsequent `resolve` observes; a `CloneForChild`
taken **before** a parent `SetMode` still resolves under the old mode (running
child keeps its mode); leaving auto resets the valve (`counts()` → 0,0) while a
session-cache grant made before the switch still auto-approves after; a
concurrent `SetMode`/`resolve` goroutine pair runs clean under `-race`.

**Done when:** `go test -race ./internal/permissions/...` green; `String()`,
`SetMode`, and the session-mode holder exist with the D10 semantics.

---

## Task 13 — Per-turn gate construction reads the session mode; classifier built regardless of configured mode

**Files:** `cmd/fuse/run.go` (`autoModeOptions`, `buildAgentCore` /
`buildChildAgent` gate construction), `cmd/fuse/shell.go` (thread the session
mode source in), tests where a seam exists.

- Introduce the `*permissions.SessionMode` at shell startup (seeded from
  `cfg.Permissions.Mode`) and thread it into the per-turn/child gate builders so
  each freshly built gate is constructed at the **session** mode, not the raw
  `cfg.Permissions.Mode`. The gate's own `SetMode` still exists for the live
  root gate; new gates simply start at the current session mode. Keep one-shot
  (`cmd/fuse/main.go`) and `mcp_server.go` paths behaviour-identical — they pass
  no session mode source and default to `cfg.Permissions.Mode` exactly as today.
- **Classifier at startup regardless of configured mode (D10 item 5):** change
  `autoModeOptions` so the classifier + workspace root + interactive options are
  wired whenever a classifier is *constructible* (a `classifier_model` alias
  resolves, or the gateway default is available) — **not** gated on
  `mode == auto`. Switching into auto mid-session then gets the full pipeline.
  **Auto with no constructible classifier stays allowed and fail-closed** (nil
  classifier ⇒ gray area asks — the existing `classifyOrAsk` nil path); this is
  the degraded posture the indicator will mark. Do not construct a classifier
  when the gateway is entirely unconfigured — keep it nil, not an erroring stub.

**Tests (write first):** a gate built from a session source at `smart` resolves
under smart, and after the source flips to `auto` a newly built gate resolves
under auto (through whatever seam `buildAgentCore` exposes; add a minimal
constructor seam if none exists — no behavior change to existing callers);
`autoModeOptions` returns a classifier option when a classifier is constructible
even though the configured mode is `smart`; returns none when no classifier is
constructible.

**Done when:** switching the session mode into auto mid-session yields a gate
with a wired classifier; `smart`-configured startup still constructs the
classifier so a later switch is fully powered; `go vet ./...` clean; existing
one-shot/mcp behavior unchanged.

---

## Task 14 — Shell TUI mode indicator in the status/input line

**Files:** `internal/tui/shell_model.go` (`ShellModel` struct, `NewShellModel`,
`View`), `cmd/fuse/shell.go` (pass the session mode source in), test.

- Give `ShellModel` a read handle on the session mode (the `*permissions.SessionMode`
  from Task 13, or a small getter func to avoid a TUI→permissions import cycle —
  prefer a `func() permissions.PermissionMode` closure or a tiny local interface
  if the import direction is a problem). `NewShellModel` gains the parameter;
  update the one production call site and `shell_test.go`.
- In `View()`, render the active mode in the default (non-running, non-approval)
  status line alongside `m.alias` — e.g. `mode: auto` — using **only** the fixed
  `PermissionMode.String()` token. When the mode is `auto` **and no classifier
  is configured/constructible**, append a static degraded marker
  (e.g. `mode: auto (degraded — no classifier)` or a `⚠` glyph) so the human
  sees the deterministic-only posture. The degraded fact is a bool the shell
  learns at startup (was a classifier constructible?) — thread it as a plain
  flag, do not re-derive it in the view.

**Tests (write first):** `View()` (or a small extracted `statusLine` helper)
contains the mode token for each of the four modes; the degraded marker appears
for auto-without-classifier and is absent for auto-with-classifier and for the
other three modes. Keep assertions on the extracted helper to avoid full-screen
snapshot brittleness.

**Done when:** the indicator shows the live mode and the degraded marker per
D10; TUI tests green.

---

## Task 15 — Shift+Tab cycles smart ↔ auto

**Files:** `internal/tui/shell_model.go` (`handleKey`), test.

- In `handleKey`, add a `tea.KeyShiftTab` case (verified name `"shift+tab"`,
  CSI `Z`) that toggles the session mode between `smart` and `auto` **only** —
  the two everyday postures — via the session mode source's `Set`, and (when a
  live root gate is held) the gate's `SetMode`. From `prompt-all` or `off`,
  define Shift+Tab as switching to `smart` first (a documented, predictable
  landing), then Shift+Tab thereafter toggles smart↔auto. Append a transcript
  line noting the new mode. Ensure it does not collide with the existing
  `tea.KeyTab` (agents view) or the completer navigation — Shift+Tab is a
  distinct `tea.KeyType`, so guard it before/independently of the `KeyTab` case
  and only when the completer is inactive and no approval is pending.

**Tests (write first):** feeding `tea.KeyMsg{Type: tea.KeyShiftTab}` toggles the
session mode smart→auto→smart; from `off`/`prompt-all` the first Shift+Tab lands
on `smart`; Shift+Tab is ignored (or deferred) while an approval is pending or
the completer is active, matching the existing key-guard ordering.

**Done when:** Shift+Tab cycles the two everyday modes and the indicator (Task
14) reflects it; tests green.

---

## Task 16 — `/mode` slash command (bare prints active + options; `/mode <name>` sets any of the four)

**Files:** `internal/tui/shell_model.go` (`handleSlash` builtin switch),
`internal/tui/builtin_provider.go` (register `/mode` for the completer), test.

- Add a `/mode` builtin to `BuiltinProvider` (`Syntax: "NAME"`, a clear
  description) so it appears in the completer, dispatched through the existing
  `KindBuiltin` → `handleSlash` path (per **completer-entry-bypass-dispatch**,
  do not re-parse expansion as free text).
- In `handleSlash`, add a `case "/mode"`: **bare `/mode`** prints the active mode
  and the four options (`smart`, `auto`, `prompt-all`, `off`) plus a one-line
  hint; **`/mode <name>`** validates `<name>` against the four (reuse
  `ParseMode`, but reject unknown tokens explicitly rather than silently
  defaulting to smart — parse then compare `String()` round-trips, or add a
  `ParseModeStrict`/validity check) and sets the session mode (+ live gate
  `SetMode` when held), echoing the switch. Unknown name ⇒ a usage line, no
  change.

**Tests (write first):** bare `/mode` appends a line naming the current mode and
lists all four options; `/mode auto`, `/mode prompt-all`, `/mode off`,
`/mode smart` each set the session mode and echo it; `/mode bogus` leaves the
mode unchanged and prints a usage/error line; `/mode` is present in
`BuiltinProvider.Commands()`.

**Done when:** all four modes reachable via `/mode`, bare `/mode` is
discoverable, invalid input is safe; tests green.

---

## Task 17 — Docs: README permission-modes section leads with the session surface

**Files:** `README.md` (permission-modes section), plus the config example if it
lives there.

- Rewrite the permission-modes section to **lead with the session surface**: the
  mode indicator, Shift+Tab (smart↔auto), and `/mode <name>` for all four modes;
  state that the config `permissions.mode` is only the **startup default** and
  the session override always wins and is never written back. Then document the
  four modes and note auto works with **zero config** (deterministic layers +
  fail-closed asks when no classifier is configured), with the degraded
  indicator. Keep the D9 trust-boundary note (`.fuse.local.yml` cannot loosen
  policy — the switcher is a human at the keyboard, ADR-0006).
- Run the full suite (`go test ./...`, and `-race` on `internal/permissions/...`
  and `internal/tui/...`) and `go vet ./...` as the D10 acceptance gate.

**Done when:** README leads with the session surface; `go test ./...` green,
`go vet` clean.

---

## Task 18 — D11: per-project permission overrides in the user-level config

**Spec:** D11 (`docs/superpowers/specs/2026-08-05-auto-mode-design.md`), added
2026-08-05. "Auto is acceptable in this project but not that one" expressed from
the **user-owned** `~/.fuse/config.yml` (trusted), so ADR-0006's trust boundary
holds — the repo never grants its own trust.

**Files:** `internal/config/schema.go`, `internal/config/loader.go`,
`internal/config/loader_test.go` (new corpus), `README.md`.

### Design

**Schema (`schema.go`):**

- Add `Projects map[string]ProjectConfig` to `rawConfig` under
  `yaml:"projects"` — the on-disk shape (absolute project path → overrides).
- New `type ProjectConfig struct { Permissions rawPermissionsConfig
  \`yaml:"permissions"\` }`. Reuse `rawPermissionsConfig` (not the resolved
  `PermissionsConfig`) so the same pointer/omitted-key discipline
  (`session_allow *bool`) and the same trusted merge path apply verbatim to a
  project entry. No field is added to the resolved `Config`/`PermissionsConfig`
  — a project override resolves *into* `c.Permissions` at load time; there is no
  separate resolved surface.

**Loader (`loader.go`):**

- Extract the existing inline per-key permission-merge block from `mergeFile`
  into a helper, e.g. `mergePermissions(c *Config, raw rawPermissionsConfig,
  trusted bool) (ignored []string)` — byte-for-byte the same loosening/tightening
  rules already in `mergeFile` (mode/session_allow/auto_approve/auto.* loosening;
  always_prompt/disabled tightening; aggregated `ignored` list). `mergeFile` calls
  it and keeps emitting the one aggregated warning. This is a pure refactor with
  no behavior change — existing loosen/tighten tests must stay green untouched.
- In `Load()`, insert a **new step between the trusted home merge and the
  untrusted `.fuse.local.yml` merge**: `applyProjectOverride(&c, home)`.
  - Read the raw home config once more is wasteful; instead have `mergeFile`
    capture the parsed `raw.Projects` for the trusted home file. Simplest clean
    shape: add a trusted-only out-param path — e.g. `Load()` parses the home
    file's `projects:` via a small dedicated read, OR `mergeFile` gains an
    optional `*map[string]ProjectConfig` sink populated only when `trusted`.
    Prefer the sink: `mergeFile(&c, homePath, true, &projects)`; the local call
    passes `nil` (a `projects:` block in `.fuse.local.yml` is handled below).
  - `applyProjectOverride`: get cwd (`os.Getwd`), canonicalize via
    `filepath.EvalSymlinks` (on error — cwd unresolvable — no-op, no override).
    Canonicalize each project key the same way (a key that fails to resolve is
    skipped, not fatal). Select the **longest key that equals cwd or is a
    path-segment ancestor of cwd**; ties impossible (keys are distinct paths).
    Merge that entry's permissions as **TRUSTED** via `mergePermissions(&c,
    entry.Permissions, true)` (full subtree incl. mode and auto.*). No match ⇒
    no-op.
  - **Path-segment ancestor test** (the `/a/b` vs `/a/bc` guard): an ancestor
    match is `cwd == key || strings.HasPrefix(cwd, key+string(os.PathSeparator))`.
    Never a raw `strings.HasPrefix(cwd, key)` — that matches `/a/bc` under `/a/b`.
    Compare the cleaned (`filepath.Clean`) canonical forms so a trailing slash in
    a key does not skew length ranking; rank by segment/length of the cleaned key.

**`.fuse.local.yml` `projects:` is loosening surface (untrusted):**

- A `projects:` key appearing in `.fuse.local.yml` must be **ignored with the
  same aggregated `warnw` warning** — a repo-planted per-project block could
  grant `mode: auto` to itself, exactly ADR-0006's threat. In the untrusted
  `mergeFile` path, if `raw.Projects` is non-empty, append `"projects"` to the
  `ignored` slice (so it rides the one aggregated warning line) and do **not**
  apply it. (The trusted home path is the only one that ever consults
  `raw.Projects`.)

**Precedence (must end unchanged):** built-ins → user global `permissions:` →
**user per-project `projects.<path>.permissions:` (new, trusted)** →
`.fuse.local.yml` tighten-only → env overrides → session switcher (D10). The
insertion point (after home, before local) is exactly what yields this order:
the project override sits above global and below the tighten-only local file,
and env + the session switcher still run after `Load()` returns, so they always
win — assert this explicitly in a test (env/session unaffected is covered by the
existing env test staying green; add a row proving a project `mode: auto` is
still overridable by a later tighten and by the existing env path).

### Tests (write first — TDD corpus rows)

New `loader_test.go` cases; each writes a `~/.fuse/config.yml` with a `projects:`
map and `t.Chdir`s (or the existing `chdirTemp`) into the relevant dir:

1. **Exact match** — key == canonical cwd ⇒ that entry's `mode` (e.g. `auto`)
   resolves; a global `permissions.mode` is overridden by it.
2. **Ancestor match** — key is a parent dir of cwd ⇒ applied.
3. **Longest-wins** — two keys both ancestors (`/p` and `/p/sub`), cwd under
   `/p/sub` ⇒ the `/p/sub` entry wins.
4. **`/a/b` vs `/a/bc` non-match** — a key `…/b` must NOT match a cwd under
   `…/bc` (the raw-prefix bug); assert the global default survives.
5. **Symlinked cwd** — cwd reached through a symlink whose `EvalSymlinks` target
   equals a project key ⇒ match (build a temp symlink, `os.Symlink`, chdir
   through it). Skip on platforms without symlink support if needed.
6. **No-match no-op** — cwd under no project key ⇒ global/default permissions
   unchanged, no warning.
7. **Local-file `projects:` ignored + warned** — a `projects:` block in
   `.fuse.local.yml` is ignored and the aggregated warning names `projects`
   (assert the one-line aggregation invariant like `TestLocalFileCannotLoosenPolicy`).

Full-subtree assertion: at least one row sets `auto.classifier_model` and
`auto.deny`/`auto.ask` in the project entry and asserts they resolve (trusted
merges the whole `auto.*` subtree, not just `mode`).

### Done when

- The seven corpus rows pass; the full-subtree row passes.
- All pre-existing loader tests stay green (pure-refactor invariant).
- `go build ./... && go vet ./... && go test -race ./...` clean.

---

## Task 19 — D11 docs: README `projects:` example + trusted-projects rationale

**Files:** `README.md` (config / permission-modes section).

- Add a `projects:` example to the user-level config docs: an absolute project
  path → a `permissions:` subtree setting `mode: auto` (and optionally
  `auto.classifier_model`), showing per-project trust.
- State the **trusted-projects rationale**: the map lives only in the user-owned
  `~/.fuse/config.yml` (never a repo file), keyed by absolute project path with
  longest-ancestor cwd match, so "auto here, not there" is grantable without
  weakening ADR-0006 — a `projects:` block in `.fuse.local.yml` is ignored and
  warned, like every other loosening key.
- Keep it consistent with the D10 session-surface framing already in the section
  (session switcher always wins; config is startup default only).

### Done when

- README shows the `projects:` example and the trusted-projects rationale;
  `go test ./...` green, `go vet` clean (re-run the D11 acceptance gate).

---

## Out of scope (do not build here)

OS-level sandboxing (Seatbelt/Landlock/bubblewrap); the MCP server HITL relay
and `fuse mcp-server`'s no-socket `AlwaysApprove` fallback (follow-up); a
two-stage classifier; Ed25519 signed policy envelopes; a hook system. **D10
adds no persistence** — the session mode is never written back to any config
file (spec D10); do not add config-write logic. **D11 adds no new resolved
config surface** — a project override resolves *into* `c.Permissions` at load
time; there is no `Config.Projects` field and nothing is written back.
