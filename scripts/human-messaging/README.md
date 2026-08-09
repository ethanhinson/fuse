# Human-messaging feature — reviewer scripts

These scripts reproduce the four human-messaging features (ADR-0022) so a reviewer
can see them working without reading the whole diff.

Features:
1. **respond-to-agent** — reply in prose to the agent that's asking (`ask_user`'s
   "Chat about this" → `Answer.Chat`).
2. **@agent-direct** — address a live node by auto-derived `@handle` (rename with
   `/rename @old @new`).
3. **/btw** — a read-only status aside answered by the harness from live tree/
   blackboard state (no model call, no agent interruption).
4. **queued + editable messages** — messages typed while an agent is busy queue
   and deliver at the next turn boundary; `/queue` opens an editor to
   reorder/edit/delete them; the async LLM router may re-target bare prose.

## 1. Run the automated tests (fastest, deterministic)

```
./scripts/human-messaging/test.sh
```

Runs every unit + integration test that backs the four features and prints a
pass/fail summary. No live model needed.

## 2. See the rendered UI (screenshots)

```
./scripts/human-messaging/screenshots.sh
```

Regenerates the PNG/txt frames for the route transcript, the `/queue` editor, and
`/btw` asides into `reports/screenshots/`. Requires `freeze` on PATH
(`go install github.com/charmbracelet/freeze@latest`); falls back to `.txt`.

## 3. Drive the real binary (needs a configured gateway/model)

```
./scripts/human-messaging/live-demo.sh
```

Boots `fuse shell` in tmux, exercises `/btw` (works offline), spawns a subagent to
show `@handle` assignment and `/queue`, and captures the pane. `/btw` works with no
model; the agent-driven parts need a working `gateway` in your fuse config.

## Key bindings / syntax reference

| input | effect |
|---|---|
| `@coder <msg>` | queue a direct message to node `@coder` (delivered next turn) |
| `@all <msg>` | broadcast to every live node |
| `/btw <question>` | read-only status answer from the harness (status/last-tool/writes/count/tree) |
| `/rename @old @new` | rename a node's handle (node ID is stable) |
| `/queue` | open the pending-message editor |
| bare prose while busy | queued to the selected/root node; async router may re-target |

In `/queue`: `j/k` move · `e` edit · `d` delete · `J/K` reorder · `Esc` close.
In an `ask_user` prompt: pick "Chat about this" to send a prose steer to the agent.
