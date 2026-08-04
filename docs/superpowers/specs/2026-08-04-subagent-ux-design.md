<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0012 — First-Class Subagent UX — Spawn, Tree Visualization & Inspect](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0012-subagent-ux.md)**
<!-- docket:backlink:end -->

# Spec: First-Class Subagent UX (change 0012)

> Design spec for the subagent spawn/observe experience in fuse: the `spawn_agent`
> tool, the `agent.Spawn()` API, the spatial-tree TUI, event routing, session
> logging, permission scoping, remote execution over SSE, the IntentPlugin
> system, and the secrets management layer. Authored across four passes:
> (1) base UX, (2) remote execution, (3) IntentPlugin, (4) secrets.

## §0 Summary

fuse gains first-class subagents: a running agent can spawn child agents via a
`spawn_agent` tool, observe them live in a spatial tree in the TUI, and compose
their results. This spec covers four layers, built incrementally:

- **Pass 1 — Base UX.** The `spawn_agent` tool, the `agent.Spawn()` Go API, the
  `AgentTree`/`AgentNode`/`AgentEvent` data model, the spatial-tree TUI, event
  routing to per-node renderers, JSONL session logging, permission/tool scoping
  for children, depth limiting, and TUI wiring.
- **Pass 2 — Remote execution.** A `RemoteExecutor` interface and an
  `SSERemoteExecutor` that dispatches an agent to a remote runner and bridges its
  Server-Sent-Events stream back into the local `AgentTree` as `AgentEvent`s.
- **Pass 3 — IntentPlugin.** An `IntentPlugin` interface that resolves a spawn's
  *intent* (git clone spec, container image, write-back target, injected files)
  into a `RemoteContext` at dispatch time, with `DocketIntentPlugin` and
  `OpenSpecIntentPlugin` implementations.
- **Pass 4 — Secrets.** A `SecretsStore` (where secrets live) and an
  `EncryptionProvider` (how they are encrypted) — two separate concerns. All raw
  token strings across the spec become **secret-name references** resolved at
  dispatch time; container secret sharing is designed with optional age-encrypted
  bundle transport.

The **PM-altitude boundary**: this spec owns the design detail (interfaces, field
names, flows, error taxonomy). Intent and scope live in the 0012 change record.

## §1 Data model

Package `internal/agent`.

### §1.1 AgentNode

```go
type AgentNode struct {
    ID          string     // ULID
    ParentID    string     // "" for root
    Label       string
    Model       string
    Status      NodeStatus
    Depth       int
    StartedAt   time.Time
    EndedAt     time.Time
    TokensIn    int
    TokensOut   int
    CostUSD     float64
    Events      []AgentEvent
    RemoteExec  bool       // true when executed via a RemoteExecutor
    RemoteJobID string     // remote job id, when RemoteExec
    children    []string   // child node IDs
}
```

### §1.2 AgentTree

```go
type AgentTree struct {
    nodes     map[string]*AgentNode
    rootID    string
    out       chan TreeUpdate // buffered 256
    dirty     bool
    remotes   map[string]RemoteExecutor
    remotesMu sync.RWMutex
    intents   map[string]IntentPlugin
    intentsMu sync.RWMutex
    secrets   SecretsStore    // set at wiring time; nil = &EnvSecretsStore{} (lazy default)
}

func (t *AgentTree) SetSecrets(s SecretsStore)
func (t *AgentTree) secretsStore() SecretsStore // returns &EnvSecretsStore{} if nil
```

`SetSecrets` is called once at wiring time (§10). Every secret resolution inside
`agent.Spawn` and `SSERemoteExecutor` goes through `t.secretsStore()`, which
returns the injected store or a lazily-constructed `&EnvSecretsStore{}` when none
was set — preserving the historical `${ENV_VAR}` behavior with zero configuration.

### §1.3 AgentEvent

```go
type AgentEvent struct {
    Kind    EventKind
    Name    string
    Payload map[string]any
    TS      time.Time
    Seq     int64
}
```

### §1.4 Enums

```go
type NodeStatus int
const (
    StatusPending NodeStatus = iota
    StatusRunning
    StatusDone
    StatusError
    StatusCancelled
)

type EventKind int
const (
    KindSpawned EventKind = iota
    KindAssistant
    KindToolCall
    KindToolResult
    KindTokens
    KindDone
    KindError
)
```

## §2 `spawn_agent` tool

A model-callable tool that spawns a child agent.

**Input schema** (`label`, `task`, `system_prompt`, `tools`, `model`, `remote`,
`remote_id`, `intent_plugin`):

- `label` (string, required) — short human label for the tree.
- `task` (string, required) — the child's task prompt.
- `system_prompt` (string, optional) — overrides the inherited system prompt.
- `tools` ([]string, optional) — subset of parent tools; see §7.
- `model` (string, optional) — model id; defaults to parent's model.
- `remote` (bool, optional) — dispatch to a `RemoteExecutor`.
- `remote_id` (string, optional) — named remote; "" = default.
- `intent_plugin` (string, optional) — named IntentPlugin; "" = NilIntentPlugin.

A `SpawnFunc` callback (`func(context.Context, SpawnOpts) (AgentHandle, error)`)
is injected into the tool at construction to **break the import cycle** between
the tool registry and the `agent` package.

**Output** = the child's final result text plus a one-line summary. The
`ToolResultMsg` for `spawn_agent` is suppressed in the parent transcript (the tree
UI carries the detail instead).

## §3 `agent.Spawn()` API

