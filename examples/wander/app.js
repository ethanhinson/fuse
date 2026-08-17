// Wander — a plain-JS vacation-rental concierge driven ENTIRELY through @fuse/sdk's public
// API (change 0056). It never touches the generated Connect stubs: createClient →
// startLoop / send / observe / isCompletion, plus the 0056 additions it dogfoods —
// onState (connection indicator), FuseTerminalError (terminal-error affordance), and an
// AbortSignal for page-unload teardown.
//
// The SDK is imported from ./vendor/fuse-sdk.js (esbuild bundle of sdk/ts/src/index.ts;
// see build.sh). Its connect-web transport points at the SAME origin; server.js reverse-
// proxies /fuse.loop.v1.* to `fuse loop-serve-net`, so the browser stays same-origin (no
// CORS) while the SDK still speaks Connect directly (no WS relay).
//
// Change 0060 (task 7) consolidated the two demos: the retired concierge demo's
// UI — the activity rail, the stat strip, the card/markdown answer rendering — was ported
// ONTO this file. Only the RENDERING moved. The transport did not: that demo drove a
// hand-rolled loop.* JSON-RPC WebSocket relay, and that is exactly what this app must not
// go back to. Every wire call below is still an @fuse/sdk public-API call.
//
// Change 0060 (task 8) added the identity UX: a picker over the demo bearer tokens, a
// Saved-stays panel filled from `list_favorites` through the agent, and a full client-side
// reset on every user switch. The reset is a SECURITY-VISIBLE property, not a cosmetic one:
// the previous principal's observe stream is torn down (SDK-idempotent abort) BEFORE the
// next principal's session starts, and a stale stream is additionally gated on a session
// generation so it can never paint the new principal's UI.

import {
  createClient,
  isCompletion,
  FuseTerminalError,
} from "./vendor/fuse-sdk.js";

// ───────────────────────────── Identity (change 0060, D5) ───────────────────────────
// The bearer credential is now CHOSEN, not hardcoded. DEV_USER is the built-in credential
// `fuse loop-serve-net` synthesizes when loop_server.auth is unset, and it stays the
// DEFAULT selection: a zero-config local backend — and the browser reconnect lane, which
// runs exactly that — behaves byte-identically to before the picker existed. The demo
// principals come from ./demo-users.json (server.js reads them out of the demo fuse
// config, so the UI can never drift from the checked-in directory).
//
// Choosing a user re-creates the SDK client with THAT token. Everything downstream is the
// server's: loop_server.auth resolves {tenant, subject}, and the identity tier mints the
// audience-bound delegation token the rentals MCP server adjudicates. The browser never
// asserts who it is beyond presenting its token.
const DEV_USER = Object.freeze({
  token: "fuse-dev-token",
  tenant: "_default",
  subject: "dev",
  label: "dev@_default",
});

function makeClient(user) {
  return createClient({
    baseUrl: window.location.origin, // same-origin; server.js proxies the Connect path.
    credentials: { token: user.token, tenant: user.tenant },
  });
}

let currentUser = DEV_USER;
// `client` is rebuilt on every user switch, so it is a binding, not a constant. Anything
// long-lived that uses it (runObserve) captures the client it started with, so a switch
// can never re-point an in-flight stream at a different principal's credential.
let client = makeClient(currentUser);

// Test instrumentation (harmless in normal use): the headless-browser CI lanes read these
// to assert no-loss/no-dup across a mid-stream network kill, to watch the connection-state
// transitions, and (0060) to assert a user switch really tears the previous principal's
// stream down. They only ever record what the SDK's public API already surfaces (seqs +
// onState) plus this app's own session bookkeeping — no reach past the SDK.
window.__wanderSeqs = [];
window.__wanderStates = [];
window.__wanderTerminal = null;
window.__wanderParks = 0; // count of loop.parked completions (turn boundaries) observed.
// Live observe streams. It must settle back to exactly 1 after a user switch: a stale
// stream from the previous principal showing up here is the leak this app must not have.
window.__wanderOpenStreams = 0;
window.__wanderResets = 0; // client-side session resets performed.
window.__wanderSavedRefreshes = 0; // list_favorites results rendered SINCE the last reset.
// The current session's loop id (null when there is no loop). The browser lane reads this
// instead of reaching into localStorage, so the assertion is about the app's live session
// rather than about the storage encoding.
window.__wanderLoopId = null;
// True when this page load resumed a stored loop instead of starting a fresh session
// (change 0062). Initialized unconditionally so the lane can read it on the first-visit
// path too, where it simply stays false.
window.__wanderRestored = false;
// True once the durable stream behind a stored session turned out to be gone (`not_found`)
// and the app fell back to a clean fresh session (change 0062, D3).
window.__wanderRestoreLost = false;
// True once a terminal `failed_precondition` parked this page: the loop finished or was
// reaped by the server's idle TTL, but the conversation is NOT lost — a reload resumes it
// from the durable events (change 0062, D2).
window.__wanderPaused = false;

// The two terminal Connect codes this app treats differently. @fuse/sdk carries the raw
// numeric `@connectrpc/connect` `Code` on FuseTerminalError.code but does NOT re-export the
// `Code` enum (sdk/ts/src/index.ts exports createClient / isCompletion / FuseTerminalError
// and the types — no enum), so the vendored bundle has nothing to import here. These are the
// canonical Connect numbers (node_modules/@connectrpc/connect/dist/esm/code.d.ts:30 and :46)
// for two members of ADR-0037's terminal set; the other two (PermissionDenied = 7,
// Unauthenticated = 16) need no constant — they are the unchanged default branch.
const CODE_NOT_FOUND = 5;
const CODE_FAILED_PRECONDITION = 9;

