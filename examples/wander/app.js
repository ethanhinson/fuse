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
let observing = false;
// cursor is the highest seq rendered so far — the resume point for the NEXT turn's observe
// so we never re-render an earlier turn's history. Starts at 0 (from the beginning).
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
// `fromSeq`, rendering streamed assistant text (model.delta), tool activity (tool.call),
// and the final answer (loop.parked). It resolves when the loop parks (isCompletion), so
// the composer re-enables for the next turn. Reconnect + no-loss/no-dup is the SDK's job.
async function observeReply(fromSeq) {
  observing = true;
  // A live message bubble the streamed text accumulates into; created on first text.
  let replyEl = null;
  let replyText = "";
  let toolEl = null;

  try {
    for await (const ev of client.observe(loopId, {
      fromSeq,
      signal: pageAbort.signal,
      onState: (s) => setConn(s),
    })) {
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
          // The terminal answer for this exchange. If we streamed nothing (a non-streamed
          // gateway), render the parked content directly; otherwise it matches the stream.
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
      cursor = ev.seq; // advance the resume point past every rendered event.
      if (isCompletion(ev)) {
        // Parked at a turn boundary: the concierge is ready for the next message.
        return ev.seq;
      }
    }
  } catch (err) {
    if (err instanceof FuseTerminalError) {
      // A terminal Connect code (auth rejected, loop gone/finished): stop, show the right
      // affordance — do NOT silently hot-loop.
      setConn("error");
      addMessage("error", "", `Connection closed (${err.code}). Refresh to start a new session.`);
    } else if (!pageAbort.signal.aborted) {
      setConn("error");
      addMessage("error", "", `Unexpected error: ${String(err)}`);
    }
    return fromSeq;
  } finally {
    observing = false;
  }
  return fromSeq;
}

async function handleSubmit(text) {
  addMessage("user", "You", text);
  sendBtn.disabled = true;
  inputEl.disabled = true;
  try {
    if (!loopId) {
      // First message: start a persistent interactive loop so context holds across turns.
      const started = await client.startLoop({
        task: `You are Wander, a friendly vacation-rental concierge. First request: ${text}`,
        model: "cloud/x",
        interactive: true,
      });
      loopId = started.loopId;
    } else {
      // Subsequent turns: inject input at the parked loop's next turn boundary.
      await client.send(loopId, text);
    }
    // Resume from the cursor (highest seq already rendered) so a new turn never re-renders
    // an earlier turn's history; the SDK's watermark dedup also drops any replay overlap.
    await observeReply(cursor);
  } catch (err) {
    setConn("error");
    addMessage("error", "", `Failed to reach the concierge: ${String(err)}`);
  } finally {
    sendBtn.disabled = false;
    inputEl.disabled = false;
    inputEl.focus();
  }
}

formEl.addEventListener("submit", (e) => {
  e.preventDefault();
  const text = inputEl.value.trim();
  if (!text || observing) return;
  inputEl.value = "";
  handleSubmit(text);
});

// Page-unload teardown (D3): abort the live observe stream so a navigation never leaks a
// stream. Idempotent — a double abort is a no-op.
window.addEventListener("pagehide", () => pageAbort.abort());