### §3.1 SpawnOpts

```go
type SpawnOpts struct {
    Label          string
    Task           string
    SystemPrompt   string
    Tools          []string
    ModelID        string
    MaxTurns       int
    MaxTokens      int
    Remote         bool
    RemoteID       string
    IntentPluginID string
}
```

### §3.2 AgentHandle / groups

```go
type AgentHandle struct {
    NodeID string
    Done   <-chan SpawnDone
    cancel context.CancelFunc
}

type SpawnDone struct {
    Result string
    Err    error
}

type SpawnGroup struct { /* ... */ }
func (g *SpawnGroup) Spawn(ctx context.Context, opts SpawnOpts) (AgentHandle, error)
func (g *SpawnGroup) Join(ctx context.Context) ([]SpawnDone, error)
```

### §3.3 Construction

`New(...Option)` with `WithTree(*AgentTree)`, `WithNode(*AgentNode)`,
`WithSpawnDepth(int)`. `WithSpawnDepth` threads the current depth so children know
their own depth (§9).

### §3.4 Local spawn semantics

1. Depth check: `depth+1 > MaxDepth` → `ErrMaxDepthExceeded`.
2. Model validate: unknown model → `ErrUnknownModel`.
3. `CloneForChild` the permission gate; `Subset` the tool registry (§7).
4. Create the node (`StatusPending`), attach to parent.
5. Launch the child agent in a goroutine; emit `KindSpawned`.
6. On completion, `node.Finish(...)`, send `SpawnDone`.

### §3.5 Remote spawn semantics

1. Depth check + model validate (as local).
2. `lookupRemote(RemoteID)` → `ErrNoRemoteExecutor` if absent.
3. `lookupIntent(IntentPluginID)` → `NilIntentPlugin` fallback if absent.
4. `plugin.Resolve(ctx, opts)` **synchronously** → `*RemoteContext`; error →
   `ErrIntentResolveFailed`.
5. **Secret resolution (synchronous, before node creation — see §13.5).**
   Resolve `exec.TokenSecret`, `rc.Clone.TokenSecret`, `rc.WriteBackTokenSecret`,
   and `rc.SecretRefs` via the tree's `SecretsStore`. Any failure fails the spawn
   here — **no node is created** (assumption 45).
6. Create the node with `RemoteExec: true`.
7. Merge `RemoteContext` (+ resolved secrets) into a `RemoteDispatchRequest`.
8. `exec.Dispatch(ctx, req)` → `<-chan AgentEvent`; error →
   `ErrRemoteDispatchFailed`.
9. Pump the channel into the tree via `tree.Emit`; on stream loss →
   `ErrRemoteStreamLost` (§8).
10. `node.Finish(...)`, send `SpawnDone`.
11. **Then** async `plugin.Collect(ctx, done, rc)` — non-fatal (§8).

## §4 TUI

Package `internal/tui`.

### §4.1 Spatial tree

A three-zone-per-row layout:

```
<glyphs><label>  ·············  <status> <timing> <tokens>
```

- **Zone 1**: indentation glyphs + label. A `☁` prefix marks a remote node.
- **Zone 2**: dot-fill leader.
- **Zone 3**: status glyph + elapsed/wall time + token counts.

Status glyphs: `●` running, `◐` pending, `○` idle, `✓` done, `✕` error.

**Example:**

```
  fuse shell ──────────────────────────────── ● 2m14s  ↑4.2k ↓8.1k
  │
  ├─ [0] Review auth middleware ─────────── ✓ 47s  ↑1.1k ↓3.2k
  │   └─ [1] Check JWT libraries ────────── ✓ 12s  ↑0.4k ↓1.1k
  │
  └─ [2] ☁ Scan dependency versions ──────── ● 8s   ↑0.3k ↓0
```

Box-drawing edges use `isLastAtDepth []bool` stack rebuilt per node. Dot-fill
(`─`) connects label to right zone. Selection wraps the full row in reverse-video.

### §4.2 AgentsModel

A 40/60 horizontal split: **tree** (left 40%) | **detail** (right 60%).

Key bindings:

| Key | Context | Action |
|---|---|---|
| `j`/`k` / `↓`/`↑` | tree | navigate nodes |
| `g`/`G` | tree | jump to first/last |
| `Enter` | tree | open detail panel |
| `j`/`k` | detail | scroll detail |
| `q` | detail | close detail |
| `x` | any | cancel selected node's subtree |
| `Tab` | any | toggle focus zone |
| `Esc` | detail→tree→shell | progressive dismiss |

### §4.3 Detail pane

Header (label, model, status, totals) + a transcript with per-line `[elapsed]`
prefixes. Example:

```
Review auth middleware      ✓ 47s   ↑1.1k ↓3.2k   $0.042
────────────────────────────────────────────────────────
[00.0s] ▸ assistant  I'll start by reading the middleware…
[01.2s] ▸ tool_call  read_file({"path":"internal/auth/mw.go"})
[01.3s] ◂ result     package auth … (312 lines)
[47.0s] ▸ assistant  Found 2 issues: …
```

For remote nodes the header shows `[remote ☁]` and the `RemoteJobID`.

`agentsExitMsg{}` is the sentinel returned to the parent model on exit — no parent
pointer stored.

### §4.4 Transition mechanics

`ShellModel` holds `agents *AgentsModel` as an optional overlay. Enter via
`m.enterAgentsView()` (same code path for `/agents` slash command and `Tab`).
Exit via `agentsExitMsg{}`. `Tab` guard: only fires when 0010's `slashCompleter`
is inactive.

