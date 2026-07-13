package fleetinterrupt

import "net/http"

// ProductHandler serves a minimal operator-only exact-turn interrupt client.
// Credentials and target references remain in memory and never enter URLs or
// browser storage.
type ProductHandler struct{ API http.Handler }

func (p ProductHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	secure(w)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; frame-ancestors 'none'; form-action 'self'")
	if r.Method != http.MethodGet || r.URL.RawQuery != "" {
		http.NotFound(w, r)
		return
	}
	switch r.URL.Path {
	case "/interrupt/":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(interruptHTML))
	case "/interrupt/app.js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = w.Write([]byte(interruptJS))
	default:
		http.NotFound(w, r)
	}
}

const interruptHTML = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="referrer" content="no-referrer"><title>Exact-turn Fleet interrupt</title></head><body><main><h1>Exact-turn Fleet interrupt</h1><p>This cancels one already-active turn. It does not stop the session, approve a request, or start work.</p><form id="interrupt"><label>Interrupt operator credential <input id="credential" type="password" autocomplete="off" required></label><button id="load" type="button">Load current targets</button><label>Current interruptable turn <select id="targets" required disabled><option value="">Load a fresh authenticated projection</option></select></label><label><input id="selected" type="checkbox" required disabled> I confirm this exact ship, persona, and observed turn</label><button id="submit" type="submit" disabled>Interrupt this exact turn</button></form><p id="result" role="status" aria-live="polite"></p></main><script src="/interrupt/app.js"></script></body></html>`

const interruptJS = `(()=>{'use strict';const q=x=>document.getElementById(x),f=q('interrupt'),s=q('submit'),o=q('result'),x=q('selected'),list=q('targets'),load=q('load'),id=()=>{const b=new Uint8Array(32);crypto.getRandomValues(b);return btoa(String.fromCharCode(...b)).replaceAll('+','-').replaceAll('/','_').replaceAll('=','')};let pending=false,credential='',cursor='',targets=[];const clear=msg=>{targets=[];cursor='';list.replaceChildren(new Option(msg||'No current targets',''));list.disabled=true;x.checked=false;x.disabled=true;enable()},enable=()=>{s.disabled=pending||list.disabled||!list.value||!x.checked};const refresh=async(first=false)=>{if(!credential)return clear('Authentication required');let u='/api/fleet/v1/interrupt-targets';if(cursor){const [e,c]=cursor.split(':');u+='?observation_epoch='+encodeURIComponent(e)+'&observation_cursor='+encodeURIComponent(c)}try{const r=await fetch(u,{headers:{Authorization:'Bearer '+credential},cache:'no-store',credentials:'omit',referrerPolicy:'no-referrer'});if(r.status===401){credential='';q('credential').value='';return clear('Authentication lost')}if(!r.ok)return clear('Projection unavailable');const v=await r.json();if(v.schema_version!==1||!Array.isArray(v.targets))return clear('Projection unavailable');cursor=String(v.observation_epoch)+':'+String(v.observation_cursor);targets=v.targets.filter(t=>Date.parse(t.expires_at)>Date.now());const prior=list.value;list.replaceChildren(new Option(targets.length?'Select a current exact turn':'No current targets',''));for(const [i,t] of targets.entries())list.add(new Option(t.ship_id+' / '+t.persona+' / generation '+t.connection_generation,String(i)));list.disabled=!targets.length;if(targets[Number(prior)])list.value=prior;x.disabled=list.disabled;x.checked=false;enable();if(first)o.textContent=targets.length?'Fresh targets loaded':'No interruptable targets'}catch(_){clear('Disconnected; targets removed')}};load.addEventListener('click',()=>{credential=q('credential').value;q('credential').value='';cursor='';refresh(true)});list.addEventListener('change',()=>{x.checked=false;enable()});x.addEventListener('change',enable);f.addEventListener('keydown',e=>{if(e.key==='Enter')e.preventDefault()});f.addEventListener('submit',async e=>{e.preventDefault();const t=targets[Number(list.value)];if(pending||!credential||!t||!x.checked||Date.parse(t.expires_at)<=Date.now())return clear('Target expired');pending=true;enable();o.textContent='submitted; waiting for the exact-turn decision';const body={schema_version:1,fleet_id:t.fleet_id,fleet_epoch:t.fleet_epoch,ship_id:t.ship_id,connection_generation:t.connection_generation,persona:t.persona,interrupt_target_ref:t.interrupt_target_ref,operation_id:id()};try{const r=await fetch('/api/fleet/v1/turn-interrupts',{method:'POST',headers:{Authorization:'Bearer '+credential,'Content-Type':'application/json'},body:JSON.stringify(body),cache:'no-store',credentials:'omit',referrerPolicy:'no-referrer'}),v=await r.json();if(r.status===401){credential='';clear('Authentication lost')}else if(v.outcome==='accepted')o.textContent='interrupting';else if(v.outcome==='interrupted')o.textContent='interrupted';else if(v.outcome==='already_terminal')o.textContent='already terminal';else if(v.outcome==='refused')o.textContent='refused: '+String(v.reason_code).slice(0,64);else o.textContent='indeterminate; do not submit a new operation automatically'}catch(_){o.textContent='indeterminate; do not submit a new operation automatically'}finally{pending=false;clear('Refresh required after submission')}});setInterval(()=>{if(credential)refresh(false);else clear('Authentication required')},1000);clear('Authentication required')})();`
