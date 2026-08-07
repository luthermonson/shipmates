// shipmates fleet UI — vanilla JS, no build step, no framework.
//
// Model:
//   - Poll /api/captains every 5s to refresh the captain list.
//   - When the operator clicks a captain, open an EventSource to
//     /api/captain/<key>/stream and append events to the feed pane.
//   - Closing/switching captains tears the EventSource down — no history is
//     retained on navigate-away (matches the stream-to-browser model).

import {
  b64ToBytes,
  escapeHTML as escape,
  humanSize,
  nowISO,
  shortModel,
  truncateName,
} from "/utils.js?v=51";
import { apiFetch } from "/api.js?v=51";

const $ = (id) => document.getElementById(id);
const captainsList = $("captains-list");
const status = $("status");
const feedTitle = $("feed-title");
const feedMeta = $("feed-meta");
const feedBody = $("feed-body");
const tellForm = $("tell-form");
const tellPersona = $("tell-persona");
const tellMessage = $("tell-message");

let selected = null;
let stream = null;
let knownCaptains = new Map();
let mateStatus = new Map(); // client_key -> [{persona, status, tool, input, pending_id, since}]

async function refreshCaptains() {
  try {
    const r = await apiFetch("/api/captains");
    if (r.status === 401) { window.location.href = "/login"; return; }
    if (!r.ok) throw new Error(`HTTP ${r.status}`);
    const data = await r.json();
    status.textContent = `${data.filter(l => l.connected).length} online`;
    status.className = data.some(l => l.connected) ? "online" : "offline";
    renderCaptains(data);
  } catch (err) {
    status.textContent = "fleet unreachable";
    status.className = "offline";
  }
}

// The first-load empty state: instead of a blank feed and a hidden drawer,
// show every ship as a tappable card with its live status. Disappears once a
// captain is selected.
const captainPicker = $("captain-picker");

function renderCaptainPicker() {
  if (selected || knownCaptains.size === 0) {
    captainPicker.hidden = true;
    captainPicker.innerHTML = "";
    return;
  }
  captainPicker.hidden = false;
  captainPicker.innerHTML = "";
  const captains = [...knownCaptains.values()].sort((a, b) => a.client_key.localeCompare(b.client_key));
  for (const captain of captains) {
    const card = document.createElement("div");
    card.className = "ship-card";
    const head = document.createElement("div");
    head.className = "head";
    const dot = document.createElement("span");
    dot.className = "dot" + (captain.connected ? " online" : "");
    head.appendChild(dot);
    head.appendChild(document.createTextNode(captain.client_key));
    const meta = document.createElement("small");
    meta.textContent = captain.connected ? `online · ${captain.repo}` : "offline";
    head.appendChild(meta);
    card.appendChild(head);
    const mates = orderMates(captain, mateStatus.get(captain.client_key) || []);
    if (mates.length > 0) {
      const row = document.createElement("div");
      row.className = "mates";
      for (const m of mates) {
        const md = document.createElement("span");
        md.className = "mate " + m.status;
        md.textContent = m.persona;
        row.appendChild(md);
      }
      card.appendChild(row);
    }
    card.onclick = () => selectCaptain(captain.client_key);
    captainPicker.appendChild(card);
  }
}

// orderMates puts the ship's own captain persona first, crew alphabetical after
// — mirrors the tab/dropdown ordering so every surface reads the same.
function orderMates(captain, mates) {
  return [...mates].sort((a, b) => {
    if (a.persona === captain.persona) return -1;
    if (b.persona === captain.persona) return 1;
    return a.persona.localeCompare(b.persona);
  });
}

function renderCaptains(data) {
  knownCaptains = new Map(data.map(l => [l.client_key, l]));
  updateTellEnabled(); // selected captain's online state may have flipped
  renderCaptainPicker();
  if (data.length === 0) {
    captainsList.innerHTML = '<li class="empty">no captains connected</li>';
    return;
  }
  data.sort((a, b) => a.client_key.localeCompare(b.client_key));
  captainsList.innerHTML = "";
  for (const captain of data) {
    const li = document.createElement("li");
    if (captain.client_key === selected) li.classList.add("selected");
    const dot = document.createElement("span");
    dot.className = "dot" + (captain.connected ? " online" : "");
    li.appendChild(dot);
    li.appendChild(document.createTextNode(captain.client_key));
    li.title = `repo=${captain.repo}\npersona=${captain.persona}\nport=${captain.port}\nlast_seen=${captain.last_seen}`;
    li.onclick = () => selectCaptain(captain.client_key);
    const mates = orderMates(captain, mateStatus.get(captain.client_key) || []);
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
        // Every chip is a talk-to affordance: select the captain and target the
        // persona in the tell form. For blocked mates that also surfaces the
        // pending pane (it filters to the selected captain).
        md.onclick = (ev) => {
          ev.stopPropagation();
          selectCaptain(captain.client_key);
          setTellPersona(m.persona);
          updateFeedTitle();
          feedFilter = m.persona; // chip tap = open that agent's conversation
          renderFeedTabs();
          renderFeed();
          // desktop nicety only: on touch, focusing pops the keyboard (and
          // iOS zooms to the field) when the user may just be looking
          if (!window.matchMedia("(pointer: coarse)").matches) tellMessage.focus();
        };
        row.appendChild(md);
      }
      li.appendChild(row);
    }
    captainsList.appendChild(li);
  }
}

// updateFeedTitle shows WHO you're addressing, not just which ship you're on:
// "security @ laptop:captain" when the tell form targets a persona, the bare
// client key otherwise. Feed content stays ship-wide either way.
function updateFeedTitle() {
  if (!selected) { feedTitle.textContent = "select a captain"; return; }
  const persona = tellPersona.value.trim();
  feedTitle.textContent = persona ? `${persona} @ ${selected}` : selected;
}

// The tell target is a dropdown built from the roster. setTellPersona ensures
// the option exists before selecting it (a <select> silently ignores .value
// assignments for missing options).
function setTellPersona(p) {
  if (![...tellPersona.options].some((o) => o.value === p)) {
    const o = document.createElement("option");
    o.value = p;
    o.textContent = p;
    tellPersona.appendChild(o);
  }
  tellPersona.value = p;
}

function renderTellOptions() {
  const current = tellPersona.value;
  const personas = new Set();
  for (const m of (mateStatus.get(selected) || [])) personas.add(m.persona);
  for (const e of feedEvents) {
    if (e.persona && !e.persona.startsWith("(")) personas.add(e.persona);
  }
  tellPersona.innerHTML = "";
  for (const p of ["all", ...orderedPersonas(personas)]) {
    const o = document.createElement("option");
    o.value = p;
    o.textContent = p;
    tellPersona.appendChild(o);
  }
  tellPersona.value = personas.has(current) || current === "all" ? current : "all";
}

// dropdown change = conversation switch, mirroring the feed tabs
tellPersona.addEventListener("change", () => {
  feedFilter = tellPersona.value;
  updateFeedTitle();
  renderFeedTabs();
  renderFeed();
});