### §4.5 Inline summaries

Depth-1 children only get an inline two-line block in the parent transcript:

Running:
```
▶ spawn_agent("Review auth middleware for vulnerabilities")
  ● Running · 14s · ↑1.1k ↓0 tokens
```

Completed:
```
✓ spawn_agent · 47s · ↑1.1k ↓3.2k tokens
  Found 2 issues: missing rate limiting on /login, JWT secret in env fallback
```

Error:
```
✕ spawn_agent · 12s · ↑0.4k ↓0.1k tokens
  error: max turns reached
```

Remote labels carry `☁` prefix. `ToolResultMsg` for `spawn_agent` is suppressed
(the inline block is its rendering). `ShellModel` holds `inlineAgents
map[string]int` (nodeID → line index) and updates the block in-place.

## §5 Event routing

- Each child node has a `NodeRenderer` that stamps events onto its `AgentNode`
  and calls `tree.Emit`.
- The root uses a `MultiRenderer(TeaRenderer, NodeRenderer)` so the same events
  drive both the live TUI and the node/session record.
- `AgentTree.Emit` is **non-blocking** (buffered `out` channel, 256). If the
  buffer is full the update is coalesced via the dirty set — node state and the
  JSONL session log are the source of truth, not the channel.
- A **250 ms dirty-node ticker** flushes pending node refreshes to the TUI
  (eventual consistency; avoids per-event redraw storms).
- `ShellModel` waits on tree updates via `waitForTreeUpdate`, mirroring the
  existing `waitForMsg` pattern.
- **Remote events fan up identically to local events** — the goroutine in §3.5
  calls the same `tree.Emit` path, so remote nodes are indistinguishable from
  local ones downstream of `Emit` (aside from `RemoteExec`/`RemoteJobID`).

## §6 Session log

- Path: `~/.fuse/sessions/YYYY-MM-DD-<6-char-ulid>.jsonl`.
- One JSON object per line:
  `{ts, node_id, kind, name, payload}`, plus `remote` and `remote_job_id` for
  remote events.
- `session.Replay(path)` reconstructs an `AgentTree` from the log.
- A **7-day retention sweep** runs on startup, deleting session files older than
  the window.

**Per-line envelope:**

```json
{
  "ts": "2026-08-04T15:22:31.402Z",
  "node_id": "01J…ULID",
  "parent_id": "01J…ULID",
  "label": "Review auth middleware",
  "depth": 1,
  "kind": "tool_call",
  "name": "read_file",
  "payload": {"path": "internal/auth/mw.go"},
  "tokens": {"in": 1109, "out": 0}
}
```

Remote events add `"remote": true, "remote_job_id": "job_abc123"`.

## §7 Permissions & tool scoping

- `ApprovalCache.Clone()` — snapshot-copy of approvals for the child.
- `PermissionGate.CloneForChild(label)` — child gate seeded from the parent
  snapshot; child approvals do not mutate the parent. Approval prompts from a
  child are prefixed `[label]` so the user knows which agent is asking; the child
  node shows `◐` (waiting) while blocked.
- **Remote spawns do not proxy permissions** — the remote runner owns its own
  gate; `RemoteDispatchRequest.Tools` is advisory; enforcement is the remote's
  responsibility.
- `Registry.Subset(names []string) (*Registry, []string)` — `spawn_agent` is
  force-included; unknown names are dropped and returned; the caller records drops
  as a `KindError` node event.

## §8 Error cases

| Error | Cause | When |
|---|---|---|
| `ErrMaxDepthExceeded` | `depth+1 > MaxDepth` | synchronous, before node creation |
| `ErrUnknownModel` | model id not in registry | synchronous |
| `ErrNoRemoteExecutor` | `RemoteID` not registered | synchronous |
| `ErrRemoteDispatchFailed` | `Dispatch` returned an error | synchronous |
| `ErrRemoteStreamLost` | SSE stream lost past `MaxRetries` | async (event) |
| `ErrIntentResolveFailed` | `plugin.Resolve` returned an error | synchronous |
| `ErrSecretNotFound` | `SecretsStore.Get` returned not-found | synchronous |
| `ErrSecretsStoreFailure` | `SecretsStore` operation returned an error | synchronous |
| `ErrEncryptionFailure` | `EncryptionProvider.Encrypt` returned an error | synchronous |

`plugin.Collect` errors are **non-fatal**: they emit a `KindError` event + a
session-log line, but leave `SpawnDone.Err` unchanged. All secret-resolution
errors are **fatal and synchronous** — they fail the spawn before any node is
created (§13.5, assumption 45).

## §9 Depth limit

```go
const MaxDepth = 5
```

`WithSpawnDepth` threads the current depth into each child agent so the check in
§3.4/§3.5 is authoritative at every level. At `MaxDepth` the tree renderer emits
a truncation row (`└─ [↳ N deeper] ─── (collapsed)`) rather than recursing.

## §10 Wiring — `cmd/fuse/shell.go`

- **`atomic.Pointer[agent.Agent]`** resolves the spawn chicken-and-egg between
  the root agent and the `spawn_agent` tool's `SpawnFunc` (the pointer is
  installed immediately after `root` is built; `SpawnFunc` dereferences lazily).
- `MultiRenderer(TeaRenderer, NodeRenderer)` is installed at the root at startup.
- `tree.RegisterRemote("", exec)` registers the default remote executor at
  startup when `cfg.RemoteExecutor.URL != ""`.
