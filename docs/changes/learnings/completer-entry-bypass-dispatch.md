---
name: completer-entry-bypass-dispatch
slug: completer-entry-bypass-dispatch
title: Completer-selected entries must bypass the text-parsing dispatch path
hook: "When a completer selects an entry, dispatch via the entry object directly — not via the text-parsing path that re-parses expansion as a command string"
promotion_state: retained
changes: []
created: 2026-08-04
updated: 2026-08-04
topics: [tui, architecture, bubbletea]
---

A slash completer produces a `SlashEntry` object with all payload attached (the skill body, the MCP template, the command name). The temptation is to call `entry.Expansion()` and feed that string back through the same text-parsing path that handles typed input. This breaks for any entry whose expansion is NOT a command string — e.g. a skill entry whose expansion is the full skill body (markdown).

**Rule:** When an entry is selected from a completer, dispatch via the **entry object**, not by re-parsing its expansion text. The text-parsing path (`handleSlash`) is for typed input only. Entry-based dispatch (`handleSlashEntry`) reads the kind and payload directly from the struct:

```go
// Wrong — expansion is the skill body, not a /command string
return m.handleSlash(entry.Expansion())

// Right — dispatch on the entry directly
return m.handleSlashEntry(entry, []string{entry.Command})
```

**How to apply:** In any TUI with a slash completer, the Enter-key handler should branch on `entry.Kind` immediately and call the appropriate entry-level handler. Only `KindBuiltin` entries (whose expansion IS a clean command string like `/exit`) can safely round-trip through the text parser.

## War story

Found during live testing after change 0010. Selecting any skill from the autocomplete overlay printed `unknown command` because `dispatchSlashEntry` passed `entry.Expansion()` (the full SKILL.md body starting with `# docket-adr —`) to `handleSlash`, which parsed the first token `#` as the command.
