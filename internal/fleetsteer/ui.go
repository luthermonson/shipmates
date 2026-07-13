package fleetsteer

import "net/http"

// ProductHandler serves a deliberately small operator-only form. Secrets and
// drafts live only in memory and are never placed in URLs or browser storage.
type ProductHandler struct{ API http.Handler }

func (p ProductHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	secure(w)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; frame-ancestors 'none'; form-action 'self'")
	if r.Method != http.MethodGet || r.URL.RawQuery != "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	switch r.URL.Path {
	case "/steer/":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(steerHTML))
	case "/steer/app.js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = w.Write([]byte(steerJS))
	default:
		http.NotFound(w, r)
	}
}

const steerHTML = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="referrer" content="no-referrer"><title>Exact-turn Fleet steer</title></head><body><main><h1>Exact-turn Fleet steer</h1><p>This sends one message to one already-active turn. It cannot start, interrupt, or approve.</p><form id="steer"><label>Operator credential <input id="credential" type="password" autocomplete="off" required></label><label>Fleet ID <input id="fleet" required></label><label>Fleet epoch <input id="epoch" inputmode="numeric" required></label><label>Ship ID <input id="ship" required></label><label>Connection generation <input id="generation" inputmode="numeric" required></label><label>Persona <input id="persona" required></label><label>Exact target reference <input id="target" required></label><label><input id="approval" type="checkbox"> Local approval is pending (steering disabled)</label><label><input id="selected" type="checkbox" required> I affirm this fresh exact ship/persona/turn selection</label><label>Message <textarea id="message" maxlength="4096" required></textarea></label><output id="bytes">0 / 4096 bytes</output><button id="submit" type="submit">Steer this exact turn</button></form><p id="result" role="status" aria-live="polite"></p></main><script src="/steer/app.js"></script></body></html>`

const steerJS = `(()=>{'use strict';const q=x=>document.getElementById(x),f=q('steer'),m=q('message'),b=q('bytes'),s=q('submit'),o=q('result'),a=q('approval'),x=q('selected');let pending=false;const bytes=()=>new TextEncoder().encode(m.value).length,enable=()=>{b.textContent=bytes()+' / 4096 bytes';s.disabled=pending||a.checked||!x.checked||bytes()<1||bytes()>4096};m.addEventListener('input',enable);a.addEventListener('change',enable);x.addEventListener('change',enable);enable();f.addEventListener('keydown',e=>{if(e.key==='Enter'&&e.target.tagName!=='TEXTAREA')e.preventDefault()});f.addEventListener('submit',async e=>{e.preventDefault();if(pending||a.checked||!x.checked||bytes()<1||bytes()>4096)return;pending=true;enable();o.textContent='submitted; waiting for the exact-turn decision';const credential=q('credential').value;q('credential').value='';const body={schema_version:1,fleet_id:q('fleet').value,fleet_epoch:Number(q('epoch').value),ship_id:q('ship').value,connection_generation:Number(q('generation').value),persona:q('persona').value,steer_target_ref:q('target').value,message:m.value};try{const r=await fetch('/api/fleet/v1/turn-steers',{method:'POST',headers:{Authorization:'Bearer '+credential,'Content-Type':'application/json'},body:JSON.stringify(body),cache:'no-store',credentials:'omit',referrerPolicy:'no-referrer'}),v=await r.json();if(v.outcome==='accepted')o.textContent='steer accepted for this turn; effect not yet known';else if(v.outcome==='refused')o.textContent='steer refused: '+String(v.reason_code).slice(0,64);else o.textContent='delivery outcome unknown; this message will not be retried automatically'}catch(_){o.textContent='delivery outcome unknown; this message will not be retried automatically'}finally{pending=false;x.checked=false;enable()}})})();`