- `tree.RegisterIntent("", plugin)` registers the default intent plugin.
- **`tree.SetSecrets(store)`** installs the configured `SecretsStore` at startup
  (§13.6). When `secrets.store` is unset, no call is made and `t.secretsStore()`
  lazily returns `&EnvSecretsStore{}`.

## §11 Remote execution (SSE event bridge)

### §11.1 RemoteExecutor interface

```go
type RemoteExecutor interface {
    Dispatch(ctx context.Context, req RemoteDispatchRequest) (<-chan AgentEvent, error)
}
```

### §11.2 SSERemoteExecutor

```go
type SSERemoteExecutor struct {
    BaseURL     string
    TokenSecret string       // secret name; resolved via SecretsStore at dispatch time
    HTTPClient  *http.Client // nil = default with 30s timeout
    MaxRetries  int          // default 3; exponential back-off (200/400/800ms)
    PublicKey   string       // optional: remote's age public key; enables encrypted bundle mode
}
```

**Dispatch flow:**

1. Resolve `TokenSecret` via `tree.secretsStore()` → bearer token.
2. `POST {BaseURL}/v1/agents/dispatch` with JSON `RemoteDispatchRequest` and
   `Authorization: Bearer <token>` → `202 {job_id, stream_url}`.
3. Open SSE at `stream_url` with `Accept: text/event-stream`.
4. Read events until `kind: done` or `kind: error`; convert each `data:` line to
   `AgentEvent` and send on the returned channel.
5. On stream drop, reconnect with `Last-Event-ID: <seq>` up to `MaxRetries`; past
   budget emit `ErrRemoteStreamLost` and close channel.
6. On cancel: `DELETE {BaseURL}/v1/agents/{job_id}` (best-effort) then close.

**SSE wire format** (what the remote must send):

```
id: <seq>
data: {"kind":"tool_call","name":"read_file","payload":{"path":"..."},"ts":"..."}

id: <seq>
data: {"kind":"done","name":"","payload":{"result":"...","summary":"...","cost_usd":0.042},"ts":"..."}
```

`kind` ∈ `spawned | assistant | tool_call | tool_result | tokens | done | error`.
The `done` payload matches `SpawnDone` fields.

### §11.3 RemoteDispatchRequest

```go
type RemoteDispatchRequest struct {
    // Task fields
    Label, Task, SystemPrompt string
    Tools                     []string
    ModelID                   string
    MaxTurns, MaxTokens       int
    ParentNodeID              string
    Depth                     int

    // Container (from IntentPlugin.Resolve)
    Image string
    Env   map[string]string

    // Repo context — exactly one of Clone or Bundle set; both nil = no repo context
    Clone  *GitCloneSpec // Clone.Token carries resolved value, not the secret name
    Bundle []byte        // base64 over the wire; for private/local repos

    // Write-back
    WriteBackBranch string
    WriteBackRemote string
    WriteBackToken  string // resolved value; never the secret name

    // Extra files injected at container start
    Files map[string][]byte // container path → content

    // Container secrets
    Secrets          map[string]string // plain; nil when EncryptedSecrets is set
    EncryptedSecrets []byte            // age-encrypted; nil when Secrets is set
    SecretsAlgorithm string            // "age" when EncryptedSecrets is set
}
```

`Clone.Token` and `WriteBackToken` on the request carry **resolved values**, never
secret names. Secret names live only on store-facing structs and are resolved
during the spawn path (§13.5).

### §11.4 Tree registry & config

- `tree.RegisterRemote(id, exec)` / `tree.lookupRemote(id)` (nil → `ErrNoRemoteExecutor`).
- Config (`remote_executor`):
  ```yaml
  remote_executor:
    url: "https://agents.example.com"
    token_secret: "fuse_remote_token"  # secret name
    public_key: "age1..."              # enables encrypted bundle mode
  ```
- `FUSE_REMOTE_URL` / `FUSE_REMOTE_TOKEN` env overrides. `FUSE_REMOTE_TOKEN`,
  when set, is treated as a literal token value and injected into `EnvSecretsStore`
  under the `fuse_remote_token` name — preserving historical env-override ergonomics.

## §12 IntentPlugin system

### §12.1 Interface — `internal/agent/intent.go`

```go
// IntentPlugin decides the execution context for a remote subagent and handles
// write-back after it completes. Integrators implement this to adapt fuse's
// remote subagent primitive to their workflow (docket, openspec, custom CI).
type IntentPlugin interface {
    Name() string
    // Resolve is called synchronously before dispatch. Errors fail the spawn.
    Resolve(ctx context.Context, opts SpawnOpts) (*RemoteContext, error)
    // Collect is called asynchronously after the remote agent completes.
    // Errors are non-fatal (KindError event + session log; SpawnDone.Err unchanged).
    Collect(ctx context.Context, done SpawnDone, rc *RemoteContext) error
}
```

### §12.2 RemoteContext

```go
type RemoteContext struct {
    Image                string
    Env                  map[string]string
    Clone                *GitCloneSpec
    Bundle               []byte
    WriteBackBranch      string
    WriteBackRemote      string
    WriteBackTokenSecret string            // secret name; resolved in spawn path
    Files                map[string][]byte // container path → content
    PluginMeta           map[string]string // opaque; passed to Collect; not sent to remote

    // SecretRefs: additional secret names the container needs beyond the git tokens.
    // Resolved via SecretsStore.ExportForContainer in the spawn path.
    SecretRefs []string
}
```

