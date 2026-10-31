# Backlog

**93 changes** — 🟢 2 in progress · 🟣 1 groomed · 🟡 2 proposed · ⚪ 3 deferred · ✅ 79 done · 🗑️ 6 killed

## 🟢 In progress (2)

| # | Title | Priority | Type | Spec | Branch | State |
|---|-------|----------|------|------|--------|-------|
| [0091](active/0091-workflows-activate-per-spawn-in-every-host-enforcing-pools-a.md) | Workflows activate per spawn in every host, enforcing pools and worker tools | `medium` | `fix` | [spec](../superpowers/specs/2026-10-31-workflows-activate-per-spawn-in-every-host-enforcing-pools-a-design.md) | `fix/workflows-activate-per-spawn-in-every-host-enforcing-pools-a` | building |
| [0081](active/0081-shell-transcript-command-breaks.md) | Echo executed slash commands + rule breaks between transcript blocks | `medium` | `feat` | [spec](../) | `feat/shell-transcript-command-breaks` | building |

## 🟣 Groomed (1)

| # | Title | Priority | Type | Spec | Readiness |
|---|-------|----------|------|------|-----------|
| [0092](active/0092-smart-mode-honors-always-prompt-with-no-human-approver-bound.md) | Smart mode honors always_prompt with no human approver bound | `medium` | `fix` | [spec](../superpowers/specs/2026-10-31-smart-mode-honors-always-prompt-with-no-human-approver-bound-design.md) | build-ready |

## 🟡 Proposed (2)

| # | Title | Priority | Type | Readiness |
|---|-------|----------|------|-----------|
| [0093](active/0093-permission-floor-and-wrapper-parity-across-modes-and-binding.md) | Permission floor and wrapper parity across modes and bindings | `medium` | `fix` | ⏳ waiting on #92 — not yet built |
| [0083](active/0083-fix-two-race-lane-flakes-in-internal-runtime-turn-span-topol.md) | Fix two race-lane flakes in internal/runtime — turn-span topology and Send-enqueue tests | `medium` | `fix` | needs-grooming |

## ⚪ Deferred (3)

| # | Title | Priority | Type |
|---|-------|----------|------|
| [0074](active/0074-sandbox-health-emitter.md) | sandbox health emitter — feed KindSandboxHealth so fuse_sandbox_unhealthy_total stops being defined-but-unfed | `medium` | `feat` |
| [0057](active/0057-egress-identity-builtin-http-tools.md) | Egress identity for built-in HTTP tools — route web_fetch/web_search through the #52 credential seam | `medium` | `feat` |
| [0029](active/0029-read-file-dedup-cache.md) | Read_file content deduplication cache | `medium` | `feat` |

```mermaid
graph TD
  0012 --> 0029
  0052 --> 0057
  0063 --> 0074
  0081
  0083
  0090 --> 0091
  0087 --> 0092
  0088 --> 0092
  0092 --> 0093
  0012:::done
  0052:::done
  0063:::done
  0087:::done
  0088:::done
  0090:::done
  classDef done fill:#d3f9d8;
```

<details><summary>✅🗑️ Archive — done + killed (85)</summary>

| # | Title | Closed |
|---|-------|--------|
| [0090](archive/2026-10-31-0090-turn-and-token-budgets-are-enforced-in-hosted-interactive-lo.md) | Turn and token budgets are enforced in hosted interactive loops | 2026-10-31 |
| [0089](archive/2026-10-31-0089-static-credential-mcp-servers-authenticate-and-every-loop-re.md) | Static-credential MCP servers authenticate, and every loop reports its MCP servers | 2026-10-31 |
| [0088](archive/2026-10-31-0088-protective-permission-patterns-match-across-slashes-and-in-e.md) | Protective permission patterns match across slashes and in every mode alike | 2026-10-31 |
| [0087](archive/2026-10-31-0087-explicit-ask-rules-are-honored-prompt-a-human-deny-when-no-h.md) | Explicit ask rules are honored: prompt a human, deny when no human is bound | 2026-10-31 |
| [0086](archive/2026-10-31-0086-loop-server-advertises-its-capabilities-models-skills-ceilin.md) | Loop server advertises its capabilities - models, skills, ceilings | 2026-10-31 |
| [0085](archive/2026-10-31-0085-start-a-loop-from-a-harnessspec.md) | Start a loop from a HarnessSpec | 2026-10-31 |
| [0084](archive/2026-10-30-0084-harnessspec-proto-and-public-validator.md) | HarnessSpec proto and public validator | 2026-10-30 |
| [0076](archive/2026-09-17-0076-fuse-server-helm-chart-compose-stack.md) | fuse server deployment — a docker-compose stack and a Helm chart over the released image (not an operator) | 2026-09-17 |
| [0075](archive/2026-09-17-0075-paas-remote-sandbox-substrate-adr.md) | PaaS/remote sandbox substrate — the provision/attach/teardown seam ADR, with a Kubernetes warm-Pod handler as first implementation | 2026-09-17 |
| [0082](archive/2026-09-13-0082-build-release-pipeline.md) | build/release pipeline — tag-driven GoReleaser releases (multi-platform archives + GHCR image + curl installer) so users adopt fuse without a Go toolchain | 2026-09-13 |
| [0065](archive/2026-09-05-0065-bash-per-tenant-filesystem-isolation.md) | bash per-tenant filesystem isolation — a Principal.Tenant-scoped bind-mount the working_dir cannot escape | 2026-09-05 |
| [0080](archive/2026-09-02-0080-tui-table-tabbed-ui.md) | Shared TUI table component + tabbed /config UI — line up the menus like Claude | 2026-09-02 |
| [0079](archive/2026-09-01-0079-tui-models-management-ui.md) | TUI models management UI — argument autocomplete + interactive mapping editor | 2026-09-01 |
| [0064](archive/2026-09-01-0064-bash-egress-control-container-network-config.md) | bash egress control — egress as the container's network configuration, not an in-process dialer allowlist | 2026-09-01 |
| [0078](archive/2026-08-21-0078-slash-command-models-listing.md) | /models slash command + slash-menu column alignment | 2026-08-21 |
| [0025](archive/2026-08-08-0025-agent-to-agent-messaging.md) | Agent-to-agent messaging — note passing for debate/refine patterns | 2026-08-08 |
| [0022](archive/2026-08-08-0022-mcp-websocket-transport.md) | WebSocket transport for MCP | 2026-08-08 |
| [0032](archive/2026-08-05-0032-shell-mode-switcher.md) | Shell permission-mode switcher — cycle smart/auto in the TUI, with a visible mode indicator | 2026-08-05 |
| [0016](archive/2026-08-05-0016-one-shot-cli-approvals.md) | Remove the AlwaysApprove one-shot bypass; surface approvals at the CLI | 2026-08-05 |
| [0011](archive/2026-08-05-0011-deep-research.md) | Deep Research Mode — Web Search + Fan-out Synthesis | 2026-08-05 |
| [0009](archive/2026-08-04-0009-mcp-management-cli.md) | `fuse mcps` MCP Server Management CLI + `/mcps` Shell Built-in | 2026-08-04 |

**Older done (collapsed)**

| Month | Done |
|-------|------|
| [2026-08](archive/) | 64 done |

</details>
