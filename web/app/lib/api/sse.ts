import { notifyUnauthorized } from "./client";

export interface SseEvent {
  id: string | null;
  event: string;
  data: string;
}

export interface StreamState {
  connected: boolean;
  reconnecting: boolean;
  attempt: number;
  /** Last HTTP status seen while opening the stream (if any). */
  status?: number;
  error?: string;
}

export interface StreamHandle {
  close: () => void;
}

const BASE_BACKOFF_MS = 500;
const MAX_BACKOFF_MS = 8000;

/**
 * SSE over fetch. Resume only complete, consumed events; an interrupted frame
 * is discarded and requested again. Consumers close on their terminal event.
 */
export function streamEvents(
  url: string,
  opts: {
    lastEventId?: string | null;
    onEvent: (ev: SseEvent) => void;
    onState?: (s: StreamState) => void;
  },
): StreamHandle {
  let closed = false;
  let lastId: string | null = opts.lastEventId ?? null;
  let attempt = 0;
  let retryTimer: ReturnType<typeof setTimeout> | undefined;
  let abort: AbortController | undefined;

  const schedule = (status?: number) => {
    if (closed) return;
    attempt += 1;
    opts.onState?.({ connected: false, reconnecting: true, attempt, status });
    if (closed) return;
    retryTimer = setTimeout(
      connect,
      Math.min(MAX_BACKOFF_MS, BASE_BACKOFF_MS * 2 ** (attempt - 1)),
    );
  };

  const dispatchFrame = (frame: string) => {
    let id: string | null = null;
    let event = "message";
    const dataLines: string[] = [];
    for (const line of frame.split("\n")) {
      if (line.startsWith(":")) continue;
      const i = line.indexOf(":");
      const field = i === -1 ? line : line.slice(0, i);
      const value = i === -1 ? "" : line.slice(i + 1).replace(/^ /, "");
      if (field === "id" && !value.includes("\0")) id = value;
      else if (field === "event") event = value;
      else if (field === "data") dataLines.push(value);
    }
    if (dataLines.length > 0) opts.onEvent({ id, event, data: dataLines.join("\n") });
    // A throwing consumer did not acknowledge the frame.
    if (id !== null) lastId = id;
  };

  const connect = async () => {
    if (closed) return;
    const controller = new AbortController();
    abort = controller;
    let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
    let status: number | undefined;
    try {
      const headers: Record<string, string> = { accept: "text/event-stream" };
      if (lastId) headers["last-event-id"] = lastId;
      const res = await fetch(url, {
        headers,
        signal: controller.signal,
        credentials: "same-origin",
      });
      status = res.status;
      if (closed) {
        await res.body?.cancel();
        return;
      }
      if (!res.ok || res.status === 204) {
        await res.body?.cancel();
        if (res.status === 204 || (res.status >= 400 && res.status < 500 && res.status !== 408 && res.status !== 429)) {
          closed = true;
          opts.onState?.({ connected: false, reconnecting: false, attempt, status, error: `Stream unavailable (HTTP ${status})` });
          if (status === 401) notifyUnauthorized();
        } else {
          schedule(status);
        }
        return;
      }
      if (!res.body || res.headers.get("content-type")?.split(";", 1)[0].trim() !== "text/event-stream") {
        await res.body?.cancel();
        closed = true;
        opts.onState?.({ connected: false, reconnecting: false, attempt, status, error: "Invalid stream response" });
        return;
      }
      attempt = 0;
      opts.onState?.({ connected: true, reconnecting: false, attempt: 0 });
      if (closed) {
        await res.body.cancel();
        return;
      }
      reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buf = "";
      let previousCR = false;
      while (!closed) {
        const { done, value } = await reader.read();
        if (done || closed) break;
        let text = decoder.decode(value, { stream: true });
        if (!text) continue;
        if (previousCR && text.startsWith("\n")) text = text.slice(1);
        previousCR = text.endsWith("\r");
        buf += text.replace(/\r\n?/g, "\n");
        let sep: number;
        while (!closed && (sep = buf.indexOf("\n\n")) !== -1) {
          dispatchFrame(buf.slice(0, sep));
          buf = buf.slice(sep + 2);
        }
      }
      if (!closed) schedule();
    } catch {
      if (!closed) schedule(status);
    } finally {
      controller.abort();
      if (reader) {
        await reader.cancel().catch(() => {});
        reader.releaseLock();
      }
    }
  };

  void connect();
  return {
    close() {
      closed = true;
      clearTimeout(retryTimer);
      abort?.abort();
    },
  };
}
