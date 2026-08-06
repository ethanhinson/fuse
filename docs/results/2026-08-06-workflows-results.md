<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0034 — Workflows — skill-bound subagent pools with typed workers and spawn quotas](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0034-workflows.md)**
<!-- docket:backlink:end -->

# Workflows — skill-bound subagent pools with typed workers and spawn quotas — results
Change: #34 · Branch: feat/workflows · PR: <url> · Plan: docs/superpowers/plans/0034-workflows.md · ADRs: 2, 7

## Verify (human)

- [ ] `fuse "<task>"` (one-shot) where a spawned child is given a `tools` subset that omits
  `spawn_agent` produces a child that cannot spawn — the same guarantee already verified for the
  `shell` and `research-probe` paths (spec Acceptance 3). This path was missing the guard until the
  post-review fix below.
- [ ] `fuse research-probe "<task>"` activates the `research` workflow: pool `{concurrent: 5, total:
  8, max_depth: 1}` is enforced, the `facet-researcher` worker's tool allowlist is honored, and the
  total-quota backstop refuses the over-quota spawn with a clear message.

## Findings

- **HIGH (fixed post-review).** The folded-in "honor the `tools` subset" fix was wired into only two
  of the three child builders. `cmd/fuse/main.go`'s one-shot `run()` builder — the default
  production path for `fuse "<task>"` — still re-registered a child-wired `spawn_agent`
  unconditionally, so a parent passing a subset omitting `spawn_agent` still produced a
  spawn-capable child, violating spec Acceptance 3 on that path. Fixed by guarding with
  `childNode.Depth >= agent.MaxDepth || !shouldWireChildSpawn(opts.Tools)`, matching
  `shell.go`/`research_probe.go`. The reconcile log had scoped the fix to shell.go +
  research_probe.go; main.go was the missed third site.
- **LOW (addressed post-review).** The permanent `total` quota is tracked by two counters — the
  atomic `reserved` backstop (rejects on `reserved > Total`, in-flight, authoritative) and
  `tree.SubtreeSpawnCount` (strips on `>= Total`, committed). Added a comment on the `reserved` field
  tying them together so the `>` vs `>=` split reads as in-flight-vs-committed rather than divergent
  policy. Also noted that `reserved` is "permanent" (decremented only on its own over-quota
  rejection), so a spawn cancelled downstream still consumes a total slot — acceptable for v1.
- Build and the `cmd` / `internal/agent` / `internal/config` / `internal/tools` suites pass; `go
  build ./...` is clean.

## Follow-ups

- **Workflow activation is live only on the `research-probe` subcommand.** The one-shot `run()` and
  `shell` paths never tag a `WorkflowRoot`, so even when a user invokes `/research` there, none of
  the 0034 pool enforcement, typed workers, tighter budget, or backstop apply — those paths remain
  pre-0034 freeform fan-out. So `research-probe` is currently the *only* place the feature is live,
  and it is a probe subcommand, not a user-reachable production entry. This is in scope as a known,
  acceptable follow-up (per the plan) — flagged here so the live surface is not mistaken for shipped
  end-user behavior. A follow-up change should tag `WorkflowRoot` on the `run()`/`shell` skill paths.
- **`go vet` lock-copy on `workflowActivation`.** Adding the `reserved atomic.Int64` field made the
  value-receiver methods `pool()` and `workerNames()` trip `go vet`'s copylocks check
  (`cmd/fuse/workflow.go:47,57`). Harmless in practice — `act` is always held and passed as
  `*workflowActivation`, and neither method reads `reserved` (only `a.cfg`) — but the receivers
  should switch to pointer receivers to silence the lint. Auto-capture is disabled in this repo, so
  this is reported here rather than minted as a stub.
