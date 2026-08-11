# fuse

fuse is a terminal-based, multi-model AI agent harness: an interactive shell
that drives LLMs (via a LiteLLM gateway) with tools, a hardened subagent
runtime, human-in-the-loop permissions, MCP integration, and an embedded skill
system.

## Building

```sh
make build      # build the fuse binary
make install    # install to your GOPATH/bin
go test ./...   # run the test suite
```

Configuration lives in `~/.fuse/config.yml` (with an optional per-repo
`.fuse.local.yml` override). See `internal/config` for the full schema.

## Research

`/research <query>` produces a cited research report. It diversifies the
question into several facets, fans out one subagent per facet to search the web
and fetch the most promising pages, deduplicates sources, and synthesizes a
single markdown report with `[N]` citation markers and a numbered source list.
The flow is driven by the existing subagent runtime and an embedded `research`
skill; a user skill named `research` shadows the built-in one. The skill is
available in both the interactive shell (via `/research`) and one-shot mode: a
one-shot task whose intent matches the skill's description (e.g. `fuse "do a
deep research on ..."`) loads and follows the skill automatically, because the
model is instructed to call the `skill` tool first for any matching request.

Two built-in tools back the flow: `web_search` (query a search provider) and
`web_fetch` (fetch a URL and extract the main article text, with a robots.txt
gate on by default).

### Search providers

You bring your own search API key. Provider resolution happens at first use and
fails loudly if nothing is configured, in this order:

1. an explicit `research.provider` in config (`brave` | `tavily` | `custom`)
2. the `BRAVE_SEARCH_API_KEY` environment variable
3. the `TAVILY_API_KEY` environment variable
4. a configured `[research.custom]` block
5. otherwise, a clear error naming all of the setup paths above

**Brave Search (primary, recommended).** Set `BRAVE_SEARCH_API_KEY`. Sign up at
api-dashboard.search.brave.com (a card is required; roughly $5/month of free
credits). fuse uses the Brave Search engine that also powers the web search in
some other AI assistants; here you supply your own key directly. This README's
Brave Search mention also serves as the attribution Brave's free credits ask
for.

**Tavily (alternative).** Set `TAVILY_API_KEY`. Tavily returns extracted page
content alongside results, which lets the research flow skip a separate fetch
for sources whose returned content already suffices.

**Custom / self-hosted (e.g. SearXNG).** Configure `[research.custom]` with a
URL template. For a SearXNG instance only the `url` is needed - the JSON field
mappings default to SearXNG's `format=json` response shape:

```yaml
research:
  custom:
    url: "https://searx.example.com/search?q={query}&format=json"
```

### The `[permissions]` config block

