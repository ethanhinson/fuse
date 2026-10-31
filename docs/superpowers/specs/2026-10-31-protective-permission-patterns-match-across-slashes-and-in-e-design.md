<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0088 — Protective permission patterns match across slashes and in every mode alike](../../changes/active/0088-protective-permission-patterns-match-across-slashes-and-in-e.md)**
<!-- docket:backlink:end -->

# Protective permission patterns match across slashes

## Problem

`internal/permissions/rules.go` (around 430-432) and `patterns.go` (around 16) both match with `path.Match`, where `*` does not span `/`. `bash:rm *` against subject `bash:rm -rf a/b` misses, so a deny or ask rule fails open whenever an argument has a slash. The allow consumer deliberately relies on that boundary (`rules.go` around 122-135): `bash:git *` must not match `git clone https://evil/x` so egress scoping still runs, and `bash:rm build/*` must not admit `rm build/../x` (ADR-0005, ADR-0049).

Smart mode matches `{tool, "bash:<argv0>"}` (`patterns.go` around 26-33, `gate.go` around 563-565). Auto matches `bash:<argv joined>` per segment plus the whole command (`rules.go` around 406-425). `bash:git` matches every git command in smart but only bare `git` in auto; `bash:git *` is the reverse.

## Design

- New `internal/permissions/cmdglob.go`, `matchProtective(pat, subject)`: `*` matches any run including `/`, `?` matches one character, and `[...]` classes follow path.Match rules. A malformed pattern matches nothing and is reported the way it is today.
- `matchesSubjects` takes a consumer kind (protective or permissive). Protective (deny, ask, always_prompt) uses `matchProtective`; permissive (auto_approve and smart allow) keeps `path.Match`.
- Subjects, the same in both modes: per parsed segment `bash:<argv0> <args...>`. Protective rules are also matched against the whole command and the head `bash:<argv0>`. Smart mode builds `{tool, bash:argv0, bash:segment}`, so it keeps its first-token allow behavior (nothing that allowed before stops allowing) and gains the full-segment subject, so `bash:git *` now works in both modes.
- Record an ADR: protective patterns match generously, permissive patterns match strictly.
- Update the user-facing permission docs to state the one matching rule.

## Testing

- `bash:rm *` deny denies `rm -rf a/b` and `rm -rf a/b && ls`.
- `bash:cat*` ask asks for `cat dir/file`.
- Allow `bash:git *` still misses `git clone https://x/y` and `git status && rm -rf /` (per-segment, ADR-0005).
- Allow `bash:rm build/*` does not allow `rm build/../x`.
- The same deny/ask table runs in smart and auto and asserts identical verdicts.
- Existing allow fixtures produce byte-identical decisions.
