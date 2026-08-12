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
- [ADR-0020](0020-born-compressed-non-destructive-segment-store.md) — Segments are born gzip-compressed with an uncompressed index, and age sweeps compress rather than delete (non-destructive GC) (Accepted) ← change #30 · relates to ADR-0017, ADR-0018, ADR-0019
- [ADR-0023](0023-structured-delegation-return-result-tool.md) — Structured delegation returns via a synthesized return_result tool, not a final-message directive (Accepted) ← change #42 · relates to ADR-0012
- [ADR-0024](0024-eventstore-independent-of-segment-store.md) — EventStore is independent of the segment store — events born plaintext, segments untouched (Accepted) ← change #43 · relates to ADR-0017, ADR-0018, ADR-0019, ADR-0020
- [ADR-0025](0025-eventstore-ordering-backpressure.md) — EventStore ordering and back-pressure — store-allocated Seq, non-blocking drop-newest-with-gap subscriber delivery (Accepted) ← change #43 · relates to ADR-0016, ADR-0019, ADR-0024, ADR-0030
- [ADR-0026](0026-handle-returning-spawn-seam-agent-free-interface.md) — Handle-returning spawn seam via an agent-free interface in internal/tools (Accepted) ← change #44 · relates to ADR-0016, ADR-0017
- [ADR-0028](0028-loopserver-new-jsonrpc-server-not-mcp-extension.md) — Binding #2 is a new internal/loopserver JSON-RPC server, not an extension of internal/mcp (Accepted) ← change #45 · relates to ADR-0027
- [ADR-0029](0029-shell-partial-runtime-binding.md) — The interactive shell is a partial Runtime binding — construction+store through the seam, turn cadence retained by the TUI (Accepted) ← change #45 · relates to ADR-0027, ADR-0028
- [ADR-0030](0030-deglobalize-eventstore-multiloop-hosting.md) — De-globalize the event store and segment sink; thread per-loop state as values so one process hosts N concurrent loops (Accepted) ← change #46 → supersedes ADR-0027 · relates to ADR-0025, ADR-0019
- [ADR-0031](0031-durable-distributed-event-store-loop-registry.md) — Durable, backend-agnostic event store + durable loop registry — existence and history survive restart and are reachable from any instance (Accepted) ← change #47 · relates to ADR-0024, ADR-0025, ADR-0027, ADR-0030
- [ADR-0033](0033-networked-binding-connect-protobuf-fuse-loop-v1.md) — Networked binding transport = Connect/protobuf (fuse.loop.v1), replacing the JSON-over-WebSocket + HTTP-replay wire (Accepted) ← change #55 → supersedes ADR-0032 · relates to ADR-0028, ADR-0030, ADR-0031
- [ADR-0034](0034-edge-enforced-auth-multi-tenancy-loop-ownership.md) — Token-authoritative tenancy + edge-enforced loop ownership over the policy-free runtime seam (Accepted) ← change #49 · relates to ADR-0030, ADR-0031, ADR-0033
- [ADR-0035](0035-sdk-local-backend-takes-prebuilt-runtime.md) — Client SDK Go local backend takes a pre-built runtime.Runtime (not config-to-build) (Accepted) ← change #50 · relates to ADR-0026, ADR-0033
- [ADR-0036](0036-tool-authz-delegated-downstream-rfc8693-egress-seam.md) — Tool/resource authz delegated to downstreams via per-call RFC 8693 delegation token exchange at a pluggable egress seam (Accepted) ← change #52 · relates to ADR-0030, ADR-0031, ADR-0034
- [ADR-0037](0037-sdk-observe-terminal-vs-transient-by-connect-code.md) — Client SDK observe classifies terminal vs transient stream failures by Connect code (Accepted) ← change #56 · relates to ADR-0033, ADR-0034, ADR-0035
- [ADR-0038](0038-committed-event-observability-projections.md) — Committed-event observability projections consume the exact store-assigned envelope (Accepted) ← change #51 · relates to ADR-0025, ADR-0031
- [ADR-0039](0039-committed-event-telemetry-dispatch-bounded-lossy.md) — Committed-event telemetry dispatch is bounded, non-blocking, and lossy under saturation (Accepted) ← change #51 · relates to ADR-0025, ADR-0038
- [ADR-0040](0040-provider-neutral-composite-observer.md) — Provider-neutral composite Observer owns production observability (Accepted) ← change #51 · relates to ADR-0039
- [ADR-0041](0041-operator-capability-for-process-global-observability.md) — Process-global observability mutations require an explicit operator capability (Accepted) ← change #51 · relates to ADR-0034, ADR-0040

## Superseded / Reversed

- [ADR-0027](0027-runtime-owns-loop-eventstore-global-holder-bridge.md) — Runtime owns the loop event store as instance state; process-global holders kept as a single-loop compatibility bridge (Superseded by ADR-30) ← change #45 · relates to ADR-0025, ADR-0019, ADR-0016
- [ADR-0032](0032-binding-3-websocket-session-http-replay-shared-dispatch.md) — Binding #3 transport — WebSocket full-session + thin stateless HTTP replay over a shared dispatch core (Superseded by ADR-33) ← change #48 · relates to ADR-0028, ADR-0030, ADR-0031

## Deprecated

_None._
