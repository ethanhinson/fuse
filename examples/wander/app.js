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
// reading. It runs until the page aborts (D3) or a FuseTerminalError stops it (D2).
async function runObserve() {
  // The in-progress reply bubble for the CURRENT turn; reset at each park.
  let replyText = "";

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
      bumpStat("events");
      const p = decodePayload(ev);
      switch (ev.kind) {
        case "turn.start": {
          bumpStat("turns");
          setPhase(pendingBubble, "Thinking through your request…");
          break;
        }
        case "model.call.start": {
          setPhase(pendingBubble, "Consulting the model…");
          break;
        }
        case "model.delta": {
          if (p.text) {
            if (!pendingBubble) pendingBubble = addPendingConcierge();
            replyText += p.text;
            pendingBubble.textContent = replyText; // drops the spinner on first token
            scrollThread();
          }
          break;
        }
        case "tool.call": {
          const name = p.name || "tool";
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
          } else if (name === "list_favorites") {
            pushActivity("tool", "✦", "Reading your saved stays", "");
          } else if (name === "spawn_agent") {
            bumpStat("agents");
            pushActivity("agent", "🤝", "Dispatched a scout",
              truncate(extractArg(p.args, ["label", "task", "prompt"]) || "sub-agent", 90));
            setPhase(pendingBubble, "Coordinating research scouts…");
          } else {
            pushActivity("tool", "⚙︎", "Tool call", name);
          }
          break;
        }
        case "tool.result": {
          // Harvest every URL the tool actually returned, so card links can be verified
          // against real results (link grounding — see harvestURLs).
          harvestURLs(p.result);
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
          if (answer) {
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
        setComposerEnabled(true);
        inputEl.focus();
      }
    }
  } catch (err) {
    if (err instanceof FuseTerminalError) {
      // A terminal Connect code (auth rejected, loop gone/finished): stop, show the right
      // affordance — do NOT silently hot-loop.
      window.__wanderTerminal = err.code;
      setConn("error");
      pushActivity("error", "⚠︎", "Stream closed", String(err.code));
      addMessage("error", "", `Connection closed (${err.code}). Refresh to start a new session.`);
    } else if (!pageAbort.signal.aborted) {
      setConn("error");
      addMessage("error", "", `Unexpected error: ${String(err)}`);
    }
  }
}

// pendingBubble is the concierge reply bubble for the turn currently in flight (module
// scope so both the submit path and the observe loop can reach it).
let pendingBubble = null;

async function handleSubmit(text) {
  addMessage("user", "you", text);
  pendingBubble = addPendingConcierge();
  turnInFlight = true;
  setComposerEnabled(false);
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
      if (loopLabel) {
        loopLabel.textContent = `Live loop ${String(loopId).slice(0, 12)}… — streaming over Connect`;
      }
      runObserve(); // fire-and-forget; it re-enables the composer at each park.
    } else {
      // Subsequent turns: inject input at the parked loop's next turn boundary. The existing
      // long-lived observe stream carries the new turn's events (composer re-enables on park).
      await client.send(loopId, text);
    }
  } catch (err) {
    setConn("error");
    addMessage("error", "", `Failed to reach the concierge: ${String(err)}`);
    pendingBubble = null;
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

// "＋ New" starts a fresh conversation by RELOADING the page. Wander is deliberately
// stateless across page loads (#54 boundary), so a reload is the honest reset: it tears the
// live observe stream down through the same pagehide abort path as any navigation rather
// than hand-rolling a second teardown route that could leak the previous principal's stream.
if (resetBtn) resetBtn.addEventListener("click", () => window.location.reload());

// Page-unload teardown (D3): abort the live observe stream so a navigation never leaks a
// stream. Idempotent — a double abort is a no-op.
window.addEventListener("pagehide", () => pageAbort.abort());

inputEl.focus();