```yaml
permissions:
  mode: smart           # off | prompt-all | smart | auto (default: smart)
  session_allow: true   # whether the [s]ession "allow for this session" option appears
  auto_approve: []      # per-segment allow patterns (see note below): "bash:git *", ...
  always_prompt: []     # patterns demoted to always-prompt, e.g. "bash:git push*"
  disabled: []          # tool names fully disabled (never runnable), e.g. "web_fetch"
  auto:                 # auto-mode surface (only consulted when mode: auto)
    classifier_model: deepseek-flash   # alias that judges gray-area commands; NEVER a chat alias
    deny: []            # extra always-deny per-segment patterns, e.g. "bash:npm publish*"
    ask: []             # extra always-ask per-segment patterns (override an allow)

# Per-project overrides (user-owned ~/.fuse/config.yml ONLY — see below).
# Absolute project path -> a permissions subtree that applies when the shell
# runs inside that path. "auto here, not there" without weakening any repo.
projects:
  /Users/me/work/trusted-app:
    permissions:
      mode: auto        # this project starts in auto; others keep the global default
      auto:
        classifier_model: deepseek-flash


`mode: auto` runs commands without a human prompt when they are provably safe.
It is layered: a bash command is split into its simple-command **segments**
(across `&&`, `||`, `;`, `|`, newlines, and the body of `bash -c`/`sh -c`), and
**each segment is evaluated independently** — static deny/ask rules first, then
the read-only safe list, then path/egress heuristics, and only genuinely
ambiguous segments reach the `classifier_model`. Deny beats ask beats allow; a
command that cannot be parsed fails closed.

**`auto_approve` (and `auto.deny`/`auto.ask`) are per-segment, not first-token
prefixes.** A pattern only approves the segment it matches, so `git status &&
rm -rf ~` is **not** auto-approved by `bash:git *`: the `git status` segment
matches and the `rm -rf ~` segment does not, and one un-approved segment denies
the whole command. There is no way to whitelist a leading `git` into approving a
trailing `rm`. Wrapping (`sh -c "rm x"`), command substitution (`$(...)` /
backticks), env-assignment prefixes (`FOO=bar cmd`), and path-qualified argv0
(`/usr/bin/rm`) all fail closed rather than slip past a prefix match.

**Trust boundary.** The permission-*loosening* keys — `mode`, `session_allow`,
`auto_approve`, and the entire `auto` block — are honored **only** from the
trusted `~/.fuse/config.yml`. A repo-plantable `.fuse.local.yml` cannot weaken
the gate: those keys are ignored there (with a startup warning), so a checked-in
file cannot flip a clone into `auto` mode or self-approve. Only the *tightening*
keys `always_prompt` and `disabled` take effect from `.fuse.local.yml`. Set
anything that grants trust in your own `~/.fuse/config.yml`.

**Per-project trust (`projects:`).** To grant "auto here, not there" you can key
a `permissions:` subtree by **absolute project path** under a `projects:` map.
When the shell starts inside a path that equals — or is a descendant of — one of
those keys, that entry's permission subtree is merged in as **trusted** (the full
subtree, `mode` and the whole `auto` block included), layered above the global
`permissions:` and below the tighten-only `.fuse.local.yml`. When several keys
are ancestors of the current directory, the **longest (most specific) key wins**;
matching is by whole path segments, so a key `…/b` never matches a directory
under `…/bc`, and symlinked working directories are resolved to their real path
before matching. Because this is pure loosening, the map is honored **only** from
your own `~/.fuse/config.yml` — a `projects:` block planted in a repo's
`.fuse.local.yml` is ignored and named in the startup warning, exactly like every
other loosening key, so a checked-in file still cannot flip a clone into `auto`.

**Switching mode in-session.** `permissions.mode` is only the **startup
default** — the permission mode is a live session surface you can flip without
restarting, and the interactive shell shows the active mode in its status line
(e.g. `mode: auto`):

- **Shift+Tab** toggles between the two everyday postures, `smart` ⇄ `auto`.
  From `prompt-all` or `off`, the first Shift+Tab lands on `smart`, and
  Shift+Tab thereafter toggles `smart` ⇄ `auto`.
- **`/mode`** (bare) prints the active mode and lists all four options;
  **`/mode <name>`** sets any of `smart`, `auto`, `prompt-all`, `off` directly.
  An unknown name is rejected with a usage line and leaves the mode unchanged.

A switch takes effect on the **next turn** — the gate is rebuilt per turn at the
current session mode, so a flip into `auto` immediately governs the following
tool calls (no restart, no stale gate). If you switch into `auto` but the
gateway has no classifier configured, the status line marks the mode
**degraded** (`mode: auto` with a degraded marker) — the deterministic rules and
read-only safe list still apply, but gray-area commands fail closed to a prompt
rather than reaching a classifier.

### The `[research]` config block

```yaml
research:
  provider: ""          # "" = auto (Brave env -> Tavily env -> custom); or brave | tavily | custom
  max_queries: 5        # facet count hint for the research flow
  max_results: 5        # results per web_search call
  max_content_kb: 50    # web_fetch content truncation (KB, word-boundary)
  respect_robots: true  # honor robots.txt in web_fetch; set false for dev/testing
  custom:
    url: ""             # template with {query} / {count}; empty = not configured
    headers: {}         # optional request headers
    results_path: results   # dotted path to the results array (SearXNG default)
    title_field: title      # per-result field mappings (SearXNG defaults)
    url_field: url
    snippet_field: content
