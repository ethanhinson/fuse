<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0003 — HITL Permission Layer + MCP Client Integration](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0003-hitl-permissions-mcp.md)**
<!-- docket:backlink:end -->

# HITL Permission Layer + MCP Client Integration

**Spec for change 0003**

---

## Overview

Every tool call the agent makes — built-in (`bash`, `write_file`, `edit_file`) or MCP-provided — passes through a single `PermissionGate` before execution. The gate resolves a merged `ToolPolicy`, auto-approves safe read-only tools, and surfaces a bubbletea confirmation prompt for anything that writes, executes, or reaches out via MCP. The user can approve once or for the whole session. MCP tool servers register their tools into the same `Registry` as built-ins, receiving identical policy treatment — there is no separate code path for "it is an MCP tool."

---

## Permission Layer

### ToolPolicy

```go
// ToolPolicy is the resolved approval stance for a single tool at a single call site.
type ToolPolicy struct {
    Enabled     bool // false = tool is disabled; execution is skipped entirely
    AutoApprove bool // true = no human prompt needed; false = HITL gate fires
}
```

### Policy Resolution (3-source merge, in precedence order)

1. **Built-in safe list** (baseline, lowest precedence): `read_file`, `list_directory`, `codeindex_*` → `AutoApprove: true`. All others default to `AutoApprove: false`.
2. **Config `permissions.auto_approve` / `permissions.always_prompt` patterns** (mid precedence): glob patterns matched against `toolName` (for named tools) or `toolName:command` (for bash, where command is the first token of the shell command). `auto_approve` promotes to `AutoApprove: true`; `always_prompt` demotes to `AutoApprove: false` even if the safe list would have passed it.
3. **Session cache** (highest precedence, runtime only): if the user chose "allow for session" on a prior call with the same `(toolName, argsFingerprint)` key, `AutoApprove: true` is returned from the cache without prompting.

`Enabled` is `false` only when `permissions.mode: off` (the tool is fully disabled for the session) or when a specific tool name appears in `permissions.disabled`. All other scenarios leave `Enabled: true`.

### Permission Modes

```yaml
permissions:
  mode: smart          # off | prompt-all | smart  (default: smart)
  session_allow: true  # whether the "allow for session" option appears in the prompt
  auto_approve:        # patterns that promote to auto-approve beyond the built-in safe list
    - "go test"
    - "make build"
    - "git status"
    - "git log"
    - "git diff"
  always_prompt:       # patterns that demote to always-prompt (override safe list)
    - "rm -rf"
    - "git push"
    - "git reset"
    - "curl"
    - "wget"
  disabled: []         # tool names that are fully disabled (Enabled: false)
```

- **`off`**: All tools auto-approve. Gate is bypassed entirely (useful for trusted local sessions).
- **`smart`** (default): Safe list + config patterns applied. Most reads pass silently; writes/exec prompt.
- **`prompt-all`**: Every tool call prompts regardless of safe list or config.

### Session Approval Cache

```go
// ApprovalCache stores session-scoped "allow for this session" decisions.
// Keyed by (toolName, argsFingerprint) where argsFingerprint is a
// stable hash of the normalized JSON args.
type ApprovalCache struct {
    mu      sync.RWMutex
    allowed map[string]struct{}
}
```

The cache is in-memory only, scoped to the process lifetime. It is never persisted to disk. A session restart clears it. The key is `toolName + ":" + sha256(normalizeArgs(args))[:8]` — short enough to be cheap, collision risk negligible for interactive sessions.

### PermissionGate

```go
// PermissionGate sits between the agent loop and the tool registry.
type PermissionGate struct {
    mode    PermissionMode
    config  PermissionsConfig
    cache   *ApprovalCache
    approve ApprovalFunc // injected; calls the TUI approval flow
}

// ApprovalFunc is the async approval channel: blocks until the user decides.
// Returns (approved, allowForSession).
type ApprovalFunc func(ctx context.Context, req ApprovalRequest) (bool, bool, error)

// Check resolves the merged policy and either passes, blocks for approval, or skips.
func (g *PermissionGate) Check(ctx context.Context, toolName, args string) (ToolPolicy, error)

// Execute wraps the registry's Execute with the gate check.
func (g *PermissionGate) Execute(ctx context.Context, name, args string) tools.Result
```

The gate lives at `internal/permissions/gate.go`. The agent loop calls `gate.Execute()` instead of `registry.Execute()` directly. The registry is not modified.

### Approval Request/Response

```go
type ApprovalRequest struct {
    ToolName string
    Args     string          // raw JSON from model
    Preview  string          // human-readable summary (e.g. "bash: rm -rf /tmp/foo")
}

type ApprovalResponse struct {
    Approved       bool
    AllowForSession bool
}
```

The TUI receives an `ApprovalRequest` as a `tea.Msg` and renders an inline confirmation block in the viewport. The input bar is temporarily replaced by `[y]es / [n]o / [s]ession` keybindings. On response, the TUI dispatches an `ApprovalResponse` msg back to the waiting goroutine via a Go channel.

---

## MCP Client Integration

### Transport

Phase 1: **stdio transport only** (covers the vast majority of public MCP servers). HTTP/SSE transport is explicitly out of scope for this change.

```go
// StdioClient manages a single MCP server process (stdio transport).
type StdioClient struct {
    name    string
    cmd     *exec.Cmd
    encoder *json.Encoder
    decoder *json.Decoder
    mu      sync.Mutex
    pending map[string]chan mcpResponse
}
```

