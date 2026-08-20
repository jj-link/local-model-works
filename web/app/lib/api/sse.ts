import { notifyUnauthorized } from "./client";

export interface SseEvent {
  id: string | null;
  event: string;
  data: string;
}

export interface StreamState {
  reconnecting: boolean;
  attempt: number;
  /** Last HTTP status seen while opening the stream (if any). */
  status?: number;
}

export interface StreamHandle {
  close: () => void;
}

const BASE_BACKOFF_MS = 500;
const MAX_BACKOFF_MS = 8000;

/**
 * SSE client over fetch + ReadableStream. Honors `id:`/`event:`/`data:`
 * frames, auto-reconnects with exponential backoff, and resumes with
 * Last-Event-ID. Consumers close the handle (and should stop reconnecting
 * themselves once a terminal `end` event arrives).
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

  const schedule = (_delay: number) => {
    if (closed) return;
    attempt += 1;
    opts.onState?.({ reconnecting: true, attempt });
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
      if (field === "id") id = value;
      else if (field === "event") event = value;
      else if (field === "data") dataLines.push(value);
    }
    if (dataLines.length === 0) return;
    if (id !== null) lastId = id;
    opts.onEvent({ id, event, data: dataLines.join("\n") });
  };

  const connect = async () => {
    abort = new AbortController();
    try {
      const headers: Record<string, string> = { accept: "text/event-stream" };
      if (lastId) headers["last-event-id"] = lastId;
      const res = await fetch(url, {
        headers,
        signal: abort.signal,
        credentials: "same-origin",
      });
      if (res.status === 401) {
        notifyUnauthorized();
        closed = true;
        return;
      }
      if (!res.ok || !res.body) {
        opts.onState?.({ reconnecting: true, attempt, status: res.status });
        schedule(0);
        return;
      }
      attempt = 0;
      opts.onState?.({ reconnecting: false, attempt: 0 });
      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buf = "";
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += decoder.decode(value, { stream: true });
        let sep: number;
        while ((sep = buf.indexOf("\n\n")) !== -1) {
          dispatchFrame(buf.slice(0, sep));
          buf = buf.slice(sep + 2);
        }
      }
      if (!closed) schedule(1000);
    } catch (err) {
      if (closed || (err instanceof DOMException && err.name === "AbortError")) return;
      schedule(0);
    }
  };

  void connect();
  return {
    close() {
      closed = true;
      if (retryTimer) clearTimeout(retryTimer);
      abort?.abort();
    },
  };
}
