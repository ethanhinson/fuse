<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0040 — Auto-mode flow parity — in-workspace edits auto-approve](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0040-auto-mode-flow-parity.md)**
<!-- docket:backlink:end -->

# Auto Mode Flow Parity — In-Workspace Edits Auto-Approve

**Spec for change 0040** · groomed 2026-08-09 (design settled in `reports/2026-08-09-auto-mode-flow-parity.md`; brainstorm skill unavailable — designed inline from the report)

Follow-up to **change 0017** (auto-mode). Preserves **ADR-0005** (per-segment allow evaluation) and **ADR-0006** (`.fuse.local.yml` tighten-only trust boundary) unchanged.

---

## Problem

Fuse's `auto` permission mode stops far more often than Claude Code's accept-edits/auto flow, even though the security model is sound (and in places stronger). The stopping is **structural**, not excess caution:

1. **`write_file` / `edit_file` are not recognized as in-workspace edits.** Both are non-bash tools absent from the safe-list (`internal/permissions/policy.go:71-98`), so every write/edit is handed to the block-biased LLM classifier on every call (`gate.go:405`). Claude Code's defining auto-mode behavior is auto-approving *in-workspace* edits. The pipeline already contains the exact check needed — `classifyHeuristic → withinWorkspace` (`heuristics.go:147`, symlink-aware containment in `workspaceRoot`) — but it only runs for **bash**, never the edit tools. The gate already holds `workspaceRoot` (`gate.go:71`, wired at `run.go:425`); both tools expose a plain `path` arg (`write.go:34`, `edit.go:35`).
2. **The classifier judges blind.** `classifyOrAsk` calls `Classify(ctx, nil, ...)` — user history is hard-wired to `nil` (`gate.go:468`). Spec 0017 D7 says the classifier "sees the user's messages"; that plumbing was never done. With only `tool + args` and a "be block-biased" system prompt (`classifier.go:54-59`), a bare `write_file internal/foo.go` reads as uncertain → ask.
3. **Read-only / orchestration non-bash tools aren't safe-listed.** `segment_read` (read-only), `web_search`, `web_fetch`, `skill`, `pipeline_run` all route to the classifier → ask.
4. **The escalation valve then pauses auto mode entirely.** 3 consecutive / 20 total classifier blocks pause auto (`gate.go:99-102`). Because #1–#3 push routine safe actions into the classifier, benign blocks accrue and trip the valve mid-task.

Net effect on a typical "implement feature X" run: reads flow, but the first edit of every file hits the blind classifier → ask; a handful trips the valve → auto pauses. This is the reported "stops a lot."

The full findings with file:line citations live in `reports/2026-08-09-auto-mode-flow-parity.md` (repo root, integration branch).

---

## Decisions

### D1 — Path-scope the edit tools (the core fix)

In `resolveAuto`'s non-bash branch (`gate.go:405-410`), before falling to the classifier, add an edit-tool branch that extracts `path` and runs the existing `withinWorkspace`:

- `write_file` / `edit_file` with a `path` resolving **inside** the canonicalized `workspaceRoot` ⇒ **allow**.
- A path that escapes the root (`../`, absolute-outside, in-workspace symlink whose target escapes) ⇒ **ask**.
- Missing/garbled `path`, or unparseable args ⇒ **ask** (fail toward the human).

Reuses `withinWorkspace` / `resolveExisting` verbatim — symlink-aware, and already handles not-yet-created files by resolving the deepest existing ancestor. No new bypass class: it admits only paths the same check bash mutations already trust.

### D2 — Extend the safe-list (but NOT `web_fetch`)

Add to `onSafeList` / `safeList` (`policy.go`): `segment_read` (read-only), `web_search`, `skill`, and `pipeline_run`.

- **`web_search` is safe-listed.** It is a controlled query to a *known, configured* search engine (Brave / Tavily / a config-driven `CustomHTTPProvider`) — the endpoint is fixed by config, not chosen by the model, so there is no arbitrary-egress surface to reputation-check. A query string cannot reach a hostile host.
- **`web_fetch` is deliberately NOT safe-listed** — see D2a. It pulls a model-chosen arbitrary URL, so the *domain* is the risk surface (a fetched page can carry malware payloads, drive-by content, prompt-injection, or exfil-beacon URLs). It stays classifier-gated with a domain-reputation signal.
- **`skill` / `pipeline_run`** are orchestration whose spawned children inherit a cloned gate (same mode/rules/classifier), so every action they take is independently gated — matching the existing `spawn_agent` rationale.

