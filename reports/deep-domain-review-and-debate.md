# Deep Domain Review & Antagonistic Debate — fuse

> A multi-perspective architectural review of the fuse platform: deep per-domain
> analysis, followed by a three-way adversarial debate (Advocate vs. Skeptic vs.
> Pragmatist-Architect), concluding with a synthesized verdict and prioritized
> action plan.

---

## 1. Platform Overview

**fuse** is a terminal-based, multi-model AI agent harness written in Go (~49k
LOC, ~160 source files, 146 test files). It drives LLMs through a LiteLLM
gateway with tools, a hardened subagent runtime, human-in-the-loop permissions,
MCP integration, an embedded skill system, web research, and pipeline
composition.

| Domain | Package | Lines | Files | Grade |
|--------|---------|-------|-------|-------|
| Agent Runtime | internal/agent | 7,846 | 35 | B+ |
| TUI | internal/tui | 8,546 | 37 | B+ |
| MCP Client | internal/mcp | 8,033 | 49 | B |
| Permissions/HITL | internal/permissions + hitl | 4,319 | 24 | A- |
| Tools | internal/tools | 3,939 | 32 | B+ |
| Config | internal/config | 2,797 | 6 | B+* |
| Research | internal/research | 2,609 | 13 | B+ |
| Pipeline | internal/pipeline | 2,551 | 11 | B+ |
| Model/Gateway | internal/model | 1,355 | 6 | A- |
| Rate Limiting | internal/ratelimit | 866 | 2 | A |
| Skills | internal/skills | 690 | 6 | A- |
| Probe | internal/probe | 474 | 3 | B+ |
| Cross-cutting | session, banner, version | ~200 | 5 | C+ |

*Config grade reduced from A- due to critical writer data-loss bug.

Governance: 14 ADRs, 39 tracked changes (30 completed, 6 killed), 27 curated
learnings in the docket system.

---

## 2. Per-Domain Deep Review Summary

### 2.1 Agent Runtime (internal/agent) — Grade: B+

The core execution substrate. Three clean interfaces (Completer, ToolExecutor,
Renderer) bound in an Agent struct with functional options.

**Standout: the ADR-0007 scheduler.** A single admission authority unifies
global spawn budget, concurrency cap, queue bounds, token quota, workflow pool
scope, and the tool-strip predicate. Every resource constraint flows through one
choke point. The four-layer race safety net (per-turn strip, synchronous Admit,
atomic enqueue-bound, call-time backstop) is more defense-in-depth than most
agent frameworks have at all.

**Blackboard shared memory** — key/value store with Wait/yield semantics,
provenance tracking, and a lost-wakeup guard. Enables agent-to-agent
coordination patterns (debate, ensemble, producer/consumer) that differentiate
fuse from single-agent chat tools. Five clean tools (read/write/wait/keys/delete)
with timeout caps.

**Two-tier context compaction** — Tier-2 LLM summarization with Tier-1
deterministic fallback, suppression windows for failing summarizers, hybrid
token accounting that resets to ground truth every turn. The deterministic
recovery on provider context-length rejection (prune harder, retry once) is a
production necessity.

**Concrete bugs found:**

| Severity | Bug | Location |
|----------|-----|----------|
| Medium | Slot leak under OOM/panic between enqueueSlot and goroutine start | spawn.go:272-274 |
| Medium | Cancel-func write race: SetCancel after addNode, concurrent CancelNode finds nil | spawn.go:274 vs tree.go:437-450 |
| Medium | No per-tool-call timeout — hanging bash blocks entire loop | loop.go:449-493 |
| Medium | Loop detector only catches consecutive identical sets; misses ABABAB | fingerprint.go:31-39 |
| Low | Subtree index rebuilt from scratch 4+ times per spawn check | subtree.go:15-23 |

**Test coverage:** ~3,500 lines, excellent edge-case testing (blackboard races,
scheduler queue bounds, visible predicates). Missing: integration tests for
full spawn-to-run-to-collect with 3+ concurrent agents; no -race CI target.

---

### 2.2 Permissions and HITL (internal/permissions, internal/hitl) — Grade: A-

The platform's strongest subsystem. Production-grade security engineering.

