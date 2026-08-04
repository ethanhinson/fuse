<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0010 — Shell Slash-Command Autocomplete + MCP & Skill Invocation](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0010-shell-slash-command-ui.md)**
<!-- docket:backlink:end -->

# Shell Slash-Command Autocomplete + MCP & Skill Invocation

**Spec for change 0010** — absorbs change 0009 (`fuse mcps` CLI + `/mcps` shell built-in)

---

## Overview

The fuse shell's current `/`-dispatch is a bare `switch` with no discovery surface. Users must memorize command names; MCP tools are invisible in the shell; skills installed mid-session require a restart to appear (a known Claude Code pain point). This change adds:

1. A **`CommandProvider` interface** — built-ins, skills, and MCP tools each implement the same source contract; the registry aggregates them uniformly.
2. **Live skill reloading** — `fsnotify` watches the three skill dirs (`~/.fuse/skills`, `~/.claude/skills`, `~/.grok/skills`); new/changed/removed skills appear in autocomplete without restarting.
3. **Live MCP reloading** — `fsnotify` watches `~/.fuse/config.yml`; when `fuse mcps add/remove` writes the config (from any terminal), the shell detects the change, diffs the server list, and reconnects only the delta.
4. A **filterable slash-command autocomplete overlay** — `/` triggers a list of all commands with kind tags and descriptions; typing narrows the list; arrow keys and Enter select.
5. **MCP tools as natural-language prompt expansions** — selecting a tool injects a prompt template the agent resolves through the existing tool executor.
6. A **`fuse mcps` top-level CLI** (absorbed from 0009) for MCP server management.

Prior art studied: Cline (source-tagged registry, separate skill/MCP load concerns), Grok-Build (unified `Namespace:tool` registry, tools as first-class entries), OpenCode (MCP as sidebar plugin — not autocomplete).

---

## New files

| Path | Purpose |
|---|---|
| `internal/tui/provider.go` | `CommandProvider` interface + `SlashEntry`, `SlashKind` |
| `internal/tui/slash_registry.go` | `SlashRegistry` — aggregates providers, filters, subscription fan-out |
| `internal/tui/builtin_provider.go` | `BuiltinProvider` — static built-in commands |
| `internal/tui/skill_provider.go` | `SkillProvider` — wraps `*skills.Set`, fsnotify dir-watcher |
| `internal/tui/mcp_provider.go` | `MCPProvider` — wraps `*mcp.Manager`, config-file watcher + incremental reload |
| `internal/tui/slash_completer.go` | `slashCompleter` bubbletea sub-model (overlay, filter, cursor) |
| `cmd/fuse/mcps.go` | `runMCPs` — routes `fuse mcps [subcommand]` |
| `internal/config/writer.go` | `AddMCPServer` / `RemoveMCPServer` — YAML node surgery |

## Modified files

| Path | Change |
|---|---|
| `internal/mcp/manager.go` | `ServerStatus`, `Manager.Status()`, `Manager.Servers()`, `Manager.Reconnect()`, stderr ring buffer |
| `internal/tui/shell_model.go` | Replace `slash map[string]skills.Skill` with `*SlashRegistry`; wire subscription loop + `registryReloadMsg` into Update/View |
| `cmd/fuse/shell.go` | Build providers, construct `SlashRegistry`, pass to `NewShellModel` |
| `cmd/fuse/main.go` | `case "mcps": return runMCPs(args[1:], cfg, stdout, stderr)` |

---

## 1. `internal/tui/provider.go` — the provider contract