### §12.3 GitCloneSpec

```go
type GitCloneSpec struct {
    URL         string
    Ref         string // exact commit SHA (pinned at Resolve time for reproducibility)
    TokenSecret string // secret name; resolved in the spawn path
    // resolvedToken is set once during resolution; only its value travels in the
    // RemoteDispatchRequest.Clone.Token field — never the name.
    resolvedToken string
}
```

### §12.4 NilIntentPlugin (zero-overhead fallback)

```go
type NilIntentPlugin struct{}
func (NilIntentPlugin) Name() string { return "nil" }
func (NilIntentPlugin) Resolve(_ context.Context, _ SpawnOpts) (*RemoteContext, error) {
    return &RemoteContext{}, nil
}
func (NilIntentPlugin) Collect(_ context.Context, _ SpawnDone, _ *RemoteContext) error {
    return nil
}
```

An empty `RemoteContext` preserves the §11 remote-only behavior: the remote
receives only the task fields and infers everything else (assumption 29).

### §12.5 DocketIntentPlugin — `internal/integrations/docket/intent.go`

```go
type DocketIntentPlugin struct {
    GitRemoteURL      string
    GitToken          string // ${ENV_VAR}-expanded at wiring time (assumption 37)
    Image             string // e.g. "ghcr.io/ethanhinson/fuse-agent:latest"
    IntegrationBranch string // e.g. "main"
    ChangeSlug        string // e.g. "0012-subagent-ux" → branch feat/0012-subagent-ux
    PlanPath          string // plan file path; "" = not yet planned
}
```

`Resolve`:
1. `resolveRef(ctx, GitRemoteURL, GitToken, IntegrationBranch)` → pinned SHA.
2. Write-back branch: `feat/<ChangeSlug>`.
3. Inject plan file at `/workspace/<PlanPath>` if `PlanPath != ""` and file exists.

`Collect`: `fetchRef(ctx, rc.WriteBackRemote, rc.WriteBackToken, rc.WriteBackBranch)` — fetches the branch locally for review. No docket status mutations (those belong in the skill).

`ChangeSlug` and `PlanPath` are set **programmatically** by `docket-implement-next` at runtime, not as static config (assumption 33).

### §12.6 OpenSpecIntentPlugin — `internal/integrations/openspec/intent.go`

```go
type OpenSpecIntentPlugin struct {
    GitRemoteURL      string
    GitToken          string // ${ENV_VAR}-expanded at wiring time (assumption 37)
    Image             string
    IntegrationBranch string
    BriefTemplate     string // template with {task} substituted at resolve time
}
```

`Resolve`: write-back branch `remote/<sanitize(label)>` (avoids `feat/` collision
with docket — assumption 35); brief injected at `/workspace/.fuse/brief.md`.

### §12.7 Shared git helpers — `internal/integrations/git.go`

```go
// resolveRef returns the HEAD commit SHA of a remote branch via git ls-remote.
func resolveRef(ctx context.Context, remoteURL, token, branch string) (string, error)

// fetchRef fetches a remote branch into the local repo for review.
// Best-effort; Collect treats errors as non-fatal.
func fetchRef(ctx context.Context, remoteURL, token, branch string) error
```

Auth is injected via a temporary `GIT_ASKPASS` script (mode `0700`, deferred
removal) — the token never appears in argv or process listings (assumption 33).
`ctx` propagates cancellation to the git subprocess (assumption 36).

### §12.8 Config — nested under `remote_executor`

```yaml
remote_executor:
  url: "..."
  token_secret: "fuse_remote_token"
  public_key: "age1..."
  intent_plugin:
    kind: docket          # "docket" | "openspec" | "" (nil plugin)
    image: "ghcr.io/ethanhinson/fuse-agent:latest"
    git_remote_url: "https://github.com/owner/repo.git"
    git_token: "${GITHUB_TOKEN}"
    integration_branch: main
```

## §13 Secrets management

Package `internal/secrets`. Secrets are split into **two orthogonal concerns**:

- **`SecretsStore`** — *where* secrets live and how they are resolved by name.
- **`EncryptionProvider`** — *how* secrets are encrypted at rest and in transit.

Every raw token string elsewhere in this spec is a **secret-name reference**
resolved at dispatch time via `SecretsStore.Get`. The `EnvSecretsStore` subsumes
the previous `${ENV_VAR}` expansion behavior — no user migration required for
env-var users (assumption 43).

### §13.1 SecretsStore interface — `internal/secrets/store.go`

```go
package secrets

// SecretsStore resolves named secrets and exports them for container injection.
type SecretsStore interface {
    // Get returns the plaintext value for the named secret.
    Get(ctx context.Context, name string) (string, error)
    // List returns all known secret names (for audit/UI; values are not returned).
    List(ctx context.Context) ([]string, error)
    // ExportForContainer resolves named secrets and packages them for injection
    // into a remote container. If publicKey is non-empty (an age public key), the
    // export is encrypted for the remote to decrypt; otherwise it is plaintext.
    ExportForContainer(ctx context.Context, names []string, publicKey string) (*ContainerSecrets, error)
}

type ContainerSecrets struct {
    // Env: resolved plaintext values — transmitted in RemoteDispatchRequest.Secrets,
    // injected as container env vars by the remote runtime. TLS is the transport boundary.
    Env map[string]string
    // EncryptedBundle: non-nil when ExportForContainer was called with a publicKey.
    // Encrypted with the remote's age public key; the container decrypts with its
    // private key (mounted at a well-known path or injected separately).
    EncryptedBundle []byte
    // BundleAlgorithm: "age" when EncryptedBundle is set; "" otherwise.
    BundleAlgorithm string
}
```