**Per-segment allow-rule evaluation (ADR-0005)** directly addresses the
Gemini/Cursor compound-command bypass CVEs. Bash commands are split into
segments (across logical operators, pipes, newlines, bash -c bodies), and each
segment is evaluated independently. A compound command like "git status AND rm -rf ~"
is NOT auto-approved by a "git *" pattern — the rm segment does not match.
Wrapping (sh -c), command substitution, env-assignment prefixes, and
path-qualified argv0 all fail closed.

**Trust boundary (ADR-0006)** — a repo-plantable .fuse.local.yml can NEVER
loosen permissions. Only tightening keys (always_prompt, disabled) take effect
from untrusted files. Loosening keys (mode, auto_approve, the entire auto block)
are ignored with a startup warning. Per-project trust via the projects: map is
honored only from the trusted home config.

**Auto-mode classifier pipeline** — 5-layer evaluation: static deny/ask rules,
read-only safe list, path/egress heuristics, classifier_model for gray areas.
Block-biased LLM classifier ("when in doubt, prefer ask or deny"). Escalation
valve: 3 consecutive or 20 total classifier blocks pauses auto mode.

**Bypass analysis:** 13 attack vectors evaluated, zero critical bypasses found.
Fail-closed at all 12 decision boundaries.

**Issues:**

| Severity | Issue | Location |
|----------|-------|----------|
| High | No connection deadlines in HITL socket relay — can block forever | hitl.go:71-80, 93-103 |
| Medium | sudoedit not in dangerousNames — elevated-privilege edits | rules.go:40-54 |
| Medium | Server-side decode errors silently dropped — client blocks forever | hitl.go:73-75 |
| Medium | context.Background() instead of propagated context | hitl.go:75 |
| Info | "Allow for session" cache is actually per-turn (rebuilt each turn) | cache.go + shell_model.go:932 |

---

### 2.3 MCP Client (internal/mcp) — Grade: B

The largest domain (8,033 lines, 49 files). Multi-transport MCP client with
stdio, streamable-HTTP (ADR-0011), and WebSocket (change 0022) transports.

**Strengths:** Capability negotiation with consistent fail-open (ADR-0010).
Streamable-HTTP transport is the best — handles 401/404 retry, session
lifecycle, SSE reassembly with rune-boundary alignment. Ref-counted subscription
tracking. Strong test coverage with transport-level integration tests.

**The core tension: 8,000 lines for a subsystem that primarily calls
tools/list and tools/call.** The reviewer's assessment is that ~3,000 lines
carry the core value; the rest is protocol-mandated surface area that is
incompletely implemented.

**Critical findings:**

| Severity | Issue |
|----------|-------|
| High | resubscribeAll implemented but never called — dead code. Once a connection drops, the MCP session is permanently dead. |
| High | Notification handlers run synchronously on the read-pump goroutine — a slow handler blocks ALL response dispatching. |
| Medium | Legacy HTTP/SSE client uses bufio.Scanner with 64KB line cap — large SSE lines silently kill the pump. |
| Medium | OAuth (~250 lines) uses contextless HTTP calls — no cancellation, no timeout. |
| Medium | Resource subscriptions (~400 lines) explicitly dogfood-only — no production users. |

**Verdict:** Streamable-HTTP + stdio are genuinely useful. Legacy HTTP/SSE,
dogfood-only subscriptions, and contextless OAuth are dead weight. 8,000 lines
should be ~5,000.

---

### 2.4 TUI (internal/tui) — Grade: B+

Bubble Tea shell with markdown rendering, slash commands, subagent UX, agents
overlay, and untrusted-byte sanitization.

**Strengths:** Transcript re-wrap architecture (hangWrap) correctly handles
both dynamic wordwrap and pre-wrapped glamour content on resize. FIFO approval
queue prevents orphaned goroutines. Rate-gate tick jitter filter prevents render
storms. Sanitization of untrusted bytes is comprehensive (control-byte stripping
for fixed-width TUI safety).

**Issues:**