function selectCaptain(key) {
  document.body.classList.remove("drawer-open"); // mobile: give the feed full width
  if (selected === key) return;
  selected = key;
  captainPicker.hidden = true;
  captainPicker.innerHTML = "";
  closeBeads();
  feedBody.innerHTML = "";
  feedEvents = [];
  // picking a ship drops you into its CAPTAIN's conversation — the captain is the
  // natural front door; "all" and crew tabs are one tap away
  const picked = knownCaptains.get(key);
  feedFilter = picked && picked.persona ? picked.persona : "all";
  renderFeedTabs();
  refreshCaptains(); // re-render to update .selected
  const captain = knownCaptains.get(key);
  feedMeta.textContent = captain && captain.connected
    ? `online · port ${captain.port}`
    : "offline";
  // Rebuild the target dropdown for this ship's roster and default it to the
  // captain's own persona (rosters differ per ship, so carrying the previous
  // selection across would leave a dangling target).
  renderTellOptions();
  setTellPersona(captain && captain.persona ? captain.persona : "all");
  updateFeedTitle();
  updateTellEnabled();
  refreshPending(); // switch the pending pane to this captain immediately
  refreshBeadsBadge(); // the ⛃ count is per-ship
  openStream(key);
}


// updateTellEnabled greys out the tell form when the selected captain is offline
// or unselected. Stops the operator from firing 504s into a dead tunnel.
function updateTellEnabled() {
  const captain = selected ? knownCaptains.get(selected) : null;
  const enabled = captain && captain.connected;
  tellPersona.disabled = !enabled;
  tellMessage.disabled = !enabled;
  const btn = tellForm.querySelector("button");
  if (btn) btn.disabled = !enabled;
  tellMessage.placeholder = enabled
    ? "message…"
    : (selected ? "captain offline" : "select a captain first");
}

function openStream(key) {
  if (stream) {
    stream.close();
    stream = null;
  }
  if (!key) return;
  stream = new EventSource(`/api/captain/${encodeURIComponent(key)}/stream`);
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
  // onerror fires on EVERY auto-reconnect attempt while the fleet is down,
  // so we only log the first one per disconnect to avoid filling the feed
  // with noise. onopen clears the flag when the stream reconnects.
  stream.onerror = () => {
    if (!disconnected) {
      disconnected = true;
      appendEvent({ time: nowISO(), persona: "(fleet)", type: "stream", text: "disconnected — reconnecting…" });
    }
  };
}

// --- per-agent feed tabs ------------------------------------------------------
//
// The stream is ship-wide (every persona's events interleave); tabs give each
// agent its own conversation view, same mental model as the terminal tabs.
// "all" shows everything; picking a persona filters the feed AND targets the
// tell form at them. Fleet-status lines ("(fleet)" persona) always show.

const feedTabsEl = $("feed-tabs");
let feedEvents = [];
let feedFilter = "all";

function eventMatchesFilter(e) {
  if (feedFilter === "all") return true;
  if (e.persona && e.persona.startsWith("(")) return true; // fleet notices
  return e.persona === feedFilter;
}

// Persona display order: "all" first, the ship's captain persona second (it
// announces itself in the tunnel identity headers), crew alphabetical after.
function orderedPersonas(personas) {
  const captainPersona = selected && knownCaptains.get(selected) ? knownCaptains.get(selected).persona : "";
  const rest = [...personas].filter((p) => p !== captainPersona).sort();
  return personas.has(captainPersona) ? [captainPersona, ...rest] : rest;
}

function renderFeedTabs() {
  feedTabsEl.innerHTML = "";
  if (!selected) return;
  const personas = new Set();
  for (const m of (mateStatus.get(selected) || [])) personas.add(m.persona);
  for (const e of feedEvents) {
    if (e.persona && !e.persona.startsWith("(")) personas.add(e.persona);
  }
  for (const p of ["all", ...orderedPersonas(personas)]) {
    const tab = document.createElement("button");
    tab.type = "button";
    tab.className = "feed-tab" + (feedFilter === p ? " active" : "");
    tab.textContent = p;
    tab.onclick = () => {
      if (!beadsPane.hidden) closeBeads(); // conversation tabs exit beads mode
      feedFilter = p;
      // "all" is a real tell target too — submit fans out to the whole crew
      setTellPersona(p);
      updateFeedTitle();
      renderFeedTabs();
      renderFeed();
    };
    feedTabsEl.appendChild(tab);
  }
}

function renderFeed() {
  feedBody.innerHTML = "";
  for (const e of feedEvents) {
    if (eventMatchesFilter(e)) appendEventDOM(e);
  }
}

// appendEventDOM renders one event with type-aware structure — the headless
// timeline. Thinking and tool results render collapsed (tap to expand); tool
// hooks render as compact chips; results carry their cost/duration stats
// (already folded into the text server-side) plus the model as a suffix.
function appendEventDOM(e) {
  const t = e.type || "?";
  const div = document.createElement("span");
  const head =
    `<span class="ts">[${escape(e.time || "")}]</span> ` +
    `<span class="who">${escape(e.persona || "?")}</span>`;

  if (t === "thinking" || t === "tool-result") {
    div.className = `ev ${t} collapsible`;
    div.innerHTML = `${head}/<span class="kind">${escape(t)}</span>: ${escape(e.text || "")}\n`;
    div.onclick = () => div.classList.toggle("expanded");
  } else if (t.startsWith("hook:")) {
    div.className = "ev hook";
    const mark = t === "hook:PreToolUse" ? "⚒" : "✓";
    div.innerHTML =
      `${head} <span class="toolchip">${mark} ${escape(e.tool || e.text || "")}</span>` +
      (e.input ? ` <span class="toolinput">${escape(String(e.input).slice(0, 140))}</span>` : "") + "\n";
  } else {
    div.className = "ev"
      + (t.startsWith("permission") ? " permission" : "")
      + (t === "result" ? " result" : "");
    div.innerHTML =
      `${head}/<span class="kind">${escape(t)}</span>: ${linkifyRefs(escape(e.text || ""))}` +
      (t === "result" && e.model ? ` <span class="model">${escape(shortModel(e.model))}</span>` : "") +
      "\n";
  }
  feedBody.appendChild(div);
  feedBody.scrollTop = feedBody.scrollHeight;
}

