#!/usr/bin/env python3
"""Replay a small agent loop against the server and print each turn's cached tokens.

Shows whether a user message after a tool loop resumes past the loop (patch 0097, warm turns) or re-reads it.
Before 0097 the "next user message" row cached 4096 of ~9.3K tokens; with warm turns it should cache ~8192
(the pass needs a quiet engine: this script waits 3 s after the answer). Run it with no other request in flight:
a stream decoding or filling its prompt blocks the pass by design, and the row then shows 4096 (seen 2026-11-04 beside
a 138K-token agent request). Verified 2026-11-04: 8192. Usage: python3 scripts/cache-replay.py [URL]
"""
import json, urllib.request, sys, time
URL = (sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8888") + "/v1/chat/completions"
def ask(messages, tools=None, kwargs=None, tag=""):
    body = {"model": "GLM-5.3-Flash", "messages": messages, "temperature": 0, "max_tokens": 4000,
            "reasoning_effort": "max", "chat_template_kwargs": {"enable_thinking": True, **(kwargs or {})}}
    if tools: body["tools"] = tools; body["tool_choice"] = "auto"
    t = time.time()
    r = urllib.request.urlopen(urllib.request.Request(URL, json.dumps(body).encode(), {"content-type": "application/json"}), timeout=300)
    d = json.load(r); u = d["usage"]; m = d["choices"][0]["message"]
    print(f"{tag}: prompt={u['prompt_tokens']} cached={u['prompt_tokens_details']['cached_tokens']} out={u['completion_tokens']} "
          f"tool_calls={len(m.get('tool_calls') or [])} reasoning_len={len(m.get('reasoning_content') or '')} {time.time()-t:.2f}s")
    return m
filler = " ".join(f"Note {i}: the lab's rack {i%7} holds node {i} with its own cooling loop and label." for i in range(260))
sys_msg = {"role": "system", "content": "You are a terse assistant with one tool, read_file. Background notes you can ignore: " + filler}
tools = [{"type": "function", "function": {"name": "read_file", "description": "Read a file", "parameters": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}}}]
u1 = {"role": "user", "content": "Read /etc/motd with the tool. Then work out how many distinct weekday names appear in it and whether the maintenance window overlaps 01:30-02:30 UTC; answer in one sentence."}
a1 = ask([sys_msg, u1], tools, tag="R1 [sys,u1]")
if not a1.get("tool_calls"): print("no tool call; stop"); sys.exit(1)
a1_msg = {"role": "assistant", "content": a1.get("content") or "", "reasoning_content": a1.get("reasoning_content") or "", "tool_calls": a1["tool_calls"]}
result = "\n".join(f"line {i}: welcome to node {i}, maintenance window is Sunday 02:00 UTC, do not reboot without a ticket." for i in range(120))
t1 = {"role": "tool", "tool_call_id": a1["tool_calls"][0]["id"], "content": result}
a2 = ask([sys_msg, u1, a1_msg, t1], tools, tag="R2 [sys,u1,a1,tool]  (the loop's next step)")
a2_msg = {"role": "assistant", "content": a2.get("content") or "", "reasoning_content": a2.get("reasoning_content") or ""}
u2 = {"role": "user", "content": "Thanks. Now say 'ok' and nothing else."}
time.sleep(3)  # the warm pass after the answer needs a quiet engine
ask([sys_msg, u1, a1_msg, t1, a2_msg, u2], tools, tag="R3 [..., a2, u2]  (next user message, default)")
ask([sys_msg, u1, a1_msg, t1, a2_msg, u2], tools, {"clear_thinking": False}, tag="R4 same, clear_thinking=false")
a1_nr = dict(a1_msg); a1_nr.pop("reasoning_content"); a2_nr = dict(a2_msg); a2_nr.pop("reasoning_content")
ask([sys_msg, u1, a1_nr, t1, a2_nr, u2], tools, tag="R5 same, client sends no reasoning_content")