const threadEl = document.getElementById("thread");
const activityEl = document.getElementById("activity");
const formEl = document.getElementById("composer");
const inputEl = document.getElementById("input");
const sendBtn = document.getElementById("send");
const resetBtn = document.getElementById("reset");
const connEl = document.getElementById("conn");
const connLabel = document.getElementById("conn-label");
const loopLabel = document.getElementById("loop-label");
const chipEls = Array.from(document.querySelectorAll(".chip"));
const savedEl = document.getElementById("saved");
const userSelect = document.getElementById("user");
const whoamiEl = document.getElementById("whoami");
const customRow = document.getElementById("custom-token-row");
const customToken = document.getElementById("custom-token");
const customTenant = document.getElementById("custom-tenant");
const customApply = document.getElementById("custom-apply");

// The pristine markup of the panels a session reset restores. Captured once, before any
// event can mutate them, so the reset restores the ORIGINAL document rather than a
// hand-rolled reconstruction that could drift from index.html.
const initialThreadHTML = threadEl.innerHTML;
const initialActivityHTML = activityEl.innerHTML;
const initialLoopLabel = loopLabel ? loopLabel.textContent : "";

// Session state. `loopId` is created lazily on the first message (persistent interactive
// loop so the concierge holds context across turns, #53) and is PERSISTED (change 0062):
// a reload restores the conversation by re-observing that loop's durable event stream from
// seq 0, which is exactly what change #54 made possible on the server. So a refresh resumes
// rather than starting a fresh loop; + New and a user switch are the gestures that
// deliberately forget the stored session.
let loopId = null;
// turnInFlight guards against a second submit while the current turn is still being answered
// (composer disabled). It is distinct from "is the observe stream open" — the persistent
// observe runs for the whole session; only a turn awaiting its reply blocks new input.
let turnInFlight = false;
// cursor is the highest seq observed so far (the SDK's own reconnect watermark is internal;
// this mirrors it for diagnostics).
let cursor = 0n;
// EXACTLY ONE live AbortController at a time — the CURRENT session's. Aborting it is the
// SDK's idempotent teardown (D3): it stops the reconnect loop, tears the underlying stream
// down, and fires onState('closed') once; a double abort is a no-op. `pagehide` aborts it
// so a navigation never leaks a stream, and a user switch aborts it (then installs a fresh
// one) so the previous principal's stream never outlives their session. runObserve is
// handed its controller, never this binding, so a reassignment cannot orphan the check a
// running stream makes against its own abort.
let sessionAbort = new AbortController();
// sessionGeneration increments on every reset. A stream from an older generation is stale:
// it may still be draining a frame or two while its abort propagates, and it must never
// paint the new principal's UI. Every observe callback checks its generation first.
let sessionGeneration = 0;
// quietTurn suppresses transcript rendering for a turn the APP initiated (the Saved-panel
// refresh), so a background list_favorites does not look like the user said something.
let quietTurn = false;
// pendingSavedRefresh is set when a favorite_listing call is seen; the refresh fires at the
// next park, because a `send` is only accepted at a parked turn boundary.
let pendingSavedRefresh = false;
// replaying is true only while a restored loop's durable stream is being replayed from seq 0
// (change 0062, D-D). It changes two things: `user.input` events render the human turns (live
// turns are echoed locally by handleSubmit instead), and the completion handler's side effects
// — focus and the Saved-panel refresh turn — are suppressed so a replay cannot act on the
// world. Nothing sets it true yet; the restore-on-load path does.
let replaying = false;

// ─────────────────── Session persistence (change 0062, D-A) ──────────────────
// localStorage["wander.session.v1"] = {"loopId": …, "tenant": …, "subject": …}.
//
// The principal is stored WITH the id, and a restore only ever fires when both fields match
// the credential in hand. Wander is a multi-identity demo: a loop restored under a different
// principal would be a cross-owner Observe, which the server rejects with PermissionDenied —
// a confusing failure where a fresh session is the honest answer. So a mismatch is never
// "try it and see"; it is treated exactly like nothing stored, as is a parse failure or an
// entry with no loopId.
//
// Every storage access is try/catch-wrapped. Storage can throw outright (Safari private
// mode, a hardened profile, disabled site data), and the demo must degrade to today's
// stateless behavior rather than break.
//
// What is stored is the principal's NAME ({tenant, subject}) — never its token. On reload the
// token is re-resolved from the demo directory the server publishes (see restoreSession), so
// no bearer credential is ever written to localStorage. The corollary is a deliberate
// limitation: a session opened under a PASTED custom token is not persisted at all (below).
const SESSION_KEY = "wander.session.v1";

function saveSession(id) {
  // A pasted token is a credential we would have to store to restore, and localStorage is
  // the wrong home for a bearer credential even in an example app. Worse, the {tenant,
  // subject} the app attaches to a custom token is the app's GUESS (the tenant box plus the
  // literal subject "custom"), not the server's resolution of that token — so a stored entry
  // for it would be a claim the app cannot back. A custom-credential session is therefore
  // deliberately not restorable. Nothing to clear here: switchUser() reset (and so cleared)
  // the store before this credential was ever used.
  if (currentUser.custom) return;
  try {
    localStorage.setItem(
      SESSION_KEY,
      JSON.stringify({ loopId: id, tenant: currentUser.tenant, subject: currentUser.subject }),
    );
  } catch {}
}

// readSession returns the stored entry VERBATIM (principal included) or null. It performs no
// principal check — it is the raw read that restoreSession needs in order to find out WHOSE
// session is stored. Everything that acts on a stored id goes through loadSession() below.
function readSession() {
  try {
    const raw = localStorage.getItem(SESSION_KEY);
    if (!raw) return null;
    const saved = JSON.parse(raw);
    if (!saved || !saved.loopId) return null;
    return {
      loopId: String(saved.loopId),
      tenant: String(saved.tenant),
      subject: String(saved.subject),
    };
  } catch {
    return null;
  }
}

// principalMatches is the security-relevant check, defined ONCE: BOTH halves of the principal
// must equal the credential currently in hand.
function principalMatches(entry) {
  return !!entry && entry.tenant === currentUser.tenant && entry.subject === currentUser.subject;
}

function loadSession() {
  const saved = readSession();
  if (!principalMatches(saved)) return null;
  return { loopId: saved.loopId };
}

function clearSession() {
  try {
    localStorage.removeItem(SESSION_KEY);
  } catch {}
}

