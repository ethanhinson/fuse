# Architecture Decision Records

Immutable, numbered record of *why*. ADRs are never archived or rewritten; once `Accepted`, only the `status:` line changes (on supersession/reversal). This index is generated — do not hand-edit.

## Active

- [ADR-0001](0001-playwright-integration-driver-cdn.md) — Playwright integration tests: pin playwright-go and override the driver CDN (Accepted) ← change #8
- [ADR-0002](0002-research-mode-skill-driven-on-subagent-runtime.md) — Research mode is skill-driven on the subagent runtime, not Go-orchestrated (Accepted) ← change #14
- [ADR-0003](0003-built-in-skills-embedded-below-user-skills.md) — Built-in skills ship embedded via go:embed and rank below user skills (Accepted) ← change #14
- [ADR-0004](0004-byo-search-key-brave-primary-provider-resolution.md) — BYO search key with Brave-primary provider resolution and a config-driven custom HTTP provider (Accepted) ← change #14
- [ADR-0005](0005-per-segment-allow-rule-evaluation.md) — Per-segment allow-rule evaluation — a deliberate deviation from the Grok Build reference (Accepted) ← change #17
- [ADR-0006](0006-fuse-local-yml-tighten-only-trust-boundary.md) — .fuse.local.yml cannot loosen permission policy (repo-plantable config trust boundary) (Accepted) ← change #17 · relates to ADR-0005
- [ADR-0007](0007-scheduler-single-admission-authority.md) — One Scheduler is the single admission, queueing, and throughput authority for subagents (Accepted) ← change #36
- [ADR-0008](0008-rate-gate-per-logical-request-tpm-steady-state.md) — Rate gate charges per logical request; tpm is a steady-state guarantee, not an instantaneous one (Accepted) ← change #36 · relates to ADR-0007
- [ADR-0009](0009-queue-bound-visibility-global-pool-only.md) — Queue-bound visibility governs the global pool only; workflow pools retain 0034's strip-at-Concurrent (Accepted) ← change #36 · relates to ADR-0007, ADR-0008
- [ADR-0010](0010-mcp-client-requires-init-handshake-fails-open-capabilities.md) — MCP client hard-fails on the initialize handshake but fails open on capability content (Accepted) ← change #19
- [ADR-0011](0011-streamable-http-mcp-transport-request-scoped.md) — Streamable HTTP MCP transport is request-scoped with in-band session ownership (Accepted) ← change #18 · relates to ADR-0010
- [ADR-0012](0012-vendor-jsonschema-validation-library.md) — Vendor a JSON-Schema validation library for structured-delegation result validation (Accepted) ← change #24
- [ADR-0013](0013-tools-registry-owns-concurrency-safety.md) — tools.Registry is the home for concurrency safety (config-watch live reload) (Accepted) ← change #21
- [ADR-0014](0014-pipeline-conditional-routing-skip-propagation.md) — Pipeline conditional-routing execution semantics (skip-propagation join) (Accepted) ← change #26
- [ADR-0015](0015-per-result-relevance-classification.md) — Per-result relevance classification over a per-result scorer interface (Accepted) ← change #28
- [ADR-0016](0016-subagent-spawn-tree-runtime.md) — Subagent runtime is an append-only spawn tree with bounded depth and width and slot-yield deadlock avoidance (Accepted) ← change #12 · relates to ADR-0002, ADR-0007
- [ADR-0017](0017-segment-store-fssink-subpackage-split.md) — Split the segment store into an agent-free internal/segment package and an agent-dependent internal/segment/fssink subpackage to break an import cycle (Accepted) ← change #30
- [ADR-0018](0018-per-session-directory-layout-flat-log-read-compat.md) — Per-session directory layout for session logs and segments, with flat-log read-compatibility and no migration (Accepted) ← change #30 · relates to ADR-0017
- [ADR-0019](0019-process-global-segment-sink-holder.md) — Process-global, lock-guarded segment sink holder as the sink-injection mechanism in cmd/fuse (Accepted) ← change #30 · relates to ADR-0017, ADR-0018

## Superseded / Reversed

_None._

## Deprecated

_None._
