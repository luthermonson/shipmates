#!/usr/bin/env bash
# End-to-end proof that the full shipmates stack runs on the claude runtime.
#
# Builds the real shipmates binary, scaffolds a scratch project, and drives
# init / add / update / live / feed / tell / show / interrupt / approvals /
# ask against a fake `claude` on PATH (fake-claude.py) that speaks the real
# stream-json stdio protocol. Nothing inside shipmates is stubbed: the only
# substitution is the CLI at the far end of the pipe.
#
# Usage (unix only — the coordination server's state directory and the
# policy loader both require openat-class primitives):
#
#   test/e2e/claude/run.sh
#
# Exits non-zero if any step fails.
set -uo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
E2E=${E2E_DIR:-$HOME/shipmates-e2e}
SRC=${SRC:-$(cd "$HERE/../../.." && pwd)}
PROJ=$E2E/project
BIN=$E2E/bin

pass=0; fail=0
step() { printf '\n=== %s\n' "$1"; }
ok()   { pass=$((pass+1)); printf 'PASS  %s\n' "$1"; }
no()   { fail=$((fail+1)); printf 'FAIL  %s\n' "$1"; printf '      %s\n' "$2"; }
check() { if [ -n "$2" ]; then ok "$1"; else no "$1" "$3"; fi; }

SERVER_PID=""
start_server() {
  shipmates server serve >> "$E2E/server.log" 2>&1 &
  SERVER_PID=$!
  for _ in $(seq 1 60); do
    shipmates feed quartermaster --after 0 >/dev/null 2>&1 && return 0
    grep -q 'listening\|ready' "$E2E/server.log" 2>/dev/null && break
    sleep 0.2
  done
  sleep 0.5
}
stop_server() {
  shipmates server stop >/dev/null 2>&1
  [ -n "$SERVER_PID" ] && { kill "$SERVER_PID" 2>/dev/null; wait "$SERVER_PID" 2>/dev/null; }
  SERVER_PID=""
  sleep 0.5
}
# restart clears the manager's in-memory sessions so the next `live` is not
# refused as busy; a finished live session stays attached to its persona.
restart_server() { stop_server; start_server; }

# live_turn <prompt>; sets SID/TID/TURN/BACKEND from the returned snapshot.
live_turn() {
  local snap
  snap=$(shipmates live quartermaster "$1" 2>"$E2E/live.err")
  echo "$snap" > "$E2E/live.json"
  read -r SID TID TURN BACKEND <<<"$(python3 -c '
import json,sys
try: s=json.load(open(sys.argv[1]))
except Exception: s={}
print(s.get("session_id",""), s.get("thread_id",""), s.get("turn_id",""), s.get("backend",""))' "$E2E/live.json")"
}

rm -rf "$E2E"; mkdir -p "$BIN" "$PROJ"

step "build shipmates + install the fake claude on PATH"
( cd "$SRC" && go build -o "$BIN/shipmates" . ) || { echo "build failed"; exit 1; }
sed 's/\r$//' "$HERE/fake-claude.py" > "$BIN/claude"
chmod +x "$BIN/claude"
export PATH="$BIN:$PATH"
check "shipmates + fake claude on PATH" "$(command -v shipmates && command -v claude)" "not on PATH"

cd "$PROJ"
git init -q . >/dev/null 2>&1

step "init + add a persona"
shipmates init > "$E2E/init.log" 2>&1
mkdir -p .shipmates
cat > .shipmates/config.yaml <<'YAML'
runtime: claude
runtimes:
  claude:
    binary: claude
containment:
  mode: none
YAML
shipmates add quartermaster > "$E2E/add.log" 2>&1
check "persona installed" "$(ls .codex/agents/quartermaster.toml 2>/dev/null)" "$(tail -3 "$E2E/add.log")"

step "GAP 3: the claude subagent file is installed for the claude runtime"
check "add wrote .claude/agents/quartermaster.md" \
  "$(ls .claude/agents/quartermaster.md 2>/dev/null)" "$(tail -3 "$E2E/add.log")"