### §13.2 EncryptionProvider interface — `internal/secrets/encryption.go`

```go
// EncryptionProvider handles encrypt/decrypt for secrets at rest and in transit.
// Implementations delegate to the backend that holds the key material.
type EncryptionProvider interface {
    Name() string
    Encrypt(ctx context.Context, plaintext []byte) (ciphertext []byte, err error)
    Decrypt(ctx context.Context, ciphertext []byte) (plaintext []byte, err error)
}
```

### §13.3 SecretsStore implementations

```go
// EnvSecretsStore reads secrets from environment variables.
// Secret names map 1:1 to env var names (case-sensitive).
// No encryption at rest — the OS process environment is the security boundary.
type EnvSecretsStore struct{}

// SopsSecretsStore reads from a SOPS-encrypted secrets file.
// SOPS handles backend selection via .sops.yaml (age, PGP, AWS KMS, GCP KMS, Azure Key Vault).
// Shells out to `sops --decrypt --output-type json <file>`.
// The decrypted file must be a flat key→value map; see assumption 44.
type SopsSecretsStore struct {
    FilePath       string // e.g. ~/.fuse/secrets.sops.yaml
    SopsConfigPath string // optional; empty = SOPS auto-discovers .sops.yaml
    // unexported: cache map[string]string (sync.RWMutex); fsnotify watcher
}

// EncryptedFileSecretsStore reads from a file encrypted by an EncryptionProvider.
// Useful when SOPS is not available but age/PGP encryption is desired.
type EncryptedFileSecretsStore struct {
    FilePath string
    Enc      EncryptionProvider
}
```

**`SopsSecretsStore` detail.** Expected secrets file format (flat, after decryption):

```json
{
  "github_token": "ghp_...",
  "fuse_remote_token": "...",
  "openai_api_key": "sk-..."
}
```

The decrypted map is **cached in memory** for the session (assumption 38).
Invalidated on fsnotify file-change event or explicit `Reload()`. SOPS backend
selection fully delegated to `.sops.yaml` discovery — fuse never selects the
KMS backend (assumption 39). `SopsConfigPath` allows config override for tests only.

### §13.4 EncryptionProvider implementations

```go
// AgeEncryptionProvider uses Mozilla age (https://age-encryption.org).
// PrivateKey is loaded from KeyFile at construction; held in memory only.
type AgeEncryptionProvider struct {
    KeyFile string // path to age secret key file
}

// SopsEncryptionProvider delegates to SOPS for encrypt/decrypt.
// Backend (age, PGP, KMS) selected by SOPS via .sops.yaml.
// Used internally by SopsSecretsStore; also available standalone.
type SopsEncryptionProvider struct {
    SopsConfigPath string // optional; empty = SOPS auto-discovers
}

// PassthroughEncryptionProvider performs no encryption.
// For dev/test only. ExportForContainer panics when publicKey is set
// (to catch misconfiguration early in tests — assumption 42).
type PassthroughEncryptionProvider struct{}
```

### §13.5 Secret resolution flow

Before `RemoteExecutor.Dispatch` is called, in `agent.Spawn` (§3.5 step 5),
**synchronously and before any node is created**:

1. Resolve `exec.TokenSecret` → bearer token:
   `bearerToken, err := tree.secretsStore().Get(ctx, exec.TokenSecret)`.
   On error: `ErrSecretNotFound` / `ErrSecretsStoreFailure` → fail spawn.
2. Resolve `rc.Clone.TokenSecret` → `GitCloneSpec.resolvedToken` (internal field;
   only its value travels in `RemoteDispatchRequest.Clone.Token`).
3. Resolve `rc.WriteBackTokenSecret` → `RemoteDispatchRequest.WriteBackToken`.
4. Resolve `rc.SecretRefs` via
   `tree.secretsStore().ExportForContainer(ctx, rc.SecretRefs, exec.PublicKey)`
   → `*ContainerSecrets`.
5. If `exec.PublicKey != ""`:
   `req.EncryptedSecrets = cs.EncryptedBundle`,
   `req.SecretsAlgorithm = cs.BundleAlgorithm`,
   `req.Secrets = nil`.
6. If `exec.PublicKey == ""`:
   `req.Secrets = cs.Env`, `req.EncryptedSecrets = nil`.

Any failure in steps 1–4 fails the spawn **before node creation** (assumption 45).
`ExportForContainer` with a non-empty `publicKey` always uses **age** for the
bundle, regardless of the store's at-rest `EncryptionProvider` (assumption 41).

### §13.6 Config additions — `internal/config/schema.go`

```go
type SecretsConfig struct {
    Store      string           `yaml:"store"`     // "env" | "sops" | "encrypted-file"
    SopsFile   string           `yaml:"sops_file"` // for store: sops
    Encryption EncryptionConfig `yaml:"encryption"`
}

type EncryptionConfig struct {
    Provider string `yaml:"provider"` // "age" | "sops" | "passthrough"
    KeyFile  string `yaml:"key_file"` // for age provider
}
```

Top-level `Config` gains `Secrets SecretsConfig \`yaml:"secrets"\``.

