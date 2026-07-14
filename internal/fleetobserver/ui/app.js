(() => {
  'use strict';
  const MAX_RENDERED_EVENTS = 200;
  let credential = '', cursor = '', timer;
  const by = id => document.getElementById(id);
  const text = (tag, value, cls) => { const node = document.createElement(tag); node.textContent = String(value ?? '').slice(0, 512); if (cls) node.className = cls; return node; };
  const oneOf = (value, allowed) => allowed.includes(value);
  const connect = ['online', 'stale', 'offline'];
  const sessions = ['absent', 'starting', 'idle', 'working', 'steering', 'interrupting', 'stopped', 'failed', 'projection_unavailable'];
  const turns = ['none', 'active', 'terminal_unknown'];
  const activities = ['idle', 'command', 'file_change', 'web_search', 'connected_tool', 'other'];
  const kinds = ['ship.online', 'ship.stale', 'ship.offline', 'ship.snapshot', 'persona.installed', 'persona.removed', 'session.state', 'turn.state', 'activity', 'agent.message', 'approval.waiting', 'request.refused', 'projection.warning'];
  async function get(path) {
    const response = await fetch(path, { method: 'GET', headers: { Authorization: 'Bearer ' + credential, Accept: 'application/json' }, cache: 'no-store', credentials: 'omit', referrerPolicy: 'no-referrer' });
    if (!response.ok) throw new Error(response.status === 401 ? 'Observer credential was not accepted.' : 'Observer request failed. Reconnecting…');
    return response.json();
  }
  function validSnapshot(snapshot) { return snapshot && snapshot.schema_version === 1 && Array.isArray(snapshot.ships) && snapshot.ships.every(ship => oneOf(ship.connectivity, connect) && Array.isArray(ship.personas) && ship.personas.every(persona => oneOf(persona.session, sessions) && oneOf(persona.turn, turns) && oneOf(persona.activity, activities))); }
  function validate(value) {
    if (!value || value.schema_version !== 1) throw new Error('Observer returned an unsupported response.');
    if (value.ships && !validSnapshot(value)) throw new Error('Observer returned an invalid fleet snapshot.');
    if (value.snapshot && !validSnapshot(value.snapshot)) throw new Error('Observer returned an invalid fleet snapshot.');
    if (value.events && (!Array.isArray(value.events) || !value.events.every(event => event.schema_version === 1 && oneOf(event.kind, kinds)))) throw new Error('Observer returned an invalid event page.');
    if (value.events && (!Number.isSafeInteger(value.next_cursor) || value.next_cursor < 0)) throw new Error('Observer returned an invalid cursor.');
    return value;
  }
  function setStatus(message, cls) { const node = by('event-status'); node.textContent = message; node.className = 'visually-hidden ' + (cls || ''); }
  function render(snapshot) {
    const root = by('ships'); root.replaceChildren();
    for (const ship of snapshot.ships || []) {
      const card = text('article', '', 'ship'); card.setAttribute('aria-labelledby', 'ship-' + ship.ship_id);
      const heading = text('h3', ship.ship_label || ship.ship_id); heading.id = 'ship-' + ship.ship_id;
      card.append(heading, text('p', 'Connection: ' + ship.connectivity, 'status ' + ship.connectivity));
      const table = document.createElement('table'); table.setAttribute('aria-label', 'Observed personas on ' + (ship.ship_label || ship.ship_id));
      const thead = document.createElement('thead'), head = document.createElement('tr'); for (const label of ['Persona', 'Session', 'Turn', 'Activity', 'Notice']) head.append(text('th', label)); thead.append(head); table.append(thead);
      const body = document.createElement('tbody');
      for (const persona of ship.personas || []) { const row = document.createElement('tr'); for (const [label, value] of [['Persona', persona.persona], ['Session', persona.session], ['Turn', persona.turn], ['Activity', persona.activity], ['Notice', persona.approval_waiting ? 'Local approval waiting' : '—']]) { const cell = text('td', value); cell.dataset.label = label; row.append(cell); } body.append(row); }
      table.append(body); card.append(table); if (ship.truncated) card.append(text('p', `${ship.omitted_persona_count} personas omitted by the bounded projection.`)); root.append(card);
    }
  }
  function eventText(event) { const data = event.data || {}; return `${event.cursor} · ${event.ship_id} · ${event.persona || 'ship'} · ${event.kind}${data.text ? ' · ' + data.text : ''}${data.label ? ' · ' + data.label : ''}`; }
  function addEvents(events) { const out = by('events'); for (const event of events || []) out.prepend(text('li', eventText(event))); while (out.children.length > MAX_RENDERED_EVENTS) out.lastElementChild.remove(); if (events && events.length) setStatus(`${events.length} observed event${events.length === 1 ? '' : 's'} received.`); }
  async function poll() {
    try {
      const query = cursor ? '?after=' + encodeURIComponent(cursor) + '&limit=100' : '?limit=100'; const result = validate(await get('/api/fleet/v1/events/stream' + query));
      if (result.gap) { by('boundary').textContent = `Reconnect boundary: ${result.gap.reason}. Fleet state was refreshed.`; by('events').replaceChildren(); setStatus('A replay gap was detected; the fleet snapshot was refreshed.'); }
      if (result.snapshot) render(result.snapshot); addEvents(result.events);
      const epoch = result.snapshot ? result.snapshot.fleet_epoch : result.gap ? result.gap.current_epoch : (result.events || [])[0]?.fleet_epoch || cursor.split(':')[0];
      if (!epoch || !Number.isSafeInteger(result.next_cursor)) throw new Error('Observer returned an invalid progress boundary.');
      cursor = `${epoch}:${result.next_cursor}`; by('error').textContent = ''; setStatus('Connected; observing read-only updates.'); timer = setTimeout(poll, 1000);
    } catch (error) { by('error').textContent = error.message || 'Observer is offline. Reconnecting…'; setStatus('Observer connection unavailable; retrying.', 'offline-message'); timer = setTimeout(poll, 2000); }
  }
  by('auth').addEventListener('submit', async event => { event.preventDefault(); clearTimeout(timer); credential = by('credential').value; by('credential').value = ''; try { const snapshot = validate(await get('/api/fleet/v1/snapshot')); by('fleet').hidden = false; by('auth-panel').hidden = true; render(snapshot); cursor = `${snapshot.fleet_epoch}:${snapshot.snapshot_cursor}`; setStatus('Connected; observing read-only updates.'); poll(); } catch (error) { credential = ''; by('error').textContent = error.message || 'Observer credential was not accepted.'; } });
})();
