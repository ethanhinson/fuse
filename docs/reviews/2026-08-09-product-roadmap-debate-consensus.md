# fuse Product Roadmap — Debate Consensus

**Date:** 2026-08-09
**Method:** Seven parallel subsystem reviews → four adversarial debate positions (Novelty Advocate, Safety/Security Critic, Architecture Pragmatist, Product/Scale Strategist) → consensus synthesis.

---

## Executive Summary

fuse is a genuinely novel multi-agent AI harness — not a wrapper around an LLM API, but original systems-level engineering in the three hardest problems of agentic computing: **context lifecycle**, **permission security**, and **multi-agent coordination**. No competing agent shell (Claude Code, Codex, Cursor, Aider, Cline) implements these with comparable rigor.

However, four load-bearing architecture debts — identified in the 2026-08-04 review and still unfixed — sit beneath the feature work, and two of them (spawn security, unsandboxed bash) form a compounding privilege-escalation chain that makes the most differentiated capabilities also the most dangerous. The debate converged on a clear consensus: **pay the security and structural debts first, then extend the moat, then build the product surface.**

The four debaters agreed on 5 of 7 top priorities — an unusually strong consensus for adversarial debate. The disagreements were about *ordering*, not *content*: the Novelty Advocate wanted to double down on extending what's differentiated; the Safety Critic and Architecture Pragmatist wanted to fix the foundations; the Product Strategist wanted to ship product-facing layers. The synthesis below resolves these into a phased roadmap where each phase's prerequisites are satisfied by the prior phase.

---

## What Is Novel (The Moat)

Seven capabilities form fuse's defensible competitive moat. These are ranked by differentiation density — how much original engineering they pack relative to any competitor.

### 1. Two-Tier Context Management with Anchored ODSNF Summarization + Lossless Segment Recovery
**The crown jewel.** Three composed layers form a complete "treat-context-as-cache-over-disk" strategy:
- **Tier-1** (`pruneOldToolResults`): deterministic stub-pruning of old tool results, always on, preserving tool-call/result pairing integrity. Only `tool` role messages are touched.
- **Tier-2** (`summarize.go`): bounded LLM summarization runs *before* Tier-1 over the unprotected region, injecting a single anchored ODSNF (Objective/Details/State/Next/Files) summary. `dropPriorSummary` removes the prior before inserting the new — exactly one living summary survives. Fail-safe is a byte-identical golden match to pre-summarization Tier-1 behavior (proven, not asserted).
- **Segment recovery** (`segment/store.go`, ADR-0020): before stubbing, the raw pre-compaction region is archived to born-compressed gzip `.md.gz` files with a lossless self-delimiting JSON raw region. The `segment_read` tool (read-only safelisted) recovers exact detail filtered by tool — avoiding re-running tools that may need fresh approval or have side effects.
- **Hybrid token accounting**: exact provider-reported usage + bytes/4 delta estimate, resetting to ground truth every turn — provably prevents estimation drift.

### 2. Per-Segment Permission Model with Deny-Toward-Human Default
Shell commands are split into `Segment{Name, Args, Raw}` structs; allow rules require ALL segments to independently match (`allSegmentsAllowed`). A whole-string allow can never rescue a dangerous segment — the inverse of naive prefix-matching. `VerdictAsk` is the zero value; unparseable commands, garbled args, unresolved symlinks, unknown verdicts, timeouts, and missing classifiers ALL route to ask, never allow (CWE-1188). Three-layer pipeline (evalRules → classifyHeuristic → classifier LLM) with fail-closed semantics. Escalation valve shared by reference across `CloneForChild` — child subtree blocks count toward parent session budget. Classifier input hygiene: only user turns reach the LLM, tool results stripped (prompt-injection defense). Symlink-aware path scoping handling not-yet-created files. SSRF detection via `net.ParseIP`.