// One writer for the loop-label text, so the mint path and the restore path cannot drift
// into two different renderings of the same fact.
function setLoopLabel(id) {
  if (!loopLabel) return;
  loopLabel.textContent = `Live loop ${String(id).slice(0, 12)}… — streaming over Connect`;
}

// ─────────────────────────── Connection indicator ───────────────────────────
// The pill is keyed on the SDK's own onState vocabulary; styles.css selects on
// .conn[data-state="…"], so the CSS key and the asserted string are the same value.
function setConn(state) {
  connEl.dataset.state = state;
  connLabel.textContent = state;
}

// ───────────────────────────── Activity rail ────────────────────────────────
const stats = { turns: 0, searches: 0, agents: 0, events: 0 };
function bumpStat(k, n = 1) {
  stats[k] += n;
  const el = document.getElementById("stat-" + k);
  if (el) el.textContent = String(stats[k]);
}

function pushActivity(cls, ico, title, meta) {
  const empty = activityEl.querySelector(".activity-empty");
  if (empty) empty.remove();
  const el = document.createElement("div");
  el.className = "act " + cls;
  const icoEl = document.createElement("span");
  icoEl.className = "ico";
  icoEl.textContent = ico;
  const body = document.createElement("div");
  body.className = "body";
  const titleEl = document.createElement("div");
  titleEl.className = "title";
  titleEl.textContent = title;
  body.appendChild(titleEl);
  if (meta) {
    const metaEl = document.createElement("div");
    metaEl.className = "meta";
    metaEl.textContent = meta;
    body.appendChild(metaEl);
  }
  el.appendChild(icoEl);
  el.appendChild(body);
  activityEl.appendChild(el);
  activityEl.scrollTop = activityEl.scrollHeight;
  while (activityEl.children.length > 60) activityEl.removeChild(activityEl.firstChild);
}

// ──────────────────────────────── Thread ────────────────────────────────────
// addMessage keeps the role class contract (`msg user` / `msg concierge` / `msg error`)
// that predates the UI port; only the inner shape (avatar + bubble) is new. The bubble
// element is returned so a streaming reply can keep appending into it.
function addMessage(role, avatar, text) {
  const el = document.createElement("div");
  el.className = `msg ${role}`;
  if (avatar) {
    const av = document.createElement("div");
    av.className = "avatar";
    av.textContent = avatar;
    el.appendChild(av);
  }
  const bubble = document.createElement("div");
  bubble.className = "bubble";
  bubble.textContent = text;
  el.appendChild(bubble);
  threadEl.appendChild(el);
  scrollThread();
  return bubble;
}

// addPendingConcierge opens the reply bubble for the turn now in flight, showing the
// spinner + phase cue until the first model.delta (or the parked answer) lands in it.
function addPendingConcierge() {
  const bubble = addMessage("concierge", "✷", "");
  const thinking = document.createElement("div");
  thinking.className = "thinking";
  const spin = document.createElement("span");
  spin.className = "spin";
  const phase = document.createElement("span");
  phase.className = "phase";
  phase.textContent = "Getting to work…";
  thinking.appendChild(spin);
  thinking.appendChild(phase);
  bubble.appendChild(thinking);
  scrollThread();
  return bubble;
}

function setPhase(bubble, text) {
  if (!bubble) return;
  const ph = bubble.querySelector(".phase");
  if (ph) ph.textContent = text;
}

function scrollThread() {
  threadEl.scrollTop = threadEl.scrollHeight;
}

function setComposerEnabled(on) {
  sendBtn.disabled = !on;
  inputEl.disabled = !on;
  for (const c of chipEls) c.disabled = !on;
}

// decodePayload turns an event's raw JSON payload bytes into an object (or {} on empty).
const decoder = new TextDecoder();
function decodePayload(ev) {
  if (!ev.payload || ev.payload.length === 0) return {};
  try {
    return JSON.parse(decoder.decode(ev.payload));
  } catch {
    return {};
  }
}