| Severity | Issue | Location |
|----------|-------|----------|
| High | Monolithic ~250-line Update switch — hard to test, error-prone | shell_model.go:323 |
| High | Value-receiver Update calling pointer-receiver mutators — fragile | shell_model.go |
| Bug | MCP expand returns prose, not command | mcp_provider.go:75 |
| Data race | scriptedCompleter.requests written by agent goroutine, read by test | harness_test.go:48 |
| Medium | 3-boolean mode flag (8 states, 5 valid) instead of enum | agents_model.go:34-60 |
| Medium | renderInlineError missing sanitize | subagent_summary.go:33 |

---

### 2.5 Tools Registry (internal/tools) — Grade: B+

ADR-0013: the registry owns concurrency safety. Centralized panic recovery and
spill truncation mean individual tools need zero boilerplate. Clean injection
seams (SpawnFunc, PipelineRunFunc, BlackboardStore) avoid import cycles.

**Tool stripping** (changes 0033/0034/0036) is excellent — unified Visible()
predicate with per-turn strip + call-time backstop. Budget exhaustion permanently
strips spawn_agent; concurrency cap reversibly strips it.

**Critical gaps:**

| Severity | Issue |
|----------|-------|
| Critical | 0028 (semantic tool relevance) — NOT IMPLEMENTED. Zero code matches. All 15+ tools advertised every turn (~10-20KB/turn waste). |
| Critical | 0029 (read-file dedup cache) — NOT IMPLEMENTED. Zero code matches. Repeated reads re-read from disk every time. |
| Medium | webSearchTool.providerOnce not thread-safe | web_search.go:101-105 |
| Medium | TOCTOU in Registry.Execute — lookup drops lock before Execute | registry.go:115-116 |
| Low | Spill-file GC is one-shot, not periodic — long sessions accumulate files |

The irony: a system with two-tier context compaction (because context windows
are constrained) wastes 10-20KB/turn on irrelevant tool schemas — the exact
problem 0028 was meant to solve.

---

### 2.6 Pipeline (internal/pipeline) — Grade: B+

DAG executor with conditional routing (ADR-0014), skip propagation, fanout, and
synthesis from natural-language goals.

**Strengths:** Sibling cancellation on error (verified by atomic test).
Deadlock-proof under tight scheduling slots (never holds scheduler slot while
awaiting children). Tarjan SCC cycle detection. YAML/JSON parity. Structured
result degradation (Expects mismatch degrades to raw text, never fails).

**Issues:** No per-run timeout. retry(N) semantics ambiguous. Glob keys not
expanded in conditions. Synthesis may produce unreachable steps. No pipeline
visualization/debug output.

---

### 2.7 Research (internal/research) — Grade: B+

Complete web research system: facet diversification, fan-out, dedup,
synthesis. BYO search key with Brave/Tavily/custom providers.

**Strengths:** Production-grade HTTP client (cancelBody timer cleanup, GetBody
rewind, Retry-After parsing). Proper robots.txt precedence (longest prefix, Allow
beats Disallow on tie). Per-domain token-bucket rate limiting. go-readability
extraction with crude tag-strip fallback. Probe subsystem with clean
agent.Renderer seam.

**Issues:** No query deduplication/caching (biggest gap). crudeStrip is O(n^2).
Arbitrary x8 readCap multiplier undocumented. No pagination. Crawl-delay ignored.

---

### 2.8 Skills, Model, Config — Grades: A-, A-, B+*

**Skills:** Clean, small, works. Two-phase frontmatter parser works around YAML
library limitations. First-wins dedup (filesystem > embedded). Issues: silent
parse errors (log.Printf), dead fields (Context/Agent).

**Model:** Excellent adapter — streaming-by-default, RateGate delta accounting
(estTokens charged at Wait, delta at Report), copy-on-configure idiom, error
enrichment ("never lose cause"). Issues: len(body)/4 token estimation is crude;
linear backoff with no jitter; registry hardcodes 12 entries with zero-valued
ContextWindow.

**Config:** Trust-boundary architecture is production-grade. Pointer discipline
correctly distinguishes omitted vs. zero. Issues: CRITICAL writer data-loss bug
— AddMCPServer/RemoveMCPServer erases all config except Gateway + MCPServers. No
Validate(). ThroughputConfig disconnected from RateGate.

---

### 2.9 Cross-Cutting — Grade: C+