// linkifyRefs turns gh-<n> / #<n> mentions in (already-escaped) feed text
// into issue links against the selected ship's repo. The #<n> form only
// matches after whitespace/punctuation-openers so escaped entities
// (&amp;#39;) and code fragments don't false-link.
function linkifyRefs(html) {
  const captain = selected ? knownCaptains.get(selected) : null;
  if (!captain || !captain.repo_url) return html;
  const link = (label, n) =>
    `<a href="${captain.repo_url}/issues/${n}" target="_blank" rel="noopener">${label}</a>`;
  return html
    .replace(/\bgh-(\d+)\b/g, (m, n) => link(m, n))
    .replace(/(^|[\s(,:])#(\d+)\b/g, (m, pre, n) => pre + link("#" + n, n));
}

function appendEvent(e) {
  feedEvents.push(e);
  // a first-seen persona grows the tab strip
  if (e.persona && !e.persona.startsWith("(") && !feedTabsEl.textContent.includes(e.persona)) {
    renderFeedTabs();
  }
  if (eventMatchesFilter(e)) appendEventDOM(e);
}

tellForm.onsubmit = async (e) => {
  e.preventDefault();
  if (!selected) return;
  const persona = tellPersona.value.trim();
  const message = tellMessage.value.trim();
  const hasAttachments = stagedAttachments.some((a) => a.status !== "done");
  if (!persona) return;
  if (!message && !hasAttachments) return;

  // "all" broadcasts: one tell per crew member, fanned out client-side.
  let targets = [persona];
  if (persona === "all") {
    targets = (mateStatus.get(selected) || []).map((m) => m.persona);
    if (targets.length === 0) {
      appendEvent({ time: nowISO(), persona: "(fleet)", type: "tell-error", text: "no crew roster yet — wait for status" });
      return;
    }
  }

  const sendBtn = tellForm.querySelector('button[type="submit"]');
  sendBtn.disabled = true;
  const origLabel = sendBtn.textContent;

  // Phase 1: upload each staged attachment (fan out; each attach auto-tells the
  // mate server-side, one tell per file). Broadcast to "all" isn't supported by
  // the attach endpoint (it targets one captain, not one persona) — the server
  // auto-tells whichever mate it configures per attach; here we just POST once
  // per file to the selected captain.
  let attachOK = true;
  if (hasAttachments) {
    const pending = stagedAttachments.filter((a) => a.status !== "done");
    let completed = 0;
    sendBtn.textContent = `uploading 0/${pending.length}…`;
    await Promise.all(pending.map(async (a) => {
      a.status = "uploading";
      a.error = null;
      renderStagedAttachments();
      try {
        const fd = new FormData();
        fd.append("file", a.file, a.file.name);
        if (message) fd.append("caption", message);
        const r = await apiFetch(
          `/api/captain/${encodeURIComponent(selected)}/attach`,
          { method: "POST", body: fd },
        );
        if (r.status === 401) { window.location.href = "/login"; throw new Error("unauthorized"); }
        if (!r.ok) {
          let msg = `HTTP ${r.status}`;
          try {
            const j = await r.json();
            if (j && j.error) msg = j.error;
          } catch {}
          throw new Error(msg);
        }
        a.status = "done";
      } catch (err) {
        a.status = "error";
        a.error = String(err.message || err);
        attachOK = false;
      } finally {
        completed++;
        sendBtn.textContent = `uploading ${completed}/${pending.length}…`;
        renderStagedAttachments();
      }
    }));
  }

  // Phase 2: send the plain text tell IF there's a caption AND no attachments
  // rode with it. When there ARE attachments, each one carried the caption and
  // triggered its own auto-tell — sending a duplicate text tell would be noisy.
  let tellOK = true;
  if (message && !hasAttachments) {
    const results = await Promise.allSettled(targets.map(async (p) => {
      const r = await apiFetch(
        `/api/captain/${encodeURIComponent(selected)}/tell/${encodeURIComponent(p)}`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ message }),
        }
      );
      if (r.status === 401) { window.location.href = "/login"; throw new Error("unauthorized"); }
      if (!r.ok) throw new Error(`${p}: HTTP ${r.status}`);
    }));
    const failures = results.filter((r) => r.status === "rejected");
    if (failures.length > 0) {
      tellOK = false;
      for (const f of failures) {
        appendEvent({ time: nowISO(), persona: "(fleet)", type: "tell-error", text: String(f.reason) });
      }
    }
  }

  sendBtn.disabled = false;
  sendBtn.textContent = origLabel;

  if (attachOK && tellOK) {
    tellMessage.value = "";
    // drop the done rows; error rows stay so the operator can retry / remove
    stagedAttachments = stagedAttachments.filter((a) => a.status !== "done");
    renderStagedAttachments();
    // no local echo: the server-side tell events arrive through the stream
  }
};

// iOS freezes background tabs and can leave the EventSource as a zombie that
// never fires onerror or reconnects. When the tab becomes visible again,
// tear the stream down and reopen it, and refresh the polled panes right away.
document.addEventListener("visibilitychange", () => {
  if (document.visibilityState !== "visible") return;
  if (selected) {
    feedBody.innerHTML = ""; // stream replays history on reconnect — avoid dupes
    feedEvents = [];
    openStream(selected);
  }
  refreshCaptains();
  refreshPending();
  refreshStatus();
});

// --- pending permission pane ----------------------------------------------
//
// Polls /api/pending every 1.5s. Renders rows in a sticky pane above the tell
// form, filtered to the SELECTED captain. The bottom-most row is the keyboard
// target: pressing 1 allows it, 2 denies it (ignored while typing in inputs).
// New entries play a short ping so the operator notices when away from the tab.

const pendingPane = document.getElementById("pending-pane");
let seenPendingIds = new Set();
let visiblePendings = []; // current rows shown, in render order (top → bottom)
let audioCtx = null;

async function refreshPending() {
  try {
    const r = await apiFetch("/api/pending");
    if (r.status === 401) { window.location.href = "/login"; return; }
    if (!r.ok) return;
    const items = await r.json();
    renderPending(items);
  } catch {
    // network blip; next tick will retry
  }
}

// Groups whose detail rows the operator has expanded (persists across the
// 1.5s re-render poll).
let expandedPendingGroups = new Set();

function renderPending(items) {
  items = (items || []).filter(it => it.client_key === selected);
  // Ping on any newly-seen id, globally (so cross-captain pendings still alert).
  const currentIds = new Set((items || []).map(i => i.client_key + ":" + i.id));
  for (const id of currentIds) {
    if (!seenPendingIds.has(id)) ping();
  }
  seenPendingIds = currentIds;

  if (items.length === 0) {
    pendingPane.hidden = true;
    pendingPane.innerHTML = "";
    visiblePendings = [];
    expandedPendingGroups.clear();
    return;
  }
  pendingPane.hidden = false;
  pendingPane.innerHTML = "";
  visiblePendings = [];

  // group by persona; single requests render as classic rows, 2+ get a
  // collapsed group header with allow/deny-all and an expandable detail list
  const groups = new Map();
  for (const it of items) {
    if (!groups.has(it.persona)) groups.set(it.persona, []);
    groups.get(it.persona).push(it);
  }

  for (const [persona, list] of groups) {
    if (list.length === 1) {
      renderPendingRow(list[0]);
      continue;
    }
    const expanded = expandedPendingGroups.has(persona);
    const head = document.createElement("div");
    head.className = "row group-head";
    const meta = document.createElement("div");
    meta.className = "meta";
    meta.innerHTML =
      `<div><span class="who">${escape(persona)}</span> wants ` +
      `<span class="tool">${list.length} approvals</span></div>`;
    const toggle = document.createElement("button");
    toggle.className = "toggle";
    toggle.textContent = expanded ? "hide ▴" : "show ▾";
    toggle.onclick = () => {
      if (expanded) expandedPendingGroups.delete(persona);
      else expandedPendingGroups.add(persona);
      refreshPending();
    };
    const allowAll = document.createElement("button");
    allowAll.className = "allow";
    allowAll.textContent = `allow all`;
    allowAll.onclick = () => resolveAll(list, "allow", head);
    const denyAll = document.createElement("button");
    denyAll.className = "deny";
    denyAll.textContent = `deny all`;
    denyAll.onclick = () => resolveAll(list, "deny", head);
    head.appendChild(meta);
    head.appendChild(toggle);
    head.appendChild(allowAll);
    head.appendChild(denyAll);
    pendingPane.appendChild(head);

    if (expanded) {
      for (const it of list) renderPendingRow(it, true);
    }
  }
  // keyboard 1/2 targets the bottom-most individually rendered row
  const rows = pendingPane.querySelectorAll(".row:not(.group-head)");
  if (rows.length > 0) rows[rows.length - 1].classList.add("target");
}

