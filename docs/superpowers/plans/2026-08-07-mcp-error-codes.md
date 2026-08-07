<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0031 — Adopt MCP-specific JSON-RPC error code range](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0031-fuse-mcp-error-codes.md)**
<!-- docket:backlink:end -->

# Plan — Adopt MCP-specific JSON-RPC error code range (change 0031)

> Plan authored inline (auto fallback): the configured plan skill `superpowers:writing-plans`
> is not invocable in this session. Build likewise runs inline (auto fallback for
> `superpowers:subagent-driven-development`). Method is TDD by choice, not by the SDD skill.

## Goal

Adopt the MCP-specific JSON-RPC error range (`-32900`..`-32999`, MCP v2025-03-26) so the fuse
MCP server emits `-32900` (ToolNotFound) for unknown tools and the fuse MCP client preserves
downstream error codes instead of flattening them to a message. Improves client diagnostics
(Claude Code / Cursor can distinguish "tool not found" from "invalid params").

## Context (verified during reconcile)

- Server: `internal/mcp/server.go`. `errResp` builds the error frame; today `-32601`
  (method not found, `server.go:85`) and `-32600` (invalid params, `server.go:110`).
- Unknown tool is NOT a protocol error today: `handleCall` → `gate.Execute` →
  `registry.Execute` returns an `isError` tool *result* (`internal/tools/registry.go:83`).
- Client: `internal/mcp/client.go`. `StdioClient.call` wraps only `resp.Error.Message`
  (`client.go:217-219`) — the code is discarded.
- The registry has no membership primitive (only `Schemas()`/`Execute()`).
- Resource-not-found (`-32901`) has no live call site — fuse's server exposes only `tools/*`.
  Constants are defined for completeness; only `-32900` gets a server call site this change.

## Task 1 — MCP error-code constants (`internal/mcp/errors.go`)

**Test first** (`internal/mcp/errors_test.go`):
- Assert the constant values: `ErrToolNotFound == -32900`, `ErrResourceNotFound == -32901`,
  `ErrPromptNotFound == -32902`, `ErrListResultEmpty == -32903`, `ErrConnectionClosed == -32904`.
- `isMCPErrorCode(code)` returns true for `-32900` and `-32999` (range bounds) and false for
  `-32899`, `-33000`, `-32601`, `0`.

**Implement:**
- New file `internal/mcp/errors.go`: the five `Err*` constants (typed `int` consts) plus an
  unexported `isMCPErrorCode(code int) bool` testing `code >= -32999 && code <= -32900`.
- Doc comment citing MCP v2025-03-26 Error Codes section.

## Task 2 — Registry membership primitive (`internal/tools/registry.go`)

**Test first** (`internal/tools/registry_test.go`, add a case):
- `Has` returns true for a registered tool, false for an unknown name, false after `Unregister`.

**Implement:**
- Add `func (r *Registry) Has(name string) bool { _, ok := r.byName[name]; return ok }`.
- Minimal, reusable primitive; keeps the server's ToolNotFound check O(1) and avoids
  allocating `Schemas()` per call.

## Task 3 — Server emits ToolNotFound (`internal/mcp/server.go`)

**Test first** (`internal/mcp/server_test.go` or existing server test file):
- Drive a `tools/call` for a name not in the registry; assert the response carries
  `error.code == -32900` and a message naming the tool. (Build a `Server` over
  `bytes.Buffer`/pipe as existing tests do, or unit-test `dispatch`/`handleCall` directly.)
- Regression: a `tools/call` for a *registered* tool still returns a normal result (goes
  through the gate), and a *disabled* registered tool still returns its gate result — NOT
  `-32900` (exists-but-disabled ≠ not-found).

**Implement:**
- In `handleCall`, after unmarshalling `callParams` and before `gate.Execute`, if
  `!s.reg.Has(p.Name)` return `s.errResp(req.ID, ErrToolNotFound, "tool not found: "+p.Name)`.
- Leave the invalid-params and method-not-found paths unchanged (out of scope).

## Task 4 — Client preserves the error code (`internal/mcp/client.go`)

**Test first** (`internal/mcp/http_client_test.go` / a stdio client test):
- Simulate a server response with `error.code == -32900`; assert the error returned by
  `call` exposes the code (via a typed `*RPCError` with `errors.As`), not just the message.
- Existing behavior (message still present) preserved.

**Implement:**
- Add an exported error type in `client.go` (or `errors.go`):
  `type RPCError struct { Code int; Message string }` with `Error() string`.
- In `StdioClient.call`, replace the `fmt.Errorf("mcp %q: %s", ...)` wrap at `client.go:218`
  with a `*RPCError{Code: resp.Error.Code, Message: resp.Error.Message}` (still wrapped with
  server name context so existing message-substring assertions keep passing — e.g.
  `Error()` renders `mcp <name>: <message>`).
- Confirm no existing caller pattern-matches the old exact string in a way this breaks;
  update any that do.

## Verification

- `go test ./internal/mcp/... ./internal/tools/...` green.
- `go build ./...` (and `go vet ./...`) clean.
- Full suite (`make test` or `go test ./...`) green before PR — single end gate.

## Out of scope (reported, not filed — auto-capture disabled)

- `server.go:110` uses `-32600` where standard JSON-RPC prescribes `-32602` for invalid params.
- `initialize` advertises `protocolVersion "2024-11-05"` (protocol-version negotiation is #19).
- `resources/*` methods / `-32901` live call site (no resource surface exists yet).
