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
        // Every chip is a talk-to affordance: select the lead and target the
        // persona in the tell form. For blocked mates that also surfaces the
        // pending pane (it filters to the selected lead).
        md.onclick = (ev) => {
          ev.stopPropagation();
          selectLead(lead.client_key);
          tellPersona.value = m.persona;
          tellPersona.dataset.autofill = m.persona;
          updateFeedTitle();
          // desktop nicety only: on touch, focusing pops the keyboard (and
          // iOS zooms to the field) when the user may just be looking
          if (!window.matchMedia("(pointer: coarse)").matches) tellMessage.focus();
        };
        row.appendChild(md);
      }
      li.appendChild(row);
    }
    leadsList.appendChild(li);
  }
}

// updateFeedTitle shows WHO you're addressing, not just which ship you're on:
// "security @ laptop:lead" when the tell form targets a persona, the bare
// client key otherwise. Feed content stays ship-wide either way.
function updateFeedTitle() {
  if (!selected) { feedTitle.textContent = "select a lead"; return; }
  const persona = tellPersona.value.trim();
  feedTitle.textContent = persona ? `${persona} @ ${selected}` : selected;
}

function selectLead(key) {
  document.body.classList.remove("drawer-open"); // mobile: give the feed full width
  if (selected === key) return;
  selected = key;
  feedBody.innerHTML = "";
  refreshLeads(); // re-render to update .selected
  const lead = knownLeads.get(key);
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
  updateFeedTitle();
  updateTellEnabled();
  refreshPending(); // switch the pending pane to this lead immediately
  openStream(key);
}

// retitle live as the operator retargets — chip taps and manual edits alike
tellPersona.addEventListener("input", updateFeedTitle);

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
    // no local echo: the server-side tell event arrives through the stream
    // within a tick; echoing here would double every line
  } catch (err) {
    appendEvent({ time: nowISO(), persona: "(bridge)", type: "tell-error", text: String(err) });
  }
};

// iOS freezes background tabs and can leave the EventSource as a zombie that
// never fires onerror or reconnects. When the tab becomes visible again,
// tear the stream down and reopen it, and refresh the polled panes right away.
document.addEventListener("visibilitychange", () => {
  if (document.visibilityState !== "visible") return;
  if (selected) {
    feedBody.innerHTML = ""; // stream replays history on reconnect — avoid dupes
    openStream(selected);
  }
  refreshLeads();
  refreshPending();
  refreshStatus();
});

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

// --- live terminal pane (multi-session, tabbed) -------------------------------
//
// "⌨ term" opens a PTY-hosted mate for whatever persona the tell form targets
// (tap a mate chip to target it) on the selected lead. Multiple terminals stay
// open concurrently — each keeps its EventSource + xterm instance alive; tabs
// on top switch between them. Tab label is persona@ship so you always know
// which mate you're typing into. The big close button closes the ACTIVE tab.

const termPane = $("term-pane");
const termHost = $("term-host");
const termTabs = $("term-tabs");
const termCloseBtn = $("term-close");
const termOpenBtn = $("term-open");

const terms = new Map(); // id -> {key, persona, base, term, fit, es, host}
let activeTermId = null;

function termSessionId(key, persona) { return key + "::" + persona; }
function activeTerm() { return terms.get(activeTermId) || null; }

function b64ToBytes(s) {
  const bin = atob(s);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return bytes;
}

function renderTermTabs() {
  termTabs.innerHTML = "";
  for (const [id, t] of terms) {
    const tab = document.createElement("button");
    tab.type = "button";
    tab.className = "term-tab" + (id === activeTermId ? " active" : "");
    const label = document.createElement("span");
    // persona@ship — ship is the lead name (client_key up to the colon)
    label.textContent = `${t.persona}@${t.key.split(":")[0]}`;
    tab.appendChild(label);
    const x = document.createElement("span");
    x.className = "x";
    x.textContent = "×";
    x.onclick = (ev) => { ev.stopPropagation(); closeTerm(id); };
    tab.appendChild(x);
    tab.onclick = () => activateTerm(id);
    termTabs.appendChild(tab);
  }
}