// runObserve is the SINGLE, long-lived observe over the interactive loop's ONE stream. It
// stays open for the whole session (one loop_id, one stream) — each turn's events flow on
// it and a `send` just injects input at the parked boundary; we do NOT re-open per turn.
// It renders streamed assistant text (model.delta) into the pending bubble, every other
// event kind into the activity rail, and each turn's final answer (loop.parked) as the
// rendered reply, re-enabling the composer on each park. Transparent reconnect +
// no-loss/no-dup across a mid-stream drop is the SDK's job — this consumer just keeps
// reading. It runs until the page aborts, a user switch aborts it (D3), or a
// FuseTerminalError stops it (D2).
//
// It is handed its OWN generation, loop id, client and AbortController rather than reading
// the module bindings, because those are all re-pointed by a user switch. A stream that
// outlives its session must be inert: every write to shared UI or state is gated on
// `generation === sessionGeneration`, so the previous principal's in-flight frames can
// never paint the next principal's transcript, saved panel, or connection pill.
async function runObserve(generation, myLoopId, myClient, abort) {
  // The in-progress reply bubble for the CURRENT turn; reset at each park.
  let replyText = "";
  // The replay-side twin of `quietTurn`. `quietTurn` is set by the LIVE submit path, so it
  // is false for every event arriving from the durable stream — but an app-initiated
  // Saved-panel refresh is a REAL turn, so its answer is in that stream too. Without this a
  // restore suppresses the quiet QUESTION and still paints its ANSWER, leaving an orphan
  // concierge bubble attached to nothing. Scoped per stream, set on the replayed quiet
  // `user.input`, cleared at the park below so it cannot leak into the next replayed turn.
  let replayQuiet = false;
  const current = () => generation === sessionGeneration;

  window.__wanderOpenStreams++;
  try {
    for await (const ev of myClient.observe(myLoopId, {
      fromSeq: 0n,
      signal: abort.signal,
      onState: (s) => {
        // Always record the transition (it is the SDK's own vocabulary, and the teardown
        // of a stale stream is exactly what a lane wants to see); only paint it if this
        // stream still owns the session.
        window.__wanderStates.push(s);
        if (current()) setConn(s);
      },
    })) {
      if (!current()) break; // superseded by a user switch: stop, render nothing.
      window.__wanderSeqs.push(Number(ev.seq));
      cursor = ev.seq;
      bumpStat("events");
      const p = decodePayload(ev);
      switch (ev.kind) {
        case "turn.start": {
          bumpStat("turns");
          setPhase(pendingBubble, "Thinking through your request…");
          break;
        }
        case "user.input": {
          // Only a replay renders human turns; live ones were already echoed by handleSubmit.
          if (!replaying) break;
          let text = String(p.content || "");
          // A deny nudge is a SYNTHETIC user turn the runtime injects — never a human said it.
          if (text.startsWith("[policy] ")) break;
          // The turn-0 seed carries the whole task: preamble + the user's first message.
          if (text.startsWith(TASK_PREAMBLE)) text = text.slice(TASK_PREAMBLE.length);
          // Every LATER turn arrives wrapped in the runtime's injection envelope
          // ("[human message]\n…", internal/agent/humanmsg.go Poll) because `send` queues
          // the text and the loop batches it in at the turn boundary. Strip it: it is
          // runtime framing, not something a human typed — and leaving it on also defeats
          // the SAVED_REFRESH_PROMPT comparison below, which is an exact match.
          if (text.startsWith(HUMAN_ENVELOPE)) text = text.slice(HUMAN_ENVELOPE.length);
          // The Saved-panel refresh is a real `send`, but the APP asked it, not the user.
          if (text === SAVED_REFRESH_PROMPT) {
            // Suppress the question AND the answer that follows it, for this turn only.
            replayQuiet = true;
            break;
          }
          if (!text.trim()) break;
          addMessage("user", "you", text);
          break;
        }
        case "model.call.start": {
          setPhase(pendingBubble, "Consulting the model…");
          break;
        }
        case "model.delta": {
          if (p.text && !quietTurn && !replayQuiet) {
            if (!pendingBubble) pendingBubble = addPendingConcierge();
            replyText += p.text;
            pendingBubble.textContent = replyText; // drops the spinner on first token
            scrollThread();
          }
          break;
        }
        case "tool.call": {
          const fullName = p.name || "tool";
          // An MCP tool arrives fully qualified ("mcp:rentals/favorite_listing"); a
          // built-in arrives bare. Match on the BASE name so the rail (and the saved-panel
          // refresh below) work for both — matching the bare name alone silently stopped
          // recognising every rentals tool the moment they came over MCP.
          const name = toolBaseName(fullName);
          if (name === "search_rentals" || name === "web_search") {
            bumpStat("searches");
            const q = extractArg(p.args, ["query", "q", "search"]) || "the web";
            pushActivity("search", "🔍", "Searching listings", truncate(q, 90));
            setPhase(pendingBubble, "Searching live listings…");
          } else if (name === "web_fetch") {
            pushActivity("search", "🌐", "Reading a listing page",
              prettyURL(extractArg(p.args, ["url", "link"])));
            setPhase(pendingBubble, "Reading the best matches…");
          } else if (name === "favorite_listing") {
            pushActivity("tool", "★", "Saving a listing",
              extractArg(p.args, ["listing_id", "id"]));
            // The write lands in the CALLING principal's set downstream; re-read it at the
            // next park rather than optimistically patching the panel here, so the panel
            // only ever shows what that principal's token was allowed to read back.
            pendingSavedRefresh = true;
          } else if (name === "list_favorites") {
            pushActivity("tool", "✦", "Reading your saved stays", "");
          } else if (name === "spawn_agent") {
            bumpStat("agents");
            pushActivity("agent", "🤝", "Dispatched a scout",
              truncate(extractArg(p.args, ["label", "task", "prompt"]) || "sub-agent", 90));
            setPhase(pendingBubble, "Coordinating research scouts…");
          } else {
            pushActivity("tool", "⚙︎", "Tool call", fullName);
          }
          break;
        }
        case "tool.result": {
          // Harvest every URL the tool actually returned, so card links can be verified
          // against real results (link grounding — see harvestURLs).
          harvestURLs(p.result);
          // The Saved panel is the rentals server's answer, verbatim: the favorites this
          // principal's delegated token was allowed to read. An errored result leaves the
          // panel alone — blanking it would misreport "you have nothing saved".
          if (toolBaseName(p.name || "") === "list_favorites" && !p.is_error) {
            renderSaved(parseFavorites(p.result));
            window.__wanderSavedRefreshes++;
          }
          setPhase(pendingBubble, "Pulling the results together…");
          break;
        }
        case "spawn.start": {
          // NOT a stat bump: the `agents` counter is incremented once, at the spawn_agent
          // tool.call above. Counting here too would double-count every scout.
          pushActivity("agent", "➤", "Scout started", p.label || p.child_node_id || "");
          break;
        }
        case "spawn.done": {
          // A scout's result text also carries real source URLs it found.
          harvestURLs(p.result);
          pushActivity("agent", "✓", "Scout reported back",
            truncate(p.result || p.label || "", 90));
          break;
        }
        case "context.summarize": {
          pushActivity("think", "🗜", "Compressing context",
            `turns ${p.turn_start}–${p.turn_end}`);
          break;
        }
        case "loop.parked": {
          // The terminal answer for this exchange. With a non-streamed gateway there is no
          // model.delta, so render the parked content directly; otherwise it matches.
          const answer = p.content || replyText;
          if (answer && !quietTurn && !replayQuiet) {
            if (!pendingBubble) pendingBubble = addPendingConcierge();
            renderAnswer(pendingBubble, answer);
          }
          break;
        }
        case "error": {
          pushActivity("error", "⚠︎", "Loop error", truncate(p.err || "unknown", 120));
          addMessage("error", "", `Error: ${p.err || "unknown"}`);
          break;
        }
      }
      if (isCompletion(ev)) {
        // Parked at a turn boundary: the concierge is ready for the next message. Reset the
        // per-turn render state and re-enable the composer — but KEEP the stream open.
        window.__wanderParks++;
        pendingBubble = null;
        replyText = "";
        turnInFlight = false;
        const wasQuiet = quietTurn || replayQuiet;
        quietTurn = false;
        replayQuiet = false;
        // The composer re-enables even during a replay — a restored session must be usable.
        setComposerEnabled(true);
        if (!wasQuiet && !replaying) inputEl.focus();
        // A favorite landed this turn: re-read the set now that the loop is parked and
        // will accept a `send` again. list_favorites never sets the flag, so this cannot
        // chain into a refresh loop. During a replay the flag is cleared WITHOUT firing:
        // a replayed favorite is history, and re-running it would spend a real model turn
        // in the middle of restoring the transcript.
        if (pendingSavedRefresh) {
          pendingSavedRefresh = false;
          if (!replaying) refreshSaved();
        }
      }
    }
  } catch (err) {
    if (!current()) {
      // A superseded stream's failure is nobody's business: this session is gone.
    } else if (err instanceof FuseTerminalError) {
      // A terminal Connect code: stop, show the RIGHT affordance for THIS code — do NOT
      // silently hot-loop, and do not treat all four alike (change 0062, D2 + D3). The
      // instrumentation is written FIRST in every branch, before anything that could
      // reset it, because the reconnect lane asserts on it.
      window.__wanderTerminal = err.code;
      pushActivity("error", "⚠︎", "Stream closed", String(err.code));
      if (err.code === CODE_NOT_FOUND) {
        // D3 — the durable stream itself is gone (a wiped/rebuilt store). This is the one
        // genuinely unrecoverable case: there are no events left to rebuild from, so the
        // honest answer is a clean fresh session rather than a retry against nothing.
        // resetSession() is safe to call from here: this stream is already ending, and its
        // abort()+generation bump only make the (already dead) stream inert — the `finally`
        // below still runs and still decrements __wanderOpenStreams.
        clearSession();
        resetSession();
        // resetSession clears the per-session instrumentation (including __wanderTerminal),
        // so re-assert the terminal code and the loss marker AFTER it.
        window.__wanderTerminal = err.code;
        window.__wanderRestoreLost = true;
        addMessage("error", "", "Your previous session is no longer available — starting a new one.");
      } else if (err.code === CODE_FAILED_PRECONDITION) {
        // D2 — reap ≠ loss. The loop finished or the server's ~30-minute idle TTL reaped it,
        // but the durable events are still there: change #54 rebuilds the transcript on the
        // next observe. So KEEP the stored session; a reload resumes this conversation.
        setConn("closed");
        setComposerEnabled(false);
        window.__wanderPaused = true;
        addMessage("error", "", "Session paused — reload to resume this conversation.");
      } else {
        // unauthenticated / permission_denied — auth rejected. Unchanged behavior.
        setConn("error");
        addMessage("error", "", `Connection closed (${err.code}). Refresh to start a new session.`);
      }
    } else if (!abort.signal.aborted) {
      setConn("error");
      addMessage("error", "", `Unexpected error: ${String(err)}`);
    }
  } finally {
    window.__wanderOpenStreams--;
  }
}

