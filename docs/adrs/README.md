# Architecture Decision Records

Immutable, numbered record of *why*. ADRs are never archived or rewritten; once `Accepted`, only the `status:` line changes (on supersession/reversal). This index is generated — do not hand-edit.

## Active

- [ADR-0001](0001-playwright-integration-driver-cdn.md) — Playwright integration tests: pin playwright-go and override the driver CDN (Accepted) ← change #8
- [ADR-0002](0002-research-mode-skill-driven-on-subagent-runtime.md) — Research mode is skill-driven on the subagent runtime, not Go-orchestrated (Accepted) ← change #14
- [ADR-0003](0003-built-in-skills-embedded-below-user-skills.md) — Built-in skills ship embedded via go:embed and rank below user skills (Accepted) ← change #14
- [ADR-0004](0004-byo-search-key-brave-primary-provider-resolution.md) — BYO search key with Brave-primary provider resolution and a config-driven custom HTTP provider (Accepted) ← change #14
- [ADR-0005](0005-per-segment-allow-rule-evaluation.md) — Per-segment allow-rule evaluation — a deliberate deviation from the Grok Build reference (Accepted) ← change #17
- [ADR-0006](0006-fuse-local-yml-tighten-only-trust-boundary.md) — .fuse.local.yml cannot loosen permission policy (repo-plantable config trust boundary) (Accepted) ← change #17 · relates to ADR-0005

## Superseded / Reversed

_None._

## Deprecated

_None._
