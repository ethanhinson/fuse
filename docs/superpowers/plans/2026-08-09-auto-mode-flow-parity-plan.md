<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0040 — Auto-mode flow parity — in-workspace edits auto-approve](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0040-auto-mode-flow-parity.md)**
<!-- docket:backlink:end -->

# Implementation Plan — Auto-mode flow parity (change 0040)

**Change:** 0040 · `auto-mode-flow-parity` · type `fix` · priority `high`
**Spec:** `docs/superpowers/specs/2026-08-09-auto-mode-flow-parity.md` (on `docket`)
**Branch:** `feat/auto-mode-flow-parity` (cut from `origin/main`)

> Plan authored by docket-implement-next's **auto-fallback** (the resolved
> `superpowers:writing-plans` skill was not invocable in this session — see the
> run report / PR body). TDD is preserved: every task writes a focused failing
> test first, then the minimal code to green it.

## Goal

Bring fuse's `auto` permission mode to Claude-Code-style flow parity: in-workspace
`write_file`/`edit_file` auto-approve, read-only/orchestration tools are safe-listed,
`web_fetch` stays gated behind a static host floor + a domain-aware classifier, the
classifier finally sees the user's messages, and only true classifier denies feed the
escalation valve — **without weakening any security layer** (ADR-0005 and ADR-0006 intact).

## Grounding (verified against `main` at plan time)

- Gate pipeline: `internal/permissions/gate.go`.
  - `resolveAuto` non-bash branch: `if name != "bash" { if onSafeList(name) { allow } ; return g.classifyOrAsk(ctx, name, args) }` (currently ~L405-410).
  - `classifyOrAsk` (~L457-484) calls `g.classifier.Classify(ctx, nil, name, command)` — the `nil` is the D3 gap; the block-comment at ~L467 says user history "is not plumbed into the gate".
  - `workspaceRoot` field (~L72), option `WithWorkspaceRoot`; wired at `cmd/fuse` `run.go:425`.
  - Valve: `escalationValve.recordBlock` fires ONLY on a classifier deny (L478); `recordNonBlock` on allow/ask. `valveConsecutiveLimit=3`, `valveTotalLimit=20`.
- Safe-list: `internal/permissions/policy.go` `safeList` map + `onSafeList` (~L71-98). Current entries: `read_file`, `list_directory`, `grep`, `spawn_agent`, `ask_user`, plus `codeindex_*`/`blackboard_*` prefixes. None of the 7 target tools present.
- Heuristic: `internal/permissions/heuristics.go` `withinWorkspace(arg, workspaceRoot)` (~L147) + `resolveExisting` (~L164) — symlink-aware, resolves deepest existing ancestor for not-yet-created files. **Reused verbatim by D1.**
- Classifier: `internal/permissions/classifier.go` `Classify(ctx, userMessages []model.Message, toolName, command string)` — `userMessages` is ALREADY a parameter; `buildMessages` (~L194) already drops non-`user` roles. D3 is pure plumbing + a `web_fetch` prompt nudge.
- Config: `internal/config/schema.go` `AutoConfig{ ClassifierModel, Deny, Ask }` (~L45). Loader `internal/config/loader.go` `mergePermissions` treats the WHOLE `auto.*` block as loosening (drops it from untrusted `.fuse.local.yml`, L119-126). `mergeFile(".fuse.local.yml", trusted=false)`.
- Agent seam: `internal/agent/loop.go` `Run` holds `messages`; tool dispatch at L557-580 calls `a.executeToolBounded(ctx, call)`; `executeToolBounded` (~L605) → `a.tools.Execute`. `ToolExecutor` interface at `internal/agent/agent.go:17-19` (do NOT widen it — carry user turns on `ctx`).
- Tool names (registered): `write_file`, `edit_file`, `segment_read`, `web_search`, `web_fetch`, `skill`, `pipeline_run`. `write_file`/`edit_file` expose a `path` arg.

## Cross-cutting rules (apply to every task)