```go
// SlashKind identifies the source of a slash command for display.
type SlashKind string

const (
    KindBuiltin SlashKind = "builtin"
    KindSkill   SlashKind = "skill"
    KindMCP     SlashKind = "mcp"
)

// SlashEntry is one item in the autocomplete list.
type SlashEntry struct {
    Command     string    // e.g. "/model", "/code-review", "/mcp:everything/echo"
    Syntax      string    // arg hint shown beside Command, e.g. "NAME" for /model
    Description string    // one-line description
    Kind        SlashKind
    Server      string    // populated for KindMCP
    expand      func() string
}

// Expansion returns the text to inject into the shell input on selection.
func (e SlashEntry) Expansion() string { return e.expand() }

// CommandProvider is a live source of slash commands.
type CommandProvider interface {
    // Commands returns the current snapshot of entries from this source.
    // Called on every registry refresh — must be safe for concurrent calls.
    Commands() []SlashEntry

    // Changes returns a channel that receives a signal (struct{}{}) whenever
    // the provider's command set may have changed. The registry fan-out loop
    // listens to all provider channels and issues a registryReloadMsg on any
    // signal. A nil channel marks a static provider (built-ins).
    Changes() <-chan struct{}

    // Close releases any background resources (file watchers, goroutines).
    Close()
}
```

---

## 2. `internal/tui/slash_registry.go` — the aggregator

```go
type SlashRegistry struct {
    providers []CommandProvider
    reload    chan struct{} // unified fan-out; the shell model reads this
}

// NewSlashRegistry starts the fan-out goroutine and returns a ready registry.
func NewSlashRegistry(providers ...CommandProvider) *SlashRegistry

// All returns the current union of all providers' entries, preserving
// provider order (built-ins first, then skills, then MCP tools).
func (r *SlashRegistry) All() []SlashEntry

// Filter returns entries whose Command or Description contains f (case-insensitive).
// Empty f returns All().
func (r *SlashRegistry) Filter(f string) []SlashEntry

// Reload signals all providers that were flagged dirty; called by the shell
// model on registryReloadMsg. Providers that detect no real change return the
// same entries; the completer re-filters regardless.
func (r *SlashRegistry) Reload()

// Changes returns the unified channel — one signal covers any provider change.
func (r *SlashRegistry) Changes() <-chan struct{}

// Close stops the fan-out goroutine and closes all providers.
func (r *SlashRegistry) Close()
```

### Fan-out goroutine

```go
// Started in NewSlashRegistry. Selects over every non-nil provider.Changes()
// channel; on any signal, drains the channel (debounce: 50 ms window) then
// sends once on r.reload. Stops when r.reload is closed.
```

50 ms debounce prevents a burst of fsnotify events (e.g. a skill dir copy) from flooding the UI with reloads.

---

## 3. `internal/tui/builtin_provider.go` — static built-ins

```go
// BuiltinProvider is a static CommandProvider. Changes() returns nil.
type BuiltinProvider struct{}

func NewBuiltinProvider() *BuiltinProvider
```

Built-in entries (in display order):

| Command | Syntax | Description |
|---|---|---|
| `/exit` | | Exit the shell |
| `/quit` | | Exit the shell |
| `/verbose` | | Toggle verbose tool output |
| `/model` | `NAME` | Switch model (e.g. sonnet, opus) |

---

## 4. `internal/tui/skill_provider.go` — live skill reloading

```go
type SkillProvider struct {
    dirs    []string      // from skills.DefaultDirs()
    mu      sync.RWMutex
    entries []SlashEntry
    ch      chan struct{}
    watcher *fsnotify.Watcher
}

func NewSkillProvider(dirs []string) (*SkillProvider, error)
```

### Startup

`NewSkillProvider` loads all skills via `skills.Load(dirs)`, builds initial entries, then starts a background goroutine that:

1. Calls `watcher.Add(dir)` for each directory in `dirs` that exists (missing dirs are skipped — watcher retries them on a 30-second ticker in case they are created later).
2. Selects on `watcher.Events` and `watcher.Errors`.

### Entry construction from a `skills.Skill`

```go
SlashEntry{
    Command:     sk.SlashCommand or ("/" + sk.Name),
    Description: sk.Description,
    Kind:        KindSkill,
    expand:      func() string { return entry.Command + " " },
}
```

### Reload on fsnotify event

On any `fsnotify.Event` under a watched dir: re-run `skills.Load(dirs)` in full (simple, correct — avoids incremental merge logic). Under the write lock, replace `entries`. Signal `ch`. The 50 ms debounce in the registry fan-out absorbs rapid bursts.