### 3. Blackboard Multi-Agent Coordination with Scheduler-Slot-Yielding
In-memory KV store shared by every agent in a tree, with provenance, glob `Keys`, and blocking `Wait` that **yields the caller's scheduler slot** while parked (D1 anti-deadlock contract). Fair round-robin dispatch across pool FIFOs with a priority lane for resumed parents (depth-2 deadlock fix). Multi-layered spawn budgeting: depth cap, tree-global spawn count, per-workflow pool quotas, session token ceiling, queue bounds. Unified visibility predicate (change 0036) collapsing all admission rules into one.

### 4. Relevance-Aware Two-Phase Context Pruning with Hybrid Borderline-Only Classifier
Phase 1: guaranteed recency floor protecting newest results. Phase 2: fill remainder with highest-relevance-scored results (5 weighted signals: tool-type, keyword overlap, failure signal, dependency-reuse, recency). Hybrid LLM classifier: only borderline results reach the model; clear-cut skip entirely. No-op degeneration mathematically proven (change 0028). On failure, arms suppression — a failing classifier never hot-loops.

### 5. Pipeline as Declarative DAG with Execution-Affecting Conditional Routing
Routing affects execution, not just labeling: branch-gated targets run only when released by a choosing router, are skipped otherwise, and skip propagates downstream. Fanout spawns N parallel instances with glob-namespaced outputs (scatter-gather). Tarjan SCC cycle detection over combined `depends_on` + `goto` edges. Routing is total (never errors). Synthesis uses LLM with JSON Schema as `Expects`, validates under Caps, re-asks with errors.

### 6. Doom-Loop Detection with Interactive Loop Approval
Hashes each turn's tool-call set (name+args, order-independent), detects period-1 (AAAA) and period-2 (ABAB) cycles. Instead of hard-aborting, forces the repeated call through human approval with a "possible loop" preview. On approval, detector resets; on rejection, aborts. More humane than hard-failing — recognizes legitimate repetition (build polling).

### 7. Layered Trust-Boundary Config Merge with Ungameable Rate Limiting
A repo-plantable `.fuse.local.yml` can only **tighten** safety brakes (strictly-lower throughput numbers, extra `always_prompt`/`disabled`), never loosen them (ADR-0006). Loosening keys dropped with per-category warning. Dual-axis (rpm+tpm) continuous-refill token bucket per provider, deliberately ungameable: refundless `Report` charges `max(est, actuals)`, never refunds an over-estimate. Charged once per logical `Complete`, not per transport attempt.

---

## Room for Improvement (The Gaps)

Synthesized across all four debate positions, organized by the degree of consensus.

### Tier A — Universal Consensus (all four debaters agree these are top priority)

| # | Gap | Risk | Root Cause |
|---|-----|------|------------|
| A1 | **Spawn security / dead CloneForChild** | Confirmed privilege-escalation path. Children run with `AlwaysApprove`; tool restriction is one-generation-deep; `spawn_agent` force-included in every `Subset` (registry.go:87-90). A grandchild runs full bash, full tools, no gate. The escalation valve is shared by reference — but the gate it shares was never wired to children. | Dead `CloneForChild`/`prefixedApprove`/`cache-clone` path in policy.go; three agent entry points (main.go, shell.go, research_probe.go) each must be independently re-wired. |
| A2 | **Blackboard has no ACLs** | Last-writer-wins; any agent overwrites any key. Snapshot is shallow-copy — reference-typed Values shared with the store (safety by convention). A confused child silently clobbers a peer's result, poisoning ensemble/debate output. `Wait` is timeout-only with no producer-death wake. | Out-of-scope follow-up from change 0023; acknowledged but never addressed. |
| A3 | **Pipeline registry & confirm gate are stubs** | `name` param reserved-but-unsupported; `confirm` param advisory (synthesis proceeds regardless). The most powerful autonomous-execution primitive has its single human gate wired to nothing. Every invocation re-synthesizes from scratch. `Execute` returns only a status string. | pipeline_run.go:Execute; synthesize.go; change 0026. |

