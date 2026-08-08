# Backlog

**39 changes** — 🟢 1 in progress · 🟡 1 proposed · ⚪ 1 deferred · ✅ 30 done · 🗑️ 6 killed

## 🟢 In progress (1)

| # | Title | Priority | Type | Spec | Branch |
|---|-------|----------|------|------|--------|
| [0028](active/0028-semantic-tool-relevance.md) | Semantic tool-result relevance scoring for smarter pruning | `medium` | `feat` | [spec](../superpowers/specs/2026-08-08-semantic-tool-relevance-design.md) | `feat/semantic-tool-relevance` |

## 🟡 Proposed (1)

| # | Title | Priority | Type | Readiness |
|---|-------|----------|------|-----------|
| [0030](active/0030-segment-store.md) | Segment store — pre-compaction transcript archive for replay | `low` | `feat` | build-ready |

## ⚪ Deferred (1)

| # | Title | Priority | Type |
|---|-------|----------|------|
| [0029](active/0029-read-file-dedup-cache.md) | Read_file content deduplication cache | `medium` | `feat` |

```mermaid
graph TD
  0027 --> 0028
  0012 --> 0029
  0027 --> 0030
  0012:::done
  0027:::done
  classDef done fill:#d3f9d8;
```

<details><summary>✅🗑️ Archive — done + killed (36)</summary>

| # | Title | Merged |
|---|-------|--------|
| [0027](archive/2026-08-08-0027-context-summarization.md) | Anchored context summarization at compression threshold | 2026-08-08 |
| [0026](archive/2026-08-08-0026-agent-workflow-composition.md) | Workflow composition — chain, fan-out, and conditional routing | 2026-08-08 |
| [0025](archive/2026-08-08-0025-agent-to-agent-messaging.md) | Agent-to-agent messaging — note passing for debate/refine patterns | 2026-08-08 |
| [0024](archive/2026-08-08-0024-structured-delegation.md) | Structured delegation — expected result schemas for spawn_agent | 2026-08-08 |
| [0023](archive/2026-08-08-0023-agent-blackboard.md) | Shared result blackboard for inter-agent communication | 2026-08-08 |
| [0022](archive/2026-08-08-0022-mcp-websocket-transport.md) | WebSocket transport for MCP | 2026-08-08 |
| [0021](archive/2026-08-08-0021-mcp-resource-subscriptions.md) | MCP resource subscriptions — push-based updates | 2026-08-08 |
| [0020](archive/2026-08-08-0020-mcp-progress-streaming.md) | MCP `$/progress` notifications and streaming tool results | 2026-08-08 |
| [0018](archive/2026-08-08-0018-mcp-streamable-http.md) | Streamable HTTP transport for MCP (v2025-03-26) | 2026-08-08 |
| [0036](archive/2026-08-07-0036-agent-scheduler.md) | Agent scheduler — global queue, cross-pool fairness, and turn-level throughput limits | 2026-08-07 |
| [0031](archive/2026-08-07-0031-fuse-mcp-error-codes.md) | Adopt MCP-specific JSON-RPC error code range | 2026-08-07 |
| [0019](archive/2026-08-07-0019-mcp-capability-negotiation.md) | MCP capability negotiation — structured init handshake | 2026-08-07 |
| [0039](archive/2026-08-06-0039-agents-tab-idle-timer-and-model.md) | Agents tab — timer runs before the first prompt; shows the default model, not the selected one | 2026-08-06 |
| [0038](archive/2026-08-06-0038-turn-budget-retirement.md) | Retire the interactive turn cap — unlimited shell turns, headless backstop, doom-loop detection | 2026-08-06 |
| [0037](archive/2026-08-06-0037-redirect-guard-lenience.md) | Redirect fail-closed guard — allow /dev/null targets and fd-dups, keep failing closed on real files | 2026-08-06 |
| [0035](archive/2026-08-06-0035-live-mode-switch.md) | Mode switch must bite mid-turn — gates read the SessionMode holder live, not a construction snapshot | 2026-08-06 |
| [0034](archive/2026-08-06-0034-workflows.md) | Workflows — skill-bound subagent pools with typed workers and spawn quotas | 2026-08-06 |
| [0032](archive/2026-08-05-0032-shell-mode-switcher.md) | Shell permission-mode switcher — cycle smart/auto in the TUI, with a visible mode indicator | 2026-08-05 |
| [0016](archive/2026-08-05-0016-one-shot-cli-approvals.md) | Remove the AlwaysApprove one-shot bypass; surface approvals at the CLI | 2026-08-05 |
| [0011](archive/2026-08-05-0011-deep-research.md) | Deep Research Mode — Web Search + Fan-out Synthesis | 2026-08-05 |
| [0009](archive/2026-08-04-0009-mcp-management-cli.md) | `fuse mcps` MCP Server Management CLI + `/mcps` Shell Built-in | 2026-08-04 |

**Older done (collapsed)**

| Month | Done |
|-------|------|
| [2026-08](archive/) | 15 done |

</details>
