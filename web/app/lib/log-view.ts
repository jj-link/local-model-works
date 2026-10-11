const MAX_LINES = 2000;
const MAX_PENDING_CHARS = 64 * 1024;

export type LogStream = "stdout" | "stderr";

export interface LogBuffer {
  raw: string[];
  operator: string[];
  pending: Record<LogStream, string>;
  hidden: number;
}

export const EMPTY_LOG_BUFFER: LogBuffer = { raw: [], operator: [], pending: { stdout: "", stderr: "" }, hidden: 0 };

export function isDiagnosticLine(line: string): boolean {
  // Never let a familiar diagnostic prefix hide a failure or warning.
  if (/\b(warn(?:ing)?|error|fatal|critical|exception|traceback|fail(?:ed|ure)?)\b/i.test(line)) return false;

  return /\bINFO\b.*\[loggers\.py:\d+\]\s+Engine\s+\d+:\s+Iteration\(\d+\):/.test(line)
    || /\[glm53-decode-floor\]\s+(?:completed_step|step)=/.test(line)
    || /\[glm53-dflash-swa-replay-v\d+\]\s+(?:replay clamp|reusing reconciled)\b/.test(line)
    || /\bINFO\b.*\[metrics\.py:\d+\]\s+SpecDecoding metrics:/.test(line)
    || /"(?:GET|HEAD)\s+\/(?:health|metrics)(?:\?[^\s"]*)?\s+HTTP\/\d(?:\.\d)?"\s+(?:200|204|304)\b/.test(line);
}

export function appendLogChunk(buffer: LogBuffer, chunk: string, stream: LogStream = "stdout"): LogBuffer {
  // Byte fragments from different files must never form a single line.
  const complete = (buffer.pending[stream] + chunk).split("\n");
  const unfinished = complete.pop() ?? "";
  const start = Math.max(0, unfinished.length - MAX_PENDING_CHARS);
  const first = unfinished.charCodeAt(start);
  // Keep recent progress even without newlines, cutting only between code points.
  const tail = unfinished.slice(start + (start > 0 && first >= 0xdc00 && first <= 0xdfff ? 1 : 0));
  const pending = { ...buffer.pending, [stream]: tail };
  if (!complete.length) return { ...buffer, pending };

  const operator = complete.filter((line) => !isDiagnosticLine(line));
  return {
    raw: [...buffer.raw, ...complete].slice(-MAX_LINES),
    operator: operator.length ? [...buffer.operator, ...operator].slice(-MAX_LINES) : buffer.operator,
    pending,
    hidden: buffer.hidden + complete.length - operator.length,
  };
}

export function finishLogBuffer(buffer: LogBuffer): LogBuffer {
  for (const stream of ["stdout", "stderr"] as const) {
    if (buffer.pending[stream]) buffer = appendLogChunk(buffer, "\n", stream);
  }
  return buffer;
}

/** Deployment events carry explicit streams; run/build events remain strings. */
export function parseLogChunks(data: string): { stream: LogStream; data: string }[] {
  let parsed: unknown;
  try {
    parsed = JSON.parse(data);
  } catch {
    return [{ stream: "stdout", data }];
  }
  if (typeof parsed === "string") return [{ stream: "stdout", data: parsed }];
  if (parsed && typeof parsed === "object") {
    if ("stdout" in parsed && typeof parsed.stdout === "string" && "stderr" in parsed && typeof parsed.stderr === "string") {
      return [{ stream: "stdout", data: parsed.stdout }, { stream: "stderr", data: parsed.stderr }];
    }
    if ("stream" in parsed && (parsed.stream === "stdout" || parsed.stream === "stderr") && "data" in parsed && typeof parsed.data === "string") {
      return [{ stream: parsed.stream, data: parsed.data }];
    }
  }
  return [{ stream: "stdout", data }];
}
