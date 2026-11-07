# fuse

fuse is a multi-model coding agent you control: it drives LLMs through a
LiteLLM-compatible gateway with file, shell and research tools, a subagent runtime,
human-in-the-loop permissions, MCP servers and skills. Use it as an interactive shell,
or give it a task and let it run to completion. There is no server: one process does one
job, which also makes it simple to run as one container per task.

## Installing

Releases ship prebuilt `fuse` binaries for darwin and linux on amd64 and arm64, with
SHA-256 checksums and build provenance attestations. No Go toolchain is required.

### Install script (recommended)

```sh
curl -fsSL https://raw.githubusercontent.com/ethanhinson/fuse/main/scripts/install.sh | sh
```

The script detects your OS/arch, resolves the latest release, downloads the
archive **and** its checksums file, verifies the SHA-256 digest, and refuses to
install anything on a mismatch, a missing entry, or a malformed digest. It
installs `fuse` into `$HOME/.local/bin` by default, warns if that directory is not on
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

Every archive contains `fuse` and `README.md` at its root.

### From source

```sh
go install github.com/ethanhinson/fuse/cmd/fuse@latest
```

## Building

```sh
make build      # build the fuse binary
make install    # install to your GOPATH/bin
go test ./...   # run the test suite
```

Configuration lives in `~/.fuse/config.yml` (with an optional per-repo
`.fuse.local.yml` override). See `internal/config` for the full schema.

Cutting a release is documented in [docs/releasing.md](docs/releasing.md).

## Tracing

fuse can export OpenTelemetry traces of its runs over OTLP. It is off by default. See
[docs/observability.md](docs/observability.md).

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
bound they prompt; with only a policy stand-in (the headless
mcp-server, the research probe, piped stdin) they are denied at the rules layer
with `unparseable command denied; no approver bound: <command>`, or with the
matching rule's reason when one fires. `auto_approve` never admits them in auto
mode; in smart mode only a first-token `auto_approve` entry such as `bash:sudo`
does. The command in every deny reason is trimmed, capped at 200 characters with "…", and has URL credentials replaced with `***@`, matching the decision event's `command` field. A matching `always_prompt` or
`auto.ask` rule is final in smart and auto mode alike: it prompts a human when
one is bound, and when the gate's approver is a policy stand-in (the headless
mcp-server, the research probe, piped stdin) it is denied with the matched pattern in the reason
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

**Stopping a one-shot run.** SIGINT, SIGTERM and SIGHUP cancel the run, stop its bash commands and MCP servers, and exit with 128 plus the signal number (130, 143, 129), after printing the reason to stderr. A second signal exits at once, skipping teardown. `--max-run-duration 30m`, or `max_run_duration: 30m` in `~/.fuse/config.yml`, ends the run the same way with exit 1; `--max-run-duration 0` turns a configured limit off for that run, and a repo's `.fuse.local.yml` cannot set it. Nothing outside a run's process group is touched, so a command that moved itself to a new session is on its own.

**`--approve-all` and `cli/` models.** `--approve-all` runs the session at mode off. With a `cli/` model, approve-all is carried to the `fuse mcp-server` tool subprocess, which runs with permissions off for that call. The relay environment is `FUSE_HITL_SOCKET`, `FUSE_HITL_PROVENANCE` and `FUSE_HITL_MODE`; `fuse mcp-server` honors `FUSE_HITL_MODE` only when `FUSE_HITL_SOCKET` is also set and only for the value `off`. That check tests only that the socket variable is non-empty, so `FUSE_HITL_MODE=off` is a permission-loosening path trusted from whoever controls the server's process environment; the parent fuse relay normally sets both.

**Trust boundary.** Anyone can ship a `.fuse.local.yml` in a repository, so that
file is untrusted. It cannot choose the gateway, models, MCP servers, skills,
instructions, research endpoints, summarizer, roles, spawn or token limits, or any
permission-loosening key such as `mode`, `session_allow`, `auto_approve` or the `auto`
block. Those are ignored there with a startup warning naming them. It can only
tighten: `permissions.always_prompt`, `permissions.disabled`, `auto.fetch_deny`,
`auto.fetch_ask`, lower throughput limits, `no_tools` and `finish_check`. Put anything
that grants trust in your own `~/.fuse/config.yml`, and put repository guidance for
the agent in `AGENTS.md`.

Sandbox settings are read only from `~/.fuse/sandbox.local.yml`. A
`.fuse/sandbox.local.yml` in a repository is ignored with a warning, so cloning a
repository cannot turn off bash containment. Auto mode also asks before an edit tool
writes inside `.git`, `.fuse` or `.fuse.local.yml`, because those affect later
processes, including git hooks that run outside any sandbox.

