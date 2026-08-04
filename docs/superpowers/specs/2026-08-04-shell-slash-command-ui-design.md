<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0010 — Shell Slash-Command Autocomplete + MCP & Skill Invocation](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0010-shell-slash-command-ui.md)**
<!-- docket:backlink:end -->

# Shell Slash-Command Autocomplete + MCP & Skill Invocation

**Spec for change 0010** — absorbs change 0009 (`fuse mcps` CLI + `/mcps` shell built-in)

---

## Overview

The fuse shell's current `/`-dispatch is a bare `switch` with no discovery surface. Users must memorize command names; MCP tools are invisible in the shell. This change adds:

1. A **filterable slash-command autocomplete overlay** in the TUI — `/` triggers a list of all commands with kind tags and descriptions; typing narrows the list; arrow keys and Enter select.
2. A **unified `SlashRegistry`** that aggregates built-ins, skills, and MCP tools from the running manager.
3. **MCP tools as natural-language prompt expansions** — selecting a tool injects a prompt template that the agent resolves through the existing tool executor (no new execution path).
4. A **`fuse mcps` top-level CLI** (absorbed from change 0009) for MCP server management and inspection.

---

## New files

| Path | Purpose |
|---|---|
| `internal/tui/slash_registry.go` | `SlashEntry`, `SlashRegistry`, `NewSlashRegistry` |
| `internal/tui/slash_completer.go` | `slashCompleter` bubbletea sub-model (overlay, filter, cursor) |
| `cmd/fuse/mcps.go` | `runMCPs` — routes `fuse mcps [subcommand]` |
| `internal/config/writer.go` | `AddMCPServer` / `RemoveMCPServer` — YAML node surgery on `~/.fuse/config.yml` |

## Modified files

| Path | Change |
|---|---|
| `internal/mcp/manager.go` | Add `ServerStatus`, `Manager.Servers() []ServerInfo`, `Manager.Status() []ServerStatus`, stderr ring buffer |
| `internal/tui/shell_model.go` | Replace `slash map[string]skills.Skill` with `*SlashRegistry`; wire `slashCompleter` into Update/View |
| `cmd/fuse/shell.go` | Build `SlashRegistry` from skills + MCP manager; pass to `NewShellModel` |
| `cmd/fuse/main.go` | `case "mcps": return runMCPs(args[1:], cfg, stdout, stderr)` |

---

## 1. `internal/tui/slash_registry.go` — the unified command registry

```go
// SlashKind tags the origin of a slash command for display.
type SlashKind string

const (
    KindBuiltin SlashKind = "builtin"
    KindSkill   SlashKind = "skill"
    KindMCP     SlashKind = "mcp"
)

// SlashEntry is one item in the autocomplete list.
type SlashEntry struct {
    // Command is the canonical slash name, e.g. "/model", "/code-review",
    // "/mcp:everything/echo". For built-ins with arguments the name omits the
    // argument placeholder ("/model", not "/model NAME").
    Command     string
    // Syntax is the display hint shown in the list beside Command,
    // e.g. "NAME" for /model, empty for most others.
    Syntax      string
    Description string
    Kind        SlashKind
    // Server is populated for KindMCP entries.
    Server      string
    // expand returns the string to inject into the shell input when this
    // entry is selected. For built-ins and skills this is Command itself
    // (possibly with a trailing space); for MCP tools it is a natural-language
    // prompt template.
    expand      func() string
}

// Expansion returns the text to inject into the shell input on selection.
func (e SlashEntry) Expansion() string { return e.expand() }

// SlashRegistry aggregates all slash-command entries from all sources.
type SlashRegistry struct {
    entries []SlashEntry
}

// NewSlashRegistry builds the registry from the three sources.
// mcpServers is a slice of (serverName, toolName, toolDescription) triples
// read from the manager after startup.
func NewSlashRegistry(
    skills map[string]skills.Skill,
    mcpTools []MCPToolInfo,
) *SlashRegistry
```

### Built-in entries (always registered, order preserved)

