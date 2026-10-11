import { describe, expect, it } from "vitest";
import { appendLogChunk, EMPTY_LOG_BUFFER, finishLogBuffer, parseLogChunks } from "../app/lib/log-view";

const iteration = "(APIServer pid=1) INFO 09-29 06:27:56 [loggers.py:182] Engine 000: Iteration(1170): 0 context requests, 1 generation requests";
const summary = "(APIServer pid=1) INFO 09-29 06:27:49 [loggers.py:310] Engine 000: Avg prompt throughput: 2048.0 tokens/s, Running: 3 reqs, Waiting: 0 reqs";

describe("operator log history", () => {
  it("hides known diagnostics and successful polls, but keeps summaries, failures and unfamiliar output", () => {
    const hidden = [
      iteration,
      "(EngineCore pid=2553) [glm53-decode-floor] completed_step=1175 contention=1 prefill_tokens={'request': 1920}",
      "[glm53-decode-floor] step=1151 mode=prefill_turn defer=credit",
      "INFO [kv_cache_coordinator.py:1248] [glm53-dflash-swa-replay-v2] replay clamp hit=136192->132608",
      "INFO [kv_cache_coordinator.py:1248] [glm53-dflash-swa-replay-v2] reusing reconciled DFlash boundary hit=125440",
      "INFO [metrics.py:120] SpecDecoding metrics: Mean acceptance length: 2.28",
      'INFO: 100.74.194.53:45996 - "GET /health HTTP/1.1" 200 OK',
      'INFO: 100.74.194.53:45996 - "HEAD /metrics?format=text HTTP/2" 204 No Content',
    ];
    const visible = [
      summary,
      "[glm53-decode-floor] fair v5 probe_chunk=256 share=0.3",
      "INFO [glm53-apc-per-group] retention_by_group=[None,None,0]",
      "WARNING [glm53-decode-floor] step=1151 request stalled",
      "[glm53-dflash-swa-replay-v2] replay clamp failed: missing boundary",
      `${iteration} ERROR: scheduler failure`,
      'WARNING: "GET /health HTTP/1.1" 200 OK but worker unavailable',
      'INFO: "GET /health HTTP/1.1" 503 Service Unavailable',
      'INFO: "POST /v1/chat/completions HTTP/1.1" 500 Internal Server Error',
      "Traceback (most recent call last):",
      '  File "scheduler.py", line 25, in schedule',
      "RuntimeError: CUDA out of memory",
      "unrecognized runtime message",
    ];
    const buffer = appendLogChunk(EMPTY_LOG_BUFFER, [...hidden, ...visible].join("\n") + "\n");
    expect(buffer.operator).toEqual(visible);
    expect(buffer.raw).toEqual([...hidden, ...visible]);
    expect(buffer.hidden).toBe(hidden.length);
  });

  it("reassembles byte-range fragments without losing a split warning or unfinished final line", () => {
    const first = appendLogChunk(EMPTY_LOG_BUFFER, iteration.slice(0, 37));
    const second = appendLogChunk(first, iteration.slice(37) + "\n[glm53-decode-floor] step=12 WARN");
    const third = appendLogChunk(second, "ING: stalled\r\nworker fail");
    const final = appendLogChunk(third, "ure");
    expect(final.raw).toEqual([iteration, "[glm53-decode-floor] step=12 WARNING: stalled\r"]);
    expect(final.operator).toEqual(["[glm53-decode-floor] step=12 WARNING: stalled\r"]);
    expect(final.pending).toEqual({ stdout: "worker failure", stderr: "" });
    expect(appendLogChunk(final, "\n").operator).toEqual(["[glm53-decode-floor] step=12 WARNING: stalled\r", "worker failure"]);
    expect(final.hidden).toBe(1);
  });

  it("keeps useful history through a diagnostic flood and bounds each view independently", () => {
    const warning = "WARNING: worker restarted";
    const flood = appendLogChunk(EMPTY_LOG_BUFFER, warning + "\n" + (iteration + "\n").repeat(2001));
    expect(flood.operator).toEqual([warning]);
    expect(flood.raw).toEqual(Array(2000).fill(iteration));
    const updates = Array.from({ length: 2001 }, (_, i) => `checkpoint ${i} loaded`);
    const next = appendLogChunk(flood, updates.join("\n") + "\n");
    expect(next.operator).toEqual(updates.slice(1));
    expect(next.raw).toEqual(updates.slice(1));
  });

  it("keeps interleaved partial lines and final unterminated output separate by stream", () => {
    let buffer = appendLogChunk(EMPTY_LOG_BUFFER, "stdout par", "stdout");
    buffer = appendLogChunk(buffer, "stderr par", "stderr");
    expect(buffer.pending).toEqual({ stdout: "stdout par", stderr: "stderr par" });
    buffer = appendLogChunk(buffer, "tial\nstdout tail", "stdout");
    buffer = appendLogChunk(buffer, "tial\nstderr tail", "stderr");
    expect(buffer.raw).toEqual(["stdout partial", "stderr partial"]);
    expect(buffer.pending).toEqual({ stdout: "stdout tail", stderr: "stderr tail" });
    const finished = finishLogBuffer(buffer);
    expect(finished.raw).toEqual(["stdout partial", "stderr partial", "stdout tail", "stderr tail"]);
    expect(finished.operator).toEqual(finished.raw);
    expect(finished.pending).toEqual({ stdout: "", stderr: "" });
  });

  it("bounds combined history while retaining operator lines through stderr diagnostic floods", () => {
    const initial = appendLogChunk(EMPTY_LOG_BUFFER, "stdout ready\n");
    const flood = appendLogChunk(initial, (iteration + "\n").repeat(2001), "stderr");
    expect(flood.operator).toEqual(["stdout ready"]);
    expect(flood.raw).toHaveLength(2000);
    expect(flood.hidden).toBe(2001);
  });

  it("bounds newline-free progress without splitting Unicode or mixing streams", () => {
    let buffer = appendLogChunk(EMPTY_LOG_BUFFER, "ready\n");
    buffer = appendLogChunk(buffer, "stderr pending", "stderr");
    for (let i = 0; i < 20; i++) {
      buffer = appendLogChunk(buffer, "𝒳".repeat(4096) + "\r", "stdout");
      expect(buffer.pending.stdout.length).toBeLessThanOrEqual(64 * 1024);
    }
    buffer = appendLogChunk(buffer, "𝒳".repeat(40000) + "tail!", "stdout");
    expect(buffer.pending.stdout.length).toBeLessThanOrEqual(64 * 1024);
    expect(buffer.pending.stdout.startsWith("𝒳")).toBe(true);
    expect(buffer.pending.stdout.endsWith("tail!")).toBe(true);
    expect(buffer.pending.stderr).toBe("stderr pending");
    const finished = finishLogBuffer(buffer);
    expect(finished.operator).toEqual(["ready", buffer.pending.stdout, "stderr pending"]);
    expect(finished.raw).toEqual(finished.operator);
  });

  it("decodes atomic deployment snapshots and typed chunks without changing single-stream consumers", () => {
    expect(parseLogChunks(JSON.stringify({ stdout: "out\n", stderr: "err\n" }))).toEqual([
      { stream: "stdout", data: "out\n" }, { stream: "stderr", data: "err\n" },
    ]);
    expect(parseLogChunks(JSON.stringify({ stream: "stderr", data: "part" }))).toEqual([{ stream: "stderr", data: "part" }]);
    expect(parseLogChunks(JSON.stringify("build output\n"))).toEqual([{ stream: "stdout", data: "build output\n" }]);
    expect(parseLogChunks("raw run output")).toEqual([{ stream: "stdout", data: "raw run output" }]);
    expect(parseLogChunks("123")).toEqual([{ stream: "stdout", data: "123" }]);
    expect(parseLogChunks('{"message":"raw application JSON"}')).toEqual([{ stream: "stdout", data: '{"message":"raw application JSON"}' }]);
  });
});