### Tier B — Strong Consensus (three of four debaters)

| # | Gap | Risk | Root Cause |
|---|-----|------|------------|
| B1 | **TUI transport fragility** | Every freeze observed is a violation of the transport invariant. Channel-and-pump bridge (bridge.go) replaced the re-arm pattern but still has a fixed buffer(64) with no backpressure and duplicated overlay routing in two places. | `waitForMsg` re-arm legacy; dual-switch Update duplication; no transport-layer test harness. |
| B2 | **Structured transcript representation** | Pre-styled `[]string` with ANSI baked at append time, mutated via absolute line indices. Forces O(transcript)-per-frame rewrap and fragile inline-block machinery. The "one living summary" invariant depends on string-prefix matching. `segment_read` turn ranges carry explicit "DRIFT 1" notes. Archive failure = permanent data loss. | Root cause of every freeze and fragile display machinery; flagged "needs redesign → structured entries rendered at view time" but not landed. |
| B3 | **Result/lifecycle SpawnResult semantics** | Cancelled/errored spawns can report `StatusDone` with `Err=nil`; `ErrRemoteStreamLost` matched by string comparison (spoofable). `AgentHandle`'s drain-safe ordering is a pointer-memo patch. Every consumer defensively re-derives whether a child succeeded. | Underspecified lifecycle; terminal states not a closed set; string-comparison matchers. |

### Tier C — Cross-Cutting (appears in multiple positions, different framings)

| # | Gap | Risk | Root Cause |
|---|-----|------|------------|
| C1 | **Unsandboxed bash** | `bash.go` runs `/bin/sh -c` with no sandboxing, no env filtering, no allowlist. The permission gate is the SOLE control, and it's itself bypassable. Impact amplifier for every other gap. | No defense-in-depth behind the permission decision. |
| C2 | **Plaintext secrets / remote execution security** | No https enforcement on remote executor URL; plaintext tokens in dispatch JSON despite encrypted mode; unsanitized error bodies reflected into LLM context; sops cache with no zeroization. | Remote executor security not treated as a threat model. |
| C3 | **Malformed deny patterns fail-open** | `patterns.go:matchesAny` discards `path.Match` errors with no logging. A misconfigured deny silently disappears; operator has no signal. | `ok, _ := path.Match(...)` in security-critical matcher. |
| C4 | **MCP HITL AlwaysApprove on missing socket** | `hitl.go:~144`: empty `FUSE_HITL_SOCKET` → auto-approve everything. External MCP servers run ungated. | Implicit permissive policy choice as the default. |
| C5 | **Confirmed data races** | `updateInlineAgent` ranges `node.Events` unlocked while children append; unlocked Status/time/token reads in renderers; Esc never denies an approval (key-name mismatch: code matches "escape", bubbletea names it "esc"); root turn on `context.Background()` is uncancellable. | Concurrency invariants not enforced. |
| C6 | **Doom-loop detector only catches period-1/2** | Longer cycles (ABCABC, period-3+) invisible, run until maxTurns exhaustion. | `fingerprint.go:hasPeriod1/hasPeriod2` only. |
| C7 | **Classifier uses `context.Background()`** | A cancelled agent run still completes an expensive classifier round-trip. | `relevance_classifier.go:Score→classify` drops parent ctx. |
| C8 | **429 retries bypass the rate gate** | Adapter retries 429s without re-consulting `Wait`; `Retry-After` headers ignored. Adapter may rapidly re-fire into a genuinely rate-limited provider. | `adapter.go` retry loop; no feedback path to bucket. |
| C9 | **Hardcoded model registry** | 12 aliases hardcoded in Go; adding a model requires recompile. Config-provided entries exist but precedence is unclear. | `model/registry.go:DefaultRegistry`. |
| C10 | **No structured audit log** | Permission verdicts produce only `DenyReason` strings; no structured record of which layer decided what. MCP notification drops are silent. JSONL logging is metadata-only. | No audit surface designed. |

