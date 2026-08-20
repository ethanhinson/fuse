<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0072 — Docker subcommand classification — read-only forms auto-approve; the rest reach the classifier instead of dying at the parse floor](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0072-docker-subcommand-classification.md)**
<!-- docket:backlink:end -->

# Docker subcommand classification — design

**Change:** 0072 · **Date:** 2026-08-20 · **Author:** settled in-session with the human after the
0070 dogfood finding

## Problem

`docker` is in `arbitraryArgWrappers`, so any command containing a docker segment fails the whole
parse (`ErrUnparseable` → ask at `LayerParse`). The parse floor precedes every other layer, so this
fail-closed is unreachable by both the classifier (parse asks are never routed to it) and the
user's `auto_approve` config (evalRules consumes segments that never exist). On a docker-compose
project every docker invocation — including provably read-only ones like
`docker compose config --quiet` — costs one guaranteed prompt.

## Decision

Docker is not an arbitrary-arg wrapper in the `sudo`/`xargs` sense: its payload executes inside a
container boundary, not on the host. It is a subcommand-shaped CLI like git, and gets git's
treatment: parse normally, deterministically allow an enumerated read-only set, and route
everything else to the probabilistic layer with full pattern-matching reach.

### 1. Parse (shellparse.go)

Remove `"docker"` from `arbitraryArgWrappers`. Docker segments flow through the standard pipeline:
opaque-arg marking, redirect write-target capture, control-flow descent — all of 0070's machinery
applies unchanged.

### 2. Read-only proof (rules.go)

`isReadOnlySafe` gains `case "docker": return isSafeDocker(seg.Args)` — a flag-inspecting name, so
it sits after the blanket opaque-arg rejection (an opaque word could BE a subcommand).

`isSafeDocker`:
- `args[0]` must be a bare subcommand word. **Any leading `-` flag defeats the proof** (`-H
  tcp://…`, `--context prod`, `-c`): global flags change which daemon/context the command hits and
  carry separate-word values whose arity we refuse to model (the 0070 `nice -n 5` lesson —
  wrapper-peel-needs-arity-model). Result: false → heuristic → classifier. Conservative, costs one
  classifier call.
- `readOnlyDockerSubcommands`: `ps`, `images`, `version`, `info`, `inspect`, `logs`, `diff`,
  `history`, `port`, `top` → true with any trailing args (no mutating flag form exists; operands
  are container/image names, and reading them is not a mutation).
- `compose`: `args[1]` must be a bare word in `readOnlyComposeSubcommands`: `config`, `ps`, `ls`,
  `logs`, `images`, `top`, `port`, `version`. A flag before the compose subcommand
  (`compose -f x.yml config`) → false (same rule as above).
- Everything else (`run`, `exec`, `build`, `pull`, `push`, `rm`, `system`, `volume`,
  `compose up/down/exec/run/build`, unknown) → false.

### 3. Heuristic (heuristics.go)

`classifyHeuristic`'s scoping loop gains `case "docker": return VerdictAsk` beside
`pkill`/`killall`. Rationale: a non-read-only docker segment's operands are images, containers,
services, and volumes — **names, not paths** (containment-proof-needs-a-real-resolved-path). Path-
scoping `compose`/`up` as cwd-relative words would resolve them against the workspace and wrongly
prove containment for `docker volume rm`, `docker system prune -f`, `docker compose down -v` — a
deterministic-allow fail-open. Nothing mutating may pass this layer; the classifier owns the gray
area. Redirect write-targets are still scoped before the switch (D4 ordering), so
`docker ps > /etc/x` asks on the target even though `ps` is read-only.

### Resulting verdict map

| Command | Layer | Verdict |
|---|---|---|
| `docker compose config --quiet` | safelist | allow |
| `cd <ws> && docker compose config --quiet 2>&1; echo ok` | heuristic | allow (cd path-scoped in-root; docker read-only; echo read-only) |
| `docker ps`, `docker logs api`, `docker compose ps` | safelist | allow |
| `docker compose config > out.yml` (in-root) | heuristic | allow (target scoped) |
| `docker compose config > /etc/x` | heuristic→classifier | ask on the write target |
| `docker compose -f x.yml config` | heuristic→classifier | classifier decides (flag defeats the static proof) |
| `docker run alpine sh`, `docker compose up -d`, `docker system prune -f` | heuristic→classifier | classifier decides; ask when none wired |
| `docker inspect $C` | heuristic→classifier | opaque arg → classifier/ask |
| config `deny: ["bash:docker *"]` | rules | deny (now reachable — segments exist) |
| config `auto_approve: ["bash:docker compose *"]` | rules | allow for literal-arg forms (opaque/redirect declines still apply) |

## Security argument

The only new deterministic-allow surface is the two enumerated read-only sets, admitted solely in
the bare-subcommand form. Every mutating, unknown, flagged, opaque, or redirect-carrying docker
form lands at ask-or-classifier — strictly wider than today's behavior only in the direction of
the probabilistic layer that 0069 deliberately allow-biased, and strictly narrower than today for
nothing. Config deny patterns GAIN reach they never had (they could not match an unparseable
command).

## Out of scope

Host-execution wrappers (`sudo`, `xargs`, `npx`, `eval`, `exec`, `watch`) stay fail-closed at
parse; docker global-flag arity modelling; podman/nerdctl/kubectl.
