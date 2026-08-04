<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0009 — `fuse mcps` MCP Server Management CLI + `/mcps` Shell Built-in](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0009-mcp-management-cli.md)**
<!-- docket:backlink:end -->

# `fuse mcps` — MCP Server Management CLI + `/mcps` Shell Built-in

**Spec for change 0009**

---

## Overview

A `fuse mcps` top-level subcommand (mirroring `fuse models`) for listing, adding, removing, and inspecting MCP servers — plus a `/mcps` slash built-in inside the interactive shell that queries the already-running `Manager`. Read path is live by default in the shell; the CLI defaults to static (config-only) with `--live` for a fresh dial.

---

## New files

| Path | Purpose |
|---|---|
| `cmd/fuse/mcps.go` | `runMCPs` — routes `fuse mcps [subcommand]` |
| `internal/config/writer.go` | `AddMCPServer` / `RemoveMCPServer` — additive YAML writes to `~/.fuse/config.yml` |

## Modified files

| Path | Change |
|---|---|
| `internal/mcp/manager.go` | `Status() []ServerStatus`, per-server stderr ring buffer |
| `internal/tui/shell_model.go` | Accept `statusFunc func() []mcp.ServerStatus`; handle `/mcps` |
| `cmd/fuse/shell.go` | Pass `mgr.Status` to `NewShellModel` after `buildSessionRegistry` |
| `cmd/fuse/main.go` | `case "mcps": return runMCPs(args[1:], cfg, stdout, stderr)` |

---

## `internal/mcp/manager.go` — Status API + log capture

### `ServerStatus`

```go
type ServerStatus struct {
    Name      string
    Transport string      // "stdio" | "http"
    AuthType  string      // "none" | "bearer" | "oauth2"
    Connected bool
    Error     string      // non-empty when Connected == false
    Tools     []string    // tool names registered; nil if not yet connected
    PID       int         // stdio only; 0 if not running
    TokenFile string      // oauth2 only; resolved path
    LogLines  []string    // last N stderr lines (stdio only)
}
```

### `Manager.Status() []ServerStatus`

Returns one `ServerStatus` per configured server, in config order. For connected servers, `Tools` is populated from the registry snapshot taken at startup. For failed servers, `Error` is the startup error string. Thread-safe (reads protected by the existing mutex).

### Stderr ring buffer

Each stdio server's stderr is captured into a per-server `[]string` ring buffer (cap 200 lines, configurable via `FUSE_MCP_LOG_LINES` env). The stdio `cmd.Stderr` is wired to a `lineCapture` writer that appends to the buffer under a mutex, discarding oldest lines when full. No change to stdout (it is the JSON-RPC channel).

---

## `internal/config/writer.go` — Additive YAML writes

Config writes target `~/.fuse/config.yml` only (never the local override). The write strategy is **YAML node surgery**: unmarshal to `*yaml.Node`, locate or create the `mcp_servers` sequence node, append/replace/delete the target entry, re-marshal preserving all other keys and comments.

```go
// AddMCPServer appends or replaces the server with cfg.Name in ~/.fuse/config.yml.
// Creates the file if absent.
func AddMCPServer(cfg MCPServerConfig) error

// RemoveMCPServer removes the server named name from ~/.fuse/config.yml.
// No-op (no error) if the name is not present.
func RemoveMCPServer(name string) error
```

Auth flags for `add`:

| Flag | Effect |
|---|---|
| `--auth none` (default) | `auth.type: none` |
| `--auth bearer --token TOKEN` | `auth.type: bearer, client_secret: TOKEN` |
| `--auth oauth2 --client-id ID --client-secret SECRET [--scopes S1,S2]` | `auth.type: oauth2` with full OAuth2 fields |

---

## `cmd/fuse/mcps.go` — subcommand routing

```
fuse mcps                        → list (static, from config)
fuse mcps list [--live]          → list; --live dials fresh connections
fuse mcps add  --name N --transport stdio --command "CMD ARGS"
               --transport http  --url URL [--auth ...] 
fuse mcps remove NAME
fuse mcps tools [NAME]           → list tools per server (requires --live or running manager)
fuse mcps logs  [NAME]           → last N stderr lines (stdio servers only)
fuse mcps debug [NAME]           → full debug dump per server
```

### `fuse mcps` / `fuse mcps list` (static)

Reads `cfg.MCPServers`. No network activity. Fast.