function applyFit(t) {
  try { t.fit.fit(); } catch { return; }
  fetch(`${t.base}/resize`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ cols: t.term.cols, rows: t.term.rows }),
  });
}

function activateTerm(id) {
  const t = terms.get(id);
  if (!t) return;
  activeTermId = id;
  termPane.hidden = false;
  for (const [oid, o] of terms) o.host.style.display = oid === id ? "block" : "none";
  renderTermTabs();
  // fit after the host is visible — hidden elements measure 0×0
  requestAnimationFrame(() => { applyFit(t); t.term.focus(); });
}

function closeTerm(id) {
  const t = terms.get(id);
  if (!t) return;
  if (t.es) t.es.close();
  t.term.dispose();
  t.host.remove();
  terms.delete(id);
  if (activeTermId === id) {
    activeTermId = null;
    const next = terms.keys().next();
    if (!next.done) { activateTerm(next.value); return; }
    termPane.hidden = true;
    // drop any keyboard-driven viewport pinning
    termPane.style.top = "";
    termPane.style.height = "";
    termPane.style.bottom = "";
  }
  renderTermTabs();
}

async function openTerminal(key, persona) {
  const id = termSessionId(key, persona);
  if (terms.has(id)) { activateTerm(id); return; }

  const base = `/api/lead/${encodeURIComponent(key)}/pty/${encodeURIComponent(persona)}`;
  try {
    const r = await fetch(`${base}/start`, { method: "POST" });
    if (r.status === 401) { window.location.href = "/login"; return; }
    if (!r.ok) throw new Error(await r.text() || `HTTP ${r.status}`);
  } catch (err) {
    appendEvent({ time: nowISO(), persona: "(bridge)", type: "term-error", text: String(err) });
    return;
  }

  const host = document.createElement("div");
  host.className = "term-instance";
  termHost.appendChild(host);

  const term = new Terminal({ fontSize: 13, scrollback: 5000 });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(host);
  term.onData((data) => fetch(`${base}/input`, { method: "POST", body: data }));
  // Ctrl+V / Cmd+V paste: xterm.js doesn't intercept it by default and the
  // keystroke would otherwise reach the mate as a literal ^V. term.paste()
  // honors bracketed-paste mode when the TUI has it enabled.
  term.attachCustomKeyEventHandler((e) => {
    if (e.type === "keydown" && (e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "v") {
      navigator.clipboard.readText().then((text) => { if (text) term.paste(text); }).catch(() => {});
      return false;
    }
    return true;
  });

  const entry = { key, persona, base, term, fit, es: null, host };
  const es = new EventSource(`${base}/stream`);
  entry.es = es;
  const write = (m) => term.write(b64ToBytes(m.data));
  es.addEventListener("snapshot", write);
  es.addEventListener("data", write);
  es.addEventListener("exit", () => {
    term.write("\r\n\x1b[2m[mate exited]\x1b[0m\r\n");
    es.close();
    entry.es = null;
  });

  terms.set(id, entry);
  activateTerm(id);
}

// refit only the visible terminal on window/keyboard resizes
window.addEventListener("resize", () => {
  const t = activeTerm();
  if (t && !termPane.hidden) applyFit(t);
});

// Mobile keyboards overlay the page without resizing it, so a fixed inset-0
// pane stays under the keys. The VisualViewport API reports the actually
// visible rect; pin the pane to it while the keyboard is up, restore when it
// drops. Fit is debounced — the keyboard animation fires a burst of resizes
// and each fit round-trips a PTY resize.
if (window.visualViewport) {
  let vvFitTimer = null;
  const vv = window.visualViewport;
  const onVV = () => {
    if (termPane.hidden) return;
    if (window.matchMedia("(max-width: 640px)").matches) {
      termPane.style.top = vv.offsetTop + "px";
      termPane.style.height = vv.height + "px";
      termPane.style.bottom = "auto";
    } else {
      termPane.style.top = "";
      termPane.style.height = "";
      termPane.style.bottom = "";
    }
    clearTimeout(vvFitTimer);
    vvFitTimer = setTimeout(() => {
      const t = activeTerm();
      if (t && !termPane.hidden) applyFit(t);
    }, 150);
  };
  vv.addEventListener("resize", onVV);
  vv.addEventListener("scroll", onVV);
}

termOpenBtn.onclick = () => {
  if (!selected) return;
  const persona = tellPersona.value.trim();
  if (!persona) { tellPersona.focus(); return; }
  openTerminal(selected, persona);
};
termCloseBtn.onclick = () => { if (activeTermId) closeTerm(activeTermId); };

// --- terminal scroll buttons --------------------------------------------------
//
// Touch scrolling inside xterm.js is unreliable, so scrolling is explicit:
// ▲/▼ buttons float bottom-right of the terminal (coarse-pointer devices
// only — desktop has the wheel). Hold to repeat; speed ramps 1→8 lines per
// tick the longer you hold. Buffer-aware:
//   - normal buffer:    local scrollback (no round trip)
//   - alternate buffer: arrow keys to the mate — TUIs own their scrolling
//     there, same as desktop alternate-scroll wheel behavior.

const termScrollUp = $("term-scroll-up");
const termScrollDown = $("term-scroll-down");

function scrollTerm(lines) {
  const t = activeTerm();
  if (!t) return;
  if (t.term.buffer.active.type === "alternate") {
    const seq = (lines < 0 ? "\x1b[A" : "\x1b[B").repeat(Math.abs(lines));
    fetch(`${t.base}/input`, { method: "POST", body: seq });
  } else {
    t.term.scrollLines(lines);
  }
}

let holdTimer = null;
let holdStart = 0;
let lastDownTap = 0;

function startHold(dir) {
  stopHold();
  holdStart = Date.now();
  const tick = () => {
    // +1 line/tick every 500ms held, capped at 8/tick (~66 lines/s at max)
    const speed = Math.min(1 + Math.floor((Date.now() - holdStart) / 500), 8);
    scrollTerm(dir * speed);
  };
  tick();
  holdTimer = setInterval(tick, 120);
}

function stopHold() {
  if (holdTimer) { clearInterval(holdTimer); holdTimer = null; }
}

// double-tap ▼ = jump to the live bottom. Normal buffer: scrollToBottom().
// Alternate buffer: End key — most TUIs (claude included) treat it as
// jump-to-latest; harmless where they don't.
function jumpToBottom() {
  const t = activeTerm();
  if (!t) return;
  if (t.term.buffer.active.type === "alternate") {
    fetch(`${t.base}/input`, { method: "POST", body: "\x1b[F" });
  } else {
    t.term.scrollToBottom();
  }
}

for (const [btn, dir] of [[termScrollUp, -1], [termScrollDown, 1]]) {
  btn.addEventListener("pointerdown", (e) => {
    e.preventDefault();
    if (dir === 1) {
      const now = Date.now();
      if (now - lastDownTap < 350) {
        lastDownTap = 0;
        stopHold();
        jumpToBottom();
        return;
      }
      lastDownTap = now;
    }
    startHold(dir);
  });
  for (const ev of ["pointerup", "pointercancel", "pointerleave"]) {
    btn.addEventListener(ev, stopHold);
  }
}

// --- mobile leads drawer ------------------------------------------------------
//
// On narrow screens (<=640px, see style.css) the leads rail is an off-canvas
// drawer toggled by the header hamburger. Selecting a lead closes it so the
// feed gets the full width; the backdrop tap dismisses without selecting.

const leadsToggle = $("leads-toggle");
const drawerBackdrop = $("drawer-backdrop");

leadsToggle.onclick = () => document.body.classList.toggle("drawer-open");
drawerBackdrop.onclick = () => document.body.classList.remove("drawer-open");
