# Backlog

**39 changes** — 🟡 17 proposed · ✅ 18 done · 🗑️ 4 killed

## 🟡 Proposed (17)

| # | Title | Priority | Type | Readiness |
|---|-------|----------|------|-----------|
| [0018](active/0018-mcp-streamable-http.md) | Streamable HTTP transport for MCP (v2025-03-26) | `high` | `feat` | needs-brainstorm |
| [0019](active/0019-mcp-capability-negotiation.md) | MCP capability negotiation — structured init handshake | `high` | `feat` | needs-brainstorm |
| [0020](active/0020-mcp-progress-streaming.md) | MCP `$/progress` notifications and streaming tool results | `medium` | `feat` | ⏳ waiting on #19 — not yet built |
| [0021](active/0021-mcp-resource-subscriptions.md) | MCP resource subscriptions — push-based updates | `medium` | `feat` | ⏳ waiting on #19 — not yet built |
| [0022](active/0022-mcp-websocket-transport.md) | WebSocket transport for MCP | `low` | `feat` | needs-brainstorm |
| [0023](active/0023-agent-blackboard.md) | Shared result blackboard for inter-agent communication | `high` | `feat` | needs-brainstorm |
| [0024](active/0024-structured-delegation.md) | Structured delegation — expected result schemas for spawn_agent | `medium` | `feat` | needs-brainstorm |
| [0025](active/0025-agent-to-agent-messaging.md) | Agent-to-agent messaging — note passing for debate/refine patterns | `medium` | `feat` | ⏳ waiting on #23 — not yet built |
| [0026](active/0026-agent-workflow-composition.md) | Workflow composition — chain, fan-out, and conditional routing | `medium` | `feat` | ⏳ waiting on #23 — not yet built |
| [0027](active/0027-context-summarization.md) | Anchored context summarization at compression threshold | `high` | `feat` | needs-brainstorm |
| [0028](active/0028-semantic-tool-relevance.md) | Semantic tool-result relevance scoring for smarter pruning | `medium` | `feat` | ⏳ waiting on #27 — not yet built |
| [0029](active/0029-read-file-dedup-cache.md) | Read_file content deduplication cache | `medium` | `feat` | needs-brainstorm |
| [0030](active/0030-segment-store.md) | Segment store — pre-compaction transcript archive for replay | `low` | `feat` | ⏳ waiting on #27 — not yet built |
| [0031](active/0031-fuse-mcp-error-codes.md) | Adopt MCP-specific JSON-RPC error code range | `low` | `chore` | build-ready |
| [0033](active/0033-spawn-tool-stripping.md) | Strip spawn_agent from tool schemas at the concurrency cap and on budget exhaustion | `high` | `feat` | build-ready |
| [0034](active/0034-workflows.md) | Workflows — skill-bound subagent pools with typed workers and spawn quotas | `high` | `feat` | ⏳ waiting on #33 — not yet built |
| [0036](active/0036-agent-scheduler.md) | Agent scheduler — global queue, cross-pool fairness, and turn-level throughput limits | `high` | `feat` | ⏳ waiting on #34 — not yet built |

```mermaid
graph TD
  0007 --> 0018
  0003 --> 0019
  0007 --> 0019
  0019 --> 0020
  0019 --> 0021
  0007 --> 0022
  0012 --> 0023
  0012 --> 0024
  0012 --> 0025
  0023 --> 0025
  0012 --> 0026
  0023 --> 0026
  0024 --> 0026
  0012 --> 0027
  0027 --> 0028
  0012 --> 0029
  0027 --> 0030
  0003 --> 0031
  0033
  0033 --> 0034
  0034 --> 0036
  0003:::done
  0007:::done
  0012:::done
  classDef done fill:#d3f9d8;
```

<details><summary>✅🗑️ Archive — done + killed (22)</summary>

| # | Title | Merged |
|---|-------|--------|
| [0039](archive/2026-08-06-0039-agents-tab-idle-timer-and-model.md) | Agents tab — timer runs before the first prompt; shows the default model, not the selected one | 2026-08-06 |
| [0038](archive/2026-08-06-0038-turn-budget-retirement.md) | Retire the interactive turn cap — unlimited shell turns, headless backstop, doom-loop detection | 2026-08-06 |
| [0037](archive/2026-08-06-0037-redirect-guard-lenience.md) | Redirect fail-closed guard — allow /dev/null targets and fd-dups, keep failing closed on real files | 2026-08-06 |
| [0035](archive/2026-08-06-0035-live-mode-switch.md) | Mode switch must bite mid-turn — gates read the SessionMode holder live, not a construction snapshot | 2026-08-06 |
| [0017](archive/2026-08-06-0017-auto-mode.md) | Auto mode — layered safe/unsafe classification for autonomous tool approval | 2026-08-06 |
| [0032](archive/2026-08-05-0032-shell-mode-switcher.md) | Shell permission-mode switcher — cycle smart/auto in the TUI, with a visible mode indicator | 2026-08-05 |
| [0016](archive/2026-08-05-0016-one-shot-cli-approvals.md) | Remove the AlwaysApprove one-shot bypass; surface approvals at the CLI | 2026-08-05 |
| [0015](archive/2026-08-05-0015-tui-hanging-indent-wrap.md) | Hanging-indent wrapping for the shell transcript | 2026-08-05 |
| [0014](archive/2026-08-05-0014-research-mode.md) | Research Mode — Web Search, Fetch & Cited Synthesis on the Subagent Runtime | 2026-08-05 |
| [0013](archive/2026-08-05-0013-startup-banner.md) | ASCII art startup banner — shell init & fuse help | 2026-08-05 |
| [0012](archive/2026-08-05-0012-subagent-ux.md) | First-Class Subagent UX — Spawn, Tree Visualization & Inspect | 2026-08-05 |
| [0011](archive/2026-08-05-0011-deep-research.md) | Deep Research Mode — Web Search + Fan-out Synthesis | 2026-08-05 |
| [0010](archive/2026-08-04-0010-shell-slash-command-ui.md) | Shell Slash-Command Autocomplete + MCP & Skill Invocation | 2026-08-04 |
| [0009](archive/2026-08-04-0009-mcp-management-cli.md) | `fuse mcps` MCP Server Management CLI + `/mcps` Shell Built-in | 2026-08-04 |
| [0008](archive/2026-08-04-0008-mcp-integration-test-harness.md) | MCP Integration Test Harness (Docker Compose + Playwright) | 2026-08-04 |
| [0007](archive/2026-08-04-0007-mcp-http-oauth.md) | Remote MCP Servers via HTTP/SSE Transport + OAuth 2.0 | 2026-08-04 |
| [0006](archive/2026-08-04-0006-tui-markdown-rendering.md) | Terminal Markdown Rendering | 2026-08-04 |
| [0005](archive/2026-08-04-0005-tui-gutter-indent-fix.md) | Fix file-read gutter indentation in TUI | 2026-08-04 |
| [0004](archive/2026-08-04-0004-skill-runtime.md) | Skill Runtime | 2026-08-04 |

**Older done (collapsed)**

| Month | Done |
|-------|------|
| [2026-08](archive/) | 3 done |

</details>
