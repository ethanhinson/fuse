# Subagent Architecture Review — 2026-08-04

Four independent deep reviews (concurrency/eventing, subagent domain model, remote
execution + secrets, TUI architecture), synthesized. Line references are against
the working tree at the time of review (branch `feat/0012-subagent-ux`, after the
bounded-adapter/trace/TUI-chain fixes landed).

## Overall verdict

The **domain concepts are right** — an agent tree with per-node event logs, a
Spawner with pluggable child builders, renderer seams keeping `internal/agent`
UI-free, a remote executor + intent-plugin + secrets-store layering. None of the
reviewers proposed changing the conceptual model.

The architecture is **incorrect in four load-bearing places**, and these four
generate essentially every freeze, race, and security hole observed so far:

1. **TUI transport primitive** — the `chan tea.Msg` + `waitForMsg` re-arm pattern
   (instead of `Program.Send`) creates an unenforceable "exactly one receiver,
   re-armed on exactly the right message types" invariant, duplicated across a
   dual-switch Update. Every freeze so far is a violation of this invariant, and
   more violations exist today.
2. **Transcript representation** — pre-styled `[]string` with ANSI baked at append
   time, mutated in place via absolute line indices. This *forces* the fragile
   inline-block machinery and the O(transcript)-per-frame rewrap.
3. **Spawn security model** — children run with `AlwaysApprove`; tool restriction
   is one-generation-deep; `spawn_agent` is force-included in every subset. The
   permission gate is bypassable by one level of indirection, which the system
   prompt actively encourages.
4. **Result & lifecycle semantics** — bare-string results discard usage, stop
   reason, and partial transcripts; cancelled remote jobs report success; the root
   turn is uncancellable.

## Verdict table

| Area | Verdict |
|---|---|
| chan+re-arm TUI transport | needs redesign → `Program.Send` |
| AgentTree Emit/dirty-flusher | needs revision → level-triggered cap-1 wakeup + snapshots |
| Approval flow (single slot ×2 copies) | needs redesign → single-homed FIFO queue/broker |
| Renderer fan-out (Multi/Node/Tea) | sound shape; TeaRenderer sink inverted (backpressure on agent) |
| Lock discipline | writers sound; TUI readers race → `NodeView` snapshots |
| SpawnFunc layering (9 positional args, duplicated 80-line closure) | needs redesign → `ChildSpawner` interface + `harness.Factory` + `toolcore` leaf pkg |
| String results | needs revision → `SpawnResult{Text, StopReason, Usage}` |
| Parallel-batch gate (all-spawn-or-sequential) | needs revision; width unbounded → semaphore |
| Permission inheritance | needs redesign → revive `CloneForChild` atop approval queue |
| Registry Subset/Clone semantics | needs revision (force-include wrong; errors ignored) |
| Cancellation | needs revision (root uncancellable; remote cancel → success) |
| Remote sync resolution before node creation | needs revision (create pending node first) |
| SSE lifecycle | needs revision (retry budget, 64KB scanner, string-matched sentinel, dropped partials) |
| Collect fire-and-forget | needs revision (ordering race defeats its purpose) |
| Secrets | needs revision (plaintext git tokens in encrypted mode; no https enforcement; error-body reflection) |
| Dual-switch overlay Update | needs redesign → root router + Screen interface |
| Inline blocks by line index + label | needs redesign (subsumed by structured transcript) |
| Rendering cost model | needs revision → per-entry render cache |
| Pre-styled []string transcript | needs redesign → structured entries rendered at view time |
| Triplicated event vocabulary | needs revision → single `AgentEvent` wire type, `Renderer` = `Emit(AgentEvent)` |
| Slash registry/completer/theme | sound |

## Confirmed bugs (beyond previously fixed ones)

Correctness / freeze class:
- Exiting the agents overlay while a permission popup is pending drops the
  `RespCh` — the requesting agent deadlocks (shell_model.go overlay exit nils
  agentsModel with its approval slot).
- Root-level parallel spawn batches still deadlock on the single approval slot
  in smart mode (loop.go:93-103 → N concurrent prompts → overwrite).
- `registryReloadMsg` while overlay active falls into `default:` and kills the
  slash-reload subscription for the session.
- Esc never denies an approval: code matches `"escape"`, bubbletea names the key
  `"esc"` (shell_model.go:589, agents_model.go:208). Verified against
  bubbletea@v1.3.10 key.go:290.
- Memory-unsafe race: `updateInlineAgent` ranges `node.Events` unlocked while
  children append (shell_model.go:838,848); unlocked Status/time/token reads
  throughout shell + overlay renderers.
- Root turn runs on `context.Background()` (shell_model.go:719) — uncancellable;
  `CancelNode(rootID)` is a no-op (no SetCancel on root).
- Cancelled remote spawns finish `StatusDone` with `Err: nil` (spawn.go:371-375;
  remote.go closes channel with no terminal event on ctx cancel).
- One-shot mode drops `Remote/RemoteID/IntentPluginID` from SpawnOpts
  (main.go:128-134) — `remote: true` silently runs locally.
- Grandchild privilege escalation: ChildBuilder subsets from the session-root
  registry, not the parent's (shell.go / main.go) — restrictions are
  one-generation cosmetics; `Subset` force-includes `spawn_agent`
  (registry.go:87-90) and its error is discarded at both call sites.