function renderPendingRow(it, indent) {
  const row = document.createElement("div");
  row.className = "row" + (indent ? " indent" : "");
  const meta = document.createElement("div");
  meta.className = "meta";
  meta.innerHTML =
    `<div><span class="who">${escape(it.persona)}</span> wants ` +
    `<span class="tool">${escape(it.tool)}</span></div>` +
    (it.input ? `<div class="cmd">${escape(it.input)}</div>` : "");
  // long commands clamp to a couple of lines; tap to read the whole thing
  const cmd = meta.querySelector(".cmd");
  if (cmd) cmd.onclick = () => cmd.classList.toggle("expanded");
  const allow = document.createElement("button");
  allow.className = "allow";
  allow.textContent = "allow";
  allow.onclick = () => resolvePending(it, "allow", row);
  const deny = document.createElement("button");
  deny.className = "deny";
  deny.textContent = "deny";
  deny.onclick = () => resolvePending(it, "deny", row);
  row.appendChild(meta);
  row.appendChild(allow);
  row.appendChild(deny);
  pendingPane.appendChild(row);
  visiblePendings.push(it);
}

async function resolveAll(list, behavior, head) {
  head.querySelectorAll("button").forEach(b => b.disabled = true);
  await Promise.allSettled(list.map((it) =>
    apiFetch(`/api/captain/${encodeURIComponent(it.client_key)}/resolve/${encodeURIComponent(it.id)}`,
      { method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ behavior }) })
  ));
  refreshPending();
}

