<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0001 — Fuse — Multi-Model Agent Harness](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0001-fuse.md)**
<!-- docket:backlink:end -->

# Fuse — Multi-Model Agent Harness (Phase 1) — results

Change: #0001 · Branch: feat/fuse · PR: <set on open> · Plan: docs/superpowers/plans/2026-08-04-fuse-phase1.md · ADRs: none

Phase 1 delivers a working, model-agnostic Go agent harness end-to-end: config + gateway adapter + model registry, a shared tool registry (bash, read_file, write_file, edit_file, list_directory) plus codeindex_impact / codeindex_callers, the core agent loop with fingerprint loop detection, a SKILL.md skills loader, and the `fuse --model X "task"` / `fuse models` / `fuse shell` CLI. Built task-by-task with TDD; whole-repo `go test ./...` is green across all 8 packages, and `go build`/`go vet` are clean. The whole-branch review returned APPROVE FOR MERGE with no Critical or Important findings.

## Verify (human)

Automated tests never touch the real gateway or a real `codeindex` binary (all stubbed). Before/at merge, smoke-test against the live stack:

- [ ] Ensure `LLM_GATEWAY_URL` / `LLM_GATEWAY_KEY` are exported in the shell (they live in `~/.zshrc` but are absent from non-interactive shells; the binary falls back to the built-in defaults `http://localhost:4000/v1` + `llm-gateway-local` when unset).
- [ ] One-shot run: `fuse --model deepseek-flash "write a one-line Go function that returns 42"` — confirm a real turn round-trips through the LiteLLM gateway and renders.
- [ ] `fuse models` lists all 9 configured models.
- [ ] `fuse shell` starts, accepts a prompt, accumulates conversation across turns, and `/exit` (or EOF) quits cleanly; `/model <name>` swaps the active model; `/verbose` toggles.
- [ ] A `codeindex_impact` / `codeindex_callers` tool call succeeds against the real `codeindex` binary on PATH (`/opt/homebrew/bin/codeindex`) in a repo with an index.

## Findings

- No ADRs: the load-bearing architecture decisions (Go + single binary; gateway-only model transport; no privileged Claude orchestrator; byte-compatible SKILL.md) were settled at brainstorm time and already live in the spec — nothing new emerged during the build that warranted a fresh ADR.
- `edit_file` was verified to enforce BOTH guards (fail on absent old_string AND on non-unique old_string) — reviewed explicitly given its edit-corruption risk.
- Agent loop invariants verified: loop-detection aborts at exactly the 3rd identical consecutive tool-call set (counter resets on change), `context.Context` is threaded into both the model call and every tool execution, and MaxTurns is a correct 0-indexed boundary.

## Follow-ups

Cosmetic minors surfaced by review, all triaged safe-to-defer (none block merge) — a human may fold these into a follow-up chore change if desired:

- `internal/tui/renderer.go` — `ModelHeader` is now orphaned (only its own test references it); `cmd/fuse/shell.go` inlines the same rule format string. Dead method + duplicated literal left by the Task 9 fix. Consider removing `ModelHeader` or routing the shell header through it.
- `internal/config` — `Config.SkillPaths` is parsed from `skill_paths` YAML but never consumed; `fuse shell` uses the hardcoded `skills.DefaultDirs()`. Wiring `SkillPaths` into the skills load is a natural small enhancement (Phase 1 plan did not require it).
- `internal/skills` — `Set.All()` and `Skill.Path` are exported but read only in tests.
- `go.mod` — `gopkg.in/yaml.v3` is still marked `// indirect` despite being directly imported; `go mod tidy` removes the comment.

Deferred Phase-2 surface (out of Phase 1 scope, natural successor changes a human may want to file): hooks, `spawn_subagent`/subagents, session persistence/resume/compression, `web_search`/Tavily, the built-in skills (`/route`, `/compare`, `/docket`, `/impact`, `/explore`, `/summary`), the `sdk/go` module, task-based routing modes, `codeindex_search`, and the full bubbletea two-pane mouse TUI.
