// shipmates bridge UI — vanilla JS, no build step, no framework.
//
// Model:
//   - Poll /api/leads every 5s to refresh the lead list.
//   - When the operator clicks a lead, open an EventSource to
//     /api/lead/<key>/stream and append events to the feed pane.
//   - Closing/switching leads tears the EventSource down — no history is
//     retained on navigate-away (matches the stream-to-browser model).

const $ = (id) => document.getElementById(id);
const leadsList = $("leads-list");
const status = $("status");
const feedTitle = $("feed-title");
const feedMeta = $("feed-meta");
const feedBody = $("feed-body");
const tellForm = $("tell-form");
const tellPersona = $("tell-persona");
const tellMessage = $("tell-message");

let selected = null;
let stream = null;
let knownLeads = new Map();
let mateStatus = new Map(); // client_key -> [{persona, status, tool, input, pending_id, since}]

async function refreshLeads() {
  try {
    const r = await fetch("/api/leads");
    if (r.status === 401) { window.location.href = "/login"; return; }
    if (!r.ok) throw new Error(`HTTP ${r.status}`);
    const data = await r.json();
    status.textContent = `${data.filter(l => l.connected).length} online`;
    status.className = data.some(l => l.connected) ? "online" : "offline";
    renderLeads(data);
  } catch (err) {
    status.textContent = "bridge unreachable";
    status.className = "offline";
  }
}

function renderLeads(data) {
  knownLeads = new Map(data.map(l => [l.client_key, l]));
  updateTellEnabled(); // selected lead's online state may have flipped
  if (data.length === 0) {
    leadsList.innerHTML = '<li class="empty">no leads connected</li>';
    return;
  }
  data.sort((a, b) => a.client_key.localeCompare(b.client_key));
  leadsList.innerHTML = "";
  for (const lead of data) {
    const li = document.createElement("li");
    if (lead.client_key === selected) li.classList.add("selected");
    const dot = document.createElement("span");
    dot.className = "dot" + (lead.connected ? " online" : "");
    li.appendChild(dot);
    li.appendChild(document.createTextNode(lead.client_key));
    li.title = `repo=${lead.repo}\npersona=${lead.persona}\nport=${lead.port}\nlast_seen=${lead.last_seen}`;
    li.onclick = () => selectLead(lead.client_key);
    const mates = mateStatus.get(lead.client_key) || [];
    if (mates.length > 0) {
      const row = document.createElement("div");
      row.className = "mates";
      for (const m of mates) {
        const md = document.createElement("span");
        md.className = "mate " + m.status;
        md.textContent = m.persona;
        md.title = m.status === "blocked"
          ? `${m.persona}: blocked on ${m.tool}${m.input ? " — " + m.input : ""}`
          : `${m.persona}: ${m.status}${m.since ? " since " + m.since : ""}`;
        if (m.status === "blocked") {
          // A blocked mate is actionable: jump to its lead so the pending
          // pane (filtered to the selected lead) surfaces the approval.
          md.onclick = (ev) => { ev.stopPropagation(); selectLead(lead.client_key); };
        }
        row.appendChild(md);
      }
      li.appendChild(row);
    }
    leadsList.appendChild(li);
  }
}

function selectLead(key) {
  if (selected === key) return;
  selected = key;
  feedBody.innerHTML = "";
  refreshLeads(); // re-render to update .selected
  const lead = knownLeads.get(key);
  feedTitle.textContent = key;
  feedMeta.textContent = lead && lead.connected
    ? `online · port ${lead.port}`
    : "offline";
  // Default the tell-form persona to whatever the lead announced as its own
  // persona (the trailing segment of the clientKey). Operator can override to
  // address a crew member instead. Updated only when empty or when the field
  // still holds the previous lead's persona — never clobber a manual edit.
  const defaultPersona = lead && lead.persona ? lead.persona : "";
  if (!tellPersona.value || tellPersona.value === tellPersona.dataset.autofill) {
    tellPersona.value = defaultPersona;
  }
  tellPersona.dataset.autofill = defaultPersona;
  updateTellEnabled();
  refreshPending(); // switch the pending pane to this lead immediately
  openStream(key);
}

// updateTellEnabled greys out the tell form when the selected lead is offline
// or unselected. Stops the operator from firing 504s into a dead tunnel.
function updateTellEnabled() {
  const lead = selected ? knownLeads.get(selected) : null;
  const enabled = lead && lead.connected;
  tellPersona.disabled = !enabled;
  tellMessage.disabled = !enabled;
  const btn = tellForm.querySelector("button");
  if (btn) btn.disabled = !enabled;
  tellMessage.placeholder = enabled
    ? "message…"
    : (selected ? "lead offline" : "select a lead first");
}

