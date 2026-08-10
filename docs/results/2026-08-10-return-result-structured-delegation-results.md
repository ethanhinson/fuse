<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0042 — Fix structured-delegation (expects) vs tool-calling collision via a return_result tool](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0042-return-result-structured-delegation.md)**
<!-- docket:backlink:end -->

# Fix structured-delegation (`expects`) vs. tool-calling collision via a `return_result` tool — results

Change: #42 · Branch: feat/return-result-structured-delegation · PR: <pending> · Plan: docs/superpowers/plans/2026-08-10-return-result-structured-delegation-plan.md · ADRs: 12, 23

## Verify (human)

Automated tests cover tool synthesis, the terminal/self-repair/exhaustion loop, the
regression (write_file + return_result coexistence), the back-compat lenient fallback, and
the pipeline path — all at the real `Agent.Run` loop seam via a scripted Completer. The check
below is an optional confidence spot-check in a real interactive session at the merge gate,
plus one recommended follow-up.

- [ ] In a real `fuse shell` session, spawn a child with an `expects` schema that also needs
      to write a file (e.g. "analyze X, write the summary to notes.md, and return {verdict}"),
      and confirm: the file is written with a real body (not the structured object), AND the
      parent receives the structured verdict. Before this change the child crammed the verdict
      into `write_file.content` with no `path`.
- [ ] (Recommended follow-up, NB-2) Gateway-seam verification per the `verify-tool-loop-at-gateway-seam`
      learning: drive the shipped binary against a scripted `LLM_GATEWAY_URL` double that logs
      each request's `tools[]`, and confirm `return_result` is offered to an Expects child and
      absent for a non-Expects child, and that a child calling it flows to `Structured`. The
      existing `cmd/fuse/structured_delegation_e2e_test.go` currently exercises only the D5
      fallback at that seam (its in-test builder does not call `SetExpects`).

## Findings

- **The fix converges at one choke point.** All behavior lives in the `agent` package driven
  off `SpawnOpts.Expects`; the three cmd-site child builders (`cmd/fuse/main.go`,
  `shell.go`, `research_probe.go`) each got ONE identical line — `a.SetExpects(opts.ExpectsSchema(), opts.ExpectsSink())`.
  A child with no Expects is byte-identical to before (no `return_result` offered). Re-grepped
  at build time (per `patch-every-cloned-child-builder`): exactly three non-test sites, no fourth.

- **Seam decision (plan deviation, option a).** The plan left the spawner→agent capture seam to a
  build-time spike. `ChildBuilder` returns only `(string, error)` and the child `Agent` is built
  inside each cmd builder, so option (b) (a purely spawner-internal channel) was not viable — no
  shared pointer already reaches `agent.New` inside the builders. Chosen: the spawner allocates an
  `ExpectsSink`, threads it via an unexported `SpawnOpts.expectsSink`, and each cmd builder passes
  it into the child `Agent` via `SetExpects`. The loop writes the captured value into the sink; the
  spawner reads it after `buildChild` returns (sequential within one goroutine — race-free by
  construction, `-race` clean).

- **ADR-0023** recorded the decision (structured delegation returns via a synthesized
  `return_result` tool, superseding the final-message-directive mechanism; ADR-0012's validator is
  reused, not reversed).

## Review notes (non-blocking, from the whole-branch review)

- **NB-1 — `SetExpects` nil-sink hardening.** `agent.go` `SetExpects` guards `schema == nil` but not
  `sink == nil`. With current wiring, schema and sink are always allocated together in `spawnLocal`,
  so this cannot happen; and `ExpectsSink.set` is nil-safe, so a hypothetical `schema!=nil/sink==nil`
  caller degrades to the fallback rather than panicking. A `schema != nil && sink == nil` guard (or a
  doc note that both must be set together) would harden the seam. Not fixed — degrades safely.
- **NB-2 — gateway-seam coverage gap.** See the recommended follow-up above.
- **NB-3 / NB-4** — minor: `return_result` args are echoed to the renderer like any tool call
  (consistent, no concern); only the first `return_result` in a multi-call response is captured
  (deterministic, harmless). No action.

## Deferred / follow-ups

- NB-2 gateway-seam verification is the one worthwhile follow-up; it was out of scope for this
  loop-level change and is noted here rather than filed (auto_capture is disabled repo-wide).
