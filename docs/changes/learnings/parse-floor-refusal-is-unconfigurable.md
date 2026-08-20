---
slug: parse-floor-refusal-is-unconfigurable
hook: "A refusal at the parse floor is invisible to EVERY later layer — user deny/allow patterns and the classifier all consume segments that never exist — so a parse-floor fail-closed is an unconfigurable hard ask; refuse at the lowest layer config can still reach."
topics: [permissions, security, shell, architecture]
changes: [72]
created: 2026-08-20
updated: 2026-08-20
promotion_state: candidate
promoted_to:
---

## Apply

In a layered permission pipeline (parse → rules/patterns → safelist → heuristic → classifier),
where a fail-closed lives determines who can override it. A refusal at the parse floor
(`ErrUnparseable`) runs before pattern matching and is deliberately not routed to the classifier —
so the command is unreachable by the user's own `deny`/`always_prompt`/`auto_approve` config AND by
the probabilistic layer. That is the right posture only for shapes where no later layer could
reason soundly (a command whose argv cannot be established). For a command that parses fine but
merely *contains a name you distrust*, encode the distrust at the rules/heuristic layer instead:
the command stays pattern-reachable (a human's deny gains teeth; a human's allow gains effect) and
the classifier keeps its shot at the gray area. Two tells you've mis-placed a refusal at the parse
floor: (1) a user adds an `auto_approve`/`deny` pattern for the command and observes it doing
nothing; (2) the prompt rate for a routine tool is 100% with no configuration that changes it.

## War story

- 2026-08-20 (#72, PR #77) — `docker` sat in `arbitraryArgWrappers`, so every docker command was
  `ErrUnparseable` → ask at `LayerParse`. First dogfood session on a docker-compose app prompted on
  the wholly read-only `cd <ws> && docker compose config --quiet 2>&1; echo …` — and no
  configuration could stop it: `auto_approve: ["bash:docker *"]` matched nothing (evalRules consumes
  segments that never existed) and the 0069 allow-biased classifier never saw the command. The fix
  moved docker's distrust down two layers: it parses normally, `isSafeDocker` deterministically
  admits the enumerated read-only forms, and `classifyHeuristic` routes every other docker form to
  the classifier without path-scoping its name operands. The host-exec wrappers (`sudo`, `xargs`,
  `npx`, `eval`) stay at the parse floor on purpose — no later layer can reason about a payload
  that executes in the caller's own environment. Related: [[containment-proof-needs-a-real-resolved-path]],
  [[no-human-allow-path-admission-is-an-allowlist]].