### Missing-dir retry

On a 30-second ticker: attempt `watcher.Add` for any dirs in `dirs` not yet watched. Supports installing the `~/.fuse/skills` directory (or `~/.claude/skills`) after the shell starts.

---

## 5. `internal/tui/mcp_provider.go` — live MCP reloading

```go
type MCPProvider struct {
    configPath string        // resolved ~/.fuse/config.yml
    toolReg    *tools.Registry
    mu         sync.RWMutex
    entries    []SlashEntry
    mgr        *mcp.Manager  // current manager; replaced on reload
    servers    []ServerInfo  // last-known server list from mgr.Servers()
    ch         chan struct{}
    watcher    *fsnotify.Watcher
}

func NewMCPProvider(configPath string, cfg config.Config, toolReg *tools.Registry) (*MCPProvider, error)
```

### Startup

`NewMCPProvider` constructs the initial `mcp.Manager` (same path as today's `buildSessionRegistry`), builds initial entries from `mgr.Servers()`, then watches `configPath` for changes.

### Config-file change → incremental reconnect

On `fsnotify.Write` on `configPath`:

1. Re-parse `config.Config` from disk.
2. Diff `cfg.MCPServers` against `p.servers` by `Name`:
   - **Added** servers: call `mcp.StartAndDiscover(srv, toolReg)` to connect and register tools; append to manager.
   - **Removed** servers: call `mgr.Stop(name)` to terminate the connection and deregister its tools.
   - **Unchanged** servers: no action.
3. Update `p.servers`, rebuild `p.entries` from `mgr.Servers()`.
4. Signal `p.ch`.

Debounce: 200 ms (config writes are larger than skill-file events; give the writer time to flush).

### Entry construction from a `ServerInfo` / tool

```go
SlashEntry{
    Command:     "/mcp:" + srv.Name + "/" + tool.Name,
    Description: tool.Description,
    Kind:        KindMCP,
    Server:      srv.Name,
    expand: func() string {
        return fmt.Sprintf(
            "Use the %s tool from the %s MCP server. %s Arguments: ",
            tool.Name, srv.Name, tool.Description,
        )
    },
}
```

---

## 6. `internal/mcp/manager.go` additions (absorbed from 0009)

### `ServerInfo` and `Manager.Servers()`

Used by `MCPProvider` to build slash entries and diff on reload.

```go
type MCPToolInfo struct {
    Name        string
    Description string
}

type ServerInfo struct {
    Name      string
    Transport string
    AuthType  string
    Tools     []MCPToolInfo
}

func (m *Manager) Servers() []ServerInfo
```

### `Manager.Stop(name string) error`

Terminates the connection to the named server and deregisters its tools from the tool registry. No-op if the server is not running.

### `ServerStatus` and `Manager.Status()` (for `fuse mcps` CLI)

```go
type ServerStatus struct {
    Name      string
    Transport string
    AuthType  string
    Connected bool
    Error     string
    Tools     []string
    PID       int      // stdio only
    TokenFile string   // oauth2 only
    LogLines  []string // last N stderr lines, stdio only
}

func (m *Manager) Status() []ServerStatus
```

### Stderr ring buffer

Each stdio server's stderr → per-server `[]string` ring (cap 200 lines, `FUSE_MCP_LOG_LINES` env override). `cmd.Stderr` wired to a `lineCapture` writer under a mutex; oldest lines discarded when full.

---

## 7. `internal/tui/slash_completer.go` — the overlay sub-model

`slashCompleter` is a plain struct driven by `ShellModel.Update` (not a `tea.Model` itself).

```go
type slashCompleter struct {
    reg     *SlashRegistry
    filter  string
    visible []SlashEntry
    cursor  int
    active  bool
    offset  int   // scroll offset for >8 items
}
```

### Activation rules

- `active = true` when input value starts with `"/"` (one or more chars).
- `active = false` on Esc, on Enter (after selection), or when input no longer starts with `"/"`.
- On `registryReloadMsg`: call `c.refresh()` — re-filter from current `reg.All()`.

### Key handling while active

| Key | Action |
|---|---|
| `Up` | move cursor up (wraps; scrolls offset) |
| `Down` | move cursor down (wraps; scrolls offset) |
| `Enter` | inject `selected().Expansion()` into input; close completer; submit if KindMCP (expansion is a complete prompt), else leave in input for editing |
| `Esc` | dismiss; clear input |
| printable | pass to text input AND re-filter (`filter = input[1:]`) |

**Enter semantics by kind:**
- `KindBuiltin` / `KindSkill`: inject the command into the input field; the existing `handleSlash` dispatch runs on submit.
- `KindMCP`: inject the full expansion template into the input field, position cursor after `"Arguments: "`, leave for the user to complete before submitting.

### Rendering

```
  ▸ /echo           [mcp:everything]  Echoes back any string
    /get-sum         [mcp:everything]  Returns the sum of two numbers
    /code-review     [skill]           Review code for correctness
    /model    NAME   [builtin]         Switch model
    ↓ 3 more
```

- Max 8 rows visible; `↓ N more` / `↑ N more` scroll indicators.
- Selected row highlighted with accent color (lipgloss).
- Kind tag right-aligned in a fixed 18-char column. Server name included for MCP: `[mcp:everything]`.
- Description truncated to terminal width minus fixed columns.

---

## 8. `internal/tui/shell_model.go` — subscription loop

### `registryReloadMsg`

```go
type registryReloadMsg struct{}
```

### Subscription tea.Cmd

```go
func waitForRegistryReload(ch <-chan struct{}) tea.Cmd {
    return func() tea.Msg {
        <-ch
        return registryReloadMsg{}
    }
}
```

Returned in `Init()` and re-armed in every `Update` that handles `registryReloadMsg` — the standard bubbletea long-running subscription pattern (mirrors the existing `waitForMsg(m.ch)` pattern for agent output).

### `NewShellModel` signature change

```go
// Before:  slash map[string]skills.Skill
// After:   reg *SlashRegistry
func NewShellModel(alias string, verbose bool, glamourStyle string,
    reg *model.Registry, slashReg *SlashRegistry, build AgentBuilder) ShellModel
```

### `handleSlash` update

Skills and built-ins continue to dispatch via the existing `switch` + skill-body injection. The `slash map[string]skills.Skill` field is replaced by `slashReg *SlashRegistry`; `handleSlash` calls `slashReg.Filter(cmd)` to resolve the entry, then dispatches by `Kind`.

---

## 9. `cmd/fuse/shell.go` — wiring

```go
builtins := tui.NewBuiltinProvider()

skillProv, err := tui.NewSkillProvider(skills.DefaultDirs())
if err != nil { /* non-fatal: log and continue with empty skills */ }

mcpProv, err := tui.NewMCPProvider(config.Path(), cfg, toolReg)
if err != nil { /* non-fatal: log and continue with no MCP entries */ }

slashReg := tui.NewSlashRegistry(builtins, skillProv, mcpProv)
defer slashReg.Close()

m := tui.NewShellModel(alias, verbose, glamourStyle, modelReg, slashReg, build)
```

`buildSessionRegistry` no longer needs to return `mcpMgr` separately — `MCPProvider` owns the manager lifecycle. If other callers need the manager (e.g. a future `/status` command), expose it via `mcpProv.Manager()`.

---

## 10. `internal/config/writer.go` — YAML node surgery (absorbed from 0009)

```go
// AddMCPServer appends or replaces the named server in ~/.fuse/config.yml.
// The fsnotify watcher on the running shell detects the file change and
// reconnects within ~200 ms.
func AddMCPServer(cfg config.MCPServerConfig) error

// RemoveMCPServer removes the named server from ~/.fuse/config.yml.
// No-op if the name is not present.
func RemoveMCPServer(name string) error
```

Auth flags for `fuse mcps add`: `--auth none|bearer|oauth2`, `--token TOKEN`, `--client-id ID --client-secret SECRET [--scopes S1,S2]`.

---

## 11. `cmd/fuse/mcps.go` — subcommand routing (absorbed from 0009)

```
fuse mcps                    → list (static, from config)
fuse mcps list [--live]      → static; --live dials fresh connections
fuse mcps add  --name N --transport stdio --command "CMD" [auth flags]
               --transport http --url URL [auth flags]
fuse mcps remove NAME
fuse mcps tools [NAME]       → list tools per server
fuse mcps logs  [NAME]       → last N stderr lines (stdio only)
```

`--live` list output:
```
NAME           TRANSPORT  AUTH    STATUS    TOOLS
filesystem     stdio      none    ok        3
brave-search   http       bearer  ok        1
broken         stdio      none    error     -
```

---

## 12. Load tests — upper bounds and throttle validation

Build tag `//go:build loadtest` — separate from the unit suite and the `integration` tag. Run with `go test -tags loadtest -v ./internal/tui/... ./internal/mcp/...`. No external infrastructure required (all load tests use temp dirs and in-process fakes).

**Purpose:** the 50 ms (skills) and 200 ms (MCP) debounce values are initial guesses. The load tests define the targets the implementation must meet, and their output is the evidence that confirms or adjusts those values before the PR merges.

### 12a. `internal/tui/slash_registry_load_test.go` — filter performance at scale

```go
// BenchmarkRegistryFilter seeds the registry with N synthetic entries
// (split 10% builtin, 40% skill, 50% MCP across 5 fake servers) and
// benchmarks Filter("ec") — a 2-char mid-word match representing worst-case
// scan cost. Run with go test -bench=. -benchtime=5s -tags loadtest.
//
// Pass targets (enforced via b.ReportMetric + t.Errorf):
//   N=100   → median < 50 µs
//   N=1000  → median < 500 µs
//   N=5000  → median < 2.5 ms   (upper practical bound for one user's setup)
//   N=10000 → recorded only; no hard cap (informational)
func BenchmarkRegistryFilter_100(b *testing.B)   { benchFilter(b, 100) }
func BenchmarkRegistryFilter_1000(b *testing.B)  { benchFilter(b, 1000) }
func BenchmarkRegistryFilter_5000(b *testing.B)  { benchFilter(b, 5000) }
func BenchmarkRegistryFilter_10000(b *testing.B) { benchFilter(b, 10000) }
```

**Throttle signal:** if N=1000 breaches 500 µs, switch `Filter` from `strings.Contains` to a pre-built index (e.g. sorted slice + binary search on prefix, or a trie for prefix-only matching). Decision deferred to implementation; the benchmark is the gate.

```go
// BenchmarkRegistryFilter_Typing simulates a human typing "/code-rev" at
// 120 WPM (~100 ms/keystroke) with 1000 entries in the registry.
// Each sub-benchmark corresponds to one additional character typed.
// Pass target: every Filter call < 1 ms (100× headroom over keystroke interval).
func BenchmarkRegistryFilter_Typing(b *testing.B)
```

### 12b. `internal/tui/skill_provider_load_test.go` — burst install + debounce validation

```go
// TestSkillProvider_BurstInstall copies 50 synthetic skill directories into a
// temp skill dir simultaneously (via 50 goroutines) and asserts:
//   1. The provider emits ≤ 5 reload signals within 2 s (debounce collapses the burst).
//   2. All 50 skills are present in Commands() after the last signal.
//   3. No data races (run with -race).
//
// If signal count > 5, the debounce window should be widened.
// Calibration target: burst of N dir-creates → ≤ ceil(N/10) signals.
func TestSkillProvider_BurstInstall(t *testing.T)

// TestSkillProvider_RapidEdit simulates a user iterating on a SKILL.md —
// 20 writes to the same file in 100 ms — and asserts:
//   1. ≤ 3 reload signals within 1 s.
//   2. Commands() reflects the final version of the file.
func TestSkillProvider_RapidEdit(t *testing.T)

// TestSkillProvider_MissingDirRetry verifies that a skill dir created after
// the provider starts is picked up within 35 s (the 30 s retry ticker + slack).
func TestSkillProvider_MissingDirRetry(t *testing.T)
```

### 12c. `internal/tui/mcp_provider_load_test.go` — config-file change and reconnect throttle

```go
// TestMCPProvider_BulkAdd writes 10 new fake MCP server entries to a temp
// config file in one atomic rename (write to temp + os.Rename — the same
// pattern config/writer.go uses) and asserts:
//   1. Exactly 1 reload signal fires within 500 ms (single config write = single signal).
//   2. All 10 servers' tools appear in Commands() after the signal.
//   3. The existing (pre-add) server is still present and unchanged.
//
// Fake MCP servers: in-process httptest.Server instances that respond to
// tools/list with a canned JSON payload — no Docker, no real MCP protocol.
func TestMCPProvider_BulkAdd(t *testing.T)

// TestMCPProvider_RemoveOne writes a config file with 3 servers, waits for
// initial load, removes 1 server entry (atomic rename), and asserts:
//   1. Removed server's tools absent from Commands() within 500 ms.
//   2. Remaining 2 servers' tools still present.
//   3. manager.Stop() was called exactly once (verified via a call-counted fake).
func TestMCPProvider_RemoveOne(t *testing.T)

// TestMCPProvider_DebounceRapidWrites writes the config file 10 times in
// 50 ms (simulating a non-atomic writer or a flaky editor) and asserts:
//   1. ≤ 3 reload signals within 1 s.
//   2. Commands() reflects the final config state.
// If signal count > 3, increase the MCP debounce window above 200 ms.
func TestMCPProvider_DebounceRapidWrites(t *testing.T)

// BenchmarkMCPProvider_Reconnect measures the time from config write to
// Commands() reflecting the new server, with fake servers that respond to
// tools/list in < 1 ms.
//
// Pass target: p95 < 300 ms end-to-end (200 ms debounce + 100 ms reconnect budget).
// If p95 > 300 ms on real (non-fake) servers, increase the reconnect timeout
// or parallelize StartAndDiscover calls across added servers.
func BenchmarkMCPProvider_Reconnect(b *testing.B)
```

### 12d. `internal/tui/slash_registry_load_test.go` (continued) — concurrent provider reloads

```go
// TestSlashRegistry_ConcurrentReload fires 100 goroutines, each triggering
// one provider signal, while 10 goroutines concurrently call Filter().
// Asserts no panics, no data races (-race), and Filter() always returns a
// non-nil slice.
func TestSlashRegistry_ConcurrentReload(t *testing.T)
```

### Throttle decision table

| Measurement | Target | Action if breached |
|---|---|---|
| `Filter` at N=1000 | < 500 µs | Add prefix index or trie |
| `Filter` at N=5000 | < 2.5 ms | Limit registry to top-N by recency or priority |
| Skill burst (50 installs) signals | ≤ 5 | Widen skill debounce above 50 ms |
| MCP rapid writes (10 in 50 ms) signals | ≤ 3 | Widen MCP debounce above 200 ms |
| MCP reconnect p95 end-to-end | < 300 ms | Parallelize `StartAndDiscover` calls |

The implementation chooses the debounce values first, runs the load tests, and adjusts until all targets pass. Results are recorded in the PR description.

---

## New dependency

`github.com/fsnotify/fsnotify` — file-system event watcher (BSD-2-Clause). Used by both `SkillProvider` and `MCPProvider`. Already used in the Go ecosystem by Viper, Air, etc.

---

## Out of scope

- Structured argument forms for MCP tool arguments (natural-language expansion only).
- In-shell MCP management as slash commands — `fuse mcps` is the CLI path.
- Watching the config file for non-MCP changes (model list, auth config, etc.).
- Skill dir watching triggers full reload, not incremental merge — simple and correct for the expected low churn rate.

## Open questions

None — design fully specified above.