```

### The `[agents]` config block

```yaml
agents:
  max_spawns: 64        # tree-global spawn budget: the total number of child
                        # agents one root turn may create, ever. The runtime
                        # appends a line like `agent budget: 7/64 used (57
                        # remaining)` to every spawn_agent result so the model
                        # can stop before it fans out too wide, and refuses the
                        # spawn outright once the ceiling is reached. Bounds
                        # runaway fan-out in the research flow. Once the budget is
                        # exhausted the spawn_agent tool is stripped from the
                        # model's schema for the rest of the session (a permanent
                        # brake, since the tree is append-only).
  max_concurrent: 16    # live-concurrency cap: the number of child agents that
                        # may RUN at once, bounded by a semaphore, independently
                        # of the total max_spawns budget. When the active child
                        # count (running + pending) reaches this cap the
                        # spawn_agent tool is stripped for that turn and reappears
                        # once children finish (a reversible brake). A negative
                        # value is clamped back to the default.
```

### Observing the research flow — `research-probe`

The research flow is emergent and prompt-driven: the model diversifies a
question into facets, fans out one subagent per facet, and each child searches
and fetches on its own. That is hard to watch in the interactive shell (it
scrolls past). `research-probe` runs the **real** flow — the
embedded `research` skill, the real `web_search`/`web_fetch` tools against your
configured provider, the real `spawn_agent` fan-out, talking to the live
gateway — headless and fully recorded, then prints an inspectable digest:

```sh
export TAVILY_API_KEY=...        # or BRAVE_SEARCH_API_KEY
fuse research-probe --model kimi "What is Litestream and how does it back up SQLite?"
```

It prints the spawn tree (root + one node per facet, with status), the census
of searches and fetches, every unique search query and fetched URL, and the
root's final synthesized report — or a clear "did not synthesize a report" line
when the flow fans out but never converges. Flags: `--model <alias>` picks the
driver model, `--trace <file>` also writes the raw gateway request/response
JSON, and `--timeout <dur>` bounds the whole run (default 3m). Nothing about the
agents is faked — the recorder is just an `agent.Renderer` layered over the
production wiring, so what you see is exactly what `/research` does.

## Networked loop-control — `loop-serve-net` (Connect/protobuf)

`fuse loop-serve-net` exposes the same policy-free multi-loop runtime that
`loop-server` serves over stdio, but over the network as a
[Connect](https://connectrpc.com) service (`fuse.loop.v1`, IDL in
`proto/fuse/loop/v1/loop.proto`). It is browser-reachable over HTTP/2 with no
proxy and speaks Connect, gRPC, and gRPC-Web (served over h2c):

```sh
fuse loop-serve-net --addr 127.0.0.1:8787
```

The service is three RPCs:

- `StartLoop` / `Send` — unary; `tenant` is a typed pass-through field.
- `Observe(from_seq)` — server-streaming history-then-live: it replays durable
  history since `from_seq`, then live-tails, deduping the replay/live overlap at
  the watermark and flagging a `gap` when a sequence hole is detected. A
  reconnecting client (any instance, resolved cross-instance via the durable
  store) simply re-opens `Observe` from its last-seen seq — this single stream
  subsumes both the live tail and catch-up replay. Idle streams receive periodic
  `keepalive` frames so a parked loop survives a gateway idle timeout.

The wire stubs are generate-and-commit (`make proto`, requires `buf` +
`protoc-gen-go`/`protoc-gen-connect-go` on PATH and `cd proto && npm ci` for the
TS `protoc-gen-es` plugin); committed Go stubs live in `internal/loopwire/v1`
and TS stubs in `proto/gen/ts`.

This replaces the earlier JSON-over-WebSocket wire (change #48, ADR-0032
superseded): there is no `/ws` or `/loops/{id}/events` route anymore.