| Component | Verdict |
|-----------|---------|
| Ratelimit | Excellent leaf package. Two-axis token bucket, per-provider overrides. |
| Session | Underdeveloped — metadata-only entries, silently ignored errors, unimplemented replay. "Observability theater." |
| Probe | High quality, narrow scope — captures spawn trees, search/fetch census, token totals. |
| Version | Fragile — hardcoded 0.1.0-dev, test pins exact string, no ldflags. |
| ADRs | B+ quality, 47% coverage. Missing ADRs for subagent runtime, skill discovery, HTTP transport. |
| Codebase health | GOOD — clean layering, three acknowledged cycle-breaking patterns, no import cycles. |

---

## 3. The Antagonistic Debate

Three positions were argued from the same body of evidence:

### 3.1 The Advocate's Case

fuse is a genuinely well-architected agent harness whose feature set is
justified by the problem domain. The complexity is real but proportionate.

**Key arguments:**
- The scheduler (ADR-0007) is A-grade architecture — the four-layer race safety
  net is more than most frameworks have.
- The permission system is production-grade security — zero bypasses found across
  13 attack vectors. This is the minimum viable security for an autonomous
  agent that executes shell commands.
- MCP's 8,000 lines maps to protocol surface area (3 transports, capability
  negotiation, subscriptions, streaming, OAuth). Not supporting MCP means
  disconnecting from a growing ecosystem.
- The blackboard is a genuine differentiator — transforms "chat with tools" into
  a "multi-agent platform."
- Two-tier context compaction solves the #1 operational problem in agent systems.
- The docket process (39 changes, 27 learnings, 6 killed) is institutional
  memory most projects lack entirely.
- Missing features (0028, 0029) are correctly staged, not abandoned.

**Verdict: B+ overall, trending A as implementation gaps close.**

### 3.2 The Skeptic's Case

fuse suffers from classic breadth-over-depth engineering. Impressive surface
area, uneven implementation quality, critical bugs in core paths, vaporware
features, and significant dead code.

**Key arguments:**
- CRITICAL: The config writer silently destroys user configuration — a data-loss
  bug in a shipped CLI feature. How did this pass review?
- VAPORWARE: 0028 and 0029 are listed as "active" but have zero code. The board
  is misleading. Meanwhile 10-20KB/turn is wasted on irrelevant tool schemas —
  in a system that has context compaction because context is constrained. "The
  irony is structural."
- DEAD CODE: MCP resubscribeAll is implemented but never called. 8,000 lines of
  MCP client but no reconnection — the one feature that makes it
  production-usable.
- RACE CONDITIONS in the core spawn path, with no -race CI target. For a system
  whose value proposition is concurrent multi-agent execution.
- NO TOOL TIMEOUT — a hanging bash command kills the entire agent. An
  operational showstopper for autonomous operation.
- HITL relay can block forever — the safety mechanism itself can become a
  denial-of-service. "The irony is sharp."
- Complexity is misallocated: 8,000 lines of MCP but no per-tool timeout.
  Sophisticated LLM classifier but config writer destroys data. Blackboard for
  coordination but session logs cannot reconstruct a conversation.
- Session logging is "observability theater." Version stuck at 0.1.0-dev.

**Verdict: B- trending C+ if critical bugs are not addressed. "Architecture
without implementation quality is a sketch, not a system."**

### 3.3 The Pragmatist's Case

Both are right. The right question is not "is fuse good or bad?" but "what
should be done, in what order?"

**Cost/benefit analysis per domain:**

| Domain | Keep? | Value | Fix Cost | Action |
|--------|-------|-------|----------|--------|
| Agent | Yes | High | Moderate | Fix races, add timeout |
| Permissions | Yes | Highest | Low | Fix HITL deadlines |
| MCP | Partial | Medium | High | Remove dead code, fix read-pump |
| TUI | Yes | High | Moderate | Extract handlers, fix races |
| Tools | Yes | High | Moderate | Implement 0028/0029 |
| Pipeline | Yes | Medium | Low | Add timeout, clarify docs |
| Research | Yes | High | Low | Add query cache |
| Config | Partial | High | Low (fix) | Fix writer FIRST |
| Model | Yes | High | Low | Minor cleanup |
| Ratelimit | Yes | High | Low | No urgent action |
| Skills | Yes | Medium | Low | Fix silent errors |
| Cross-cutting | Partial | Mixed | Low | Fix version, backfill ADRs |