check "it carries the persona's role, not just a name" \
  "$(grep -F 'You are the **quartermaster**' .claude/agents/quartermaster.md)" \
  "$(head -20 .claude/agents/quartermaster.md 2>/dev/null)"
check "shipmates-only frontmatter is elided" \
  "$([ -z "$(sed -n '2,/^---$/p' .claude/agents/quartermaster.md | grep -E '^(byline|memoryDir|permissions|domainGlob):')" ] && echo yes)" \
  "$(sed -n '1,/^---$/p' .claude/agents/quartermaster.md)"
check "the artifact is manifest-tracked" \
  "$(grep -F '.claude/agents/quartermaster.md' .shipmates/manifest.json)" \
  "$(cat .shipmates/manifest.json)"

step "GAP 3: update preserves a hand-edited subagent file, and is idempotent"
printf -- '---\nname: quartermaster\n---\n\nmy own instructions\n' > .claude/agents/quartermaster.md
shipmates update --accept ours > "$E2E/update-ours.log" 2>&1
check "a hand edit survives update --accept ours" \
  "$(grep -F 'my own instructions' .claude/agents/quartermaster.md)" \
  "$(cat .claude/agents/quartermaster.md)"
# Same rule the canonical Codex artifact follows: when the operator edited the
# file and the catalog has NOT moved, there is nothing to reconcile, so the
# edit is kept whatever --accept says. Only a moved catalog makes it a conflict.
shipmates update --accept theirs > "$E2E/update-theirs.log" 2>&1
check "an edit against an unmoved catalog is kept even with --accept theirs" \
  "$(grep -F 'my own instructions' .claude/agents/quartermaster.md)" \
  "$(cat .claude/agents/quartermaster.md)"
# Deleting a tracked artifact is how an operator asks for it back.
rm .claude/agents/quartermaster.md
shipmates update --accept theirs > "$E2E/update-readd.log" 2>&1
check "a deleted-but-tracked agent file is re-added from the catalog" \
  "$(grep -F 'You are the **quartermaster**' .claude/agents/quartermaster.md 2>/dev/null)" \
  "$(tail -3 "$E2E/update-readd.log")"
cp .claude/agents/quartermaster.md "$E2E/agent-before-idempotent.md"
shipmates update --accept theirs > "$E2E/update-again.log" 2>&1
check "a second update leaves the agent file byte-identical" \
  "$(cmp -s "$E2E/agent-before-idempotent.md" .claude/agents/quartermaster.md && echo yes)" \
  "$(diff "$E2E/agent-before-idempotent.md" .claude/agents/quartermaster.md)"

count_hooks() {
  python3 - <<'PY'
import json
try: s=json.load(open('.claude/settings.json'))
except Exception: print(-1); raise SystemExit
print(sum(1 for g in s.get('hooks',{}).get('SessionStart',[])
            for h in g.get('hooks',[]) if 'load-memory' in h.get('command','')))
PY
}

step "GAP 2: the memory hook is installed for the claude runtime"
# `init` ran before the runtime was selected in config, so `update` is the
# re-assert path an operator would use after switching runtimes.
shipmates update --accept theirs > "$E2E/update.log" 2>&1
H1=$(count_hooks)
check "memory hook present exactly once after update" "$([ "$H1" = "1" ] && echo yes)" "count=$H1"
shipmates update --accept theirs >> "$E2E/update.log" 2>&1
H2=$(count_hooks)
check "a second update does not duplicate it" "$([ "$H2" = "1" ] && echo yes)" "count=$H2"
check "the hook is in the shape claude executes (matcher group)" \
  "$(python3 -c "
import json;s=json.load(open('.claude/settings.json'))
g=s['hooks']['SessionStart'][0]
print('yes' if 'hooks' in g and 'command' not in g else '')")" "$(cat .claude/settings.json)"