### Tier D — Moat Extension Opportunities (Novelty Advocate's priorities)

| # | Opportunity | Current State | Next Evolution |
|---|-------------|---------------|----------------|
| D1 | **Cross-session memory hierarchy** | `segment_read` only recovers running session's segments | Semantic index over archived segments; query past work by intent; SQLite-backed segment store with atomic writes |
| D2 | **Batched relevance classifier** | Batch machinery exists in `classify()` but live path scores per-result | Drive the batched path — ~10x fewer model calls |
| D3 | **Runtime-configurable budget constants** | `summarizeSuppressTurns=5`, `pruneProtectTokens=40k`, `pruneThresholdPct=85` all hardcoded | Make config-tunable for unusual model context windows |
| D4 | **Per-segment permissions for MCP tool arguments** | Only bash has the per-segment model | Extend `allSegmentsAllowed` to MCP tool arguments — argument-level permissions for arbitrary tools |
| D5 | **Period-N doom-loop detection** | Only period-1/2 | Suffix-array or cycle detection over fingerprint window |
| D6 | **Per-tool timeout configuration** | Single global `DefaultToolTimeout=120s` | Per-tool timeout config; orchestration tools exempt |

### Tier E — Product Surface Gaps (Product Strategist's priorities)

| # | Gap | User Value |
|---|-----|------------|
| E1 | **Multi-agent orchestration UX** | Debate/ensemble/critic patterns need TUI affordances (side-by-side positions, verdicts, votes), real mid-run human injection, producer-death wake |
| E2 | **Research mode as first-class feature** | Most demoable adoption driver; needs multi-provider fan-out, result dedup, dedicated TUI surface, fixed robots engine |
| E3 | **MCP bidirectional richness** | Advertise client capabilities (roots/sampling/elicitation); network server mode; OAuth2 refresh; reconnection; prompts capability |
| E4 | **Model portability & cost management** | Config-driven model registry; 429 feedback loop; jittered backoff; per-model cost tracking |
| E5 | **Observability & replay** | Structured audit log; `fuse replay` command; MCP notification metrics; persistent cost dashboard |

---

## Consensus Roadmap

The debate resolved into four phases. Each phase's prerequisites are satisfied by the prior phase. The ordering reflects the universal consensus that **security debts must be paid before the multi-agent capabilities that amplify them are extended**, and that **structural foundations must be solid before product surfaces are built on them**.

### Phase 0: Security Criticals (blocks everything)
*Agreed by: all four debaters. These are the "can't ship multi-agent without these" items.*

