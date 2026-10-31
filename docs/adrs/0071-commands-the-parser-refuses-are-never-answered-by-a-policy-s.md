---
id: 71
slug: 'commands-the-parser-refuses-are-never-answered-by-a-policy-s'
title: 'Commands the parser refuses are never answered by a policy stand-in'
status: 'Accepted'
date: '2026-10-31'
supersedes: []
reverses: []
relates_to: [70, 68]
change: 96
---

## Context

The parser refuses commands it cannot decompose honestly (arbitrary-arg wrappers such as `sudo`, `npx`, `xargs`; a path-qualified argv0; a non-inert `env` assignment; an opaque command word; `bash` without `-c`; process substitution; over-length input). Auto mode asked for them before reading any rule, and smart mode's floor could not see them, so under a policy stand-in (loop servers, headless mcp-server, research probe, piped stdin) the most dangerous shapes were approved while their plain forms were denied.

## Decision

- A command the parser refuses is never answered by a policy stand-in. In auto and smart mode, with only a policy stand-in bound, it is denied at the rules layer with `unparseable command denied; no approver bound: <command>`, `decided_by: policy`. With a human bound it prompts, with the ask event at the parse layer.
- Protective rules (`auto.deny`, `auto.ask`, `always_prompt`) match the raw command text of an unparseable command in every mode, with the slash-spanning matcher, and produce the same reasons as for parsed commands. Permissive rules do not match it in auto mode; in smart mode the legacy first-token `auto_approve` subject keeps admitting it, as ADR-0068 pinned.
- This narrows ADR-0068's carve-out ("commands the parser refuses keep their prior behavior") and closes its known residual. ADR-0068's decisions about parsed commands stand, so it is not superseded.

## Consequences

Hosted and headless runs that relied on a stand-in approving `sudo`, `npx`, `xargs`, path-qualified binaries, non-inert `env` prefixes or `bash script.sh` now get a deny the model can act on; the only unattended rescue is a smart-mode first-token `auto_approve` entry. Protective rules naming a refused wrapper start firing in auto mode. Attended runs still prompt; smart-mode asks for these commands now report `layer: parse` instead of `layer: human`.

## Alternatives considered

- Extending the parser to accept more forms: out of scope, and each accepted form is a new judgment surface.
- Denying at `LayerParse`: the layer names where a verdict was decided, and nothing at the parse layer decides; the human-bound ask stays there because nothing has been decided yet.
- Honoring `auto.deny` in smart mode for these commands: smart mode has never read `auto.*`; widening its rule set is a different change.