step "the hook command itself resolves persona memory"
mkdir -p .shipmates/memory/quartermaster
echo '# remembered: the mast is stepped' > .shipmates/memory/quartermaster/notes.md
MEM=$(SHIPMATES_PERSONA=quartermaster shipmates hook load-memory 2>/dev/null)
check "hook load-memory prints persona memory" "$(echo "$MEM" | grep -F 'the mast is stepped')" "$MEM"

step "start the server"
start_server
live_turn "sleepms:25000"
check "server up and live turn started on the claude backend" \
  "$([ "$BACKEND" = "claude" ] && [ -n "$TURN" ] && echo yes)" \
  "snapshot=$(cat "$E2E/live.json") err=$(cat "$E2E/live.err") server=$(tail -3 "$E2E/server.log")"

step "feed --follow streams the turn"
timeout 6 shipmates feed quartermaster --follow > "$E2E/feed1.log" 2>&1
check "feed shows turn.started" "$(grep -F 'turn.started' "$E2E/feed1.log")" "$(tail -5 "$E2E/feed1.log")"
check "feed carries the runtime's assistant text" "$(grep -F 'fake hello' "$E2E/feed1.log")" "$(tail -5 "$E2E/feed1.log")"

step "tell: steer the exact live turn"
shipmates tell quartermaster "$SID" "$TID" "$TURN" "port ten degrees" > "$E2E/tell.log" 2>&1
sleep 1
timeout 5 shipmates feed quartermaster --follow > "$E2E/feed2.log" 2>&1
check "steer reached the runtime mid-turn" \
  "$(grep -F 'steer-received:port ten degrees' "$E2E/feed2.log")" \
  "$(tail -3 "$E2E/feed2.log"); tell=$(cat "$E2E/tell.log")"
check "a stale turn id is refused, not redirected" \
  "$(shipmates tell quartermaster "$SID" "$TID" deadbeefdeadbeef "wrong turn" 2>&1 | grep -i 'stale')" \
  "stale tell was not refused"

step "show: attach a text file into the running turn"
printf 'the bilge pump log\nline two\n' > bilge.log
shipmates show quartermaster bilge.log --caption "look at this" > "$E2E/show-text.log" 2>&1
sleep 1
timeout 5 shipmates feed quartermaster --follow > "$E2E/feed3.log" 2>&1
check "text attachment folded into the live turn" \
  "$(grep -F 'the bilge pump log' "$E2E/feed3.log")" \
  "$(tail -3 "$E2E/feed3.log"); show=$(cat "$E2E/show-text.log")"

step "show: attach an image into the running turn"
python3 -c "
import base64
open('dot.png','wb').write(base64.b64decode('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=='))"
shipmates show quartermaster dot.png --caption "one pixel" > "$E2E/show-image.log" 2>&1
sleep 1
timeout 5 shipmates feed quartermaster --follow > "$E2E/feed4.log" 2>&1
check "image reached the runtime as a base64 image content block" \
  "$(grep -F 'image-received:image/png' "$E2E/feed4.log")" \
  "$(tail -3 "$E2E/feed4.log"); show=$(cat "$E2E/show-image.log")"

step "interrupt: cancel the exact live turn"
shipmates interrupt quartermaster "$SID" "$TID" "$TURN" > "$E2E/interrupt.log" 2>&1
sleep 2
timeout 5 shipmates feed quartermaster --follow > "$E2E/feed5.log" 2>&1
check "interrupt ended the turn before its 25s deadline" \
  "$(grep -E 'turn.failed|turn.completed' "$E2E/feed5.log")" \
  "$(tail -3 "$E2E/feed5.log"); interrupt=$(cat "$E2E/interrupt.log")"

write_policy() { cat > .shipmates/policies/quartermaster.yaml; }

step "GAP 1: live approval round-trip, allowed by project policy"
write_policy <<'YAML'
version: 1
allow:
  - id: gitstatus
    kind: process.exec
    match:
      command_exact: git status
    reason: read-only inspection
ask: []
deny: []
YAML
restart_server
live_turn "approve:git status"
sleep 2
timeout 6 shipmates feed quartermaster --follow > "$E2E/feed6.log" 2>&1
check "policy-allowed approval published request.allowed" \
  "$(grep -F 'request.allowed' "$E2E/feed6.log")" \
  "$(tail -4 "$E2E/feed6.log"); snap=$(cat "$E2E/live.json") err=$(cat "$E2E/live.err")"
