---
id: 49
slug: allowlist-admission-on-deterministic-allow-paths
title: Admission sets on a no-human deterministic-allow path are allowlists, not denylists
status: Accepted
date: 2026-08-19
supersedes: []
reverses: []
relates_to: [44, 48]
change: 70
---

## Context

Change 0070 widened auto-mode's shell parser (`internal/permissions/shellparse.go`) so that
env-prefixed commands (`FOO=1 make`) could be evaluated at all, instead of unconditionally failing
closed with `ErrUnparseable`. The approved spec called for a **denylist** of dangerous variable
names — `LD_PRELOAD`, `DYLD_*`, `PATH`, `IFS`, `GIT_SSH_COMMAND`, `NODE_OPTIONS`, and similar — and
that is what was built (commit e555b57).

Adversarial review of the branch found the denylist failed open on the toolchain exec hooks it had
not enumerated. Both

    CC=/tmp/evil make
    GOFLAGS=-toolexec=/tmp/evil go build ./...

reached `VerdictAllow` at the heuristic layer with no human in the loop, and would execute an
arbitrary out-of-workspace binary. That defeats the one containment property the parser otherwise
holds: a path-qualified `argv[0]` and a bare `bash script.sh` both fail closed *precisely* so the
agent cannot exec an arbitrary out-of-tree binary. `MAKEFLAGS`, `CGO_CFLAGS`, `CGO_LDFLAGS`, `LD`,
`AR`, and `GOENV` are the same shape. Since the pre-0070 behavior was an unconditional
`ErrUnparseable`, this was a widening the change never intended to make.

The decisive property is the *shape of the failure*, not the length of the list. The set of
environment variables that change **what code runs** cannot be enumerated — it grows with every
toolchain, language runtime, and build system the agent may ever encounter — and every name someone
forgets is a fail-**open** with no human present to catch it. The original code's own comment
predicted exactly this: "enumeration is exactly how this denylist would silently rot."

## Decision

**On any deterministic-allow path that runs with no human present, an admission set is an allowlist
of inputs established inert — never a denylist of inputs known dangerous.** Denylists are
appropriate only where the fail direction points *toward* the human: where a missed entry costs a
prompt, not an unreviewed action. The test is not "have we listed the dangerous cases?" but "when
this set is wrong, who finds out?"

Applied to change 0070: `dangerousEnvVars` / `dangerousEnvVarPrefixes` were replaced with an
allowlist of names established inert **for the auto-approve path specifically** — `CGO_ENABLED`,
`GOCACHE`, `GOMAXPROCS`, `GOOS`, `GOARCH`, `NO_COLOR`, `TERM`, `TZ`, `LANG`, `LC_*`, `CI`, and
similar — with every unrecognised name failing closed to the human. Both consumers,
`assignsAreBenign` (the `FOO=1 make` prefix path) and `peelEnv` (the `env FOO=1 make` path), share
the one helper, so the two admission sets cannot drift apart. Note that `CGO_ENABLED` is inert while
`CGO_CFLAGS` and `CGO_LDFLAGS` are not, so no bare `CGO_` prefix is admitted — prefix entries are
only ever added where every name under the prefix is inert. Implemented in commit 7db9070.

## Consequences

- The fail direction is bounded and correct: an unrecognised input costs exactly one human prompt,
  where previously it could cost silent arbitrary code execution.
- The cost is friction. A legitimate but unlisted variable — a project-specific
  `MY_APP_ENV=staging` — now prompts where the denylist would have passed it. That is the intended
  trade: an allowlist grows by deliberate human addition, which is a reviewable act, whereas a
  denylist grew by remembering, which is not.
- The rule generalizes past env vars. Any future widening of a no-human deterministic-allow path —
  flag admission, subcommand admission, host admission, operand admission — is designed as an
  allowlist, and a proposed denylist on such a path is a review finding on its face.
- It also bounds review cost: the correctness question for an allowlist is "is each admitted entry
  inert?", which is answerable, rather than "is the excluded set complete?", which is not.
- Same fail-open family as the containment work in change 0068 (containment proofs over operands
  that are not paths) and the `web_fetch` host floor of ADR-0048, where deterministic approval is
  likewise granted only from an established-good set.