```
NAME           TRANSPORT  AUTH
filesystem     stdio      none
brave-search   http       bearer
my-remote      http       oauth2
```

### `fuse mcps list --live`

Dials each server fresh (same path as `mcp.NewManager`), calls `tools/list`, then closes. Adds STATUS and TOOLS columns. OAuth2 servers that have a cached token file use it; if the token is expired a refresh is attempted silently; if no token exists the flow is skipped and STATUS shows `no-token`.

```
NAME           TRANSPORT  AUTH    STATUS     TOOLS
filesystem     stdio      none    ok         3
brave-search   http       bearer  ok         1
my-remote      http       oauth2  no-token   -
broken         stdio      none    error      -
```

### `fuse mcps add`

Validates required flags, constructs `config.MCPServerConfig`, calls `config.AddMCPServer`, prints `added filesystem`. Errors on duplicate name (prompt to remove first or use `--force` to overwrite).

### `fuse mcps remove NAME`

Calls `config.RemoveMCPServer`. Errors if name not found. Prints `removed filesystem`.

### `fuse mcps tools [NAME]`

Dials fresh (like `--live`). Prints tools grouped by server. If NAME given, only that server.

```
filesystem (3 tools):
  read_file         Read a file from the filesystem
  write_file        Write contents to a file
  list_directory    List directory contents

brave-search (1 tool):
  web_search        Execute a web search using Brave Search API
```

### `fuse mcps logs [NAME]`

Reads from `Manager.Status().LogLines`. In the CLI path there is no running Manager, so this subcommand requires `--pid PID` to locate a running fuse shell process and signal it — or it is most usefully run from inside the shell as `/mcps logs`. If no running shell is found, prints an advisory.

### `fuse mcps debug [NAME]`

Static + resolved paths; does not dial.

```
filesystem:
  transport:    stdio
  command:      npx @modelcontextprotocol/server-filesystem /tmp
  stderr_cap:   200 lines

my-remote:
  transport:    http
  url:          https://example.com/mcp
  auth:         oauth2
  client_id:    fuse-client
  scopes:       openid profile
  token_file:   /Users/you/.fuse/mcp-tokens/my-remote.json
  token_expiry: (run `fuse mcps list --live` to check)
```

---

## Shell built-in `/mcps`

### Threading the Manager into ShellModel

`NewShellModel` gains a new optional parameter:

```go
func NewShellModel(
    alias string,
    verbose bool,
    reg *model.Registry,
    slash map[string]skills.Skill,
    build AgentBuilder,
    mcpStatus func() []mcp.ServerStatus,  // nil = no MCP configured
) ShellModel
```

`cmd/fuse/shell.go` passes `mgr.Status` after `buildSessionRegistry`:

```go
toolReg, mgr, err := buildSessionRegistry(cfg, set.Lookup)
// ...
m := tui.NewShellModel(alias, verbose, reg, set.SlashCommands(), build, mgr.Status)
```

### `/mcps` behaviour

When `mcpStatus` is nil (no servers configured), prints `no MCP servers configured`.

Otherwise renders the same table as `fuse mcps list --live` but using the **already-connected manager** — no new dials, instant response. Also renders a `LogLines` count and a hint to use `/mcps logs NAME` for the buffer.

```
NAME           TRANSPORT  AUTH    STATUS     TOOLS
filesystem     stdio      none    ok         3
brave-search   http       bearer  ok         1
```

`/mcps logs NAME` — prints the ring buffer for that server inline in the TUI output pane.

`/mcps tools [NAME]` — lists tools inline, same format as CLI.

`/mcps debug [NAME]` — debug dump inline.

`/mcps add` and `/mcps remove` — write config then print a restart advisory (`restart shell to apply`), since the Manager is initialized at shell start.

---

## Output formatting

All tabular output uses `text/tabwriter` (same as `fuse models`). Column headers are uppercase. Missing values render as `-`. Errors render inline in the STATUS column as `error: <msg>` (truncated to 40 chars). No color — plain stdout, consistent with the rest of the CLI.

---

## Out of scope

- Hot-reload of MCP servers in a running shell (restart required after `add`/`remove`)
- Log streaming (ring buffer is read-only snapshot; no tail -f)
- IPC to a running shell's manager from the CLI `logs` subcommand (advisory only)
- Editing individual server fields in-place (remove + add is the workflow)
