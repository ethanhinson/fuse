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

import {
  createClient,
  isCompletion,
  FuseTerminalError,
} from "./vendor/fuse-sdk.js";

// The dev token → _default tenant is the built-in credential `fuse loop-serve-net` uses
// when loop_server.auth is unset (local demo). Configure real tokens for a shared server.
const client = createClient({
  baseUrl: window.location.origin, // same-origin; server.js proxies the Connect path.
  credentials: { token: "fuse-dev-token", tenant: "_default" },
});

// Test instrumentation (harmless in normal use): the headless-browser reconnect CI lane
// reads these to assert no-loss/no-dup across a mid-stream network kill and to watch the
// connection-state transitions. They only ever record what the SDK's public API already
// surfaces (seqs + onState) — no reach past the SDK.
window.__wanderSeqs = [];
window.__wanderStates = [];
window.__wanderTerminal = null;
window.__wanderParks = 0; // count of loop.parked completions (turn boundaries) observed.

const chatEl = document.getElementById("chat");
const formEl = document.getElementById("composer");
const inputEl = document.getElementById("input");
const sendBtn = document.getElementById("send");
const connEl = document.getElementById("conn");
const connLabel = document.getElementById("conn-label");

// Session state. Wander is stateless across page loads (#54 boundary): a refresh starts a
// fresh loop. `loopId` is created lazily on the first message (persistent interactive loop
// so the concierge holds context across turns, #53).
let loopId = null;
// turnInFlight guards against a second submit while the current turn is still being answered
// (composer disabled). It is distinct from "is the observe stream open" — the persistent
// observe runs for the whole session; only a turn awaiting its reply blocks new input.
let turnInFlight = false;
// cursor is the highest seq observed so far (the SDK's own reconnect watermark is internal;
// this mirrors it for diagnostics).
let cursor = 0n;
// One AbortController per page lifetime: aborting it on pagehide tears the live observe
// stream down (D3) so a navigation never leaks a stream.
const pageAbort = new AbortController();

function setConn(state) {
  connEl.dataset.state = state;
  connLabel.textContent = state;
}

function addMessage(role, who, text) {
  const el = document.createElement("div");
  el.className = `msg ${role}`;
  if (who) {
    const whoEl = document.createElement("div");
    whoEl.className = "who";
    whoEl.textContent = who;
    el.appendChild(whoEl);
  }
  const textEl = document.createElement("div");
  textEl.className = "text";
  textEl.textContent = text;
  el.appendChild(textEl);
  chatEl.appendChild(el);
  chatEl.scrollTop = chatEl.scrollHeight;
  return textEl;
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

// observeReply drives one concierge turn: it consumes the loop's event stream from
// runObserve is the SINGLE, long-lived observe over the interactive loop's ONE stream. It
// stays open for the whole session (one loop_id, one stream) — each turn's events flow on
// it and a `send` just injects input at the parked boundary; we do NOT re-open per turn.
// It renders streamed assistant text (model.delta), tool activity (tool.call), and each
// turn's final answer (loop.parked), re-enabling the composer on each park. Transparent
// reconnect + no-loss/no-dup across a mid-stream drop is the SDK's job — this consumer just
// keeps reading. It runs until the page aborts (D3) or a FuseTerminalError stops it (D2).
async function runObserve() {
  // The in-progress reply bubble for the CURRENT turn; reset at each park.
  let replyEl = null;
  let replyText = "";
  let toolEl = null;

  try {
    for await (const ev of client.observe(loopId, {
      fromSeq: 0n,
      signal: pageAbort.signal,
      onState: (s) => {
        setConn(s);
        window.__wanderStates.push(s);
      },
    })) {
      window.__wanderSeqs.push(Number(ev.seq));
      cursor = ev.seq;
      const p = decodePayload(ev);
      switch (ev.kind) {
        case "model.delta": {
          if (p.text) {
            if (!replyEl) replyEl = addMessage("concierge", "Concierge", "");
            replyText += p.text;
            replyEl.textContent = replyText;
            chatEl.scrollTop = chatEl.scrollHeight;
          }
          break;
        }
        case "tool.call": {
          toolEl = addMessage("tool", "", `🔎 looking things up… (${p.name || "tool"})`);
          break;
        }
        case "tool.result": {
          if (toolEl) toolEl.parentElement.remove();
          toolEl = null;
          break;
        }
        case "loop.parked": {
          // The terminal answer for this exchange. With a non-streamed gateway there is no
          // model.delta, so render the parked content directly; otherwise it matches.
          if (!replyText && p.content) {
            addMessage("concierge", "Concierge", p.content);
          } else if (replyEl && p.content && replyText !== p.content) {
            replyEl.textContent = p.content;
          }
          break;
        }
        case "error": {
          addMessage("error", "", `Error: ${p.err || "unknown"}`);
          break;
        }
      }
      if (isCompletion(ev)) {
        // Parked at a turn boundary: the concierge is ready for the next message. Reset the
        // per-turn render state and re-enable the composer — but KEEP the stream open.
        window.__wanderParks++;
        replyEl = null;
        replyText = "";
        toolEl = null;
        turnInFlight = false;
        sendBtn.disabled = false;
        inputEl.disabled = false;
        inputEl.focus();
      }
    }
  } catch (err) {
    if (err instanceof FuseTerminalError) {
      // A terminal Connect code (auth rejected, loop gone/finished): stop, show the right
      // affordance — do NOT silently hot-loop.
      window.__wanderTerminal = err.code;
      setConn("error");
      addMessage("error", "", `Connection closed (${err.code}). Refresh to start a new session.`);
    } else if (!pageAbort.signal.aborted) {
      setConn("error");
      addMessage("error", "", `Unexpected error: ${String(err)}`);
    }
  }
}

async function handleSubmit(text) {
  addMessage("user", "You", text);
  turnInFlight = true;
  sendBtn.disabled = true;
  inputEl.disabled = true;
  try {
    if (!loopId) {
      // First message: start a persistent interactive loop so context holds across turns,
      // then open the ONE long-lived observe stream (kept open for the whole session).
      const started = await client.startLoop({
        task: `You are Wander, a friendly vacation-rental concierge. First request: ${text}`,
        model: "cloud/x",
        interactive: true,
      });
      loopId = started.loopId;
      runObserve(); // fire-and-forget; it re-enables the composer at each park.
    } else {
      // Subsequent turns: inject input at the parked loop's next turn boundary. The existing
      // long-lived observe stream carries the new turn's events (composer re-enables on park).
      await client.send(loopId, text);
    }
  } catch (err) {
    setConn("error");
    addMessage("error", "", `Failed to reach the concierge: ${String(err)}`);
    turnInFlight = false;
    sendBtn.disabled = false;
    inputEl.disabled = false;
  }
}

formEl.addEventListener("submit", (e) => {
  e.preventDefault();
  const text = inputEl.value.trim();
  if (!text || turnInFlight) return;
  inputEl.value = "";
  handleSubmit(text);
});

// Page-unload teardown (D3): abort the live observe stream so a navigation never leaks a
// stream. Idempotent — a double abort is a no-op.
window.addEventListener("pagehide", () => pageAbort.abort());