| Priority | Work | Effort | Files |
|----------|------|--------|-------|
| 0.1 | **Wire CloneForChild** — resurrect the dead tested `CloneForChild`/`prefixedApprove`/`cache-clone` path. Every child gets a cloned, independently-gated permission gate whose approval surface is NOT `AlwaysApprove`. Make tool restriction transitive (subset from parent's active registry, not session-root). Make `spawn_agent` inclusion opt-in (a parent must be able to produce a non-spawning leaf). Collapse the three agent entry points behind a single `ChildBuilder` factory. | High | policy.go, hitl.go, registry.go, cmd/fuse/{main,shell,research_probe}.go |
| 0.2 | **Fix malformed-pattern fail-open** — `validate.go` compiles every deny/ask/allow glob at Load; rejects malformed patterns (hard config-load failure). `patterns.go:matchesAny` never discards `path.Match` errors. | Low | patterns.go, validate.go |
| 0.3 | **Fix MCP HITL default** — empty `FUSE_HITL_SOCKET` for a non-explicitly-trusted MCP server → fail-closed (deny or refuse to start). Permissive path is explicit logged opt-in. | Low | hitl.go |
| 0.4 | **Fix Esc-never-denies** — match bubbletea's actual key name ("esc"); add test that Esc produces a deny. | Trivial | shell.go, approval.go |
| 0.5 | **Fix confirmed data races** — lock `node.Events` append/iterate boundary; snapshot Status/time/token under lock for renderers; make root turn's context cancellable. | Medium | internal/agent/tree.go, internal/tui/renderers |
| 0.6 | **Wire pipeline confirm gate** — synthesized pipelines MUST pause for human approval when `confirm` is false in an interactive session. Add routing-complexity cap to `Validate`. | Medium | pipeline_run.go, validate.go |
| 0.7 | **Enforce https on remote executor** — reject `http://` remote URLs at config validation; strip/redact tokens from dispatch JSON in encrypted mode; sanitize remote error bodies before LLM context; zeroize sops cache on session end. | Medium | config/validate.go, remote executor |

### Phase 1: Load-Bearing Redesigns (unblocks product velocity)
*Agreed by: Architecture Pragmatist (primary), Safety Critic (spawn security overlaps Phase 0), Product Strategist (as observability/replay prerequisites).*

| Priority | Work | Effort | Files |
|----------|------|--------|-------|
| 1.1 | **Structured transcript representation** — replace pre-styled `[]string` with structured entries rendered at view time. Eliminates O(transcript)-per-frame rewrap, fragile inline-block machinery, string-prefix summary matching, and segment turn-range drift. Archive failure no longer = permanent data loss. | High | internal/tui/shell_model.go, internal/tui/renderer.go, internal/agent/loop.go |
| 1.2 | **TUI transport consolidation** — collapse the duplicated overlay routing logic (handleKey + handleOverlayMsg) into a single path. Remove the fixed buffer(64) bottleneck; add backpressure or unbounded-with-flow-control. Build a transport-layer test harness that exercises real spawn wiring (retires the "verify-tool-loop-at-gateway-seam" mandatory learning). | High | internal/tui/bridge.go, internal/tui/shell_model.go |
| 1.3 | **SpawnResult lifecycle** — define typed terminal states (Done/Cancelled/Failed/Timeout) with mandatory non-nil Err for non-Done. Kill string-comparison matchers (`errors.Is`/`As` over typed sentinels). Fold `AgentHandle` doneMemo into the typed result. Fix SSE/remote lifecycle bugs (30s timeout, unchecked scanner, spoofable error strings). | High | internal/agent/spawn.go, internal/agent/handle.go |
| 1.4 | **Blackboard safety** — per-key ownership/ACLs with CAS versioning (writes allowed only by key owner or explicit ACL). Deep-copy or freeze-on-read `Snapshot`. Producer-death wake for `Wait` (tree-idle signal, not just timeout). Structured audit log of blackboard writes. | High | internal/agent/blackboard.go |
| 1.5 | **Execution sandbox (defense-in-depth)** — add a sandbox behind the permission gate: at minimum filter env (strip credentials), restricted PATH, bound writable paths to workspace. Strong version: Linux namespace+seccomp or macOS sandbox-exec restricting FS writes outside workspace and blocking network egress unless allowed. | High | internal/tools/bash.go |

### Phase 2: Extend the Moat (deepens differentiation)
*Agreed by: Novelty Advocate (primary). These extend the capabilities that are already novel — widening the gap before competitors catch up. Prerequisites: Phase 0 (security) and Phase 1 (foundations) must be solid so the extensions don't compound debt.*

| Priority | Work | Effort | Files |
|----------|------|--------|-------|
| 2.1 | **Cross-session memory hierarchy** — make `segment_read` recover across sessions. Build a semantic index over archived segments (query past work by intent). Replace unbounded `index.json` with SQLite-backed store (atomic writes, retention policies, integrity checksums). Add streaming/pagination to `segment_read` (currently 32KB cap). | High | internal/segment/, internal/tools/segment_read.go |
| 2.2 | **Drive batched relevance classifier** — the batch machinery exists in `classify()` but the live path scores per-result. Wire the batched path — ~10x fewer model calls. Thread loop ctx into classifier (currently `context.Background()`). Make heuristic weights runtime-configurable. | Medium | internal/agent/relevance_classifier.go, internal/agent/relevance.go |
| 2.3 | **Runtime-configurable context budgets** — make `summarizeSuppressTurns`, `pruneProtectTokens`, `pruneThresholdPct`, `summarizerInputBudgetTokens` config-tunable. | Low | internal/config/schema.go, internal/agent/loop.go |
| 2.4 | **Extend per-segment permissions to MCP tool arguments** — generalize `allSegmentsAllowed` to structured tool calls beyond bash. Makes fuse the only agent with argument-level permissions for arbitrary tools. Add structured audit log of permission verdicts. Implement two-stage CoT classifier. | High | internal/permissions/, internal/mcp/ |
| 2.5 | **Period-N doom-loop detection** — generalize via suffix-array or cycle detection over the fingerprint window. Per-tool timeout configuration. Wire doom-loop to escalation valve (loop in auto mode escalates to prompt-all). | Medium | internal/agent/fingerprint.go |
| 2.6 | **Finer-grained tool parallelism** — currently parallelism only triggers when ALL calls are `spawn_agent`. Add per-call safety annotation so mixed calls (e.g. two `read_file` + one `spawn`) can parallelize. | Medium | internal/agent/loop.go |

### Phase 3: Product Surface (makes it usable and adoptable)
*Agreed by: Product Strategist (primary). These convert the engineering marvel into a product. Prerequisites: Phase 1 (structured transcript, TUI transport) for the UX surfaces; Phase 0 (security) for trust.*

| Priority | Work | Effort | Files |
|----------|------|--------|-------|
| 3.1 | **Pipeline registry & reusable artifacts** — implement the reserved `name` param (filesystem-backed `~/.fuse/pipelines/` with YAML, versioning, listing). Cache synthesized pipelines (goal-hash-keyed). Return structured step-by-step trace from `Execute`. Add backoff/jitter to `spawnWithRetry`. Incremental synthesis repair (carry partial pipeline). | High | internal/pipeline/, internal/tools/pipeline_run.go |
| 3.2 | **Multi-agent orchestration UX** — TUI affordances for debate/critic/ensemble patterns (structured rendering of positions, verdicts, votes). Real mid-run human injection (bounded push path, not just turn-boundary self-pull). Orchestration view modes in AgentsModel. | High | internal/tui/agents_model.go, internal/agent/human_handles.go |
| 3.3 | **Research mode as first-class feature** — move facet fan-out into `internal/research` (FacetedSearch with N sub-queries across providers). Multi-provider fallback in `resolve.go`. Cross-provider result deduplication. Fix robots.txt engine (honor User-Agent groups, path wildcards, cache expiry). Dedicated research TUI mode (facet progress, source sidebar, citation-linked report). Fix Tavily content-squash. | High | internal/research/, internal/tui/, internal/skills/embedded/ |
| 3.4 | **MCP bidirectional richness** — advertise client capabilities (roots/sampling/elicitation). Network server mode (HTTP/SSE/TCP listener). OAuth2 token refresh + 401-retry. Reconnection logic (wire staged `resubscribeAll()`). Expose prompts capability. Include skill tool in MCP server mode. Health-checking/keepalive. | High | internal/mcp/, cmd/fuse/mcp_server.go |
| 3.5 | **Model portability & cost management** — config-driven model registry (move hardcoded aliases to config). Feed 429/`Retry-After` back to rate bucket. Re-consult gate on 429 retry. Jittered exponential backoff. Queue depth bounds on `Bucket.Wait`. Per-model cost tracking and session spending caps. | Medium | internal/model/, internal/ratelimit/, internal/config/ |
| 3.6 | **Observability & replay** — structured audit log for permission verdicts (`PermissionAuditEntry` with tool, args, verdict, deciding-layer, reason, timestamp, node-ID). `fuse replay` command (replay session from JSONL + segment store). MCP notification drop metrics. Persistent cost/token dashboard. | High | internal/permissions/, internal/session/, cmd/fuse/ |

---

## Debate Resolution Notes

### Where all four agreed
- **Spawn security (A1)** is the #1 priority — all four placed it in their top 2. The dead `CloneForChild`, `AlwaysApprove` for children, and force-included `spawn_agent` are a confirmed privilege-escalation path that the system prompt actively encourages exploiting.
- **Blackboard ACLs (A2)** — all four flagged it. The Novelty Advocate saw it as the unlock for ensemble patterns; the Safety Critic saw it as a data-integrity hazard; the Pragmatist saw it as a co-traveler with spawn security; the Product Strategist saw it as a prerequisite for orchestration UX.
- **Pipeline registry/confirm gate (A3)** — three placed it explicitly; the Safety Critic flagged the confirm gate specifically.

### Where the debate was productive
- **The Novelty Advocate vs. the Pragmatist**: the Advocate argued for doubling down on what's differentiated; the Pragmatist argued that structural debts must be paid first. **Resolution**: the moat extension (Phase 2) is sequenced AFTER the structural redesigns (Phase 1) because extending a novel system on rotten foundations compounds the debt — e.g., adding cross-session memory on top of a fragile `index.json` that crashes corrupt is wasted work.
- **The Safety Critic vs. the Product Strategist**: the Critic argued for security above all; the Strategist argued for product adoption. **Resolution**: the security criticals (Phase 0) are small, targeted fixes (most are Low/Trivial effort) that unblock trust, while the product surfaces (Phase 3) are the larger investments that land cleaner once the foundations (Phase 1) are correct. The sandbox (Phase 1.5) is the bridge: it's defense-in-depth that makes the product safe to demo.
- **TUI transport**: the Novelty Advocate did not flag it; the other three did. **Resolution**: it's in Phase 1 because three of four debaters identified it as the root cause of every freeze, and the Product Strategist's observability/replay goals depend on a structured transcript.

### What was deferred (consensus to not do now)
- Adding more embedded skills beyond `research` — the embedding is hardcoded (embed.go) and not data-driven, but this is low-priority until the skill system is more mature.
- Collapsing the two parallel permission matching layers (patterns.go vs rules.go) — complex refactor, low immediate risk if malformed-pattern fail-open is fixed (Phase 0.2).
- Reputation system with real upstream lists and subdomain matching — the current scaffold is acknowledged as minimal, but the hard blocklist deny is the load-bearing control; reputation is a nudge.
- Pipeline join-after-branch richer semantics (ADR-0014 conservative v1 consequence) — sound first version, defer richer joins.

---

## Success Criteria

- **Phase 0 complete when**: a spawned child at any depth inherits a gated permission gate (not `AlwaysApprove`); malformed deny patterns are rejected at config load; Esc denies approvals; the pipeline confirm gate is real; remote secrets are never plaintext over http.
- **Phase 1 complete when**: the TUI has not frozen in any tested multi-agent scenario; the transcript is a structured data model rendered at view time; `SpawnResult` has a closed set of terminal states with typed errors; the blackboard has per-key ownership and deep-copy snapshots; bash runs in a sandbox with bounded writable paths.
- **Phase 2 complete when**: `segment_read` recovers across sessions with a semantic index; the batched classifier path is live (measurable model-call reduction); context budgets are config-tunable; MCP tool arguments have per-segment permission semantics.
- **Phase 3 complete when**: pipelines are named/saved/reused artifacts with structured traces; the TUI has orchestration view modes; research mode has a dedicated surface with multi-provider fan-out; fuse advertises MCP client capabilities and serves over network; models are config-driven with cost tracking; `fuse replay` works.
