# fuse

fuse is a terminal-based, multi-model AI agent harness: an interactive shell
that drives LLMs (via a LiteLLM gateway) with tools, a hardened subagent
runtime, human-in-the-loop permissions, MCP integration, and an embedded skill
system.

## Installing

Releases ship prebuilt `fuse` binaries for darwin and linux on amd64 and arm64,
a multi-arch container image on GHCR, and SHA-256 checksums with build
provenance attestations. No Go toolchain is required.

### Install script (recommended)

```sh
curl -fsSL https://raw.githubusercontent.com/ethanhinson/fuse/main/scripts/install.sh | sh
```

The script detects your OS/arch, resolves the latest release, downloads the
archive **and** its checksums file, verifies the SHA-256 digest, and refuses to
install anything on a mismatch, a missing entry, or a malformed digest. It
installs three binaries — `fuse` plus both egress forwarders (see the caveat
below) — into `$HOME/.local/bin` by default, warns if that directory is not on
your `PATH`, and never invokes `sudo`. It does not read or write
`~/.fuse/config.yml`.

Three environment variables tune it:

| Variable | Effect |
| --- | --- |
| `FUSE_VERSION` | Install a specific version (`v0.1.0` or `0.1.0`) instead of the latest release. |
| `FUSE_INSTALL_DIR` | Install destination. Default `$HOME/.local/bin`. Point it at `/usr/local/bin` yourself if you want that — the script will not elevate for you. |
| `FUSE_RELEASE_BASE_URL` | Override the download base (an `https://` URL, a `file://` URL, or a plain local directory such as a GoReleaser snapshot `dist/`). With this set, `FUSE_VERSION` must be set too unless the version can be read from the local tree. |

### Manual archive download

Archive names carry the version **without** a leading `v`, while the release
download path keeps the tag verbatim:

```sh
VERSION=0.1.0
curl -fsSLO "https://github.com/ethanhinson/fuse/releases/download/v${VERSION}/fuse_${VERSION}_darwin_arm64.tar.gz"
curl -fsSLO "https://github.com/ethanhinson/fuse/releases/download/v${VERSION}/fuse_${VERSION}_checksums.txt"

# Verify the bytes, then the provenance.
# (macOS has no sha256sum; use `shasum -a 256 --ignore-missing -c` there.)
sha256sum --ignore-missing -c "fuse_${VERSION}_checksums.txt"
gh attestation verify "fuse_${VERSION}_darwin_arm64.tar.gz" --repo ethanhinson/fuse

tar -xzf "fuse_${VERSION}_darwin_arm64.tar.gz"
```

Every archive contains `fuse`, `fuse-egress-forward-linux-amd64`,
`fuse-egress-forward-linux-arm64`, and `README.md` at its root — the darwin
archives included, because the forwarder that matters is the one matching the
*sandbox image's* architecture, not the host's. Keep the three binaries in the
same directory.

### Container image

```sh
docker run --rm ghcr.io/ethanhinson/fuse:latest version
docker run --rm -p 8787:8787 ghcr.io/ethanhinson/fuse:latest \
  loop-serve-net --addr 0.0.0.0:8787
```

`loop-serve-net` defaults to `--addr 127.0.0.1:8787`, which a published port
cannot reach from outside the container, so bind `0.0.0.0` explicitly when you
publish it.

Tags are `:X.Y.Z`, `:X.Y`, and `:latest`, each a multi-arch manifest over
linux/amd64 and linux/arm64. A pre-release tag (`v0.2.0-rc.1`) publishes
`:X.Y.Z` **only** — it moves neither of the floating tags, `:X.Y` or `:latest`,
so pinning `:X.Y` never gets you a release candidate. The entrypoint is `/fuse`,
so arguments to `docker run` are fuse's own subcommands.