---

## 4. Synthesized Verdict

### Where the Advocate Wins

The architecture is genuinely sound. The scheduler, blackboard, permission
system, two-tier compaction, and tool-stripping design are well-conceived and
differentiating. The docket process demonstrates engineering discipline rare in
early-stage projects. The permission system's fail-closed posture, with zero
bypasses found, is the platform's crown jewel. These features are useful,
well-architected, and worth keeping.

### Where the Skeptic Wins

The implementation has real gaps that the advocate underweights. A data-loss bug
in a shipped CLI feature is not "one bug in one file" — it is a quality-control
failure that undermines trust in the config layer. Race conditions in the core
spawn path, with no -race CI, are unacceptable for a concurrency-centric
platform. No per-tool-call timeout is an operational showstopper. Dead code
(resubscribeAll) and vaporware (0028, 0029 listed as "active" with zero code)
indicate that the docket board's status does not always reflect reality. The
skeptic is right that fuse needs to stop adding features and fix what it has.

### Where Both Miss

The advocate treats MCP's 8,000 lines as entirely protocol-mandated. The skeptic
treats it as entirely over-engineering. The truth is in between: ~5,000 lines
are justified, ~3,000 are dead weight (legacy transport, dogfood subscriptions,
contextless OAuth). The right response is to cut the dead weight, not defend or
attack the whole.

### Final Grade: B

A sophisticated, well-architected platform with genuine differentiators, held
back by critical implementation gaps in shipped features, dead code in critical
paths, and unimplemented features mislabeled as active. The architecture
deserves B+; the implementation discipline earns B-. The synthesis is B.

---

## 5. Prioritized Action Plan

### Release 0.2.0 — Critical Fixes (P0, before ANY new feature)

| # | Fix | Location | Effort |
|---|-----|----------|--------|
| 1 | Fix config writer data loss — round-trip ALL config fields | config/writer.go:82-90, 108-112 | Small |
| 2 | Fix spawn slot leak — move slot release defer into goroutine body | agent/spawn.go:272-274 | Small |
| 3 | Fix cancel-func race — store cancel func before addNode | agent/spawn.go:274 vs tree.go:437-450 | Small |
| 4 | Add per-tool-call timeout — configurable, default 120s, with escalation | agent/loop.go:449-493 | Medium |
| 5 | Fix HITL relay deadlines — dial/read deadlines, error on decode failure, context | hitl/hitl.go:71-80, 93-103 | Small |
| 6 | Fix web_search thread safety — sync.Once for provider resolution | tools/web_search.go:101-105 | Trivial |
| 7 | Wire up or remove MCP resubscribeAll — dead code is a liability | mcp/subscriptions.go | Small |
| 8 | Switch version to ldflags injection — fix test to assert non-empty | internal/version | Trivial |

### Release 0.2.1 — High-Value Features and Fixes (P1)

| # | Task | Value |
|---|------|-------|
| 9 | Implement read-file dedup cache (0029) — content-addressed LRU | Eliminates redundant disk I/O |
| 10 | Implement semantic tool relevance (0028) — prune tool schemas by context | 10-20KB/turn context savings |
| 11 | Extract TUI Update handlers from monolithic switch | Highest-value TUI refactor |
| 12 | Fix MCP expand returning prose not command | tui/mcp_provider.go:75 |
| 13 | Add Config.Validate() — check Mode, MaxSpawns, referenced models | Prevents silent misconfiguration |
| 14 | Connect ThroughputConfig to RateGate — dead config surface is misleading | Config/model alignment |
| 15 | Fix TUI test harness data race — add mutex to scriptedCompleter | harness_test.go:48 |
| 16 | Add sudoedit to dangerousNames | Defense-in-depth |

### Release 0.2.2 — Debt Reduction (P2)

| # | Task |
|---|------|
| 17 | Remove/deprecate legacy MCP HTTP/SSE transport |
| 18 | Move MCP notification handlers off read-pump goroutine |
| 19 | Add -race CI target + integration test for concurrent spawn-to-run-to-collect |
| 20 | Backfill ADR for subagent runtime (change 0012) — most significant missing ADR |
| 21 | Reduce config merge boilerplate |
| 22 | Add research query deduplication/caching |
| 23 | Enrich or deprecate session logging in favor of probe |
| 24 | Improve loop detector to catch period-2 patterns |

