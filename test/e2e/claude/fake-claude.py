#!/usr/bin/env python3
"""Fake `claude` speaking the real stream-json stdio protocol.

Mirrors what claude 2.1.153 does on
`-p --input-format stream-json --output-format stream-json --verbose
 --permission-prompt-tool stdio`:

  * one `system`/`init` preamble per process
  * one `result` frame per user turn
  * a user message written mid-turn is folded into the running turn
  * `control_request` subtype interrupt -> `control_response` + an
    error_during_execution result
  * `can_use_tool` control_request blocks the turn until a control_response
    arrives

Turn text conventions used by the E2E script:
  approve:<command>   ask permission for Bash(<command>) before finishing
  sleepms:<n>         hold the turn open for n milliseconds
"""
import json
import os
import sys
import threading
import time

out_lock = threading.Lock()


def emit(obj):
    with out_lock:
        sys.stdout.write(json.dumps(obj) + "\n")
        sys.stdout.flush()


def text(s):
    emit({"type": "assistant", "message": {"role": "assistant",
                                           "content": [{"type": "text", "text": s}]}})


session_id = "fake"
argv = sys.argv[1:]
for i, a in enumerate(argv):
    if a in ("--session-id", "--resume") and i + 1 < len(argv):
        session_id = argv[i + 1]

emit({"type": "system", "subtype": "init", "session_id": session_id,
      "claude_code_version": "fake-2.1.153", "argv": argv})

state = {"active": False, "interrupt": None}
state_lock = threading.Lock()
approvals = {}
approval_lock = threading.Lock()
threads = []


def blocks_of(msg):
    content = msg.get("message", {}).get("content", [])
    if isinstance(content, str):
        return [{"type": "text", "text": content}]
    return content


def run_turn(prompt, intr, req_id):
    text("fake hello")
    if prompt.startswith("approve:"):
        command = prompt[len("approve:"):]
        answer = {}
        event = threading.Event()
        with approval_lock:
            approvals[req_id] = (answer, event)
        emit({"type": "control_request", "request_id": req_id, "request": {
            "subtype": "can_use_tool", "tool_name": "Bash", "display_name": "Bash",
            "description": "fake tool call", "tool_use_id": "toolu_fake",
            "input": {"command": command, "description": "fake tool call"},
            "permission_suggestions": [
                {"type": "setMode", "mode": "acceptEdits", "destination": "session"}],
        }})
        if event.wait(timeout=60):
            body = answer.get("body") or {}
            text("approval:%s:%s" % (body.get("behavior", ""), body.get("message", "")))
        else:
            text("approval:timeout:")
    hold = 0.0
    if prompt.startswith("sleepms:"):
        try:
            hold = int(prompt[len("sleepms:"):]) / 1000.0
        except ValueError:
            hold = 0.0
    result = {"type": "result", "subtype": "success", "is_error": False,
              "session_id": session_id}
    if intr.wait(timeout=hold):
        result = {"type": "result", "subtype": "error_during_execution",
                  "is_error": True, "session_id": session_id}
    with state_lock:
        state["active"] = False
    emit(result)


seq = 0
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        msg = json.loads(line)
    except ValueError:
        continue
    kind = msg.get("type")

    if kind == "user":
        prompt = ""
        images = []
        for b in blocks_of(msg):
            if b.get("type") == "text":
                prompt = b.get("text", "")
            elif b.get("type") == "image":
                src = b.get("source", {})
                images.append("%s/%d" % (src.get("media_type", "?"),
                                         len(src.get("data", ""))))
        with state_lock:
            busy = state["active"]
        if busy:
            # Mid-turn message: folded into the running turn, echoed so the
            # test can prove it reached the child.
            for img in images:
                text("image-received:" + img)
            text("steer-received:" + prompt)
            continue
        seq += 1
        intr = threading.Event()
        with state_lock:
            state["active"] = True
            state["interrupt"] = intr
        for img in images:
            text("image-received:" + img)
        t = threading.Thread(target=run_turn,
                             args=(prompt, intr, "fake-approval-%d" % seq),
                             daemon=True)
        threads.append(t)
        t.start()

    elif kind == "control_request":
        if msg.get("request", {}).get("subtype") != "interrupt":
            continue
        emit({"type": "control_response",
              "response": {"subtype": "success", "request_id": msg.get("request_id")}})
        with state_lock:
            if state["active"] and state["interrupt"] is not None:
                state["interrupt"].set()

    elif kind == "control_response":
        resp = msg.get("response", {})
        with approval_lock:
            entry = approvals.pop(resp.get("request_id"), None)
        if entry:
            answer, event = entry
            answer["body"] = resp.get("response", {})
            event.set()

for t in threads:
    t.join(timeout=5)