// pendingBubble is the concierge reply bubble for the turn currently in flight (module
// scope so both the submit path and the observe loop can reach it).
let pendingBubble = null;

// handleSubmit drives one turn. `quiet` marks a turn the APP asked for rather than the
// user (the Saved-panel refresh): it is a real agent turn on the real loop — same
// credential, same tool path — it simply renders no transcript bubbles.
async function handleSubmit(text, quiet = false) {
  // The user acting is the exact, timer-free end of a replay: a restored loop is parked and
  // idle, so no further event can arrive on it until someone sends. Everything from here on
  // is live.
  replaying = false;
  quietTurn = quiet;
  if (!quiet) {
    addMessage("user", "you", text);
    pendingBubble = addPendingConcierge();
  }
  turnInFlight = true;
  setComposerEnabled(false);
  // Pin the session this turn belongs to: an await below can straddle a user switch, and
  // a reply must never be started for (or attributed to) a principal who has been replaced.
  const generation = sessionGeneration;
  const myClient = client;
  const myAbort = sessionAbort;
  try {
    if (!loopId) {
      // First message: start a persistent interactive loop so context holds across turns,
      // then open the ONE long-lived observe stream (kept open for the whole session).
      const started = await myClient.startLoop({
        task: TASK_PREAMBLE + text,
        model: "cloud/x",
        interactive: true,
      });
      if (generation !== sessionGeneration) return; // switched mid-start: drop this loop.
      loopId = started.loopId;
      window.__wanderLoopId = loopId;
      saveSession(loopId);
      setLoopLabel(loopId);
      // fire-and-forget; it re-enables the composer at each park.
      runObserve(generation, loopId, myClient, myAbort);
    } else {
      // Subsequent turns: inject input at the parked loop's next turn boundary. The existing
      // long-lived observe stream carries the new turn's events (composer re-enables on park).
      await myClient.send(loopId, text);
    }
  } catch (err) {
    if (generation !== sessionGeneration) return; // superseded: not this session's problem.
    setConn("error");
    addMessage("error", "", `Failed to reach the concierge: ${String(err)}`);
    pendingBubble = null;
    quietTurn = false;
    turnInFlight = false;
    setComposerEnabled(true);
  }
}

function submit(text) {
  text = (text || "").trim();
  if (!text || turnInFlight) return;
  inputEl.value = "";
  handleSubmit(text);
}

// ─────────────────────── Saved stays (change 0060, D5) ──────────────────────
// The panel is the rentals MCP server's answer, not a client-side tally: it is filled ONLY
// from a `list_favorites` tool result, which the server adjudicated against the calling
// principal's own delegated token. That is why switching users shows a different list —
// and why the app never merges, caches, or carries a list across principals.

// TASK_PREAMBLE is shared by the startLoop task builder (above) and, in a later task, the
// restore-time strip of the turn-0 user.input event — kept as one constant so the two cannot
// drift apart.
const TASK_PREAMBLE = "You are Wander, a friendly vacation-rental concierge. First request: ";