- **TDD.** Failing test first, minimal green, refactor. No network in tests — embedded pinned snapshots are the fixtures.
- **Fail-closed everywhere.** Any parse/resolution failure ⇒ `VerdictAsk` (toward the human), never `VerdictAllow`. (Mirrors existing pipeline posture.)
- **Learning `patch-every-cloned-child-builder`:** for any wiring that lives in cloned builders, grep the site list at fix time. Here the gate is cloned via `CloneForChild` (single site) and the classifier via `cloneForChild` — verify the new prompt-context path rides the clone. Also grep `WithWorkspaceRoot`/`NewClassifier` call sites in `cmd/fuse` before touching wiring.
- **Learning `fail-closed-guard-calibrate-benign-set`:** the `web_fetch` host floor must use *positive* allow-tests (literal loopback/RFC-1918 CIDRs, exact blocklist membership), never substring/prefix spoofables; verify a routine well-known-host fetch still flows.
- **Learning `gate-as-tool-executor`:** keep new policy inside the gate/permissions package; do not add branches to the agent loop beyond the single D3 ctx attach.
- **ADR-0005** (per-segment bash allow) untouched — D1 adds allow paths for the *edit tools* only. **ADR-0006** — `fetch_deny`/`fetch_ask` are classified *tightening* keys (fail-safe per ADR-0006's "each new key classified or defaults to loosening"): they must merge from `.fuse.local.yml`, the loosening `auto.*` fields must not.

---

## Task 1 — D1: path-scope `write_file` / `edit_file` (the core fix)

**Files:** `internal/permissions/gate.go`, new helper (same file or `heuristics.go`), `internal/permissions/gate_test.go` (+ `heuristics_test.go` if the helper lands there).

1. **Test first** (`gate_test.go`, table-driven on `resolveAuto` in auto mode, classifier nil):
   - (a) `write_file` with in-workspace `path` ⇒ `VerdictAllow`.
   - (b) `edit_file` with in-workspace `path` ⇒ `VerdictAllow`.
   - (c) `path` with `../` escaping root ⇒ `VerdictAsk`.
   - (d) not-yet-created in-workspace file ⇒ `VerdictAllow` (exercises `resolveExisting`).
   - (e) in-workspace symlink whose target escapes ⇒ `VerdictAsk`.
   - (f) missing/garbled `path` / unparseable args ⇒ `VerdictAsk`.
   - Set the gate's `workspaceRoot` to a `t.TempDir()` canonicalized via `filepath.EvalSymlinks`.
2. **Implement:** in `resolveAuto`'s non-bash branch, BEFORE `classifyOrAsk`, add an edit-tool branch:
   ```
   if isEditTool(name) {           // write_file || edit_file
       p, ok := editPath(args)     // json-decode {"path": ...}; ok=false on missing/garbled
       if !ok { return VerdictAsk, "" }
       if withinWorkspace(p, g.workspaceRoot) { return VerdictAllow, "" }
       return VerdictAsk, ""
   }
   ```
   - `editPath` decodes the shared `{"path": string}` shape (both tools expose it). Empty/absent path ⇒ `ok=false`.
   - Reuse `withinWorkspace` verbatim — no new containment logic, no new bypass class.
3. **Green + refactor.** Keep `isEditTool`/`editPath` unexported in the permissions package.

**Acceptance:** the 6 cases above pass; a `write_file` in-workspace no longer reaches the classifier (verified in Task 6's valve test).

## Task 2 — D2: extend the safe-list (NOT `web_fetch`)

**Files:** `internal/permissions/policy.go`, `internal/permissions/safelist_test.go`.

1. **Test first** (`safelist_test.go`): `onSafeList` true for `segment_read`, `web_search`, `skill`, `pipeline_run`; and **false** for `web_fetch`, `write_file`, `edit_file` (those route to floor/heuristic/classifier). Add a smart+auto mode assertion via `resolveAuto`/`resolve` for at least `web_search` (allow) and `web_fetch` (not auto-allowed).
2. **Implement:** add the four names to `safeList` with a per-entry rationale comment matching the existing `spawn_agent`/blackboard style:
   - `segment_read` — read-only over an existing transcript segment; no side effects.
   - `web_search` — controlled query to a config-fixed engine (Brave/Tavily/custom); endpoint not model-chosen, so no arbitrary-egress surface.
   - `skill` / `pipeline_run` — orchestration; spawned children inherit a cloned gate and are independently re-gated (same rationale as `spawn_agent`).
   - Do **not** add `web_fetch`.
3. **Green.** Note in-comment: a user can re-gate any via `permissions.always_prompt`.

**Acceptance:** four tools auto-approve in smart & auto; `web_fetch` does not.

## Task 3 — D2a part A: `reputation` package (embedded blocklist + popularity, MIT-only)

**Files (new):** `internal/permissions/reputation/reputation.go`, `internal/permissions/reputation/reputation_test.go`, `internal/permissions/reputation/data/` (pinned snapshots), `internal/permissions/reputation/generate.go` (`go:generate`), `internal/permissions/reputation/LICENSES/` (MIT + CC BY 3.0 texts + attribution).

> **Reconcile decision (licensing): MIT-only.** Bundle StevenBlack/hosts (MIT) + Peter Lowe's list (explicit redistribution permission) for the blocklist; **drop hagezi TIF (GPL-3.0)** so fuse ships no GPL data. Keep Majestic Million (CC BY 3.0) for the known-good popularity nudge with its credit line. TIF may be added later behind a build tag if wanted — out of scope here.

1. **Snapshots.** Commit small, pinned, dated snapshots under `data/` (a bounded subset is acceptable for the bundle — the SSRF guard + config `fetch_deny` + classifier are independent layers, so exhaustive coverage is not required). Embed via `//go:embed`. Store the source URLs + pin date in `generate.go`'s doc comment; the generator re-pulls and re-pins (run manually / via `make`, never at build or test time).
2. **API (test first):**
   - `func Blocked(host string) bool` — exact host membership against the parsed blocklist (normalize: lowercase, strip trailing dot; parse `0.0.0.0 domain` hosts-file lines and one-domain-per-line). Positive membership test only — no substring matching.
   - `func KnownGood(host string) bool` — membership in the embedded popularity list (Majestic Million subset) — used only as an allow-*nudge*, never a bypass.
   - Loaded once via `sync.Once` into maps.
3. **Tests** (`reputation_test.go`, no network): embedded snapshots parse and are non-empty; a couple of known-bad fixtures resolve `Blocked==true`; a known-good host resolves `KnownGood==true`; a random unknown host is neither. Assert LICENSES files exist and are non-empty.

**Acceptance:** package builds offline; embeds parse; membership tests pass; no GPL data bundled.

## Task 4 — D2a part B: `fetchhost.go` host floor

**Files (new):** `internal/permissions/fetchhost.go`, `internal/permissions/fetchhost_test.go`.

1. **API (test first):** `func classifyFetchHost(rawURL string, autoCfg config.AutoConfig) (Verdict, decidedBy string)` returning:
   - malformed/opaque URL (no parseable host) ⇒ `VerdictAsk`, `"malformed-url"`.
   - **SSRF guard (hardcoded, positive tests):** IP-literal host in loopback / RFC-1918 (`10/8`, `172.16/12`, `192.168/16`), link-local `169.254/16`, IPv6 `::1` / `fc00::/7`, or `localhost` ⇒ `VerdictDeny`, `"ssrf"`. Use `net.ParseIP` + `net/netip` prefix checks — never string prefixes.
   - `fetch_deny` host glob match ⇒ `VerdictDeny`, `"config-deny"`; `fetch_ask` glob match ⇒ `VerdictAsk`, `"config-ask"` (glob via `path.Match` on the host; deny beats ask).
   - `reputation.Blocked(host)` ⇒ `VerdictDeny`, `"blocklist"`.
   - otherwise ⇒ a sentinel meaning "fall through to classifier" (e.g. `VerdictAsk` with `decidedBy=="fallthrough"`, or a dedicated `verdictFallthrough`). The known-good seed + `reputation.KnownGood` produce the allow-*nudge* string consumed in Task 5, not a verdict here.
   - Hardcoded known-good host seed (from spec D2a): the code/dev + official-docs + reference host set, incl. `*.github.io`, `*.readthedocs.io`, `*.wikipedia.org`, `*.stackexchange.com`, and mild `*.gov`/`*.edu`/`*.dev` TLD signals — matched via suffix rules that are themselves not spoofable (exact host or a real dot-boundary suffix).
2. **Tests** (`fetchhost_test.go`) — assert the DECIDING LAYER (`decidedBy`), not just the verdict, per spec: (a) well-known host ⇒ fallthrough + allow-nudge, (b) each SSRF class ⇒ deny/`ssrf`, (c) blocklist fixture ⇒ deny/`blocklist`, (d) `fetch_deny` glob ⇒ deny, `fetch_ask` glob ⇒ ask, (e) malformed URL ⇒ ask/`malformed-url`.

**Acceptance:** each layer decides correctly and is individually asserted; guards use positive/CIDR tests.

## Task 5 — D2a part C: route `web_fetch` through the floor + domain-aware classifier prompt

**Files:** `internal/permissions/gate.go`, `internal/permissions/classifier.go`, `internal/config/schema.go`, `internal/config/loader.go`, `gate_test.go`, `classifier_test.go`, `loader_test.go`.

1. **Schema (test first, `loader_test.go`):** add `FetchDeny []string \`yaml:"fetch_deny"\`` and `FetchAsk []string \`yaml:"fetch_ask"\`` to `AutoConfig`.
2. **Loader — tighten-only merge (test first):** split `mergePermissions`' `auto.*` handling so:
   - `classifier_model` / `deny` / `ask` stay **loosening** (trusted-only; ignored+reported from `.fuse.local.yml`).
   - `fetch_deny` / `fetch_ask` are **tightening** — merged from either source (append/replace like `always_prompt`/`disabled`).
   - Test: an untrusted `.fuse.local.yml` with `auto.fetch_deny` set IS honored; the same file's `auto.classifier_model`/`auto.deny` are dropped and reported as ignored. A trusted config honors all.
   - Keep the single aggregated ignored-keys warning line intact.
3. **Gate routing (test first, `gate_test.go`):** in `resolveAuto`'s non-bash branch, when `name == "web_fetch"`, run `classifyFetchHost(url, g.cfg.Auto)` first:
   - `VerdictDeny` ⇒ return deny with a layer-named reason (`"denied by auto-mode web_fetch host floor (<decidedBy>): <host>"`).
   - `VerdictAsk` (config-ask / malformed) ⇒ return `VerdictAsk`.
   - fallthrough ⇒ `classifyOrAsk(ctx, name, args)` (the URL is already the classifier `command`), passing the allow-nudge so the prompt can weigh reputation.
   - `web_fetch` is NOT safe-listed, so it never short-circuits at the `onSafeList` check.
4. **Classifier prompt nudge (test first, `classifier_test.go`):** extend `pendingCallPrompt` (or a `web_fetch`-specific variant) to name the target host and instruct the block-biased classifier to weigh domain reputation — an allow-nudge for a known-good/ranked host, ask/deny bias for unknown/low-rep. The nudge is prompt text only, NEVER an absolute bypass (a compromised `*.github.io` must stay deniable). Thread the nudge from the gate to `Classify` via a small parameter or a `web_fetch`-aware prompt path; keep input hygiene (only system + user turns + pending-call turn).

**Acceptance:** SSRF/blocklisted/`fetch_deny` hosts stop; well-known hosts flow through to the classifier with a nudge; `fetch_deny`/`fetch_ask` honored from `.fuse.local.yml`; loosening `auto.*` still dropped from it.

## Task 6 — D3: give the classifier its user-message context

**Files:** `internal/agent/loop.go`, `internal/permissions/gate.go`, `internal/permissions/classifier_test.go` (or a gate seam test), NO change to `agent.go`'s `ToolExecutor`.

1. **ctx key (test first):** add an unexported `ctxKey` type + `WithUserMessages(ctx, []model.Message) context.Context` and `userMessagesFrom(ctx) []model.Message` (nil-safe: absent ⇒ nil) in the permissions package (or a tiny shared spot the gate reads).
2. **Attach in the loop:** in `Run`'s per-turn tool-dispatch block (before both the allSpawn and sequential branches at L557-580), derive `tctx := permissions.WithUserMessages(ctx, userTurns(messages))` and pass `tctx` into `executeToolBounded`. `userTurns` filters `messages` to `Role=="user"` (the classifier's `buildMessages` also filters, so this is defense-in-depth; keeping it here avoids passing non-user content across the seam at all).
3. **Read in the gate:** `classifyOrAsk` replaces `Classify(ctx, nil, …)` with `Classify(ctx, userMessagesFrom(ctx), name, command)`. Update the stale L467 comment.
4. **Tests:** a gate/classifier seam test with a stub completer asserts the forwarded `[]model.Message` CONTAINS the user turns and contains **no** `tool`/`assistant` messages (hygiene). Nil-safe: absent ctx value ⇒ today's behavior (nil userMessages).
5. **Clone check:** verify a child gate (`CloneForChild`) still reads ctx-carried messages (it will — ctx flows through `Execute`, independent of the clone). Grep `executeToolBounded` call sites to confirm the single dispatch path.

**Acceptance:** classifier receives user turns; hygiene preserved; nil-safe; interface unchanged.

## Task 7 — D4: accept-edits posture + valve confirmation

**Files:** `internal/permissions/valve_test.go` (+ `gate_test.go` if needed). Likely **no production change** — D1 already keeps in-workspace edits out of `classifyOrAsk`, and `recordBlock` already fires only on a classifier deny.

1. **Test:** a run of N in-workspace `write_file`/`edit_file` calls does NOT advance the valve (`consecutive`/`total` stay 0), because they resolve `VerdictAllow` before `classifyOrAsk`. Only classifier denies advance it.
2. **Confirm** `valveConsecutiveLimit=3` stays; leave `valveTotalLimit=20` unchanged (D4 retune deferred to post-landing measurement per spec — record as an open follow-up, not a code change).

**Acceptance:** benign edits never trip the valve; only true classifier denies count.

## Task 8 — Regression + wiring sweep

1. Run the full `internal/permissions/...`, `internal/config/...`, and `internal/agent/...` suites. The existing 0017 bypass corpus must stay green.
2. Grep `cmd/fuse` for gate/classifier construction sites (`New(`, `WithWorkspaceRoot`, `NewClassifier`, `CloneForChild`, child builders per `patch-every-cloned-child-builder`) and confirm no site needs a change beyond what Tasks 1-7 touched (D3's ctx attach is the only agent-loop seam).
3. `go build ./...` and `go vet ./...` clean.

**Acceptance:** whole repo builds, vets, and tests green.

## Test matrix (from spec "Testing notes")

- Edit-tool scoping (Task 1): in-workspace allow · `../` ask · symlink-out ask · not-yet-created allow · garbled path ask.
- Safe-list (Task 2): `segment_read`/`web_search`/`skill`/`pipeline_run` allow (smart+auto); `web_fetch` NOT auto-allowed.
- `web_fetch` host floor (Task 4/5): well-known ⇒ classifier+nudge · SSRF classes ⇒ deny · blocklist ⇒ deny · `fetch_deny`/`fetch_ask` globs · malformed ⇒ ask; assert the deciding layer.
- Embedded lists (Task 3): snapshots parse/non-empty; known-bad blocked; known-good ranked; no network.
- Loader (Task 5): `fetch_deny`/`fetch_ask` parse and merge from `.fuse.local.yml` (tightening); `auto.*` loosening still dropped from it.
- Classifier context (Task 6): forwarded messages contain user turns, no tool-result/assistant messages.
- Valve (Task 7): in-workspace edits do not advance; only classifier denies do.
- Regression: 0017 bypass corpus stays green.

## Sequencing

Tasks 1-2 (D1+D2) → 3-5 (D2a) → 6 (D3) → 7 (D4) → 8 (sweep). One PR.

## Out of scope (spec)

OS sandboxing; two-stage classifier CoT; any bash segment / egress / dangerous-command change; any live domain-reputation API (no `fetch_reputation` key); config schema beyond `AutoConfig.fetch_deny`/`fetch_ask`; hagezi TIF GPL-3.0 data (dropped by reconcile); `valveTotalLimit` retune (deferred).