| Command | Syntax | Description |
|---|---|---|
| `/exit` | | Exit the shell |
| `/quit` | | Exit the shell |
| `/verbose` | | Toggle verbose tool output |
| `/model` | `NAME` | Switch model (e.g. sonnet, opus) |

### Skill entries

One entry per skill in the `slash` map. `Description` comes from `Skill.Description`; `Command` from `Skill.SlashCommand`; `Kind = KindSkill`; `expand` returns the slash command name (the existing handler injects `sk.Body` at dispatch time).

### MCP tool entries

```go
// MCPToolInfo is the minimal tool descriptor the slash registry needs.
// Built by cmd/fuse/shell.go from mgr.Servers() at shell startup.
type MCPToolInfo struct {
    Server      string // e.g. "everything"
    Tool        string // e.g. "echo"
    Description string
}
```

`Command` = `"/mcp:" + server + "/" + tool` (matches `MCPTool.Name()`'s existing format).
`Kind = KindMCP`. `expand` returns:

```
Use the [tool] tool from the [server] MCP server. [description] Arguments: 
```

(Trailing space so the user can continue typing arguments immediately.)

### Filtering

```go
// Filter returns entries whose Command contains the filter string (case-insensitive).
// An empty filter returns all entries.
func (r *SlashRegistry) Filter(f string) []SlashEntry
```

---

## 2. `internal/tui/slash_completer.go` — the overlay sub-model

`slashCompleter` is a plain struct (not a `tea.Model` itself — it is driven by `ShellModel.Update`). It holds:

```go
type slashCompleter struct {
    reg     *SlashRegistry
    filter  string      // everything after the leading "/"
    visible []SlashEntry
    cursor  int
    active  bool
}
```

### Activation rules (in `ShellModel.Update`)

- `active` becomes `true` when the input value starts with exactly `"/"` (one character) and the cursor is at position 0 or 1.
- `active` becomes `false` on `Esc`, on Enter (after selection), or when the input no longer starts with `"/"`.
- While `active`, every `KeyMsg` that would normally update the text input instead:
  - `Up` / `Down`: move `cursor`, clamped to `[0, len(visible)-1]`.
  - `Enter`: call `selected()` (see below), dismiss, and submit the expansion as the shell prompt.
  - `Esc`: dismiss, clear the input.
  - Any other printable key: pass through to the text input AND re-filter (`filter = input[1:]`).

### `selected()` — expansion

On Enter:
1. If the entry `Kind == KindBuiltin` or `KindSkill`: inject `entry.Expansion()` into the input, close the completer, and let the existing `handleSlash` logic handle dispatch on submit.
2. If `Kind == KindMCP`: inject `entry.Expansion()` (the natural-language template) into the input, position the cursor after "Arguments: ", close the completer. The user completes the sentence and submits — the agent dispatches via the existing tool executor.

### Rendering (`View` fragment)

Rendered below the text input field using `lipgloss`. Maximum visible rows: 8 (scroll if more). Each row:

```
  ▸ /echo          [mcp:everything]   Echoes back any string
    /get-sum        [mcp:everything]   Returns the sum of two numbers
    /code-review    [skill]            Review code for correctness
    /model          [builtin]  NAME    Switch model
```

- Selected row highlighted with the shell's accent color.
- `[kind:server]` or `[kind]` tag right-aligned within a fixed-width column.
- Description truncated to terminal width minus fixed columns.
- Rows beyond 8 are hidden; "↓ N more" appears at the bottom when scrolled.

---

## 3. `internal/mcp/manager.go` — Status API + log capture (absorbed from 0009)

### `ServerStatus` and `Manager.Status()`

```go
type ServerStatus struct {
    Name      string
    Transport string   // "stdio" | "http"
    AuthType  string   // "none" | "bearer" | "oauth2"
    Connected bool
    Error     string   // non-empty when Connected == false
    Tools     []string // tool names registered; nil if not yet connected
    PID       int      // stdio only; 0 if not running
    TokenFile string   // oauth2 only; resolved path
    LogLines  []string // last N stderr lines (stdio only)
}

func (m *Manager) Status() []ServerStatus
```

### `Manager.Servers() []ServerInfo`

Used by `cmd/fuse/shell.go` to populate the slash registry at startup:

```go
type ServerInfo struct {
    Name  string
    Tools []MCPToolInfo
}

func (m *Manager) Servers() []ServerInfo
```

Built during `startAndDiscover`: store the `(serverName, toolName, description)` triples alongside the registered `*MCPTool` list.

### Stderr ring buffer

Each stdio server's stderr is captured into a per-server `[]string` ring buffer (cap 200 lines, configurable via `FUSE_MCP_LOG_LINES` env). `cmd.Stderr` is wired to a `lineCapture` writer that appends under a mutex, discarding oldest lines when full.

---

## 4. `internal/config/writer.go` — Additive YAML writes (absorbed from 0009)

Config writes target `~/.fuse/config.yml` only (never the local override). Strategy: **YAML node surgery** — unmarshal to `*yaml.Node`, locate or create the `mcp_servers` sequence node, append/replace/delete the target entry, re-marshal preserving all other keys and comments.

```go
// AddMCPServer appends or replaces the server with cfg.Name in ~/.fuse/config.yml.
// Creates the file if absent. Changes take effect on next fuse startup.
func AddMCPServer(cfg config.MCPServerConfig) error

// RemoveMCPServer removes the server named name from ~/.fuse/config.yml.
// No-op (no error) if the name is not present.
func RemoveMCPServer(name string) error
```

---

## 5. `cmd/fuse/mcps.go` — subcommand routing (absorbed from 0009)

```
fuse mcps                        → list (static, from config)
fuse mcps list [--live]          → static list; --live dials fresh connections
fuse mcps add  --name N --transport stdio --command "CMD ARGS"
               --transport http  --url URL [--auth none|bearer|oauth2] [auth flags]
fuse mcps remove NAME
fuse mcps tools [NAME]           → list tools per server (requires live manager or --live)
fuse mcps logs  [NAME]           → last N stderr lines (stdio servers only)
```

Auth flags for `add`: `--token TOKEN` (bearer), `--client-id ID --client-secret SECRET [--scopes S1,S2]` (oauth2).

### `fuse mcps` / `fuse mcps list` (static output)

```
NAME           TRANSPORT  AUTH
filesystem     stdio      none
brave-search   http       bearer
```

### `fuse mcps list --live`

Dials each server, calls `tools/list`, closes. Adds STATUS and TOOLS columns. OAuth2 with no cached token shows `no-token`; failed servers show `error` with one-line reason.

```
NAME           TRANSPORT  AUTH    STATUS    TOOLS
filesystem     stdio      none    ok        3
brave-search   http       bearer  ok        1
broken         stdio      none    error     -
```

---

## 6. `cmd/fuse/shell.go` — wiring

```go
// After buildSessionRegistry returns mgr:
infos := mgr.Servers()
var mcpTools []tui.MCPToolInfo
for _, srv := range infos {
    mcpTools = append(mcpTools, srv.Tools...)
}
reg := tui.NewSlashRegistry(set.SlashCommands(), mcpTools)
m := tui.NewShellModel(alias, verbose, glamourStyle, modelReg, reg, build)
```

`NewShellModel` signature changes: `slash map[string]skills.Skill` → `reg *SlashRegistry`.

---

## Open questions

- **Empty manager**: when no MCP servers are configured, `mgr.Servers()` returns nil — the registry contains only built-ins and skills. No special handling needed.
- **Failed servers at startup**: already skipped by `NewManager`'s existing warning-and-continue logic. Their tools simply do not appear in the registry.
- **`/mcps` shell built-in**: dropped. The autocomplete list surfaces MCP tool availability; `fuse mcps` (CLI) handles management. No in-shell management command needed.
- **MCP tools with argument schemas**: the expansion template is always natural-language ("Arguments: "); structured arg forms are out of scope.
