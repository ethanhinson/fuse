# Backlog

**60 changes** — 🟢 1 in progress · 🟡 5 proposed · ⚪ 1 deferred · ✅ 47 done · 🗑️ 6 killed

## 🟢 In progress (1)

| # | Title | Priority | Type | Spec | Branch |
|---|-------|----------|------|------|--------|
| [0056](active/0056-sdk-viability-hardening-wander.md) | SDK viability hardening — dogfood @fuse/sdk by building Wander, fix what blocks a real web app | `medium` | `feat` | [spec](../superpowers/specs/2026-08-11-sdk-viability-hardening-wander-design.md) | `feat/sdk-viability-hardening-wander` |

## 🟡 Proposed (5)

| # | Title | Priority | Type | Readiness |
|---|-------|----------|------|-----------|
| [0051](active/0051-loop-observability-otel-metrics.md) | Observability for the loop — OTEL traces + Prometheus metrics + Grafana + Loki, as a projection over the event stream | `medium` | `feat` | needs-brainstorm |
| [0054](active/0054-durable-resumable-sessions.md) | Durable, resumable sessions — a conversation survives disconnect; refresh restores transcript + memory | `medium` | `feat` | needs-brainstorm |
| [0057](active/0057-egress-identity-builtin-http-tools.md) | Egress identity for built-in HTTP tools — route web_fetch/web_search through the #52 credential seam | `medium` | `feat` | needs-brainstorm |
| [0058](active/0058-bash-tool-egress-containment.md) | bash tool egress containment — define the authz posture for a tool that can reach anything | `medium` | `feat` | needs-brainstorm |
| [0060](active/0060-wander-live-rentals-mcp-demo-light-up-59-s-live-data-backend.md) | Wander live rentals MCP demo — light up #59's live data backend + wire the rentals server into the concierge app | `medium` | `feat` | needs-brainstorm |

## ⚪ Deferred (1)

| # | Title | Priority | Type |
|---|-------|----------|------|
| [0029](active/0029-read-file-dedup-cache.md) | Read_file content deduplication cache | `medium` | `feat` |

```mermaid
graph TD
  0012 --> 0029
  0046 --> 0051
  0053 --> 0054
  0050 --> 0056
  0052 --> 0057
  0052 --> 0058
  0060
  0012:::done
  0046:::done
  0050:::done
  0052:::done
  0053:::done
  classDef done fill:#d3f9d8;
```

<details><summary>✅🗑️ Archive — done + killed (53)</summary>

| # | Title | Merged |
|---|-------|--------|
| [0059](archive/2026-08-11-0059-mcp-full-binding-wiring-e2e-tested.md) | Wire MCP into every loop binding and prove it end-to-end — no more features on untestable paths | 2026-08-11 |
| [0055](archive/2026-08-11-0055-grpc-protobuf-transport-idl-defined-loop-wire-successor-to-4.md) | Connect/protobuf transport — IDL-defined loop.* wire, successor to #48 | 2026-08-11 |
| [0053](archive/2026-08-11-0053-persistent-conversational-loop.md) | Persistent conversational loop — interactive mode so one loop_id carries a multi-turn chat | 2026-08-11 |
| [0052](archive/2026-08-11-0052-tool-identity-propagation.md) | Tool/resource identity propagation — per-call RFC 8693 token exchange to downstream MCP/APIs | 2026-08-11 |
| [0050](archive/2026-08-11-0050-client-sdk.md) | Client SDK — Runtime-parity Go + TS/JS libraries, same API local-or-remote | 2026-08-11 |
| [0049](archive/2026-08-11-0049-auth-multi-tenancy.md) | Auth / multi-tenancy — loop_id ownership and per-tenant isolation for the deployed service | 2026-08-11 |
| [0048](archive/2026-08-11-0048-networked-runtime-binding.md) | Networked binding over the Runtime seam — WS live observe + HTTP start/send/replay | 2026-08-11 |
| [0047](archive/2026-08-11-0047-durable-distributed-event-store.md) | Durable / distributed event store — survives restart and is shared across instances | 2026-08-11 |
| [0046](archive/2026-08-10-0046-multi-loop-host-deglobalize-event-store.md) | De-globalize the event store + multi-loop host — one process hosts N loops keyed by loop_id | 2026-08-10 |
| [0045](archive/2026-08-10-0045-runtime-interface-and-binding.md) | Runtime interface + second binding — prove the platform boundary is emergent | 2026-08-10 |
| [0044](archive/2026-08-10-0044-spawn-handle-async.md) | Spawn handle-async — location-transparent spawning behind a handle-returning contract | 2026-08-10 |
| [0043](archive/2026-08-10-0043-runtime-eventstore-seam.md) | Runtime EventStore seam — typed, pluggable, introspectable loop event stream | 2026-08-10 |
| [0042](archive/2026-08-10-0042-return-result-structured-delegation.md) | Fix structured-delegation (expects) vs tool-calling collision via a return_result tool | 2026-08-10 |
| [0041](archive/2026-08-09-0041-agents-panel-ux.md) | Agents split-panel UX — focus indicator, reliable scrolling, blackboard readability | 2026-08-09 |
| [0040](archive/2026-08-09-0040-auto-mode-flow-parity.md) | Auto-mode flow parity — in-workspace edits auto-approve | 2026-08-09 |
| [0025](archive/2026-08-08-0025-agent-to-agent-messaging.md) | Agent-to-agent messaging — note passing for debate/refine patterns | 2026-08-08 |
| [0022](archive/2026-08-08-0022-mcp-websocket-transport.md) | WebSocket transport for MCP | 2026-08-08 |
| [0032](archive/2026-08-05-0032-shell-mode-switcher.md) | Shell permission-mode switcher — cycle smart/auto in the TUI, with a visible mode indicator | 2026-08-05 |
| [0016](archive/2026-08-05-0016-one-shot-cli-approvals.md) | Remove the AlwaysApprove one-shot bypass; surface approvals at the CLI | 2026-08-05 |
| [0011](archive/2026-08-05-0011-deep-research.md) | Deep Research Mode — Web Search + Fan-out Synthesis | 2026-08-05 |
| [0009](archive/2026-08-04-0009-mcp-management-cli.md) | `fuse mcps` MCP Server Management CLI + `/mcps` Shell Built-in | 2026-08-04 |

**Older done (collapsed)**

| Month | Done |
|-------|------|
| [2026-08](archive/) | 32 done |

</details>
