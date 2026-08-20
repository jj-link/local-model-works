package backend

import "encoding/base64"

// graderImage is the digest-pinned grader image: Docker Hub
// library/python 3.12-slim, linux/amd64 manifest-list digest fetched
// 2026-08-19 from registry-1.docker.io. The tag may move; the digest
// cannot.
const graderImage = "python:3.12-slim@sha256:876416ecde9aca2bcc90e1fb0c7a9500bbf749f5788b70f82d4c5a5c2357f8b4"

// graderScript is the per-language throughput grader (python3, stdlib
// only). It reads LMW_BASE_URL, LMW_MODEL, LMW_LANG, LMW_PROMPTS,
// LMW_MAX_TOKENS, and LMW_TEMPERATURE from the environment, fires the
// classic Snake-game prompt (rewritten in LMW_LANG with a per-prompt
// random nonce) at /v1/chat/completions streaming, prints progress lines,
// and ends with exactly one "RESULT:<json>" line. It exits 0 unless the
// endpoint is unreachable (exit 2).
const graderScript = `"""Single-stream decode throughput grader for one language (stdlib only)."""
import json
import os
import sys
import time
import urllib.error
import urllib.request
import uuid


def post_stream(base, model, prompt, max_tokens, temperature):
    url = base.rstrip("/") + "/v1/chat/completions"
    body = {
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
        "max_tokens": max_tokens,
        "temperature": temperature,
        "stream": True,
        "stream_options": {"include_usage": True},
    }
    req = urllib.request.Request(
        url, data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json"})
    t0 = time.perf_counter()
    ttft, last, usage = None, t0, None
    chunks, finished = 0, False
    with urllib.request.urlopen(req, timeout=600) as resp:
        for raw in resp:
            line = raw.decode("utf-8", "replace").strip()
            if not line.startswith("data:"):
                continue
            data = line[5:].strip()
            if data == "[DONE]":
                break
            try:
                obj = json.loads(data)
            except ValueError:
                continue
            if obj.get("usage"):
                usage = obj["usage"]
            for choice in obj.get("choices") or []:
                if choice.get("finish_reason") == "stop":
                    finished = True
                delta = choice.get("delta") or {}
                if delta.get("content") or delta.get("reasoning_content") or delta.get("reasoning"):
                    now = time.perf_counter()
                    if ttft is None:
                        ttft = now - t0
                    chunks += 1
                    last = now
    total = time.perf_counter() - t0
    return {"ttft": ttft, "total": total,
            "gen": (last - t0) - (ttft or 0.0),
            "chunks": chunks, "usage": usage,
            "ok": finished or usage is not None}


def pct(xs, q):
    if not xs:
        return 0.0
    xs = sorted(xs)
    if len(xs) == 1:
        return float(xs[0])
    pos = q * (len(xs) - 1)
    lo = int(pos)
    hi = min(lo + 1, len(xs) - 1)
    return xs[lo] + (xs[hi] - xs[lo]) * (pos - lo)


def stats(xs):
    if not xs:
        return {"avg": 0, "p50": 0, "p90": 0, "p99": 0}
    return {"avg": sum(xs) / len(xs), "p50": pct(xs, 0.5),
            "p90": pct(xs, 0.9), "p99": pct(xs, 0.99)}


def main():
    base = os.environ["LMW_BASE_URL"]
    model = os.environ.get("LMW_MODEL", "local")
    lang = os.environ.get("LMW_LANG", "python")
    n = int(os.environ.get("LMW_PROMPTS", "4"))
    max_tokens = int(os.environ.get("LMW_MAX_TOKENS", "512"))
    temperature = float(os.environ.get("LMW_TEMPERATURE", "0"))

    latencies, ttfts, gens = [], [], []
    prompt_tokens = completion_tokens = successes = ok_count = conn_errors = 0
    wall0 = time.perf_counter()
    for i in range(n):
        nonce = uuid.uuid4().hex
        prompt = ("Write a complete, well-structured implementation of the "
                  "classic Snake game in " + lang + " using pygame. Include a "
                  "scoreboard, increasing difficulty, a pause feature, and "
                  "clear comments. Then explain the design choices you made "
                  "in detail. nonce=" + nonce)
        try:
            r = post_stream(base, model, prompt, max_tokens, temperature)
        except Exception as e:
            print(f"[{lang} {i + 1}/{n}] FAILED: {e}", flush=True)
            if isinstance(e, OSError):
                conn_errors += 1
            continue
        u = r["usage"] or {}
        out_tok = u.get("completion_tokens") or r["chunks"]
        prompt_tokens += u.get("prompt_tokens") or 0
        completion_tokens += out_tok
        successes += 1
        if r["ok"]:
            ok_count += 1
        latencies.append(r["total"] * 1000)
        gens.append(r["gen"])
        if r["ttft"] is not None:
            ttfts.append(r["ttft"] * 1000)
        ttft_s = "none" if r["ttft"] is None else f"{r['ttft']:.3f}s"
        print(f"[{lang} {i + 1}/{n}] prompt_tok={u.get('prompt_tokens')} "
              f"out_tok={out_tok} ttft={ttft_s} total={r['total']:.2f}s "
              f"ok={int(r['ok'])}", flush=True)
    wall = time.perf_counter() - wall0
    total_gen = sum(gens)
    result = {
        "lang": lang,
        "model": model,
        "requests": n,
        "successes": successes,
        "ok_count": ok_count,
        "prompt_tokens": prompt_tokens,
        "completion_tokens": completion_tokens,
        "total_tokens": prompt_tokens + completion_tokens,
        "wall_seconds": wall,
        "tokens_per_second": (completion_tokens / total_gen) if total_gen > 0 else 0.0,
        "latency_ms": stats(latencies),
        "first_token_ms": stats(ttfts),
    }
    print("RESULT:" + json.dumps(result), flush=True)
    if successes == 0 and conn_errors > 0:
        sys.exit(2)


if __name__ == "__main__":
    main()
`

// graderScriptB64 is the base64 of graderScript, passed as the grader
// container's final argv element so the script body never appears in
// command logs.
var graderScriptB64 = base64.StdEncoding.EncodeToString([]byte(graderScript))