One loosening
path does not come from config: `fuse mcp-server` runs at mode off when its
environment has `FUSE_HITL_MODE=off` and any non-empty `FUSE_HITL_SOCKET`, so
it trusts whoever controls that process environment (normally the parent fuse
relay).

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
  max_spawns: 16        # tree-global spawn budget: the total number of child
                        # agents one root turn may create, ever. The runtime
                        # appends a line like `agent budget: 7/64 used (57
                        # remaining)` to every spawn_agent result so the model
                        # can stop before it fans out too wide, and refuses the
                        # spawn outright once the ceiling is reached. Bounds
                        # runaway fan-out in the research flow. Once the budget is
                        # exhausted the spawn_agent tool is stripped from the
                        # model's schema for the rest of the session (a permanent
                        # brake, since the tree is append-only).
  max_concurrent: 8     # live-concurrency cap: the number of child agents that
                        # may RUN at once, bounded by a semaphore, independently
                        # of the total max_spawns budget. When the active child
                        # count (running + pending) reaches this cap the
                        # spawn_agent tool is stripped for that turn and reappears
                        # once children finish (a reversible brake). A negative
                        # value is clamped back to the default.
```

### Gateway retries

```yaml
gateway:
  max_attempts: 6     # total tries per model request; 1 means no retries
  retry_backoff: 1s   # base wait; doubles each attempt up to 30s, with jitter
```

Transport errors, timeouts, 429s and 5xx responses are retried, and a stream that ends
without finishing counts as a failure. A `Retry-After` header is honored up to 2 minutes.
A streamed reply with no text and no tool call gets one retry without streaming, because
some gateway routes end a reasoning stream early. If replies stay empty, the agent asks
twice more and then the run fails with a nonzero exit.

### The `instructions` config key

```yaml
instructions: |
  You are a code reviewer. Flag correctness problems first, then style.
```

The text is added to the main agent's system prompt after the persona prompt; subagents
keep their own prompts. It is read from `~/.fuse/config.yml`, not `.fuse.local.yml`.
It may be at most 16384 characters.

### AGENTS.md

At the start of a one-shot run or a shell session, fuse also adds project instructions
to the main agent's system prompt, in this order: `~/.fuse/AGENTS.md` if present, then
every `AGENTS.md` from the repository root (the nearest ancestor containing `.git`,
including a linked worktree's `.git` file) down to the working directory. With no
repository, only the working directory's file is read, and the walk never goes above
the repository root. Each file is wrapped in a `<project_instructions path="...">` tag
so the model can see where it came from. Spawned subagents do not receive it.

A missing or empty file changes nothing. The total is capped at 32KB; past that the last
file is truncated with a visible note and a warning on stderr. A repository `AGENTS.md`
that resolves through a symlink outside the repository is skipped with a warning.

Like `.fuse.local.yml`, an `AGENTS.md` comes from the repository, so it is untrusted
input to the model: it steers the model's behaviour but grants no permissions, and the
permission rules above decide what tool calls are actually allowed.

### The `roles` config key

```yaml
roles:
  reviewer:
    description: Reviews one part of a change for correctness, then style.
    instructions: |
      You review the files you are given. Report correctness problems first.
    tools: [read_file, grep]   # empty or omitted: inherit the main agent's tools
    model: fast                # empty or omitted: inherit the main agent's model
```

A role says who a helper is. The description is what the main agent reads when choosing
whom to hand work to; it is required and may be at most 512 characters. Instructions may be
at most 16384 characters, and a config may define at most 32 roles. Role names use lowercase
letters, digits, `-` and `_`. A non-empty `tools` list is the helper's whole tool set and
can only narrow it; the helper can start its own helpers only when the list names
`spawn_agent`. Tool names and model aliases are not checked when the config is loaded.
Roles are read from `~/.fuse/config.yml`, not from `.fuse.local.yml`.

When roles are defined, `spawn_agent` offers a `role` parameter that lists each role's
description. The role's instructions become the helper's brief, in the place the top-level
`instructions` takes for the main agent. The call's `tools` may only narrow a role's list.
The call's `model` wins over the role's. `role` cannot be combined with `system_prompt`.

`no_tools: true` makes an agent tool-less: it is offered no tools at all and its requests
carry none. At the top level of a config it makes the run tool-less. A role has three
states: an empty `tools` list inherits the main agent's tools, a non-empty list narrows
them, and `no_tools: true` gives it none. The key is honoured from every config
layer. The interactive shell and `research-probe` ignore it and print a warning, and
`cli/` models refuse it. A tool-less helper started with `expects` is offered only
`return_result`, so callers still get a structured result back.

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

In `fuse shell` you can talk to an agent while it works. A message you type is read at the
agent's next step. `/interrupt MESSAGE` cancels what the agent is doing right now (the model
call or the running command) so the message lands at once. Steps the agent had not started
yet are skipped, and every tool call still gets a result, so the conversation stays valid.

## MCP servers

The shell and one-shot runs attach the MCP servers in `mcp_servers`, and their tools are
offered to the model as `mcp:<server>/<tool>`. A server that cannot connect is skipped and
logged; the run continues. Authentication is `none`, `bearer` or `static` (both send
`client_secret` as a bearer token), or `oauth2`.

```yaml
mcp_servers:
  - name: docs
    transport: streamable-http
    url: https://mcp.example.com
    auth: {type: bearer, client_secret: "${DOCS_TOKEN}"}
```

## License

MIT — see [LICENSE](LICENSE).