check "the runtime received the allow answer" \
  "$(grep -F 'approval:allow:' "$E2E/feed6.log")" "$(tail -4 "$E2E/feed6.log")"
check "the approved turn completed" \
  "$(grep -F 'turn.completed' "$E2E/feed6.log")" "$(tail -4 "$E2E/feed6.log")"

step "GAP 1: live approval round-trip, denied by project policy"
write_policy <<'YAML'
version: 1
allow: []
ask: []
deny:
  - id: nocurl
    kind: process.exec
    match:
      command_exact: curl evil.example
    reason: exfiltration risk
YAML
restart_server
live_turn "approve:curl evil.example"
sleep 2
timeout 6 shipmates feed quartermaster --follow > "$E2E/feed7.log" 2>&1
check "policy-denied approval published request.denied" \
  "$(grep -F 'request.denied' "$E2E/feed7.log")" \
  "$(tail -4 "$E2E/feed7.log"); snap=$(cat "$E2E/live.json") err=$(cat "$E2E/live.err")"
check "the runtime received the deny answer with a rationale" \
  "$(grep -F 'approval:deny:Denied by the shipmates operator' "$E2E/feed7.log")" "$(tail -4 "$E2E/feed7.log")"
check "the denied turn still completed (never wedged)" \
  "$(grep -F 'turn.completed' "$E2E/feed7.log")" "$(tail -4 "$E2E/feed7.log")"

step "GAP 1: live approval with no rule and no operator is refused, not dropped"
write_policy <<'YAML'
version: 1
allow: []
ask: []
deny: []
YAML
restart_server
live_turn "approve:whoami"
sleep 2
timeout 6 shipmates feed quartermaster --follow > "$E2E/feed8.log" 2>&1
check "unmediated approval published request.refused/mediation_unavailable" \
  "$(grep -F 'mediation_unavailable' "$E2E/feed8.log")" "$(tail -4 "$E2E/feed8.log")"
check "the refused turn still completed" \
  "$(grep -F 'turn.completed' "$E2E/feed8.log")" "$(tail -4 "$E2E/feed8.log")"

step "ask --runtime claude answers approvals from policy"
write_policy <<'YAML'
version: 1
allow:
  - id: gitstatus
    kind: process.exec
    match:
      command_exact: git status
    reason: read-only inspection
ask: []
deny: []
YAML
shipmates --runtime claude ask quartermaster "approve:git status" > "$E2E/ask.log" 2>&1
check "ask allowed the tool from policy and finished" \
  "$(grep -F 'approval:allow:' "$E2E/ask.log")" "$(tail -6 "$E2E/ask.log")"
shipmates --runtime claude ask quartermaster "approve:rm -rf /" > "$E2E/ask2.log" 2>&1
check "ask denied an unruled tool instead of hanging" \
  "$(grep -F 'approval:deny:' "$E2E/ask2.log")" "$(tail -6 "$E2E/ask2.log")"

step "GAP 3: the runtime spawns with --agent and finds the installed definition"
# The fake resolves --agent the way the real binary does — by file name under
# .claude/agents/ — and echoes the persona's own heading back, so a missing or
# empty agent file is distinguishable from a loaded one. That the real claude
# 2.1.153 honors this exact file is proven separately: with --agent it answers
# as the quartermaster and names .shipmates/memory/quartermaster/, without it
# it answers as a generic Claude agent.
shipmates --runtime claude ask quartermaster "agent?" > "$E2E/ask-agent.log" 2>&1
check "the runtime loaded the installed subagent definition" \
  "$(grep -F 'agent:quartermaster:loaded:You are the **quartermaster**' "$E2E/ask-agent.log")" \
  "$(tail -6 "$E2E/ask-agent.log")"

step "teardown"
stop_server

printf '\n===== E2E RESULT: %d passed, %d failed =====\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