- Child failure discards the entire partial transcript (`("", rerr)` on
  ErrMaxTurns/ErrLoopDetected).
- SSE: fallback client `Timeout: 30s` kills any stream >30s (remote.go:82);
  retry counter never resets on progress (remote.go:124); 64KB bufio.Scanner
  limit with `scanner.Err()` unchecked (remote.go:190,222) → deterministic
  stream_lost on large events; `ErrRemoteStreamLost` matched by error-string
  comparison (spawn.go:362) — spoofable; duplicate delivery on resume unfiltered.
- `plugin.Collect` runs after `doneCh` send on `context.Background()` — the
  parent can use a write-back branch before it's fetched; hung fetch leaks.
- Double `os.ExpandEnv` on git tokens (shell.go wiring + intent plugins).
- `lookupIntent` silently falls back to NilIntentPlugin on unknown id.
- Ctrl+L leaves stale inline lineIdx maps → later ticks overwrite arbitrary
  transcript lines; label collisions cross-wire two nodes onto one block;
  child completing while overlay open leaves "Running" inline forever.
- wordwrap doesn't hard-break long tokens → layout shear on long URLs/JSON;
  shell approval box borders shear on narrow terminals (overlay path is
  correct); byte-indexed truncation produces mojibake on multi-byte runes.

Security:
- Children bypass the approval gate entirely (`AlwaysApprove`), while the
  correct, tested `CloneForChild`/`prefixedApprove`/cache-clone design sits dead.
- Git write-back + clone tokens always plaintext in dispatch JSON even when the
  encrypted-secrets bundle mode is active (spawn.go:306,313).
- No https enforcement on the remote executor URL; silent plaintext-secrets
  downgrade when PublicKey unset.
- Remote-controlled dispatch error body (remote.go:111) flows unsanitized into
  the LLM conversation — a malicious runner echoing the request would reflect
  secrets into model context.
- sops store caches all decrypted secrets for the session; askpass token briefly
  on disk; no zeroization (partially inherent to Go).

## Target architecture (consolidated)

1. **Transport**: migrate agent→TUI to `Program.Send` via a small
   `Sender{atomic.Pointer[tea.Program]}`; bridge goroutines forward
   `tree.Updates()` and `slashReg.Changes()`; delete `waitForMsg` /
   `waitForRegistryReload` / `waitForTreeUpdate` and every re-arm return. Shrink
   `tree.out` to cap-1 level-triggered wakeup; delete dirty map + flusher.
2. **Approvals**: single `approvalController` above screens; FIFO queue with IDs;
   gate sends withdrawal on ctx-done; overlay renders the same queue. Then
   re-enable `CloneForChild(label)` for children.
3. **TUI structure**: `RootModel` router owning subscriptions + modal approval
   layer; `Screen` interface for transcript/agents views; structured transcript
   (`Entry` types, raw markdown retained, per-entry width-keyed render cache;
   subagent entries keyed by tool-call ID rendering live from the tree).
   TUI consumes `NodeView` snapshots, never `*AgentNode`.
4. **Spawn layering**: `internal/toolcore` leaf package holding `Result`/`Tool`;
   `agent.ChildSpawner` interface consumed by the spawn tool;
   `internal/harness.Factory` replacing both 80-line closures (subset from the
   *parent's* registry; CloneForChild perms; semaphore width cap; structured
   `SpawnResult{Text, StopReason, Usage}`; partial transcripts returned with
   stop reason on budget exhaustion).
5. **Events**: one `AgentEvent` wire type with typed payloads; `Renderer` →
   single-method `Emit(AgentEvent)`; tree append is the canonical sink, tea
   forwarding is a tap on `tree.Updates()` — kills the double-path and the
   drain-and-discard branch.
6. **Remote**: create node before resolution (pending → error on failure); two
   HTTP clients (bounded dispatch/cancel; unbounded-body stream with header
   timeout + read-idle watchdog); reset retry budget on progress; larger scanner
   buffer; match stream-lost by event name/flag; map ctx-cancel to
   StatusCancelled; run Collect before done-send under a bounded ctx; move git
   tokens into the encrypted bundle; require https or explicit opt-out; truncate
   dispatch error bodies.

## Suggested order

- **Phase 0 (quick wins, shippable immediately)**: esc/escape fix ×2;
  `CopyEvents()` + NodeView snapshots; approval queue (fixes two live deadlocks);
  clear inline maps on Ctrl+L; width-cap shell approval block; rune-safe
  truncation; surface `Subset` errors; spawnRemote cancel status + sentinel by
  name; scanner buffer + `scanner.Err()`; split remote HTTP clients; one-shot
  SpawnOpts remote fields; return partial transcript with stop reason; spawn
  width semaphore.
- **Phase 1**: `Program.Send` migration (deletes the re-arm machinery and its
  whole bug class).
- **Phase 2**: structured transcript store + render cache.
- **Phase 3**: `toolcore` + `ChildSpawner` + `harness.Factory`; revive
  `CloneForChild`; structured SpawnResult; subset-from-parent.
- **Phase 4**: event-vocabulary consolidation; root-router screen split.