MCP JSON-RPC 2.0 messages are exchanged over the server process's stdin/stdout. Stderr from the MCP server is captured and surfaced in the Fuse TUI as a collapsible "MCP server log" region (not surfaced to the model).

### Tool Discovery

On session start (before the first user turn), Fuse:
1. Spawns each configured MCP server process.
2. Sends `tools/list` request; waits up to 5s per server.
3. Wraps each returned tool as an `MCPTool` implementing `tools.Tool`.
4. Registers all discovered tools in the global `Registry` under the name `mcp:<server-name>/<tool-name>`.

```go
// MCPTool wraps a single MCP server tool as a tools.Tool.
type MCPTool struct {
    client      *StdioClient
    serverName  string
    toolName    string
    description string
    inputSchema map[string]any
}

func (t *MCPTool) Name() string        { return "mcp:" + t.serverName + "/" + t.toolName }
func (t *MCPTool) Description() string { return t.description }
func (t *MCPTool) Parameters() map[string]any { return t.inputSchema }
func (t *MCPTool) Execute(ctx context.Context, args string) tools.Result {
    // calls tools/call on the MCP server, returns result
}
```

### Config Shape

```yaml
mcp_servers:
  - name: filesystem
    transport: stdio
    command: ["npx", "@modelcontextprotocol/server-filesystem", "/path/to/project"]
    env:
      NODE_ENV: production
  - name: github
    transport: stdio
    command: ["npx", "@modelcontextprotocol/server-github"]
    env:
      GITHUB_TOKEN: "${GITHUB_TOKEN}"
```

Environment variable interpolation (`${VAR}`) is performed at load time from the process environment. A server that fails to start (non-zero within 3s or `tools/list` times out) logs a warning and is skipped — the session continues with remaining servers.

### Policy for MCP Tools

MCP tools are **not** on the built-in safe list by default. All MCP tools start with `AutoApprove: false` (always prompt). The user can promote specific MCP tools or entire servers via config patterns:

```yaml
permissions:
  auto_approve:
    - "mcp:filesystem/read_file"    # a specific MCP tool
    - "mcp:filesystem/*"            # all tools from the filesystem server
```

---

## TUI Integration

### Approval Prompt

When `PermissionGate.Check()` needs human input, it:
1. Sends a `PermissionRequestMsg` to the bubbletea `Program` via `program.Send()`.
2. Blocks on a `chan ApprovalResponse` until the model responds or `ctx` is cancelled.

The TUI's `Update()` handles `PermissionRequestMsg` by:
- Appending an approval block to the viewport (styled distinctly, e.g. amber background).
- Switching the input bar to keybinding mode: `y` = yes (once), `s` = yes (session), `n` = no, `Escape` = no.
- On keypress, sending `ApprovalResponseMsg` which resolves the channel and restores the input bar.

### Rendering

```
┌─────────────────────────────────────────────────────────────┐
│  ⚠  Permission required                                     │
│                                                             │
│  Tool:  bash                                                │
│  Cmd:   rm -rf /tmp/build-artifacts                         │
│                                                             │
│  [y] allow once   [s] allow for session   [n] deny          │
└─────────────────────────────────────────────────────────────┘
```

The block is appended inline in the transcript, not a modal overlay, keeping the conversation readable.

---

## Package Layout

```
internal/
  permissions/
    gate.go          — PermissionGate, ApprovalFunc, Execute()
    policy.go        — ToolPolicy, policy resolution, safe list
    cache.go         — ApprovalCache (session-scoped in-memory map)
    patterns.go      — glob matching for auto_approve / always_prompt
  mcp/
    client.go        — StdioClient (JSON-RPC 2.0 over stdin/stdout)
    tool.go          — MCPTool implementing tools.Tool
    manager.go       — MCPManager: spawn servers, discover tools, register
  config/
    schema.go        — add PermissionsConfig + MCPServerConfig structs
```

---

## Agent Loop Wiring

`cmd/fuse/shell.go` (and `run.go` for one-shot mode) currently calls `registry.Execute()`. After this change, the wiring is:

```
agent.Run(ctx, history)
     │
     └─→ gate.Execute(ctx, toolName, args)   ← new
              │
              ├─ gate.Check() → auto-approve → registry.Execute(ctx, toolName, args)
              └─ gate.Check() → need approval → TUI prompt → (approved) → registry.Execute()
```

The `Agent` struct gains a `gate *permissions.PermissionGate` field. The `PermissionGate` holds a reference to the `Registry`. The approval function (`ApprovalFunc`) is injected at construction time by the shell wiring — the TUI shell passes a channel-backed implementation; one-shot mode (`fuse "task"`) passes a no-op that always approves (no human available, mode implicitly `off`).

---

## Error Handling

- **User denies**: `gate.Execute()` returns `tools.Result{IsError: true, Output: "tool call denied by user"}`. The model receives this as a tool result and can explain the situation or try an alternative.
- **Context cancelled during approval wait**: `gate.Execute()` unblocks, returns `tools.Result{IsError: true, Output: "approval cancelled: context done"}`.
- **MCP server crash mid-session**: `MCPTool.Execute()` returns an error result; the tool remains registered but all subsequent calls return the same error. A warning is appended to the TUI transcript.
- **MCP server start failure**: Logged at session start; server is skipped; session continues.

---

## Out of Scope (this change)

- HTTP/SSE MCP transport.
- MCP resource and prompt endpoints (tools only).
- Persistent approval history across sessions.
- Per-tool audit log / approval replay.
- MCP server health monitoring / restart.
- Approval UI in one-shot (non-TUI) mode — one-shot silently auto-approves (consistent with `mode: off` semantics for non-interactive runs).