`RemoteExecutorConfig` gains:
- `PublicKey string \`yaml:"public_key"\`` — remote's age public key.
- `TokenSecret string \`yaml:"token_secret"\`` — secret name (was `token`).

**Full example config:**

```yaml
secrets:
  store: sops
  sops_file: ~/.fuse/secrets.sops.yaml
  encryption:
    provider: age
    key_file: ~/.fuse/key.age

remote_executor:
  url: "https://agents.example.com"
  token_secret: "fuse_remote_token"
  public_key: "age1..."
  intent_plugin:
    kind: docket
    image: "ghcr.io/ethanhinson/fuse-agent:latest"
    git_remote_url: "https://github.com/owner/repo.git"
    git_token: "${GITHUB_TOKEN}"    # ENV_VAR expansion; see assumption 37
    integration_branch: main
```

The loader constructs the `SecretsStore` from `secrets.store` and calls
`tree.SetSecrets(store)`. When `secrets` is absent, no store is set and
`t.secretsStore()` lazily defaults to `&EnvSecretsStore{}`.

## §14 File table

| Path | Purpose | Status |
|---|---|---|
| `internal/agent/tree.go` | `AgentTree`, `AgentNode`, `AgentEvent`, enums, `Emit`, dirty ticker, `SetSecrets`/`secretsStore`, remote + intent registries | new/modified |
| `internal/agent/spawn.go` | `Spawn`, `SpawnOpts`, `AgentHandle`, `SpawnGroup`, local + remote semantics, secret resolution | new |
| `internal/agent/remote.go` | `RemoteExecutor`, `SSERemoteExecutor`, `RemoteDispatchRequest`, SSE bridge, error sentinels | new |
| `internal/agent/intent.go` | `IntentPlugin`, `RemoteContext`, `GitCloneSpec`, `NilIntentPlugin` | new |
| `internal/integrations/git.go` | `resolveRef`, `fetchRef`, `GIT_ASKPASS` helper | new |
| `internal/integrations/docket/intent.go` | `DocketIntentPlugin` | new |
| `internal/integrations/openspec/intent.go` | `OpenSpecIntentPlugin` | new |
| `internal/secrets/store.go` | `SecretsStore`, `ContainerSecrets`, `EnvSecretsStore`, `SopsSecretsStore`, `EncryptedFileSecretsStore` | **new** |
| `internal/secrets/encryption.go` | `EncryptionProvider`, `AgeEncryptionProvider`, `SopsEncryptionProvider`, `PassthroughEncryptionProvider` | **new** |
| `internal/tools/spawn_agent.go` | `spawn_agent` tool, `SpawnFunc` injection | new |
| `internal/tui/agents_model.go` | `AgentsModel`, spatial tree layout, detail pane, key bindings, `agentsExitMsg` | new |
| `internal/tui/subagent_summary.go` | Inline depth-1 summaries, `inlineAgents` map, three render states | new |
| `internal/tui/renderer.go` | `NodeRenderer`, `MultiRenderer`, dirty ticker, `waitForTreeUpdate` | new/modified |
| `internal/session/log.go` | JSONL session log, `Replay`, retention sweep | new |
| `internal/permissions/cache.go` | `ApprovalCache.Clone` | modified |
| `internal/permissions/gate.go` | `PermissionGate.CloneForChild` | modified |
| `internal/tools/registry.go` | `Registry.Subset` | modified |
| `internal/config/schema.go` | `SecretsConfig`, `EncryptionConfig`, `RemoteExecutorConfig` additions, top-level `Secrets` | modified |
| `internal/config/loader.go` | Construct `SecretsStore` + `EncryptionProvider`; `${ENV}` git_token expansion | modified |
| `cmd/fuse/shell.go` | `atomic.Pointer` wiring, `MultiRenderer`, executor/intent/secrets registration | modified |

## §15 Testing notes

- **Depth limits** — spawn at `MaxDepth` boundary; `WithSpawnDepth` threads correctly.
- **Tool scoping** — `Subset` drops unknowns with a node event; `spawn_agent` always present.
- **Event routing** — buffer-overflow path; 250ms coalescing; no event content lost.
- **Remote SSE** — fake SSE server: 202 dispatch, streamed events, `Last-Event-ID` reconnect, DELETE on cancel, stream-loss → `ErrRemoteStreamLost`.
- **Intent plugin** — `Resolve` error → `ErrIntentResolveFailed`; `Collect` error → non-fatal (event + log).
- **Git helper token safety** — token never in argv; `GIT_ASKPASS` temp script exercised; `ctx` cancellation kills subprocess.
- **`SopsSecretsStore`** — mock `sops` binary via PATH injection; cache invalidation on file change; `List()` never returns values.
- **`AgeEncryptionProvider`** — encrypt→decrypt roundtrip; wrong key fails.
- **Secret resolution in `Spawn`** — missing `TokenSecret` → `ErrSecretNotFound` synchronously; partial resolution fails before Dispatch with no node created.
- **`ExportForContainer` with publicKey** — `req.Secrets` nil, `req.EncryptedSecrets` non-nil; matching private key decrypts.
- **`PassthroughEncryptionProvider` + publicKey** — panics (misconfiguration guard).
- **`agentsExitMsg` transition** — entering/leaving the agents view does not leave a dangling parent pointer.
- **Inline summary** — block updates in-place at `spawnLineIdx`; `ToolResultMsg` for `spawn_agent` is suppressed.

## Assumptions

