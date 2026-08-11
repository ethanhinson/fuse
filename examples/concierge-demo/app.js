// Concierge web client for fuse binding #3 (networked runtime binding).
//
// It speaks the exact loop.* JSON-RPC protocol the WS transport serves:
//
//   → loop.start   { task, model, interactive }       ⇒ { loop_id }
//   → loop.observe { loop_id, from_seq }              ⇒ { replayed, last_seq }
//   ← loop.event   { loop_id, event, gap }            (id-less server push)
//   → loop.send    { loop_id, input }                 ⇒ {}   (follow-up turns, same loop)
//
// One WebSocket carries the whole session. Every loop.event is an event.Event:
//   { seq, ts, turn, kind, payload }  with kind ∈ turn.start | model.call.start |
//   model.call.end | tool.call | tool.result | spawn.start | spawn.done |
//   loop.parked | error | …
//
// Completion is DETERMINISTIC via loop.parked: in interactive mode the server emits
// it when the loop answers and parks awaiting the next message, carrying the final
// answer text. The client renders that reply and re-enables input — it never guesses
// completion from the shape of the event stream (which is unreliable once the loop
// persists across turns instead of finishing). Intermediate steps stream to the rail.

const WS_URL = (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/ws';
const MODEL = 'glm'; // capable gateway model; never Claude for demo traffic

// ─────────────────────────────── DOM ───────────────────────────────
const $ = (s) => document.querySelector(s);
const thread = $('#thread');
const activity = $('#activity');
const input = $('#input');
const sendBtn = $('#send');
const dot = $('#dot');
const connText = $('#conn-text');
const loopLabel = $('#loop-label');

const stats = { turns: 0, searches: 0, agents: 0, events: 0 };
function bumpStat(k, n = 1) { stats[k] += n; $('#stat-' + k).textContent = stats[k]; }

// ─────────────────────────── Connection ────────────────────────────
let ws = null;
let ready = false;
let loopId = null;
let observing = false;
let lastSeq = 0;               // client-tracked watermark (reconnect cursor)
const seen = new Set();        // dedup at watermark
let pendingBubble = null;      // the assistant bubble currently "thinking"
let busy = false;              // a turn is in flight
let firstTaskSent = false;

function setConn(state, text) {
  dot.className = 'dot dot-' + state;
  connText.textContent = text;
}

function connect() {
  setConn('idle', 'connecting…');
  ws = new WebSocket(WS_URL);
  ws.onopen = () => { ready = true; setConn('live', 'connected · binding #3'); refreshSendState(); };
  ws.onclose = () => { ready = false; setConn('off', 'disconnected'); refreshSendState(); };
  ws.onerror = () => { setConn('off', 'connection error'); };
  ws.onmessage = (e) => {
    let msg; try { msg = JSON.parse(e.data); } catch { return; }
    routeMessage(msg);
  };
}

let rpcId = 0;
const nextId = () => ++rpcId;
function rpc(method, params) {
  ws.send(JSON.stringify({ jsonrpc: '2.0', id: nextId(), method, params }));
}

// ───────────────────────── Protocol routing ────────────────────────
function routeMessage(msg) {
  // loop.start result → we have a loop_id; immediately observe from 0.
  if (msg.id && msg.result && typeof msg.result.loop_id === 'string') {
    loopId = msg.result.loop_id;
    loopLabel.innerHTML = `Live loop <code>${loopId.slice(0, 12)}…</code> — streaming over WebSocket`;
    if (!observing) { observing = true; rpc('loop.observe', { loop_id: loopId, from_seq: 0 }); }
    return;
  }
  // loop.observe ack → { replayed, last_seq }
  if (msg.id && msg.result && 'replayed' in msg.result) {
    lastSeq = Math.max(lastSeq, msg.result.last_seq || 0);
    return;
  }
  // loop.send ack (empty object) — nothing to do; events already flowing.
  if (msg.id && msg.result && !('loop_id' in msg.result) && !('replayed' in msg.result)) return;

  // error frame
  if (msg.id && msg.error) { pushActivity('error', '⚠︎', 'Server error', msg.error.message); finishTurn(true); return; }

  // id-less server push: loop.event
  if (msg.method === 'loop.event' && msg.params) {
    const ev = msg.params.event;
    if (!ev) return;
    if (seen.has(ev.seq)) return;      // dedup at watermark
    seen.add(ev.seq);
    lastSeq = Math.max(lastSeq, ev.seq);
    if (msg.params.gap) pushActivity('think', '↺', 'Gap detected — re-syncing', `seq ${ev.seq}`);
    handleEvent(ev);
  }
}

// ─────────────────────── Event → UI mapping ────────────────────────
function decodePayload(ev) {
  // Event.Payload is raw JSON already parsed by JSON.parse of the whole frame.
  return ev.payload || {};
}

// realURLs accumulates every URL that actually appeared in a web_search / web_fetch
// tool RESULT this session. Card links are cross-checked against it so a URL the
// model invented (not grounded in a real search result) is flagged, not shown as a
// trustworthy listing link. This is the client half of link-grounding; the prompt is
// the model half.
const realURLs = new Set();
function harvestURLs(text) {
  if (!text) return;
  const re = /https?:\/\/[^\s"'<>)\]]+/g;
  let m;
  while ((m = re.exec(String(text))) !== null) realURLs.add(m[0].replace(/[.,;]+$/, ''));
}
function urlIsGrounded(u) {
  if (!u) return false;
  if (realURLs.has(u)) return true;
  // host-level match: a listing deep-link counts if its host showed up in a result.
  try { const h = new URL(u).host; for (const r of realURLs) { try { if (new URL(r).host === h) return true; } catch {} } } catch {}
  return false;
}

function handleEvent(ev) {
  bumpStat('events');
  const p = decodePayload(ev);
  switch (ev.kind) {
    case 'turn.start':
      bumpStat('turns');
      setPhase('Thinking through your request…');
      break;

    case 'model.call.start':
      setPhase('Consulting the model…');
      break;

    case 'model.call.end': {
      // Purely a phase cue. The authoritative "answer ready" signal is loop.parked
      // (below), emitted by the server when the interactive loop reaches its terminal
      // turn — so the client never has to guess which model.call.end was final.
      const hasTools = Array.isArray(p.tool_calls) && p.tool_calls.length > 0;
      if (hasTools) setPhase('Deciding what to look up…');
      else if (p.content && p.content.trim()) {
        // Remember the latest no-tool content as a FALLBACK answer, in case the
        // server is an older build without loop.parked (graceful degradation).
        lastAnswerContent = p.content;
      }
      break;
    }

    case 'tool.call': {
      const name = p.name || 'tool';
      if (name === 'web_search') {
        bumpStat('searches');
        const q = extractArg(p.args, ['query', 'q', 'search']) || 'the web';
        pushActivity('search', '🔍', 'Searching the web', truncate(q, 90));
        setPhase('Searching live listings…');
      } else if (name === 'web_fetch') {
        const u = extractArg(p.args, ['url', 'link']) || '';
        pushActivity('search', '🌐', 'Reading a listing page', prettyURL(u));
        setPhase('Reading the best matches…');
      } else if (name === 'spawn_agent') {
        bumpStat('agents');
        const label = extractArg(p.args, ['label', 'task', 'prompt']) || 'sub-agent';
        pushActivity('agent', '🤝', 'Dispatched a scout', truncate(label, 90));
        setPhase('Coordinating research scouts…');
      } else if (name === 'skill') {
        const s = extractArg(p.args, ['name', 'skill']) || '';
        pushActivity('tool', '✦', 'Using skill', s || 'research');
      } else {
        pushActivity('tool', '⚙︎', 'Tool call', name);
      }
      break;
    }

    case 'tool.result':
      // Harvest every URL the tool actually returned, so card links can be verified
      // against real results (link grounding). ToolResultPayload.result is a string.
      harvestURLs(p.result);
      break;

    case 'spawn.start':
      pushActivity('agent', '➤', 'Scout started', p.label || p.child_node_id || '');
      break;
    case 'spawn.done':
      // A scout's result text also carries real source URLs it found.
      harvestURLs(p.result);
      pushActivity('agent', '✓', 'Scout reported back', truncate(p.result || p.label || '', 90));
      break;

    case 'context.summarize':
      pushActivity('think', '🗜', 'Compressing context', `turns ${p.turn_start}–${p.turn_end}`);
      break;

    case 'error':
      pushActivity('error', '⚠︎', 'Loop error', truncate(p.err || 'unknown', 120));
      break;

    case 'loop.parked':
      // Deterministic end-of-exchange: the interactive loop answered and is now
      // waiting for the next message. Render the carried answer and re-enable input.
      // This is the reliable replacement for guessing completion from the stream.
      renderAnswer(p.content || lastAnswerContent || '(no answer returned)');
      lastAnswerContent = null;
      finishTurn(false);
      setConn('live', 'connected · your turn');
      break;

    case 'turn.end':
      // No-op for completion now (loop.parked owns that). Kept only so the switch is
      // exhaustive/readable; the phase cue below keeps the spinner honest between turns.
      break;
  }
}

// lastAnswerContent is only a FALLBACK for an older server that doesn't emit
// loop.parked: it holds the most recent no-tool model content, which the loop.parked
// handler prefers over but falls back to. Completion itself is driven by loop.parked,
// not by reconstructing it from the stream.
let lastAnswerContent = null;

// ───────────────────────── Rendering ───────────────────────────────
function setPhase(text) {
  if (!pendingBubble) return;
  const ph = pendingBubble.querySelector('.phase');
  if (ph) ph.textContent = text;
}

function pushActivity(cls, ico, title, meta) {
  const empty = activity.querySelector('.activity-empty');
  if (empty) empty.remove();
  const el = document.createElement('div');
  el.className = 'act ' + cls;
  el.innerHTML = `<span class="ico">${ico}</span><div class="body"><div class="title"></div>${meta ? '<div class="meta"></div>' : ''}</div>`;
  el.querySelector('.title').textContent = title;
  if (meta) el.querySelector('.meta').textContent = meta;
  activity.appendChild(el);
  activity.scrollTop = activity.scrollHeight;
  // cap the rail
  while (activity.children.length > 60) activity.removeChild(activity.firstChild);
}

function addUserMessage(text) {
  const el = document.createElement('div');
  el.className = 'msg user';
  el.innerHTML = `<div class="avatar">you</div><div class="bubble"></div>`;
  el.querySelector('.bubble').textContent = text;
  thread.appendChild(el);
  scrollThread();
}

function addPendingAssistant() {
  const el = document.createElement('div');
  el.className = 'msg assistant';
  el.innerHTML = `<div class="avatar">✷</div><div class="bubble">
    <div class="thinking"><span class="spin"></span><span class="phase">Getting to work…</span></div>
  </div>`;
  thread.appendChild(el);
  pendingBubble = el.querySelector('.bubble');
  scrollThread();
}

function renderAnswer(md) {
  if (!pendingBubble) addPendingAssistant();
  pendingBubble.innerHTML = mdToHtml(md);
  const cards = extractCards(md);
  if (cards.length >= 2) {
    const wrap = document.createElement('div');
    wrap.className = 'cards';
    for (const c of cards) {
      const card = document.createElement('div');
      card.className = 'card';
      const grounded = urlIsGrounded(c.url);
      card.innerHTML =
        `<div class="c-title"></div>` +
        (c.price ? `<div class="c-price"></div>` : '') +
        `<div class="c-desc"></div>` +
        (c.url && grounded ? `<a class="c-link" target="_blank" rel="noopener">View listing ↗</a>` : '') +
        (c.url && !grounded ? `<span class="c-unverified" title="This link was not found in any live search result">⚠︎ unverified link</span>` : '');
      card.querySelector('.c-title').textContent = c.title;
      if (c.price) card.querySelector('.c-price').textContent = c.price;
      card.querySelector('.c-desc').textContent = c.desc;
      if (c.url && grounded) { const a = card.querySelector('.c-link'); a.href = c.url; }
      wrap.appendChild(card);
    }
    pendingBubble.appendChild(wrap);
  }
  pendingBubble = null;
  scrollThread();
}

function finishTurn(isError) {
  busy = false;
  refreshSendState();
  if (isError && pendingBubble) {
    pendingBubble.innerHTML = `<span style="color:var(--danger)">The concierge hit an error. Try again?</span>`;
    pendingBubble = null;
  }
  setConn('live', 'connected · idle');
}

function scrollThread() { thread.scrollTop = thread.scrollHeight; }

// ─────────────────────── Sending / turns ───────────────────────────
function refreshSendState() {
  sendBtn.disabled = !ready || busy || !input.value.trim();
}

function startConversation(text) {
  const task =
    `You are "Wander", a warm, concise vacation-rental concierge in an ONGOING chat with a traveler. ` +
    `Use the web_search tool (and web_fetch when helpful) to find REAL, currently-listed vacation rentals that match each request. ` +
    `This is a multi-turn conversation: after you answer, the traveler may ask follow-ups ("what about Aspen?", "cheaper?", "for 6 people") — treat each as a continuation, keeping earlier context.\n\n` +
    `LINK RULES (important): only cite a URL that literally appeared in one of YOUR web_search or web_fetch tool results this conversation. Never invent, guess, or complete a URL. If you did not get a real link for a stay, omit the link rather than fabricate one.\n\n` +
    `FORMAT: a short friendly intro, then 2–4 specific stays, each on its own line as:\n` +
    `**<Property name>** — <price per night> — <one-line why it fits> — <real source URL from a tool result>\n` +
    `Keep it skimmable.\n\n` +
    `The traveler's first request: "${text}".`;
  addUserMessage(text);
  addPendingAssistant();
  busy = true; lastAnswerContent = null;
  refreshSendState();
  // interactive:true → the server-side loop PARKS at each answer awaiting the next
  // loop.send, so this one loop_id carries the whole conversation (change: persistent
  // conversational loop). Follow-ups reuse the SAME loop with server-side history.
  rpc('loop.start', { task, model: MODEL, interactive: true });
  firstTaskSent = true;
}

function sendFollowup(text) {
  addUserMessage(text);
  addPendingAssistant();
  busy = true; lastAnswerContent = null;
  refreshSendState();
  rpc('loop.send', { loop_id: loopId, input: text });
}

function submit(text) {
  text = text.trim();
  if (!text || !ready || busy) return;
  input.value = '';
  refreshSendState();
  if (!firstTaskSent || !loopId) startConversation(text);
  else sendFollowup(text);
}

// ─────────────────────────── Helpers ───────────────────────────────
function extractArg(args, keys) {
  if (!args) return '';
  let obj = args;
  if (typeof args === 'string') { try { obj = JSON.parse(args); } catch { return args; } }
  for (const k of keys) if (obj && obj[k]) return String(obj[k]);
  // nested queries[] shape some providers use
  if (obj && Array.isArray(obj.queries) && obj.queries.length) return String(obj.queries[0]);
  return '';
}
function truncate(s, n) { s = String(s || ''); return s.length > n ? s.slice(0, n - 1) + '…' : s; }
function prettyURL(u) { try { const x = new URL(u); return x.hostname.replace(/^www\./, '') + x.pathname.slice(0, 30); } catch { return truncate(u, 60); } }

// very small, safe markdown → HTML (escape first, then a few inline patterns)
function mdToHtml(md) {
  let s = String(md).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  s = s.replace(/\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)/g, '<a href="$2" target="_blank" rel="noopener">$1</a>');
  s = s.replace(/(^|[\s(])((https?:\/\/[^\s<)]+))/g, '$1<a href="$2" target="_blank" rel="noopener">$2</a>');
  s = s.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
  s = s.replace(/`([^`]+)`/g, '<code>$1</code>');
  // headings
  s = s.replace(/^###?\s*(.+)$/gm, '<h3>$1</h3>');
  // bullets
  s = s.replace(/^\s*[-*]\s+(.+)$/gm, '<li>$1</li>');
  s = s.replace(/(<li>[\s\S]*?<\/li>)/g, (m) => '<ul>' + m + '</ul>').replace(/<\/ul>\s*<ul>/g, '');
  // paragraphs
  s = s.split(/\n{2,}/).map((blk) => /^<(h3|ul|li)/.test(blk.trim()) ? blk : '<p>' + blk.replace(/\n/g, '<br>') + '</p>').join('');
  return s;
}

// pull structured rental cards out of the answer when it uses the **Name** — price — why — url shape
function extractCards(md) {
  const cards = [];
  const lines = String(md).split(/\n+/);
  for (const line of lines) {
    const m = line.match(/^\s*(?:[-*]\s*)?\*\*(.+?)\*\*\s*[—-]\s*(.+)$/);
    if (!m) continue;
    const title = m[1].trim();
    const rest = m[2];
    const urlMatch = rest.match(/(https?:\/\/[^\s)]+)/);
    const url = urlMatch ? urlMatch[1] : '';
    const priceMatch = rest.match(/(\$\s?[\d,]+(?:\s*(?:\/|per)\s*night|\/nt|\s*night)?)/i);
    const price = priceMatch ? priceMatch[1].replace(/\s+/g, ' ').trim() : '';
    let desc = rest.replace(url, '').replace(priceMatch ? priceMatch[1] : '', '').replace(/[—-]\s*/g, ' ').replace(/\s+/g, ' ').trim();
    desc = desc.replace(/\[|\]|\(|\)/g, '').trim();
    if (title) cards.push({ title, price, desc: truncate(desc, 140), url });
  }
  return cards.slice(0, 4);
}

// ─────────────────────────── Wire up UI ────────────────────────────
$('#composer').addEventListener('submit', (e) => { e.preventDefault(); submit(input.value); });
input.addEventListener('input', refreshSendState);
document.querySelectorAll('.chip').forEach((c) => c.addEventListener('click', () => { input.value = c.textContent; submit(c.textContent); }));
$('#reset').addEventListener('click', () => {
  thread.innerHTML = '';
  activity.innerHTML = '<div class="activity-empty">The concierge’s reasoning, web searches, and sub-agents will stream here.</div>';
  loopId = null; observing = false; firstTaskSent = false; busy = false; seen.clear(); lastSeq = 0; realURLs.clear();
  Object.keys(stats).forEach((k) => { stats[k] = 0; $('#stat-' + k).textContent = 0; });
  loopLabel.textContent = 'Ask about a destination and I’ll search the live web for real stays.';
  // fresh WS session per conversation keeps loop isolation clean
  try { ws.close(); } catch {}
  connect();
  input.focus();
});

connect();
input.focus();