// HUMAN_ENVELOPE is the runtime's injection framing for a `send`: the loop batches queued
// human text into one user message prefixed with this marker (internal/agent/humanmsg.go,
// HumanInjector.Poll). Only the replay path sees it — a live turn is echoed from the
// composer, before the runtime wraps anything.
const HUMAN_ENVELOPE = "[human message]\n";

// SAVED_REFRESH_PROMPT is the quiet turn's ask. It is phrased for the model, which decides
// to call list_favorites; the app cannot (and must not) invoke a tool itself.
const SAVED_REFRESH_PROMPT = "Show me my saved stays.";

function refreshSaved() {
  if (turnInFlight) {
    // The loop only accepts input at a parked boundary; re-arm for the next park instead
    // of dropping the refresh.
    pendingSavedRefresh = true;
    return;
  }
  handleSubmit(SAVED_REFRESH_PROMPT, true);
}

// parseFavorites reads the listing ids out of a list_favorites result. The rentals server
// returns a JSON array of ids; the model's tool plumbing may wrap it in surrounding text,
// so fall back to the first bracketed array before giving up.
function parseFavorites(result) {
  if (!result) return [];
  const s = String(result);
  const asArray = (v) => (Array.isArray(v) ? v.map(String) : null);
  try {
    const direct = asArray(JSON.parse(s));
    if (direct) return direct;
  } catch {}
  const m = s.match(/\[[^\]]*\]/);
  if (m) {
    try {
      const nested = asArray(JSON.parse(m[0]));
      if (nested) return nested;
    } catch {}
  }
  return [];
}

function renderSaved(ids) {
  savedEl.replaceChildren();
  if (!ids.length) {
    const empty = document.createElement("div");
    empty.className = "saved-empty";
    empty.textContent = "Nothing saved yet.";
    savedEl.appendChild(empty);
    return;
  }
  for (const id of ids) {
    const el = document.createElement("div");
    el.className = "saved-item";
    el.dataset.listing = id;
    el.textContent = id;
    savedEl.appendChild(el);
  }
}

// ──────────────────── Session reset + user switching (D5) ───────────────────
// resetSession returns the page to its pristine, no-loop state. It is the ONLY way a
// session ends short of a navigation, and it does the teardown FIRST: the live observe
// stream is aborted through the SDK's idempotent teardown before any state is cleared, so
// there is no window in which a torn-down session's frames could repaint the cleared UI.
// Then the generation advances, which makes any frame still in flight on the old stream
// inert (runObserve checks it), and a fresh AbortController takes over for the next
// session — leaving, as before, exactly one live controller.
function resetSession() {
  sessionAbort.abort();
  sessionAbort = new AbortController();
  sessionGeneration++;

  loopId = null;
  window.__wanderLoopId = null;
  // The two fresh-start gestures (+ New and switchUser) both funnel through here, so both
  // also forget the stored session — no second teardown path.
  clearSession();
  cursor = 0n;
  turnInFlight = false;
  pendingBubble = null;
  quietTurn = false;
  pendingSavedRefresh = false;
  replaying = false;
  realURLs.clear();

  threadEl.innerHTML = initialThreadHTML;
  activityEl.innerHTML = initialActivityHTML;
  if (loopLabel) loopLabel.textContent = initialLoopLabel;
  renderSaved([]);
  setConn("idle");
  for (const k of Object.keys(stats)) {
    stats[k] = 0;
    const el = document.getElementById("stat-" + k);
    if (el) el.textContent = "0";
  }
  setComposerEnabled(true);

  // Per-session instrumentation restarts with the session (the new loop's seqs begin at 1
  // again); __wanderStates stays cumulative, so a lane can still see the teardown.
  window.__wanderSeqs = [];
  window.__wanderParks = 0;
  window.__wanderTerminal = null;
  window.__wanderSavedRefreshes = 0;
  window.__wanderResets++;
  // A reset is a fresh session by definition, never a restored one — and never a paused or
  // a lost one either. (The not_found path re-asserts __wanderRestoreLost right after the
  // reset it triggers, so clearing it here is what makes the flag mean "the session I am
  // looking at now replaced a lost one", not "some session was once lost".)
  window.__wanderRestored = false;
  window.__wanderPaused = false;
  window.__wanderRestoreLost = false;
}

// adoptUser makes `user` the credential in hand: it re-points the SDK client and the UI's
// identity affordances, and NOTHING else. It deliberately does no teardown and starts no
// turn, because it has two callers with different needs — switchUser (which must reset the
// previous principal's session FIRST) and the restore path (which must establish the stored
// principal's credential BEFORE loadSession is consulted, with no session to tear down and
// no model turn to spend). Keeping it credential-only is what lets both share one definition
// of "who we are now" without the restore inheriting a switch's side effects.
function adoptUser(user) {
  currentUser = user;
  client = makeClient(user);
  whoamiEl.textContent = user.label;
  syncPickerSelection();
}

// switchUser resets, re-credentials the SDK client, then asks the NEW principal's agent
// for their saved stays. The order matters: nothing of the previous principal survives
// into the request that follows.
function switchUser(user) {
  if (!user || !user.token) return;
  resetSession();
  adoptUser(user);
  refreshSaved();
}

// demoUsers[0] is always DEV_USER; the rest come from the demo directory.
let demoUsers = [DEV_USER];

function renderPicker() {
  userSelect.replaceChildren();
  demoUsers.forEach((u, i) => {
    const opt = document.createElement("option");
    opt.value = String(i);
    opt.textContent = u.label;
    userSelect.appendChild(opt);
  });
  const custom = document.createElement("option");
  custom.value = "custom";
  custom.textContent = "paste a token…";
  userSelect.appendChild(custom);
  // Re-select whoever is current so populating the list never silently switches user.
  syncPickerSelection();
}

// syncPickerSelection points the picker at whoever `currentUser` is, WITHOUT rebuilding the
// option list (so it is safe to call from inside the picker's own change handler). DEV_USER
// is always demoUsers[0], so the only credential that is not in the list is a pasted one —
// which belongs on the "paste a token…" option, not silently back on dev.
function syncPickerSelection() {
  const idx = demoUsers.indexOf(currentUser);
  userSelect.value = idx >= 0 ? String(idx) : "custom";
}