1. `spawn_agent` is a normal tool subject to the same permission gate as any other; no special-casing beyond force-inclusion in `Subset`.
2. Child agents inherit the parent's model unless `model` is explicitly set in `SpawnOpts`.
3. A child's `system_prompt`, when set, fully replaces the inherited system prompt (not appended).
4. `SpawnFunc` injection is the chosen cycle-breaker between `tools` and `agent`, over an interface in a third package.
5. The `AgentTree` is the single source of truth for tree state; the TUI is a pure projection.
6. `TreeUpdate` channel buffer of 256 is sufficient for interactive workloads; overflow drops oldest and surfaces a counter rather than blocking the agent.
7. ULIDs are used for node IDs for lexicographic-sortable, time-ordered identity without coordination.
8. The 40/60 tree/detail split is fixed (not user-resizable) in this change.
9. `x` cancels only the selected node's subtree (cancel propagates to descendants via context).
10. Depth-1-only inline summaries keep the parent transcript readable; deeper nodes are visible only in the tree view.
11. `ToolResultMsg` suppression for `spawn_agent` applies both inline and in the parent transcript.
12. Session logs are per-run, not per-tree; a replayed log reconstructs one tree.
13. 7-day retention is swept on startup, best-effort, non-fatal on error.
14. `ApprovalCache.Clone` is a snapshot copy; later parent approvals do not retroactively propagate to already-spawned children.
15. Remote spawns do not proxy local permissions; the remote runner owns its gate.
16. `Registry.Subset` silently force-includes `spawn_agent` even if omitted from the names list.
17. `MaxDepth = 5` is a compile-time constant in this change (not configurable).
18. `WithSpawnDepth` is the sole depth-threading mechanism; depth is not read back from the tree.
19. `atomic.Pointer[agent.Agent]` is the chosen resolution for the root agent/tool chicken-and-egg.
20. A single default remote (`""`) and default intent (`""`) are registered at startup; named ones are a future extension.
21. `RemoteExecutor.Dispatch` returns a receive-only channel that the spawn path owns and drains; the executor closes it on completion.
22. SSE reconnect uses `Last-Event-ID`; the remote is responsible for replaying from that sequence without redelivering the last-seen event.
23. `MaxRetries` bounds reconnect attempts; exhaustion is `ErrRemoteStreamLost` (an async event, not a synchronous error).
24. `DELETE /v1/agents/{job_id}` is the cancel contract; the remote is responsible for teardown.
25. `RemoteDispatchRequest.Bundle` is base64-encoded over the wire.
26. Remote events fan up through `tree.Emit` identically to local events; the tree cannot distinguish origin except via `AgentNode.RemoteExec`.
27. `IntentPlugin.Resolve` runs synchronously on the dispatch path; a slow plugin blocks the spawn goroutine's launch but not the parent agent's turn (Spawn returns a handle quickly).
28. `IntentPlugin.Collect` runs asynchronously after `SpawnDone`; its errors are non-fatal and do not change `SpawnDone.Err`.
29. `NilIntentPlugin` returns an empty `RemoteContext`, making a "remote spawn with no intent" well-defined; the remote infers everything.
30. `DocketIntentPlugin` pins the integration branch to a SHA at resolve time so the remote clones a stable ref.
31. Write-back branch naming is plugin-specific (`feat/<slug>` for docket, `remote/<sanitize(label)>` for openspec).
32. Injected files use absolute container paths under `/workspace`.
33. Git auth uses a `GIT_ASKPASS` temp script so tokens never appear on the command line or in process listings. The script is created in `os.MkdirTemp`, given `0700`, and deferred-removed.
34. `IntentPluginConfig` is nested under `remote_executor` in the config schema.
35. `OpenSpecIntentPlugin` write-back uses `remote/<label-slug>` to avoid colliding with docket's `feat/` namespace when both integrations coexist.
36. `resolveRef`/`fetchRef` shell out via `exec.Command("git", ...)`; a git binary is assumed present on the dispatch host. `ctx` propagates cancellation.
37. **`git_token` in `IntentPluginConfig` retains `${ENV_VAR}` expansion** via `os.ExpandEnv` at wiring time for backwards-compatibility with the IntentPlugin pass. This is an inconsistency to resolve in a follow-on cleanup change (both `git_token` and `token_secret` should use `SecretsStore`).
38. **`SopsSecretsStore` caches the decrypted secrets map for the session** (not re-decrypted per call). Cache invalidated on fsnotify file-change event and on explicit `Reload()`. In-memory only.
39. **SOPS backend selection is fully delegated to SOPS** via `.sops.yaml` discovery. fuse never configures the KMS backend. `SopsConfigPath` allows override for test isolation only.
40. **`List()` returns secret names, never values** — for audit/UI display only.
41. **`ExportForContainer` with a non-empty `publicKey` always uses age encryption** regardless of the store's configured `EncryptionProvider`. The store's provider governs at-rest encryption only; age is the wire format for the encrypted bundle.
42. **`PassthroughEncryptionProvider.ExportForContainer` panics when `publicKey` is set** — developer guard against misconfiguration; not a runtime error path.
43. **`EnvSecretsStore` maps secret names 1:1 to env var names, case-sensitive.** This matches existing `${ENV_VAR}` behavior; no migration required.
44. **`SopsSecretsStore` requires a flat JSON/YAML secrets file** (key→string at the top level; no nesting). Nested SOPS files are not supported in this change.
45. **Secret resolution happens synchronously in `agent.Spawn`, before any node is created.** A missing or unresolvable secret fails the spawn without creating a dangling node in the tree.