The image is `gcr.io/distroless/static-debian12:nonroot` (UID 65532) with no
shell and **no container runtime** inside it. Because the bash tool's security
boundary is a container (ADR-0044), a bash tool used by a fuse running *inside*
this image needs either the remote sandbox substrate (change #75) or a mounted
container socket — and a mounted socket is approximately host root, a tradeoff
the deployment change (#76) owns rather than inherits silently. See the comment
block in [`Dockerfile`](Dockerfile).

**Deploying the server.** A docker-compose dev stack and a Helm chart both ship
in [`deploy/`](deploy) and run this image unmodified — see
[docs/deploying.md](docs/deploying.md) for both quickstarts, the config-as-Secret
model, the bash-tool tri-state, and the rolling-update semantics.

### From source

```sh
go install github.com/ethanhinson/fuse/cmd/fuse@latest
```

**Postgres needs a build tag.** The `pgstore` event store is behind `-tags
pgstore`. The released binaries, the image, and `make build` all carry it; a
plain `go install` does not, so it produces an **fsstore-only** binary that
ignores a Postgres DSN. `fuse version` prints the truth on its third line
(`backends: fsstore,pgstore` vs `backends: fsstore`). If you want Postgres from
source:

```sh
go install -tags pgstore github.com/ethanhinson/fuse/cmd/fuse@latest
```

**Forwarder caveat.** `go install` builds only the `fuse` binary. fuse locates
the in-container egress forwarder beside its own executable and nowhere else —
`<exeDir>/dist/fuse-egress-forward-linux-<arch>`, then
`<exeDir>/fuse-egress-forward-linux-<arch>` (see the `egress-forwarder` comment
in [`Makefile`](Makefile) and `defaultEgressForwarderCandidates` in
[`cmd/fuse/sandbox.go`](cmd/fuse/sandbox.go)). Without it, `egress.mode: enforce`
is still *safe* but is **deny-all**: containers run with `--network none` and no
declared destination is reachable. fuse says so loudly on stderr at startup
rather than looking like a broken network. Build and place the artifact with:

```sh
make egress-forwarder                                  # writes dist/fuse-egress-forward-linux-{amd64,arm64}
cp dist/fuse-egress-forward-linux-* "$(dirname "$(command -v fuse)")/"
```

The install script and the release archives do this for you.

## Building

```sh
make build      # build the fuse binary
make install    # install to your GOPATH/bin
go test ./...   # run the test suite
```

Configuration lives in `~/.fuse/config.yml` (with an optional per-repo
`.fuse.local.yml` override). See `internal/config` for the full schema.

Cutting a release is documented in [docs/releasing.md](docs/releasing.md).

## Loop observability

`loop-serve-net` can independently expose Prometheus metrics, OpenTelemetry traces, and payload-free structured logs. The source-controlled [development-only reference stack](docs/observability.md) provisions Prometheus, Grafana, an OTEL Collector, and Tempo (intentionally not Loki). See [observability operations](docs/observability.md) for cardinality limits, rotation, authenticated replay, and the Grafana-to-Tempo workflow.

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
  auto_approve: []      # per-segment allow patterns, strict glob (see below): "bash:git *", ...
  always_prompt: []     # protective patterns demoted to always-prompt, e.g. "bash:git push*"
  disabled: []          # tool names fully disabled (never runnable), e.g. "web_fetch"
  auto:                 # auto-mode surface (only consulted when mode: auto)
    classifier_model: deepseek-flash   # alias that judges gray-area commands; NEVER a chat alias
    deny: []            # extra always-deny protective patterns, e.g. "bash:npm publish*"
    ask: []             # extra always-ask protective patterns (override an allow)
    fetch_deny: []      # host-glob floor: matched web_fetch hosts are denied before
                        # the classifier is ever consulted, e.g. ["*.internal.example"]
    fetch_ask: []       # host-glob floor: matched web_fetch hosts are forced to a
                        # human ask before the classifier is ever consulted
    allow_push: false   # true = `git push` auto-approves; false = it routes to the classifier
    write_roots: []     # extra dirs scoped like the workspace (e.g. ["/tmp"]); the
                        # per-session scratch dir under ~/.fuse/tmp/ is always included

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

**`web_fetch` known-good hosts auto-approve with no prompt and no classifier
call.** A hardcoded seed of reputable hosts (docs sites, code hosting, etc.)
plus the bundled top-sites popularity set auto-approve `web_fetch` at the
floor, before the classifier is ever consulted — this is a zero-review GET of
any path/query under that host. `auto.fetch_deny` and `auto.fetch_ask` sit
ahead of that floor and override it: a host matching `fetch_deny` is always
denied and a host matching `fetch_ask` always forces a human prompt,
regardless of the seed or popularity list.

**`auto_approve` (and `auto.deny`/`auto.ask`) are per-segment, not first-token
prefixes.** A pattern only approves the segment it matches, so `git status &&
rm -rf ~` is **not** auto-approved by `bash:git *`: the `git status` segment
matches and the `rm -rf ~` segment does not, and one un-approved segment denies
the whole command. There is no way to whitelist a leading `git` into approving a
trailing `rm`. Wrapping (`sh -c "rm x"`), command substitution (`$(...)` /
backticks), env-assignment prefixes (`FOO=bar cmd`), and path-qualified argv0
(`/usr/bin/rm`) all fail closed rather than slip past a prefix match.

**How patterns match (every mode alike).** A bash command is matched per
parsed segment against `bash:<argv0> <args>`, for example `bash:rm -rf a/b`.
Protective patterns (`always_prompt`, `auto.deny`, `auto.ask`) are also
matched against the segment head `bash:<argv0>` and against the whole command,
and their `*` matches any run of characters **including `/`**, so
`deny: ["bash:rm *"]` denies `rm -rf a/b` and `ask: ["bash:npm publish*"]`
matches `npm publish ./pkg/dir`. Protective patterns also match the command as written, before wrappers such as
`timeout`, `nice`, `env` and `bash -c` are peeled, so a rule naming the wrapper
fires in every mode for wrapper forms the shell parser accepts (`timeout`,
`nice`, `nohup`, `time`, `stdbuf`, `env` with inert assignments, `bash -c`).
Commands the parser refuses (`sudo`, `npx`, `xargs`, a path-qualified argv0,
`env` with non-inert assignments, `bash` without `-c`) are matched by protective
rules against the command text as written in every mode. With a human approver
bound they prompt; with only a policy stand-in (the loop servers, the headless
mcp-server, the research probe, piped stdin) they are denied at the rules layer
with `unparseable command denied; no approver bound: <command>`, or with the
matching rule's reason when one fires. `auto_approve` never admits them in auto
mode; in smart mode only a first-token `auto_approve` entry such as `bash:sudo`
does. The command in every deny reason is trimmed, capped at 200 characters with "…", and has URL credentials replaced with `***@`, matching the decision event's `command` field. A matching `always_prompt` or
`auto.ask` rule is final in smart and auto mode alike: it prompts a human when
one is bound, and when the gate's approver is a policy stand-in (the loop servers, the headless
mcp-server, the research probe, piped stdin, the orchestrator's deny stand-in) it is denied with the matched pattern in the reason
(`matched ask rule <pattern>; no approver bound`). No later layer (the read-only
safelist, smart mode's allow step, heuristics or classifier) can override it. Allow patterns (`auto_approve`) are strict: their `*` stops at
`/`, so `bash:git *` does not approve `git clone https://host/repo` and
`bash:rm build/*` does not approve `rm build/../x`, and every segment must
match on its own. `?` matches one character and `[...]` classes work as in a
shell glob; a malformed pattern matches nothing. "Every mode alike" covers
these segment, whole-command, and head subjects. Smart mode additionally keeps
its legacy subjects for `always_prompt` and `auto_approve`, the bare tool name
and the raw first token of the command (so `bash:git` still approves
a command that starts with `git`, subject to the exceptions below), which auto
mode does not use; apart from those exceptions no existing smart-mode rule
changes meaning. In smart mode a catastrophic segment (one the parser accepts)
is never approved by an allow rule: smart mode prompts when a human is bound and
denies at the rules layer when the gate's approver is a policy stand-in, and an `rm` or
`dd` with an unresolved shell expansion no longer matches a first-token allow. Protective rules match generously because they only ever tighten;
allow rules match strictly because a match grants permission. Provenance belongs to each gate's approver, so a child built with the always-approve stand-in inside an attended session is a policy gate, and `decided_by` on its `permission.decision` events says `policy`. A policy stand-in that answers no is recorded as `tool call denied by policy approver`; a person who answers no is `tool call denied by user`.

**`--approve-all` and `cli/` models.** `--approve-all` runs the session at mode off. With a `cli/` model, approve-all is carried to the `fuse mcp-server` tool subprocess, which runs with permissions off for that call. The relay environment is `FUSE_HITL_SOCKET`, `FUSE_HITL_PROVENANCE` and `FUSE_HITL_MODE`; `fuse mcp-server` honors `FUSE_HITL_MODE` only when `FUSE_HITL_SOCKET` is also set and only for the value `off`.

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

The probe answers approvals with a policy stand-in: an `always_prompt` or
`auto.ask` match, and in smart mode a catastrophic command, is denied with the
pattern in the reason. Move the pattern to `auto_approve` or drop the rule to
let it run.
A command the permission parser refuses (`sudo`, `npx`, `xargs`, a
path-qualified binary, `env` with non-inert assignments) is denied the same way,
with `unparseable command denied; no approver bound: <command>`: the probe
cannot run one unless it runs in smart mode with a first-token `auto_approve`
rule naming it, such as `bash:sudo`.

It prints the spawn tree (root + one node per facet, with status), the census
of searches and fetches, every unique search query and fetched URL, and the
root's final synthesized report — or a clear "did not synthesize a report" line
when the flow fans out but never converges. Flags: `--model <alias>` picks the
driver model, `--trace <file>` also writes the raw gateway request/response
JSON, and `--timeout <dur>` bounds the whole run (default 3m). Nothing about the
agents is faked — the recorder is just an `agent.Renderer` layered over the
production wiring, so what you see is exactly what `/research` does.

## Steering a running agent

You can talk to an agent while it works, and you can hand that job to an
**orchestrator**: a second model that watches the agent and steers it for you.
This works in every way you can run fuse.

Two things to know first:

- **A steer** is a message to a running agent. It is read at the agent's next
  step. Add **interrupt** and fuse cancels what the agent is doing right now
  (the model call or the running command) so the message lands at once. Steps
  the agent had not started yet are skipped, and every tool call still gets a
  result, so the conversation stays valid.
- **The orchestrator** is a chat. It has three tools: look at what the agent is
  doing, read one event in full, and steer the agent. It has no shell and no
  file access. Its messages reach the agent labelled as guidance from the
  orchestrator, spoken for you, and newer than the agent's earlier instructions.

| How you run fuse | Steer | Chat with the orchestrator |
|---|---|---|
| `fuse shell` | type while it works; `/interrupt MESSAGE` to cut in | `/orchestrator` (stays on until `/orchestrator off`), or `/orchestrator MESSAGE` for one message |
| `fuse "task"` | start with `--control /tmp/run.sock`, then `fuse steer --socket /tmp/run.sock --loop ID [--interrupt] "msg"` | `fuse orchestrate --socket /tmp/run.sock --loop ID` |
| `fuse loop-serve-net` (Docker, compose, Helm) | `fuse steer --url URL --token T --loop ID`, or `Send` with `interrupt` | `fuse orchestrate --url URL --token T --loop ID`, or start a loop with `orchestrate` set (below) |
| `fuse loop-server` (stdio) | `loop.send` with `"interrupt": true` | `loop.start` with `"orchestrate": "<loop id>"` |
| Go and TypeScript SDKs | `client.Interrupt` / `client.SendWith` (Go), `client.interrupt` / `client.sendWith` (TS) | `StartLoopConfig.Orchestrate` (Go), `orchestrate` in `startLoop` (TS) |

The one-shot run prints the exact `fuse steer` line, with its loop id, when it
starts with `--control`. The control socket is private to your user (mode 0600).

Starting an orchestrator on a server makes it a loop like any other. It lives in
the same tenant as the loop it watches and needs the same permission to drive
it, then you chat with it through `Send` and `Observe`:

```go
worker, _ := client.StartLoop(ctx, fuse.StartLoopConfig{Task: "migrate the schema", Model: "minimax", Interactive: true})
orch, _ := client.StartLoop(ctx, fuse.StartLoopConfig{Task: "Tell me what it is doing.", Model: "deepseek-flash", Orchestrate: worker})
client.Send(ctx, orch, "Tell it to run the tests before it finishes. Interrupt it if it is mid-edit.")
```

Pick the orchestrator's model in `~/.fuse/config.yml` (home file only, so a
repository cannot choose which model steers your agent):

```yaml
orchestrator:
  model: deepseek-flash   # an alias from `fuse models`; empty means the session's model
```

`fuse orchestrate` runs the orchestrator model on your machine, using your
gateway config, and reaches the agent only through the wire. `--model` overrides
the alias for one session.

### More than one replica

A hosted fuse can run as many replicas as you like (the Helm chart ships an
autoscaler), and a steer or a chat message can reach any of them.

- **Each loop runs on one replica**, the one that started it. The replicas share
  Postgres, so every replica can read every loop.
- **A message that lands on a different replica is not lost or refused.** That
  replica writes it into the loop's stream (a `steer.request` event) and answers
  right away. The replica that runs the loop picks it up within a moment. The
  request is stored before you are told it was accepted, and the agent's next
  `user.input` event records that it was delivered, so nothing is delivered twice,
  even across a restart. An orchestrator on one replica can steer a loop on
  another the same way.
- **When a replica goes away, its chat loops move.** On a clean shutdown (a
  scale-down or rollout) the replica marks its loops as no longer running before
  it exits. After a crash, the loop is treated as abandoned once its lease runs
  out (30 seconds by default). Either way, the next message to any replica
  resumes the loop from its stream, with its full conversation. Work that was
  half done on the replica that went away is not continued; a non-chat run that
  was in progress is lost.
- **Use Postgres.** Replicas on the filesystem store share nothing in a normal
  cluster. The Helm chart will not render without a database (`postgres.dsn`,
  `postgres.existingSecret` or the dev-only `postgres.dev.enabled`). Raise
  `replicaCount`, or set `autoscaling.enabled: true`, to run more than one.

## Networked loop-control — `loop-serve-net` (Connect/protobuf)

`fuse loop-serve-net` exposes the same policy-free multi-loop runtime that
`loop-server` serves over stdio, but over the network as a
[Connect](https://connectrpc.com) service (`fuse.loop.v1`, IDL in
`proto/fuse/loop/v1/loop.proto`). It is browser-reachable over HTTP/2 with no
proxy and speaks Connect, gRPC, and gRPC-Web (served over h2c):

Neither `loop-server` nor `loop-serve-net` has a human approver, so an operator
`always_prompt` or `auto.ask` rule cannot prompt anyone: a matching call is
denied with the pattern in the reason, in smart mode (the default) and auto mode
alike, and the model sees that reason as the tool result. To let such a command
run unattended, move its pattern to `auto_approve` or remove it.
A command the permission parser refuses (`sudo`, `npx`, `xargs`, a
path-qualified binary, `env` with non-inert assignments) is denied the same way,
with `unparseable command denied; no approver bound: <command>`: a hosted loop
cannot run one unless the loop runs in smart mode with a first-token
`auto_approve` rule naming it, such as `bash:sudo`.

```sh
fuse loop-serve-net --addr 127.0.0.1:8787
```

### Authentication and multi-tenancy (change #49)

Unlike the stdio `loop-server` and the local CLI paths, this networked binding
**requires a bearer token on every request** — identity and authorization live at
the Connect edge, never in the policy-free runtime seam. Present the credential as
an `Authorization: Bearer <token>` header; a missing or unknown token is rejected
with `Unauthenticated` before the handler runs.

The token→principal map and the owner-liveness lease TTL come from the trusted
`~/.fuse/config.yml` `loop_server:` block (a credential-bearing surface — a
repo-plantable `.fuse.local.yml` cannot mint tokens):

```yaml
loop_server:
  lease_ttl: "30s"            # owner-liveness lease; empty ⇒ 30s runtime default
  auth:
    - token: s3cr3t-alice     # the bearer credential a client presents
      tenant: acme            # isolation boundary; empty ⇒ the _default tenant
      subject: alice          # authorization subject, recorded as a loop's owner
      observability_operator: true # grants global logging reload/reopen
    - token: s3cr3t-bob
      tenant: acme
      subject: bob
```

Each principal resolves to a `tenant` (the isolation boundary every store/registry
op is keyed by) and a `subject` (recorded as a loop's owner). Authorization is then:

- **Tenant spoof** — a request whose wire `tenant` differs from the token's tenant
  is rejected `PermissionDenied`.
- **Cross-tenant** — reaching a loop under a different tenant returns `NotFound`
  (the tenant-scoped resolve simply misses it; a tenant cannot even learn another
  tenant's loop exists).
- **Cross-owner** — a same-tenant caller that is not the loop's recorded owner is
  rejected `PermissionDenied`.

The **lease TTL** is the owner-liveness window: the owning instance renews the
lease while a loop is live, and a loop whose lease has expired is treated as
abandoned and re-ownable by a cold instance on reconnect (so a crashed owner does
not strand a loop). Reconnecting a dropped `Observe` is just re-opening
`Observe(from_seq)` with the same token — the stream dedups replay/live at the
watermark, so there is no loss or duplication across the handoff.

If neither `loop_server.auth` nor `loop_server.issuers` is set, a single built-in dev token (`fuse-dev-token`,
mapped to the `_default` tenant) is synthesized so local development works out of
the box — the server still authenticates every request; it never runs open.
Configure real tokens for any shared or deployed server.

#### Trusted token issuers

Static tokens need a config change and a restart for every new caller. When
another service (a CMS, a web app) signs its own users in and starts loops on
their behalf, declare it as a **trusted issuer** instead: it shares an HS256
secret with fuse and mints a short-lived token per request that names the user.
New users then need no change on the fuse side.

```yaml
loop_server:
  auth: [...]                       # static tokens keep working and are checked first
  issuers:
    - issuer: cms                   # must equal the token's iss
      signing_key: <random secret>  # HS256 shared secret, at least 32 bytes
      audience: fuse-loop           # must equal the token's aud
      tenants: [_default]           # tenants this issuer may assert; empty means only _default
      max_ttl: 1h                   # refuse tokens whose exp - iat is longer (default 1h)
      observability_operator: false # granted to every principal of this issuer (default false)
```

The bearer token is a compact JWS. fuse refuses it unless all of this holds:

| Part | Rule |
| --- | --- |
| header `alg` | exactly `HS256` (`none`, `RS256`, a missing alg and every other value are refused) |
| `iss` | names a configured issuer; that issuer's key must verify the signature |
| `sub` | non-empty string; becomes the principal's subject, recorded as the loop's owner |
| `aud` | equals the issuer's `audience` (a string, or an array that contains it) |
| `iat`, `exp` | both required; `exp` in the future, `iat` at most 30s in the future, `exp - iat` at most `max_ttl` |
| `nbf` | optional; honored when present |
| `tenant` | optional, default `_default`; must be in the issuer's `tenants` |

The result is the same `{tenant, subject}` principal a static entry gives, so
ownership and tenant checks work unchanged. A token cannot grant
`observability_operator`; only the issuer's config entry can. Every refusal is
the same `Unauthenticated` error with no detail. Configuring any issuer also
turns off the built-in dev token. Like `auth`, `issuers` is honored only from
`~/.fuse/config.yml`.

Minting a token in TypeScript (Node, no library needed):

```ts
import { createHmac } from "node:crypto";

function fuseLoopToken(secret: string, sub: string, tenant?: string): string {
  const b64 = (v: object) => Buffer.from(JSON.stringify(v)).toString("base64url");
  const now = Math.floor(Date.now() / 1000);
  const claims = { iss: "cms", sub, aud: "fuse-loop", iat: now, exp: now + 300, ...(tenant ? { tenant } : {}) };
  const input = `${b64({ alg: "HS256", typ: "JWT" })}.${b64(claims)}`;
  return `${input}.${createHmac("sha256", secret).update(input).digest("base64url")}`;
}

// fetch(`${fuseUrl}/fuse.loop.v1.LoopService/StartLoop`, {
//   method: "POST",
//   headers: { Authorization: `Bearer ${fuseLoopToken(secret, user.id)}`, "Content-Type": "application/json" },
//   body: JSON.stringify({ task: "..." }),
// });
```

Mint a fresh token per call (or cache one for less than its lifetime). A
long-running `Observe` stream is authenticated once, when it opens.

The service is four RPCs:

- `StartLoop` / `Send` — unary. The authoritative tenant is the authenticated
  token's; the wire `tenant` field, when set, must match it (a mismatch is a
  spoof, see above).
- `GetCapabilities` - unary and read-only, with the same authentication as
  `StartLoop`. It starts no loop. For the authenticated tenant it returns:
  - models: alias, gateway id, effective context window, effective max output
    (the entry's `max_tokens`, else the top-level `max_tokens`), persona,
    default flag, and the optional `label` / `description` / `tags`
  - installed skills
  - one ceiling per tighten-only field, keyed by the refusal field path
    (`operator_set: false` means a spec cannot set that field). Ceilings are
    strict: a spec value must be strictly below the ceiling's value, and a
    value equal to it is refused
  - `secret_ref` names for the tenant, names only (a host-restricted secret
    still resolves only for its hosts)
  - spec MCP auth kinds
  - server version and digest algorithm

  `cli/` routes are never listed. Model entries carry optional display keys,
  and `advertise: false` hides an entry from the listing:

  ```yaml
  models:
    # Well-known tags: tools, vision, reasoning, long-context.
    fast: {id: cloud/fast, label: Fast coder, description: Cheap and quick, tags: [tools, long-context]}
    internal: {id: cloud/internal, advertise: false}
  ```
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

### MCP on every binding, with the real per-principal identity (change #59)

MCP tool execution is attached on **every** loop binding — the shell, the one-shot
CLI, the research probe, and both loop-servers — through one composition-root helper,
`mcpAttach(cfg, w)` in `cmd/fuse`. It resolves, together, the identity-propagation
egress seam (`mcp.WithCredentialSource`, when an identity-propagating MCP server is
configured — change #52) **and** the complete-mediation `TargetMediator` gate, so no
binding can wire a manager without also wiring the egress credential seam and the
target-allowlist gate. A misconfiguration is logged loud at startup, never silent.

On the **loop-server** paths this is first-class and **per-loop**: each hosted loop
gets its **own** `mcp.Manager`, its tools registered into **that loop's own** cloned
tool registry, and the manager closed at loop teardown — two concurrent loops never
share a manager, a registry, or a credential. Crucially, the loop-server threads the
**real authenticated principal** the Connect edge resolved (`sub` = the initiating
user, `act` = fuse) into the egress, so each outbound MCP `tools/call` presents a
short-lived, **audience-bound, per-principal delegation token** minted for that user
and tenant — never a shared static token and never the `_default` shim. The minted
token lives only on the outbound transport header: it never appears in the event
stream, the durable store, logs, or model context.

The identity travels as a **context value** stamped by the composition root
(`toolidentity.WithPrincipal`) through the runtime's policy-free `LoopContext` /
`LoopTeardown` seams — `internal/runtime` imports no auth, MCP, or `cmd/fuse` type
(ADR-0030 preserved). A permanent CI acceptance lane
(`cmd/fuse/loop_serve_net_rentals_acceptance_test.go`) drives two authenticated
principals concurrently through the loop-server binding against a real stateful
rentals MCP server (`internal/mcpdemo/rentals`, importable by Wander), under `-race`,
asserting the full #52 checklist — distinct per-principal tokens, downstream 403 as a
distinguishable tool error, per-principal write isolation, wrong-audience rejection,
complete mediation of an undeclared target even under auto-approve, and no credential
leak — so a regression kills the build.

## License

MIT — see [LICENSE](LICENSE).