function openStream(key) {
  if (stream) {
    stream.close();
    stream = null;
  }
  if (!key) return;
  stream = new EventSource(`/api/lead/${encodeURIComponent(key)}/stream`);
  let disconnected = false;
  stream.onopen = () => { disconnected = false; };
  stream.onmessage = (m) => {
    try {
      const e = JSON.parse(m.data);
      appendEvent(e);
    } catch {
      // ignore malformed lines
    }
  };
  // onerror fires on EVERY auto-reconnect attempt while the bridge is down,
  // so we only log the first one per disconnect to avoid filling the feed
  // with noise. onopen clears the flag when the stream reconnects.
  stream.onerror = () => {
    if (!disconnected) {
      disconnected = true;
      appendEvent({ time: nowISO(), persona: "(bridge)", type: "stream", text: "disconnected — reconnecting…" });
    }
  };
}

function appendEvent(e) {
  const div = document.createElement("span");
  div.className = "ev" + (e.type && e.type.startsWith("permission") ? " permission" : "");
  div.innerHTML =
    `<span class="ts">[${escape(e.time || "")}]</span> ` +
    `<span class="who">${escape(e.persona || "?")}</span>/` +
    `<span class="kind">${escape(e.type || "?")}</span>: ` +
    `${escape(e.text || "")}\n`;
  feedBody.appendChild(div);
  feedBody.scrollTop = feedBody.scrollHeight;
}

function escape(s) {
  return String(s)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;");
}

function nowISO() {
  return new Date().toISOString().replace("T", " ").slice(0, 19);
}

tellForm.onsubmit = async (e) => {
  e.preventDefault();
  if (!selected) return;
  const persona = tellPersona.value.trim();
  const message = tellMessage.value.trim();
  if (!persona || !message) return;
  try {
    const r = await fetch(
      `/api/lead/${encodeURIComponent(selected)}/tell/${encodeURIComponent(persona)}`,
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ message }),
      }
    );
    if (r.status === 401) { window.location.href = "/login"; return; }
    if (!r.ok) throw new Error(`HTTP ${r.status}`);
    tellMessage.value = "";
  } catch (err) {
    appendEvent({ time: nowISO(), persona: "(bridge)", type: "tell-error", text: String(err) });
  }
};

// --- pending permission pane ----------------------------------------------
//
// Polls /api/pending every 1.5s. Renders rows in a sticky pane above the tell
// form, filtered to the SELECTED lead. The bottom-most row is the keyboard
// target: pressing 1 allows it, 2 denies it (ignored while typing in inputs).
// New entries play a short ping so the operator notices when away from the tab.

const pendingPane = document.getElementById("pending-pane");
let seenPendingIds = new Set();
let visiblePendings = []; // current rows shown, in render order (top → bottom)
let audioCtx = null;

async function refreshPending() {
  try {
    const r = await fetch("/api/pending");
    if (r.status === 401) { window.location.href = "/login"; return; }
    if (!r.ok) return;
    const items = await r.json();
    renderPending(items);
  } catch {
    // network blip; next tick will retry
  }
}

function renderPending(items) {
  items = (items || []).filter(it => it.client_key === selected);
  // Ping on any newly-seen id, globally (so cross-lead pendings still alert).
  const currentIds = new Set((items || []).map(i => i.client_key + ":" + i.id));
  for (const id of currentIds) {
    if (!seenPendingIds.has(id)) ping();
  }
  seenPendingIds = currentIds;

  if (items.length === 0) {
    pendingPane.hidden = true;
    pendingPane.innerHTML = "";
    visiblePendings = [];
    return;
  }
  pendingPane.hidden = false;
  pendingPane.innerHTML = "";
  visiblePendings = items;
  items.forEach((it, idx) => {
    const isTarget = idx === items.length - 1; // bottom-most is the kb target
    const row = document.createElement("div");
    row.className = "row" + (isTarget ? " target" : "");
    const meta = document.createElement("div");
    meta.className = "meta";
    meta.innerHTML =
      `<div><span class="who">${escape(it.persona)}</span> wants ` +
      `<span class="tool">${escape(it.tool)}</span></div>` +
      (it.input ? `<div class="cmd">${escape(it.input)}</div>` : "");
    const allow = document.createElement("button");
    allow.className = "allow";
    allow.innerHTML = `allow ${isTarget ? '<span class="key">1</span>' : ""}`;
    allow.onclick = () => resolvePending(it, "allow", row);
    const deny = document.createElement("button");
    deny.className = "deny";
    deny.innerHTML = `deny ${isTarget ? '<span class="key">2</span>' : ""}`;
    deny.onclick = () => resolvePending(it, "deny", row);
    row.appendChild(meta);
    row.appendChild(allow);
    row.appendChild(deny);
    pendingPane.appendChild(row);
  });
}

async function resolvePending(it, behavior, row) {
  row.querySelectorAll("button").forEach(b => b.disabled = true);
  try {
    const r = await fetch(
      `/api/lead/${encodeURIComponent(it.client_key)}/resolve/${encodeURIComponent(it.id)}`,
      { method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ behavior }) }
    );
    if (r.status === 401) { window.location.href = "/login"; return; }
    refreshPending();
  } catch {
    row.querySelectorAll("button").forEach(b => b.disabled = false);
  }
}