### Explicitly Defer or Kill

| Item | Decision | Rationale |
|------|----------|-----------|
| MCP WebSocket transport | Defer | No real user demand |
| MCP resource subscriptions | Defer | Dogfood-only, no production users |
| MCP OAuth | Defer | No real OAuth MCP server in use |
| Session replay | Kill or commit | Unimplemented, misleading package doc |
| Pipeline visualization | Defer | Nice-to-have, not essential |

---

## 6. The Debate's Key Insight

The strongest argument in the entire debate is the skeptic's observation that
fuse's complexity is misallocated, not merely excessive.

The platform has:
- 8,000 lines of MCP client but no per-tool-call timeout
- A sophisticated LLM classifier for auto-mode but a config writer that destroys
  user data
- A blackboard for agent-to-agent coordination but session logs that cannot
  reconstruct a conversation
- Two-tier context compaction but 15+ tools advertised every turn (10-20KB
  waste) with the solution (0028) listed as "active" but unimplemented

The resolution is not to argue about whether the complexity is justified, but to
reallocate engineering effort from breadth to depth: fix the critical bugs,
implement the staged features that address real waste, cut the dead code, and
only then resume building. The architecture has earned the right to be completed.

---

This review was produced by 8 parallel domain reviewers feeding a 3-way
antagonistic debate (Advocate, Skeptic, Pragmatist-Architect), synthesized into
this report. All findings cite specific file:line locations in the fuse codebase.

---

## 7. Empirical Verification Addendum (2026-08-08)

Every concrete claim in this report was re-checked against the actual code by 18
independent verifiers, each returning a file:line-cited verdict. Headline result:
**13 of 18 claims CONFIRMED, 4 PARTIALLY_TRUE, 0 REFUTED** — the review held up
well. All eight P0 fixes in §5 were subsequently implemented, each with a
regression test; the full suite passes under `-race` (16/16 packages) and an
independent GLM review confirmed 8/8 resolved.

Four claims were overstated or imprecise and are corrected here so the record is
accurate:

1. **0028 does NOT fix the tool-schema waste (category error).** The report's
   centerpiece "structural irony" — that context compaction coexists with
   10–20KB/turn of unpruned tool schemas, "the solution (0028) listed as active
   but unimplemented" — conflates two different things. Change 0028 prunes tool
   *results* by relevance; it does **not** prune which tool *schemas* are
   advertised per turn. Both facts are true (0028 is unimplemented on `main`, and
   all ~14–18 tools are advertised every turn via `Schemas()` with only
   `spawn_agent` stripped) — but 0028 was never the fix for the schema waste. The
   per-turn schema waste is a **separate, still-unfiled problem.**

2. **0028/0029 status: not "both active."** On the board today, **0028** is
   `in-progress` (spec + plan + branch `feat/semantic-tool-relevance`) and
   **0029** is `deferred` (no branch/spec). The "vaporware listed as active"
   framing is softer than stated; the *code-absent-on-main* claim is accurate for
   both.

3. **MCP dead weight is ~1,600 lines, not ~3,000.** The line/file totals
   (8,033 / 49) are exact, and dogfood-only subscriptions (~1,052) and contextless
   OAuth (~573) are real dead weight. But the legacy HTTP/SSE transport is **not**
   dead — `manager.go` actively routes `http`/`sse` config to it (the newer
   streamable-HTTP client handles only `streamable-http`). Confirmed
   dead/problematic code is ~1,625 lines. (The legacy transport *does* still carry
   the 64KB SSE-scanner bug — a real defect, just not dead code.)

4. **TUI value-receiver Update is a style smell, not a High-severity bug.** The
   pattern is real (`Update` on a value receiver calling pointer-receiver
   mutators), but Go auto-addresses the value and every path returns `m`, so no
   mutations are lost. Inconsistent with `AgentsModel` (pointer receiver) and
   Bubble Tea convention — worth cleaning up, but **downgrade from High to a
   consistency/debt item.**

Everything else in §2–§6 verified as written.