async function resolvePending(it, behavior, row) {
  row.querySelectorAll("button").forEach(b => b.disabled = true);
  try {
    const r = await apiFetch(
      `/api/captain/${encodeURIComponent(it.client_key)}/resolve/${encodeURIComponent(it.id)}`,
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
  const rows = pendingPane.querySelectorAll(".row:not(.group-head)");
  const row = rows[rows.length - 1];
  if (!row) return;
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
// Polls /api/status (the fleet fans out to every connected captain's
// /status.json) and re-renders the captain list so each row shows its mates as
// colored chips: red=blocked, yellow=working, green=idle, blue=done. Status is
// derived server-side from hook events and the pending queue — no heuristics.

async function refreshStatus() {
  try {
    const r = await apiFetch("/api/status");
    if (r.status === 401) { window.location.href = "/login"; return; }
    if (!r.ok) return;
    const items = await r.json();
    const next = new Map();
    for (const it of items || []) {
      if (!next.has(it.client_key)) next.set(it.client_key, []);
      next.get(it.client_key).push(it);
    }
    mateStatus = next;
    renderCaptains([...knownCaptains.values()]);
    renderFeedTabs(); // roster changes grow/shrink the conversation tabs
    renderTellOptions(); // ...and the tell dropdown
  } catch {
    // network blip; next tick will retry
  }
}

updateTellEnabled(); // initial: form starts disabled until a captain is selected
refreshCaptains();
setInterval(refreshCaptains, 5000);
refreshPending();
setInterval(refreshPending, 1500);
refreshStatus();
setInterval(refreshStatus, 3000);

// --- live terminal pane (multi-session, tabbed) -------------------------------
//
// "⌨ term" opens a PTY-hosted mate for whatever persona the tell form targets
// (tap a mate chip to target it) on the selected captain. Multiple terminals stay
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

function renderTermTabs() {
  termTabs.innerHTML = "";
  for (const [id, t] of terms) {
    const tab = document.createElement("button");
    tab.type = "button";
    tab.className = "term-tab" + (id === activeTermId ? " active" : "");
    const label = document.createElement("span");
    // persona@ship — ship is the captain name (client_key up to the colon)
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
  // owner-wins: while another viewer types, our fit 409s — fine, we reflow
  // to the writer's geometry instead of fighting over it
  apiFetch(`${t.base}/resize?client=${t.client}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ cols: t.term.cols, rows: t.term.rows }),
  });
}

// termInput sends keystrokes, enforcing the single-writer lock: a 409 means
// another viewer holds the keyboard — surface the takeover bar instead of
// silently eating input.
function termInput(t, data) {
  apiFetch(`${t.base}/input?client=${t.client}`, { method: "POST", body: data })
    .then((r) => {
      if (r.status === 409) showTermLock(t);
      // lease expiry or a release means we hold the keyboard again
      else if (r.ok && t.lockBar) t.lockBar.hidden = true;
    })
    .catch(() => {});
}

// A refresh or closed page never runs closeTerm, so hand back every held
// keyboard on the way out (sendBeacon survives page teardown; the session
// cookie rides along). Without this, the NEXT page load's fresh client ids
// find all their own old locks held and every terminal shows the take-over
// bar until the lease expires.
window.addEventListener("pagehide", () => {
  for (const t of terms.values()) {
    try { navigator.sendBeacon(`${t.base}/release?client=${t.client}`); } catch { /* best effort */ }
  }
});

function showTermLock(t) {
  if (t.lockBar) { t.lockBar.hidden = false; return; }
  const bar = document.createElement("div");
  bar.className = "term-lock";
  bar.appendChild(document.createTextNode("view-only — another viewer holds the keyboard "));
  const btn = document.createElement("button");
  btn.type = "button";
  btn.textContent = "take over";
  btn.onclick = async () => {
    try {
      const r = await apiFetch(`${t.base}/takeover?client=${t.client}`, { method: "POST" });
      if (r.ok) bar.hidden = true;
    } catch { /* next keystroke re-surfaces the bar */ }
  };
  bar.appendChild(btn);
  t.lockBar = bar;
  t.host.appendChild(bar);
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
  // best-effort: hand the keyboard back so the next typist claims cleanly
  apiFetch(`${t.base}/release?client=${t.client}`, { method: "POST", keepalive: true }).catch(() => {});
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

// --- attach grace period ------------------------------------------------------
//
// Attaching a terminal takes over the shipmate's session, killing a mid-turn
// headless proc and losing the in-flight answer. When the mate is working (or
// blocked on an approval), wait for the turn to drain behind a spinner —
// with "attach now" (take over anyway) and "cancel" escape hatches.

const attachOverlay = $("attach-overlay");
const attachMsg = $("attach-msg");
const attachNowBtn = $("attach-now");
const attachCancelBtn = $("attach-cancel");

function mateBusy(key, persona) {
  const m = (mateStatus.get(key) || []).find((x) => x.persona === persona);
  return m && (m.status === "working" || m.status === "blocked") ? m.status : null;
}

function waitForIdle(key, persona) {
  return new Promise((resolve) => {
    let timer = null;
    const done = (result) => {
      clearInterval(timer);
      attachOverlay.hidden = true;
      attachNowBtn.onclick = null;
      attachCancelBtn.onclick = null;
      resolve(result);
    };
    attachOverlay.hidden = false;
    attachNowBtn.onclick = () => done("attach");
    attachCancelBtn.onclick = () => done("cancel");
    const tick = async () => {
      await refreshStatus();
      const busy = mateBusy(key, persona);
      if (!busy) { done("attach"); return; }
      attachMsg.textContent = busy === "blocked"
        ? `${persona} is blocked on an approval — resolve it, attach now, or cancel`
        : `${persona} is mid-turn — waiting to attach…`;
    };
    tick();
    timer = setInterval(tick, 1500);
  });
}

async function openTerminal(key, persona) {
  const id = termSessionId(key, persona);
  if (terms.has(id)) { activateTerm(id); return; }

  if (mateBusy(key, persona)) {
    if (await waitForIdle(key, persona) === "cancel") return;
    if (terms.has(id)) { activateTerm(id); return; } // raced with another attach
  }

  const base = `/api/captain/${encodeURIComponent(key)}/pty/${encodeURIComponent(persona)}`;
  try {
    const r = await apiFetch(`${base}/start`, { method: "POST" });
    if (r.status === 401) { window.location.href = "/login"; return; }
    if (!r.ok) throw new Error(await r.text() || `HTTP ${r.status}`);
  } catch (err) {
    // upstream failures can hand back whole HTML error pages — keep one line
    const msg = String(err).replace(/\s+/g, " ").slice(0, 200);
    appendEvent({ time: nowISO(), persona: "(fleet)", type: "term-error", text: msg });
    return;
  }

  const host = document.createElement("div");
  host.className = "term-instance";
  termHost.appendChild(host);

  const term = new Terminal({ fontSize: 13, scrollback: 5000 });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(host);
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

  const entry = {
    key, persona, base, term, fit, es: null, host,
    client: Math.random().toString(36).slice(2, 10), // writer-lock identity
    lockBar: null,
  };
  term.onData((data) => termInput(entry, data));
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
    const mobile = window.matchMedia("(max-width: 640px)").matches;

    // Whole-layout shrink: when the keyboard eats part of the viewport, pin
    // the body to the VISIBLE rect — same trick the term pane uses. Height
    // alone isn't enough: iOS scrolls the layout viewport to reveal the
    // caret, and scrollTo(0,0) loses that fight, sliding the header off the
    // top. Translating by vv.offsetTop follows whatever scroll Safari
    // insists on, so header/tabs/feed compress above the keys instead.
    // Skipped while the term pane is open: body transform would turn the
    // pane's position:fixed coordinates body-relative and double-offset it.
    const keyboardUp = mobile && window.innerHeight - vv.height > 60;
    if (keyboardUp && termPane.hidden) {
      document.body.style.height = vv.height + "px";
      document.body.style.transform = vv.offsetTop > 1 ? `translateY(${vv.offsetTop}px)` : "";
      window.scrollTo(0, 0);
      feedBody.scrollTop = feedBody.scrollHeight; // keep the latest lines visible
    } else {
      document.body.style.height = "";
      document.body.style.transform = "";
    }

    // Full-screen terminal pane follows the visible rect too.
    if (!termPane.hidden) {
      if (mobile) {
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
    }
  };
  vv.addEventListener("resize", onVV);
  vv.addEventListener("scroll", onVV);
}

// --- beads pane ---------------------------------------------------------------
//
// View of the selected ship's beads work graph (bd list --json via the
// tunnel). Toggles over the feed; 404 means the ship has no beads workspace.
// The graph is agent-written and changes while you're looking, so the pane
// live-refreshes on a poll while open; expanded detail rows survive the
// re-render. With a ship selected the pane can also write: quick-create a
// bead, or close one from its detail row.

const beadsPane = $("beads-pane");
const beadsOpenBtn = $("beads-open");
let beadsTimer = null;
let beadsLastJSON = ""; // skip re-render (and detail refetch) when unchanged
const expandedBeads = new Set(); // bead ids whose detail row is open

function closeBeads() {
  beadsPane.hidden = true;
  beadsOpenBtn.classList.remove("active");
  clearInterval(beadsTimer);
  beadsTimer = null;
  beadsLastJSON = "";
  expandedBeads.clear();
  if (selected) {
    feedBody.hidden = false;
    tellForm.style.display = "";
  } else {
    renderCaptainPicker(); // back to the fleet overview
  }
}

async function openBeads() {
  if (!beadsPane.hidden) { closeBeads(); return; }
  beadsPane.innerHTML = '<div class="empty">loading beads…</div>';
  beadsPane.hidden = false;
  feedBody.hidden = true;
  captainPicker.hidden = true;
  tellForm.style.display = "none"; // no tell target while reading the graph
  beadsOpenBtn.classList.add("active");
  await refreshBeads(true);
  clearInterval(beadsTimer);
  beadsTimer = setInterval(refreshBeads, 5000);
}

async function refreshBeads(force) {
  if (beadsPane.hidden) return;
  try {
    // ship selected → that ship's graph; nothing selected → fleet-wide union
    const url = selected
      ? `/api/captain/${encodeURIComponent(selected)}/beads`
      : "/api/beads";
    const r = await apiFetch(url);
    if (r.status === 401) { window.location.href = "/login"; return; }
    if (r.status === 404) {
      beadsPane.innerHTML = '<div class="empty">no beads workspace on this ship</div>';
      return;
    }
    if (!r.ok) throw new Error(`HTTP ${r.status}`);
    const text = await r.text();
    if (force) refreshBeadsBadge(); // writes just happened; resync the ⛃ count
    if (!force && text === beadsLastJSON) return;
    beadsLastJSON = text;
    renderBeads(JSON.parse(text));
  } catch (err) {
    if (force) beadsPane.innerHTML = `<div class="empty">beads unavailable: ${escape(String(err).slice(0, 120))}</div>`;
    // poll-tick failures keep the last good render; next tick retries
  }
}

// externalRefURL resolves a bead's external_ref to a clickable URL. Full URLs
// pass through; the `gh-<n>` convention resolves against the carrying ship's
// repo origin (GitHub redirects /issues/<n> to /pull/<n> when it's a PR, so
// one path shape covers both).
function externalRefURL(ref, captainKey) {
  if (!ref) return null;
  if (/^https?:\/\//.test(ref)) return ref;
  const m = /^gh-(\d+)$/.exec(ref);
  if (!m) return null;
  const captain = knownCaptains.get(captainKey);
  if (!captain || !captain.repo_url) return null;
  return `${captain.repo_url}/issues/${m[1]}`;
}

// refLink renders an external_ref as an anchor when resolvable, a plain chip
// otherwise. Anchor clicks must not toggle the bead detail row.
function refLink(ref, captainKey) {
  const url = externalRefURL(ref, captainKey);
  if (!url) return `<span class="bref">${escape(ref)}</span>`;
  return `<a class="bref" href="${escape(url)}" target="_blank" rel="noopener" onclick="event.stopPropagation()">${escape(ref)}</a>`;
}

function renderBeads(beads) {
  beadsPane.innerHTML = "";
  renderBeadCreate();
  if (!beads || beads.length === 0) {
    const empty = document.createElement("div");
    empty.className = "empty";
    empty.textContent = "no open beads — the crew has a clean slate";
    beadsPane.appendChild(empty);
    return;
  }
  for (const b of beads) {
    const row = document.createElement("div");
    row.className = "bead";
    // fleet view: show which ships carry the bead (synced graphs dedupe here)
    const ships = (b.ships || []).map((s) => s.split(":")[0]).join(", ");
    const detailKey = selected || (b.ships && b.ships[0]);
    row.innerHTML =
      `<span class="bstatus ${escape(b.status || "open")}">${escape(b.status || "?")}</span>` +
      `<span class="bid">${escape(b.id || "")}</span>` +
      `<span class="btitle">${escape(b.title || "")}</span>` +
      (b.external_ref ? refLink(b.external_ref, detailKey) : "") +
      (ships && !selected ? `<span class="bships">${escape(ships)}</span>` : "") +
      (b.priority !== undefined && b.priority !== null ? `<span class="bprio" title="priority">p${escape(String(b.priority))}</span>` : "");
    if (detailKey) row.onclick = () => toggleBeadDetail(row, b.id, detailKey);
    beadsPane.appendChild(row);
    // live refresh: reopen the detail rows the operator had expanded
    if (detailKey && expandedBeads.has(b.id)) expandBeadDetail(row, b.id, detailKey);
  }
}

// renderBeadCreate mounts the quick-create affordance at the top of the pane:
// a "+ new bead" button that unfolds into a title/description form. Only with
// a ship selected — a create has to land on ONE ship's graph.
function renderBeadCreate() {
  if (!selected) return;
  const bar = document.createElement("div");
  bar.className = "bead-create";
  const btn = document.createElement("button");
  btn.type = "button";
  btn.textContent = "+ new bead";
  btn.onclick = () => {
    btn.hidden = true;
    const form = document.createElement("form");
    form.innerHTML =
      '<input name="title" placeholder="title" maxlength="200" required>' +
      '<textarea name="description" placeholder="description (optional)" rows="2"></textarea>' +
      '<div class="row"><button type="button" class="cancel">cancel</button> <button type="submit">create</button></div>';
    form.querySelector(".cancel").onclick = () => { form.remove(); btn.hidden = false; };
    form.onsubmit = async (e) => {
      e.preventDefault();
      // form.elements, not form.title — a form's named-control access is
      // shadowed by the built-in title attribute
      const title = form.elements.title.value.trim();
      if (!title) return;
      form.querySelectorAll("button").forEach((b) => (b.disabled = true));
      try {
        const r = await apiFetch(`/api/captain/${encodeURIComponent(selected)}/bead`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ title, description: form.elements.description.value.trim() }),
        });
        if (r.status === 401) { window.location.href = "/login"; return; }
        if (!r.ok) throw new Error(await r.text() || `HTTP ${r.status}`);
        form.remove();
        btn.hidden = false;
        refreshBeads(true);
      } catch (err) {
        form.querySelectorAll("button").forEach((b) => (b.disabled = false));
        appendEvent({ time: nowISO(), persona: "(fleet)", type: "bead-error", text: String(err).slice(0, 160) });
      }
    };
    bar.appendChild(form);
    form.elements.title.focus();
  };
  bar.appendChild(btn);
  beadsPane.appendChild(bar);
}

// tap a bead to expand its full record (bd show) inline; tap again to fold
function toggleBeadDetail(row, id, captainKey) {
  const existing = row.nextElementSibling;
  if (existing && existing.classList.contains("bead-detail")) {
    existing.remove();
    expandedBeads.delete(id);
    return;
  }
  expandedBeads.add(id);
  expandBeadDetail(row, id, captainKey);
}

async function expandBeadDetail(row, id, captainKey) {
  const detail = document.createElement("div");
  detail.className = "bead-detail";
  detail.textContent = "loading…";
  row.after(detail);
  try {
    const r = await apiFetch(`/api/captain/${encodeURIComponent(captainKey)}/bead/${encodeURIComponent(id)}`);
    if (r.status === 401) { window.location.href = "/login"; return; }
    if (!r.ok) throw new Error(`HTTP ${r.status}`);
    const arr = await r.json();
    const b = Array.isArray(arr) ? arr[0] : arr;
    if (!b) throw new Error("empty record");
    const lines = [];
    if (b.description) lines.push(`<div class="bdesc">${escape(b.description)}</div>`);
    const meta = [];
    if (b.external_ref) meta.push(`ref: ${refLink(b.external_ref, captainKey)}`);
    if (b.assignee) meta.push(`assignee: ${escape(b.assignee)}`);
    if (b.issue_type) meta.push(`type: ${escape(b.issue_type)}`);
    if (b.owner) meta.push(`owner: ${escape(b.owner)}`);
    if (b.created_at) meta.push(`created: ${escape(String(b.created_at).slice(0, 16).replace("T", " "))}`);
    if (b.updated_at) meta.push(`updated: ${escape(String(b.updated_at).slice(0, 16).replace("T", " "))}`);
    if (b.dependency_count) meta.push(`depends on: ${escape(String(b.dependency_count))}`);
    if (b.dependent_count) meta.push(`blocks: ${escape(String(b.dependent_count))}`);
    if (b.comment_count) meta.push(`comments: ${escape(String(b.comment_count))}`);
    lines.push(`<div class="bmeta">${meta.join(" · ")}</div>`);
    detail.innerHTML = lines.join("");
    if (b.status !== "closed") {
      renderBeadClose(detail, id, captainKey); // red ✕, absolute top-right of the card
      const actions = document.createElement("div");
      actions.className = "bead-actions";
      // one row that wraps as a unit: [✎][assign to…][priority][dispatch]
      const grp = renderBeadAssign(actions, b, id, captainKey);
      renderBeadEdit(grp, b, id, captainKey, detail);
      detail.appendChild(actions);
    }
  } catch (err) {
    detail.textContent = "detail unavailable: " + String(err).slice(0, 120);
  }
}

// beadActionError surfaces a failed write without leaving the pane.
function beadActionError(err) {
  appendEvent({ time: nowISO(), persona: "(fleet)", type: "bead-error", text: String(err).slice(0, 200) });
}

function renderBeadClose(actions, id, captainKey) {
  const closeBtn = document.createElement("button");
  closeBtn.type = "button";
  closeBtn.className = "bead-close icon";
  closeBtn.textContent = "✕";
  closeBtn.title = "close bead";
  closeBtn.onclick = async () => {
    const reason = prompt(`close ${id} — reason (optional):`);
    if (reason === null) return; // cancelled
    closeBtn.disabled = true;
    try {
      const cr = await apiFetch(
        `/api/captain/${encodeURIComponent(captainKey)}/bead/${encodeURIComponent(id)}/close`,
        { method: "POST", headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ reason: reason.trim() }) }
      );
      if (cr.status === 401) { window.location.href = "/login"; return; }
      if (!cr.ok) throw new Error(await cr.text() || `HTTP ${cr.status}`);
      expandedBeads.delete(id);
      refreshBeads(true);
    } catch (err) {
      closeBtn.disabled = false;
      beadActionError(err);
    }
  };
  actions.appendChild(closeBtn);
}

// buildBeadPriority is a lone select that applies on change — a priority
// tweak is low-stakes, so no confirm button cluttering the bar.
function buildBeadPriority(b, id, captainKey) {
  const pSel = document.createElement("select");
  pSel.className = "prio";
  pSel.title = "priority (applies immediately)";
  for (let p = 0; p <= 4; p++) {
    const o = document.createElement("option");
    o.value = String(p);
    o.textContent = "p" + p;
    if (b.priority === p) o.selected = true;
    pSel.appendChild(o);
  }
  pSel.onchange = async () => {
    pSel.disabled = true;
    try {
      const r = await apiFetch(
        `/api/captain/${encodeURIComponent(captainKey)}/bead/${encodeURIComponent(id)}/update`,
        { method: "POST", headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ priority: pSel.value }) }
      );
      if (r.status === 401) { window.location.href = "/login"; return; }
      if (!r.ok) throw new Error(await r.text() || `HTTP ${r.status}`);
      refreshBeads(true);
    } catch (err) {
      pSel.disabled = false;
      beadActionError(err);
    }
  };
  return pSel;
}

// renderBeadEdit: ✎ toggles an inline title/description editor — beads are a
// shared human+agent surface, not agent-only. Saves via /bead/{id}/update.
function renderBeadEdit(container, b, id, captainKey, detail) {
  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = "icon";
  btn.textContent = "✎";
  btn.title = "edit title & description";
  btn.onclick = () => {
    const existing = detail.querySelector(".bead-edit");
    if (existing) { existing.remove(); return; }
    const form = document.createElement("form");
    form.className = "bead-edit";
    form.innerHTML =
      '<input name="title" maxlength="200" required>' +
      '<textarea name="description" rows="4"></textarea>' +
      '<div class="row"><button type="button" class="cancel">cancel</button> <button type="submit">save</button></div>';
    form.elements.title.value = b.title || "";
    form.elements.description.value = b.description || "";
    form.querySelector(".cancel").onclick = () => form.remove();
    form.onsubmit = async (e) => {
      e.preventDefault();
      const title = form.elements.title.value.trim();
      if (!title) return;
      form.querySelectorAll("button").forEach((x) => (x.disabled = true));
      try {
        const r = await apiFetch(
          `/api/captain/${encodeURIComponent(captainKey)}/bead/${encodeURIComponent(id)}/update`,
          { method: "POST", headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ title, description: form.elements.description.value.trim() }) }
        );
        if (r.status === 401) { window.location.href = "/login"; return; }
        if (!r.ok) throw new Error(await r.text() || `HTTP ${r.status}`);
        refreshBeads(true);
      } catch (err) {
        form.querySelectorAll("button").forEach((x) => (x.disabled = false));
        beadActionError(err);
      }
    };
    detail.appendChild(form);
    form.elements.title.focus();
  };
  container.prepend(btn); // ✎ opens the row, before the assign select
}

// renderBeadAssign mounts the cross-ship dispatch control: a fleet-wide
// persona@ship picker + dispatch button. Assigning routes through the fleet,
// which updates the graph, force-pulls the target ship, and TELLS the mate —
// assignment IS dispatch, not just bookkeeping.
function renderBeadAssign(actions, b, id, captainKey) {
  const row = document.createElement("div");
  row.className = "grp";
  const sel = document.createElement("select");
  const ph = document.createElement("option");
  ph.value = "";
  ph.textContent = "assign to…";
  sel.appendChild(ph);
  for (const [key, mates] of mateStatus) {
    const captain = knownCaptains.get(key);
    if (!captain || !captain.connected) continue;
    const ship = key.split(":")[0];
    for (const m of orderMates(captain, mates)) {
      const o = document.createElement("option");
      o.value = JSON.stringify({ ship: key, persona: m.persona });
      o.textContent = `${m.persona}@${ship}`;
      if (b.assignee === `${m.persona}@${ship}`) o.textContent += " (current)";
      sel.appendChild(o);
    }
  }
  const btn = document.createElement("button");
  btn.type = "button";
  btn.textContent = "dispatch";
  btn.onclick = async () => {
    if (!sel.value) return;
    const target = JSON.parse(sel.value);
    btn.disabled = true;
    try {
      const r = await apiFetch(
        `/api/captain/${encodeURIComponent(captainKey)}/bead/${encodeURIComponent(id)}/assign`,
        { method: "POST", headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ ship: target.ship, persona: target.persona, title: b.title || "" }) }
      );
      if (r.status === 401) { window.location.href = "/login"; return; }
      if (!r.ok) throw new Error(await r.text() || `HTTP ${r.status}`);
      const res = await r.json();
      appendEvent({ time: nowISO(), persona: "(fleet)", type: "bead:dispatch",
        text: res.queued
          ? `${id} assigned to ${res.assignee} — ship offline, dispatch queued for reconnect`
          : `${id} → ${target.persona}@${target.ship.split(":")[0]}` });
      refreshBeads(true);
    } catch (err) {
      btn.disabled = false;
      beadActionError(err);
    }
  };
  row.appendChild(sel);
  row.appendChild(buildBeadPriority(b, id, captainKey)); // priority sits before the verb
  row.appendChild(btn);
  actions.appendChild(row);
  return row;
}

beadsOpenBtn.onclick = openBeads;

// --- beads count badge ---------------------------------------------------------
//
// The ⛃ button shows the selected ship's open-bead count so graph activity is
// visible without opening the pane. The captain caches the count (30s TTL,
// invalidated on fleet-mediated writes), so this poll is cheap.

async function refreshBeadsBadge() {
  if (!selected) { beadsOpenBtn.textContent = "⛃ beads"; return; }
  try {
    const r = await apiFetch(`/api/captain/${encodeURIComponent(selected)}/beads/summary`);
    if (!r.ok) { beadsOpenBtn.textContent = "⛃ beads"; return; }
    const s = await r.json();
    beadsOpenBtn.textContent = s.open > 0 ? `⛃ beads (${s.open})` : "⛃ beads";
  } catch { /* keep the last label; next tick retries */ }
}
setInterval(refreshBeadsBadge, 30000);

termOpenBtn.onclick = async () => {
  if (!selected) return;
  const persona = tellPersona.value.trim();
  if (!persona) { tellPersona.focus(); return; }
  if (persona === "all") {
    // "all" is a UI pseudo-target, not an agent — spawning `claude --agent
    // all` would create a nameless mate. Open a tab per crew member instead.
    const roster = (mateStatus.get(selected) || []).map((m) => m.persona);
    if (roster.length === 0) {
      appendEvent({ time: nowISO(), persona: "(fleet)", type: "term-error", text: "no crew roster yet — wait for status" });
      return;
    }
    for (const p of roster) await openTerminal(selected, p);
    return;
  }
  openTerminal(selected, persona);
};
termCloseBtn.onclick = () => { if (activeTermId) closeTerm(activeTermId); };

// --- open-another-terminal menu ------------------------------------------------
//
// The term pane is a full-screen takeover, which used to make it a dead end:
// you could close tabs but not open new ones without leaving. ＋ drops a menu
// of the active terminal's ship roster; picking a mate opens (or focuses) its
// terminal without ever leaving term mode.

const termAddBtn = $("term-add");
const termAddMenu = $("term-add-menu");

termAddBtn.onclick = (ev) => {
  ev.stopPropagation();
  if (!termAddMenu.hidden) { termAddMenu.hidden = true; return; }
  const t = activeTerm();
  const key = t ? t.key : selected;
  if (!key) return;
  termAddMenu.innerHTML = "";
  const captain = knownCaptains.get(key);
  const mates = orderMates(captain || { persona: "" }, mateStatus.get(key) || []);
  if (mates.length === 0) {
    const none = document.createElement("div");
    none.className = "none";
    none.textContent = "no crew roster yet";
    termAddMenu.appendChild(none);
  }
  const ship = key.split(":")[0];
  for (const m of mates) {
    const item = document.createElement("button");
    item.type = "button";
    const isOpen = terms.has(termSessionId(key, m.persona));
    item.className = "item" + (isOpen ? " open" : "");
    item.textContent = `${m.persona}@${ship}` + (isOpen ? " ✓" : "");
    item.onclick = () => {
      termAddMenu.hidden = true;
      openTerminal(key, m.persona);
    };
    termAddMenu.appendChild(item);
  }
  termAddMenu.hidden = false;
};

// tap-away dismiss
document.addEventListener("pointerdown", (e) => {
  if (termAddMenu.hidden) return;
  if (termAddMenu.contains(e.target) || e.target === termAddBtn) return;
  termAddMenu.hidden = true;
});

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
    termInput(t, seq);
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
    termInput(t, "\x1b[F");
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

// --- mobile captains drawer ------------------------------------------------------
//
// On narrow screens (<=640px, see style.css) the captains rail is an off-canvas
// drawer toggled by the header hamburger. Selecting a captain closes it so the
// feed gets the full width; the backdrop tap dismisses without selecting.

const captainsToggle = $("captains-toggle");
const drawerBackdrop = $("drawer-backdrop");

captainsToggle.onclick = () => document.body.classList.toggle("drawer-open");
drawerBackdrop.onclick = () => document.body.classList.remove("drawer-open");

// --- attach bar: file / camera / photos + drag-drop + paste ---------------------
//
// Files stage locally (thumbnail + filename + size) and only upload on Send.
// Each staged file POSTs to /api/captain/<key>/attach as multipart/form-data
// with `file` (required) and `caption` (optional). The backend relays the
// binary to the ship's .shipmates/inbox/ and auto-tells the target mate — so
// the UI here is deliberately fire-and-collect, not full workflow.

const MAX_ATTACH_BYTES = 10 * 1024 * 1024; // 10 MB — matches backend cap
let stagedAttachments = []; // [{ id, file, previewURL|null, status: "staged"|"uploading"|"done"|"error", error }]
let stagedSeq = 0;
const stagedPane = $("attach-staged");
const tellCompose = $("tell-compose");
if (stagedPane) stagedPane.hidden = true; // no chips at boot

function stageAttachFromEvent(ev) {
  const input = ev.target;
  const files = Array.from(input.files || []);
  for (const f of files) stageAttach(f);
  // reset the input so picking the same file twice still fires onchange
  input.value = "";
}
// exposed for inline onchange handlers in index.html
window.stageAttachFromEvent = stageAttachFromEvent;

function stageAttach(file) {
  if (!file) return;
  if (file.size > MAX_ATTACH_BYTES) {
    const mb = (file.size / (1024 * 1024)).toFixed(1);
    appendEvent({
      time: nowISO(),
      persona: "(fleet)",
      type: "tell-error",
      text: `attachment ${file.name} is ${mb} MB — max 10 MB`,
    });
    return;
  }
  const id = ++stagedSeq;
  let previewURL = null;
  if (file.type && file.type.startsWith("image/")) {
    try { previewURL = URL.createObjectURL(file); } catch {}
  }
  stagedAttachments.push({ id, file, previewURL, status: "staged", error: null });
  renderStagedAttachments();
}

function removeStaged(id) {
  const idx = stagedAttachments.findIndex((a) => a.id === id);
  if (idx === -1) return;
  const a = stagedAttachments[idx];
  if (a.previewURL) { try { URL.revokeObjectURL(a.previewURL); } catch {} }
  stagedAttachments.splice(idx, 1);
  renderStagedAttachments();
}

function renderStagedAttachments() {
  if (!stagedPane) return;
  stagedPane.innerHTML = "";
  if (stagedAttachments.length === 0) {
    stagedPane.hidden = true;
    return;
  }
  stagedPane.hidden = false;
  for (const a of stagedAttachments) {
    const chip = document.createElement("div");
    chip.className = "attach-thumb " + a.status;
    chip.title = `${a.file.name} · ${humanSize(a.file.size)}`;

    const thumb = document.createElement("div");
    thumb.className = "thumb";
    if (a.previewURL) {
      const img = document.createElement("img");
      img.src = a.previewURL;
      img.alt = a.file.name;
      thumb.appendChild(img);
    } else {
      thumb.textContent = "📄";
      thumb.classList.add("generic");
    }
    chip.appendChild(thumb);

    const meta = document.createElement("div");
    meta.className = "meta";
    const nm = document.createElement("div");
    nm.className = "name";
    nm.textContent = truncateName(a.file.name);
    const sz = document.createElement("div");
    sz.className = "size";
    sz.textContent = humanSize(a.file.size);
    meta.appendChild(nm);
    meta.appendChild(sz);
    if (a.status === "error") {
      const err = document.createElement("div");
      err.className = "err";
      err.textContent = a.error || "failed";
      meta.appendChild(err);
    } else if (a.status === "uploading") {
      const up = document.createElement("div");
      up.className = "up";
      up.textContent = "uploading…";
      meta.appendChild(up);
    } else if (a.status === "done") {
      const ok = document.createElement("div");
      ok.className = "ok";
      ok.textContent = "sent";
      meta.appendChild(ok);
    }
    chip.appendChild(meta);

    const close = document.createElement("button");
    close.type = "button";
    close.className = "remove";
    close.setAttribute("aria-label", `remove ${a.file.name}`);
    close.textContent = "✕";
    close.onclick = () => removeStaged(a.id);
    chip.appendChild(close);

    stagedPane.appendChild(chip);
  }
}

// Drag-and-drop over the compose area. Keep the highlight class on the outer
// container so the border-flash covers both attach bar and tell form.
if (tellCompose) {
  let dragDepth = 0;
  const showDrag = () => tellCompose.classList.add("dragover");
  const hideDrag = () => tellCompose.classList.remove("dragover");
  tellCompose.addEventListener("dragenter", (e) => {
    if (!e.dataTransfer || !Array.from(e.dataTransfer.types || []).includes("Files")) return;
    e.preventDefault();
    dragDepth++;
    showDrag();
  });
  tellCompose.addEventListener("dragover", (e) => {
    if (!e.dataTransfer || !Array.from(e.dataTransfer.types || []).includes("Files")) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = "copy";
  });
  tellCompose.addEventListener("dragleave", (e) => {
    dragDepth = Math.max(0, dragDepth - 1);
    if (dragDepth === 0) hideDrag();
  });
  tellCompose.addEventListener("drop", (e) => {
    if (!e.dataTransfer) return;
    e.preventDefault();
    dragDepth = 0;
    hideDrag();
    const files = Array.from(e.dataTransfer.files || []);
    for (const f of files) stageAttach(f);
  });
}

// Paste from clipboard anywhere on the page — but only image blobs. Text pastes
// into the tell input stay text. If the user pastes into the persona <select>
// or a beads input, we still capture image data (that's the useful case).
document.addEventListener("paste", (e) => {
  if (!selected) return;
  const items = e.clipboardData && e.clipboardData.items;
  if (!items) return;
  let staged = 0;
  for (const item of items) {
    if (item.kind !== "file") continue;
    const f = item.getAsFile();
    if (!f) continue;
    if (!f.type.startsWith("image/")) continue;
    // native clipboard files often come in as "image.png" with no name; give
    // them a timestamped one so the operator can tell them apart in the tray.
    const ext = (f.type.split("/")[1] || "png").split("+")[0];
    const named = new File([f], `clipboard-${Date.now()}.${ext}`, { type: f.type });
    stageAttach(named);
    staged++;
  }
  if (staged > 0) e.preventDefault();
});