// loadDemoUsers fetches the demo directory server.js publishes from the demo fuse config.
// A missing/blank endpoint is normal (a plain static host, or no demo config): the picker
// then offers only the built-in dev credential.
async function loadDemoUsers() {
  const users = [DEV_USER];
  try {
    const res = await fetch("./demo-users.json", { cache: "no-store" });
    if (res.ok) {
      const list = await res.json();
      for (const u of Array.isArray(list) ? list : []) {
        if (!u || !u.token) continue;
        const tenant = String(u.tenant || "_default");
        const subject = String(u.subject || "user");
        users.push({ token: String(u.token), tenant, subject, label: `${subject}@${tenant}` });
      }
    }
  } catch {
    // Non-fatal by design: the picker degrades to the dev credential.
  }
  demoUsers = users;
  renderPicker();
}

// ─────────────────────────────── Link grounding ─────────────────────────────
// realURLs accumulates every URL that actually appeared in a tool RESULT this session.
// Card links are cross-checked against it, so a URL the model INVENTED (not grounded in a
// real search result) is flagged rather than presented as a trustworthy listing link. This
// is the client half of link grounding; the prompt is the model half. It came across with
// the UI from the retired demo and is deliberately kept — a concierge that renders
// hallucinated booking links as real ones demos the opposite of the point.
const realURLs = new Set();
function harvestURLs(text) {
  if (!text) return;
  const re = /https?:\/\/[^\s"'<>)\]]+/g;
  let m;
  while ((m = re.exec(String(text))) !== null) realURLs.add(m[0].replace(/[.,;]+$/, ""));
}
function urlIsGrounded(u) {
  if (!u) return false;
  if (realURLs.has(u)) return true;
  // host-level match: a listing deep-link counts if its host showed up in a result.
  try {
    const h = new URL(u).host;
    for (const r of realURLs) {
      try {
        if (new URL(r).host === h) return true;
      } catch {}
    }
  } catch {}
  return false;
}

// ───────────────────────────── Answer rendering ─────────────────────────────
// renderAnswer replaces the pending bubble's streamed plain text with the concierge look:
// lightly-rendered markdown plus rental cards when the answer uses the card shape. The
// markdown is HTML-ESCAPED FIRST and only then given a fixed set of inline patterns, so
// model output can never inject markup.
function renderAnswer(bubble, md) {
  bubble.classList.add("rendered");
  bubble.innerHTML = mdToHtml(md);
  const cards = extractCards(md);
  if (cards.length >= 2) {
    const wrap = document.createElement("div");
    wrap.className = "cards";
    for (const c of cards) {
      const card = document.createElement("div");
      card.className = "card";
      const title = document.createElement("div");
      title.className = "c-title";
      title.textContent = c.title;
      card.appendChild(title);
      if (c.price) {
        const price = document.createElement("div");
        price.className = "c-price";
        price.textContent = c.price;
        card.appendChild(price);
      }
      const desc = document.createElement("div");
      desc.className = "c-desc";
      desc.textContent = c.desc;
      card.appendChild(desc);
      if (c.url && urlIsGrounded(c.url)) {
        const a = document.createElement("a");
        a.className = "c-link";
        a.target = "_blank";
        a.rel = "noopener";
        a.href = c.url;
        a.textContent = "View listing ↗";
        card.appendChild(a);
      } else if (c.url) {
        const warn = document.createElement("span");
        warn.className = "c-unverified";
        warn.title = "This link was not found in any live search result";
        warn.textContent = "⚠︎ unverified link";
        card.appendChild(warn);
      }
      wrap.appendChild(card);
    }
    bubble.appendChild(wrap);
  }
  scrollThread();
}

// very small, safe markdown → HTML (escape first, then a few inline patterns)
function mdToHtml(md) {
  let s = String(md).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
  s = s.replace(/\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)/g, '<a href="$2" target="_blank" rel="noopener">$1</a>');
  s = s.replace(/(^|[\s(])((https?:\/\/[^\s<)]+))/g, '$1<a href="$2" target="_blank" rel="noopener">$2</a>');
  s = s.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
  s = s.replace(/`([^`]+)`/g, "<code>$1</code>");
  s = s.replace(/^###?\s*(.+)$/gm, "<h3>$1</h3>");
  s = s.replace(/^\s*[-*]\s+(.+)$/gm, "<li>$1</li>");
  s = s.replace(/(<li>[\s\S]*?<\/li>)/g, (m) => "<ul>" + m + "</ul>").replace(/<\/ul>\s*<ul>/g, "");
  s = s
    .split(/\n{2,}/)
    .map((blk) => (/^<(h3|ul|li)/.test(blk.trim()) ? blk : "<p>" + blk.replace(/\n/g, "<br>") + "</p>"))
    .join("");
  return s;
}

// extractCards pulls structured rental cards out of an answer that uses the
// `**Name** — price — why — url` shape; anything else just renders as prose.
function extractCards(md) {
  const cards = [];
  for (const line of String(md).split(/\n+/)) {
    const m = line.match(/^\s*(?:[-*]\s*)?\*\*(.+?)\*\*\s*[—-]\s*(.+)$/);
    if (!m) continue;
    const title = m[1].trim();
    const rest = m[2];
    const urlMatch = rest.match(/(https?:\/\/[^\s)]+)/);
    const url = urlMatch ? urlMatch[1] : "";
    const priceMatch = rest.match(/(\$\s?[\d,]+(?:\s*(?:\/|per)\s*night|\/nt|\s*night)?)/i);
    const price = priceMatch ? priceMatch[1].replace(/\s+/g, " ").trim() : "";
    let desc = rest
      .replace(url, "")
      .replace(priceMatch ? priceMatch[1] : "", "")
      .replace(/[—-]\s*/g, " ")
      .replace(/\s+/g, " ")
      .trim();
    desc = desc.replace(/\[|\]|\(|\)/g, "").trim();
    if (title) cards.push({ title, price, desc: truncate(desc, 140), url });
  }
  return cards.slice(0, 4);
}

// ─────────────────────────────── Small helpers ──────────────────────────────
// toolBaseName strips an MCP tool's server qualification: "mcp:rentals/list_favorites" →
// "list_favorites". A built-in tool has no qualification and passes through unchanged.
function toolBaseName(name) {
  const s = String(name || "");
  const slash = s.lastIndexOf("/");
  return slash >= 0 ? s.slice(slash + 1) : s;
}

function extractArg(args, keys) {
  if (!args) return "";
  let obj = args;
  if (typeof args === "string") {
    try {
      obj = JSON.parse(args);
    } catch {
      return args;
    }
  }
  for (const k of keys) if (obj && obj[k]) return String(obj[k]);
  if (obj && Array.isArray(obj.queries) && obj.queries.length) return String(obj.queries[0]);
  return "";
}
function truncate(s, n) {
  s = String(s || "");
  return s.length > n ? s.slice(0, n - 1) + "…" : s;
}
function prettyURL(u) {
  try {
    const x = new URL(u);
    return x.hostname.replace(/^www\./, "") + x.pathname.slice(0, 30);
  } catch {
    return truncate(u, 60);
  }
}

// ─────────────────────────────── Wire up the UI ─────────────────────────────
formEl.addEventListener("submit", (e) => {
  e.preventDefault();
  submit(inputEl.value);
});

for (const chip of chipEls) {
  chip.addEventListener("click", () => submit(chip.textContent));
}

// "＋ New" starts a fresh conversation with the SAME principal. It goes through the exact
// teardown a user switch does (abort the live stream first, then clear) rather than
// reloading the page — one reset path, exercised by both gestures, is what keeps the
// switch path honest.
if (resetBtn) resetBtn.addEventListener("click", () => resetSession());

// ─────────────────────────── Identity picker wiring ─────────────────────────
userSelect.addEventListener("change", () => {
  if (userSelect.value === "custom") {
    customRow.hidden = false;
    customToken.focus();
    return; // nothing switches until a token is actually supplied.
  }
  customRow.hidden = true;
  const next = demoUsers[Number(userSelect.value)];
  if (!next || next === currentUser) return;
  switchUser(next);
});

customApply.addEventListener("click", () => {
  const token = customToken.value.trim();
  if (!token) {
    customToken.focus();
    return;
  }
  const tenant = customTenant.value.trim() || "_default";
  // `custom: true` marks a credential the app cannot name or re-resolve later: its token is
  // pasted (never persisted — see saveSession) and its {tenant, subject} is a guess, not the
  // server's resolution. Sessions started under it are deliberately not restorable.
  switchUser({ token, tenant, subject: "custom", label: `custom@${tenant}`, custom: true });
});

whoamiEl.textContent = currentUser.label;
renderPicker();
// Fetch the demo directory in the background. Deliberately NOT followed by a saved-panel
// refresh: the page must not start a loop before the user says something (#54's stateless
// boundary), so the panel first fills on an explicit user switch or after a favorite lands.
// The promise is kept: restoreSession awaits it — and ONLY it — when the stored session
// belongs to a principal other than the one the page loads with.
const demoUsersReady = loadDemoUsers();

// Restore-on-load (change 0062, D1).
//
// The page always loads as DEV_USER, but a session may have been minted under any demo
// principal, so a restore has to re-establish that principal's credential BEFORE it can
// decide anything. The order below is the whole point and must not be rearranged:
//
//   1. read the stored entry (raw — no principal filtering: we are asking WHOSE it is);
//   2. if it names the credential already in hand (the common dev case), fall straight
//      through with NO await, so that path is byte-identical to a synchronous restore;
//   3. otherwise wait for the demo directory and look the stored principal up in it. The
//      TOKEN comes from the directory, never from storage — we persist a principal's name,
//      not its credential. A principal that is no longer in the directory (removed from the
//      demo config, a failed fetch, or a pasted credential that was never persisted in the
//      first place) is not a restorable session: forget it and stay a clean fresh session;
//   4. adopt that credential, and only THEN consult loadSession().
//
// loadSession() stays the authoritative gate in every path: it is re-run after the adoption
// and still refuses anything whose {tenant, subject} does not match the credential now in
// hand. That is what keeps a restore from ever issuing a cross-owner Observe — which the
// server would reject with PermissionDenied, a confusing failure where a fresh session is
// the honest answer. Adopting a directory principal is not an escalation: those tokens are
// published to this page already (server.js's opt-in demo directory) and the picker hands
// any of them out on one click; storage can only name a principal, never mint a credential.
async function restoreSession() {
  const stored = readSession();
  if (!stored) return;
  if (!principalMatches(stored)) {
    const generation = sessionGeneration;
    await demoUsersReady;
    // The user can act during that await (the composer is live). If they did — started a
    // turn, or switched/reset, which bumps the generation — their session wins outright:
    // never re-credential or re-point a loop out from under a live one.
    if (generation !== sessionGeneration || loopId !== null || turnInFlight) return;
    const owner = demoUsers.find(
      (u) => u.tenant === stored.tenant && u.subject === stored.subject,
    );
    if (!owner) {
      clearSession();
      return;
    }
    adoptUser(owner);
  }
  const restored = loadSession();
  if (!restored) return;
  loopId = restored.loopId;
  window.__wanderLoopId = loopId;
  // Everything the durable stream is about to replay is history, not live activity: the
  // flag stays true until the user's next submit (a parked loop emits nothing on its own).
  replaying = true;
  setLoopLabel(loopId);
  window.__wanderRestored = true;
  // fire-and-forget, exactly as on the mint path: it re-enables the composer at each park.
  runObserve(sessionGeneration, loopId, client, sessionAbort);
}

restoreSession();

// Page-unload teardown (D3): abort the CURRENT session's observe stream so a navigation
// never leaks a stream. Idempotent — a double abort, or an abort after a switch already
// tore that session down, is a no-op.
window.addEventListener("pagehide", () => sessionAbort.abort());

inputEl.focus();