Each entry carries a rationale comment like the existing `spawn_agent` / blackboard entries. A user who wants any of them gated again can demote via `permissions.always_prompt`.

### D2a — `web_fetch` domain-reputation gating

`web_fetch` remains routed to the classifier in `resolveAuto`'s non-bash branch (its `url` arg is already passed as the classifier's `command`, so the target domain is *already visible* to the verdict call — `research.Scraper` also already enforces robots.txt at fetch time). Two additive refinements, both fail-safe:

1. **Static host deny/ask floor (deterministic, runs before the classifier).** Extract the host from the `url` and check it against a small built-in reputation floor plus optional config lists:
   - A built-in **deny** set of known-bad / no-fetch hosts (malware, IP-literal hosts, `localhost`/loopback and RFC-1918 private ranges — SSRF guard) ⇒ **deny**.
   - `permissions.auto.fetch_deny` / `permissions.auto.fetch_ask` config globs (host-matched, per-project via the existing `projects:` map, honoring the ADR-0006 trust boundary — these are *tightening* keys so they may come from either config file) ⇒ deny / ask.
   - A malformed/opaque URL (no parseable host) ⇒ **ask** (fail toward the human).
   - Otherwise fall through to the classifier.
2. **Classifier sees the domain explicitly.** The pending-call prompt for `web_fetch` names the target host and instructs the (block-biased) classifier to weigh domain reputation — an unrecognized or low-reputation host biases toward ask/deny, a well-known documentation/reference host biases toward allow. No external reputation API call in v1 (the classifier's own knowledge is the signal); a live reputation lookup is a noted future upgrade, out of scope here.

Net: `web_fetch` of a well-known host in an active research task flows; a fetch of a sketchy or private-range host stops. This is the "spam/malware signal from the domain" the human called out — the host is the discriminator, not the mere fact of fetching.

### D3 — Give the classifier its context (fulfills 0017 D7)

Plumb the user's messages to `classifyOrAsk` so gray-area verdicts are informed rather than blind-block-biased. To avoid widening the `agent.ToolExecutor` interface (`agent.go:17-19`, implemented widely), carry the user turns on the `context.Context`:

- A new unexported `ctxKey`; `agent/loop.go:executeToolBounded` attaches the current user messages before `a.tools.Execute` (`loop.go:607`/`618`).
- `classifyOrAsk` reads them off `ctx` (nil-safe: absent ⇒ today's behavior).
- **Input hygiene preserved** — `buildMessages` already drops non-user roles (`classifier.go:197-205`), so tool results and actor reasoning still never reach the classifier.

### D4 — Accept-edits posture + valve retune

- In-workspace writes/edits (D1) never reach the classifier, so they never feed the valve — this alone stops benign work from tripping it.
- Only a classifier **deny** counts toward the valve, never an **ask** (today `recordBlock` fires only on deny — keep, and verify D1's asks bypass `classifyOrAsk` entirely, which they do).
- Leave `valveConsecutiveLimit = 3` (a real signal). Revisit `valveTotalLimit` after measurement now that only true gray-area denies count.

---

## Security review

- **No new bypass.** D1 auto-approves only paths resolving inside the canonicalized workspace root, using the same symlink-resolving check bash mutations already trust. An in-workspace symlink pointing out is caught (the link is resolved).
- **ADR-0006 trust boundary intact.** No loosening key is read from `.fuse.local.yml`; `workspaceRoot` is process-derived (`run.go:425`).
- **ADR-0005 unchanged.** Per-segment bash evaluation is untouched; D1 adds allow paths for the edit tools only, never relaxing segment evaluation.
- **Classifier hygiene intact (D3).** Only `role=="user"` turns are forwarded.
- **`web_fetch` egress stays gated (D2a).** It is *not* safe-listed; a static host-deny floor (malware/IP-literal/loopback/RFC-1918 SSRF guard) runs before a domain-reputation-aware classifier verdict. `fetch_deny`/`fetch_ask` are tightening keys, so they honor ADR-0006 from either config file.
- **Escape hatches preserved.** Out-of-workspace edits, egress, dangerous commands still stop; `always_prompt` re-arms any safe-listed tool.

---

## What changes (files)

- `internal/permissions/gate.go` — non-bash edit-tool branch (D1); `web_fetch` host-floor + domain-aware classifier routing (D2a); ctx-read for classifier context (D3).
- `internal/permissions/policy.go` — safe-list additions: `segment_read`, `web_search`, `skill`, `pipeline_run` — **not** `web_fetch` (D2).
- `internal/permissions/classifier.go` — accept forwarded user messages (already parameterized as `Classify(ctx, userMessages, …)`); domain-reputation instruction in the `web_fetch` pending-call prompt (D2a).
- `internal/permissions/heuristics.go` (or a small new `fetchhost.go`) — host extraction + built-in deny floor (malware/IP-literal/loopback/RFC-1918) and config-glob host matching (D2a).
- `internal/config/schema.go` — `AutoConfig` gains `fetch_deny` / `fetch_ask` (`[]string`, host globs); tightening keys, so honored from either config file (D2a).
- `internal/agent/loop.go` — attach user messages to ctx before tool execute (D3).
- Tests across `internal/permissions/*_test.go`, `internal/config/loader_test.go`, and any agent seam test.

Config schema addition: `AutoConfig` gains `fetch_deny` / `fetch_ask` (D2a). `deny` / `ask` / `classifier_model` are unchanged.

---

## Testing notes

- **Edit-tool scoping** (`heuristics_test.go` / `gate_test.go`): `write_file` / `edit_file` with (a) in-workspace path → allow, (b) `../` escape → ask, (c) symlink-out-of-root → ask, (d) not-yet-created in-workspace file → allow, (e) missing/garbled `path` → ask.
- **Safe-list** (`safelist_test.go`): each newly added tool (`segment_read`, `web_search`, `skill`, `pipeline_run`) → allow in smart & auto; **`web_fetch` → NOT auto-allowed** (routes to the host floor / classifier).
- **`web_fetch` host floor** (`gate_test.go` / new `fetchhost_test.go`): (a) well-known host → falls through to classifier, (b) IP-literal / `localhost` / `10.x`/`192.168.x`/`172.16-31.x` → deny (SSRF), (c) `fetch_deny` glob match → deny, `fetch_ask` glob → ask, (d) malformed URL / no host → ask. Assert the *layer* that decides, not just the outcome.
- **Loader** (`loader_test.go`): `fetch_deny`/`fetch_ask` parse; as tightening keys they merge from `.fuse.local.yml` too (unlike the loosening `auto.*` block).
- **Classifier context** (`classifier_test.go`): stub gateway asserts forwarded messages contain the user turns and still contain **no** tool-result / assistant messages.
- **Valve** (`valve_test.go`): a run of in-workspace edits does not advance the valve; only classifier denies do.
- **Regression:** the existing bypass corpus (spec 0017 "Testing notes") stays green.

---

## Out of scope

- OS-level sandboxing (Seatbelt/Landlock/bubblewrap).
- Two-stage classifier CoT (spec 0017's noted future upgrade).
- Any change to bash segment evaluation, the (bash) egress boundary, or the dangerous-command list.
- A **live** domain-reputation API for `web_fetch` — v1 uses the built-in host floor + the classifier's own knowledge; a network reputation lookup is a future upgrade.
- Config schema additions **beyond** `AutoConfig.fetch_deny` / `fetch_ask` (D2a).

---

## Recommended sequencing (for the build plan)

1. **D1 + D2** first — isolated to `internal/permissions/`, no agent seam; removes ~90% of the stopping and is independently shippable/testable.
2. **D2a** (`web_fetch` host floor + config keys) — self-contained in `permissions` + `config`; can ride with D1+D2 or land right after.
3. **D3** next — adds the agent→ctx seam; improves residual gray-area quality (and feeds the D2a domain verdict).
4. **D4** last — guided by measurement once D1–D3 land.

May build as one PR or split D1+D2 / D3+D4; the plan step decides.