// Keyboard 1/2 resolve the bottom-most pending row. Ignored when the operator
// is typing in any input (so typing "1" in the tell box doesn't approve).
window.addEventListener("keydown", (e) => {
  const tag = (e.target && e.target.tagName) || "";
  if (tag === "INPUT" || tag === "TEXTAREA" || e.target.isContentEditable) return;
  if (visiblePendings.length === 0) return;
  if (e.key !== "1" && e.key !== "2") return;
  e.preventDefault();
  const target = visiblePendings[visiblePendings.length - 1];
  const row = pendingPane.lastElementChild;
  resolvePending(target, e.key === "1" ? "allow" : "deny", row);
});

// ping is a single soft tone via the Web Audio API — no asset to embed.
function ping() {
  try {
    if (!audioCtx) audioCtx = new (window.AudioContext || window.webkitAudioContext)();
    const o = audioCtx.createOscillator();
    const g = audioCtx.createGain();
    o.connect(g); g.connect(audioCtx.destination);
    o.type = "sine"; o.frequency.value = 880;
    g.gain.setValueAtTime(0.0001, audioCtx.currentTime);
    g.gain.exponentialRampToValueAtTime(0.08, audioCtx.currentTime + 0.02);
    g.gain.exponentialRampToValueAtTime(0.0001, audioCtx.currentTime + 0.25);
    o.start(); o.stop(audioCtx.currentTime + 0.3);
  } catch { /* audio unavailable, silent */ }
}

// --- mate status dots -------------------------------------------------------
//
// Polls /api/status (the bridge fans out to every connected lead's
// /status.json) and re-renders the lead list so each row shows its mates as
// colored chips: red=blocked, yellow=working, green=idle, blue=done. Status is
// derived server-side from hook events and the pending queue — no heuristics.

async function refreshStatus() {
  try {
    const r = await fetch("/api/status");
    if (r.status === 401) { window.location.href = "/login"; return; }
    if (!r.ok) return;
    const items = await r.json();
    const next = new Map();
    for (const it of items || []) {
      if (!next.has(it.client_key)) next.set(it.client_key, []);
      next.get(it.client_key).push(it);
    }
    mateStatus = next;
    renderLeads([...knownLeads.values()]);
  } catch {
    // network blip; next tick will retry
  }
}

updateTellEnabled(); // initial: form starts disabled until a lead is selected
refreshLeads();
setInterval(refreshLeads, 5000);
refreshPending();
setInterval(refreshPending, 1500);
refreshStatus();
setInterval(refreshStatus, 3000);

// --- live terminal pane -----------------------------------------------------
//
// "⌨ term" opens a PTY-hosted mate on the selected lead: POST .../pty/{p}/start
// spawns (or finds) `claude --agent {p}` under a ConPTY on the lead's machine,
// then an EventSource on .../stream delivers base64 screen bytes (snapshot =
// backscroll, data = live) straight into xterm.js. Keystrokes go back via
// POST .../input, so this is a read-write attach. One terminal at a time.

const termPane = $("term-pane");
const termHost = $("term-host");
const termTitle = $("term-title");
const termCloseBtn = $("term-close");
const termOpenBtn = $("term-open");

let term = null;       // xterm.js instance
let termES = null;     // EventSource for the PTY stream
let termBase = null;   // API base for the attached mate

function b64ToBytes(s) {
  const bin = atob(s);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return bytes;
}

function closeTerminal() {
  if (termES) { termES.close(); termES = null; }
  if (term) { term.dispose(); term = null; }
  termBase = null;
  termHost.innerHTML = "";
  termPane.hidden = true;
}

async function openTerminal(key, persona) {
  closeTerminal();
  const base = `/api/lead/${encodeURIComponent(key)}/pty/${encodeURIComponent(persona)}`;
  try {
    const r = await fetch(`${base}/start`, { method: "POST" });
    if (r.status === 401) { window.location.href = "/login"; return; }
    if (!r.ok) throw new Error(await r.text() || `HTTP ${r.status}`);
  } catch (err) {
    appendEvent({ time: nowISO(), persona: "(bridge)", type: "term-error", text: String(err) });
    return;
  }
  termBase = base;
  termPane.hidden = false;
  termTitle.textContent = `${persona} @ ${key}`;

  term = new Terminal({ cols: 120, rows: 30, fontSize: 13, scrollback: 5000 });
  term.open(termHost);
  term.onData((data) => {
    // keystrokes from the browser to the mate's PTY; fire-and-forget
    if (termBase) fetch(`${termBase}/input`, { method: "POST", body: data });
  });
  term.focus();

  termES = new EventSource(`${base}/stream`);
  const write = (m) => { if (term) term.write(b64ToBytes(m.data)); };
  termES.addEventListener("snapshot", write);
  termES.addEventListener("data", write);
  termES.addEventListener("exit", () => {
    if (term) term.write("\r\n\x1b[2m[mate exited]\x1b[0m\r\n");
    if (termES) { termES.close(); termES = null; }
  });
}

termOpenBtn.onclick = () => {
  if (!selected) return;
  const persona = tellPersona.value.trim();
  if (!persona) { tellPersona.focus(); return; }
  openTerminal(selected, persona);
};
termCloseBtn.onclick = closeTerminal;
