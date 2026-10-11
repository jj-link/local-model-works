import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import { AlertCircle, Pause, Play } from "lucide-react";
import { cn } from "~/lib/utils";
import { streamEvents, type StreamHandle } from "~/lib/api/sse";
import { appendLogChunk, EMPTY_LOG_BUFFER, finishLogBuffer, isDiagnosticLine, parseLogChunks } from "~/lib/log-view";

/**
 * SSE log pane with bounded raw and operator histories. Filtering is
 * presentation-only; switching views never reopens or discards the stream.
 * Reconnects resume with Last-Event-ID.
 */
export function LogPane({
  url,
  active,
  className,
  height = "h-80",
  summarize = false,
}: {
  url: string;
  /** Whether the source is still producing output. */
  active: boolean;
  className?: string;
  height?: string;
  /** Offer a concise operator view by default for deployment output. */
  summarize?: boolean;
}) {
  const [buffer, setBuffer] = useState(EMPTY_LOG_BUFFER);
  const [rawDiagnostics, setRawDiagnostics] = useState(false);
  const [follow, setFollow] = useState(true);
  const [wrap, setWrap] = useState(false);
  const [reconnecting, setReconnecting] = useState(false);
  const [connected, setConnected] = useState(false);
  const [lastReceived, setLastReceived] = useState<number | null>(null);
  const [ended, setEnded] = useState(false);
  const [httpError, setHttpError] = useState<number | null>(null);
  const [streamError, setStreamError] = useState<string | null>(null);
  const operatorView = summarize && !rawDiagnostics;
  const lines = operatorView ? buffer.operator : buffer.raw;
  const pending = Object.values(buffer.pending).filter((line) => line && (!operatorView || !isDiagnosticLine(line)));
  const scrollRef = useRef<HTMLDivElement>(null);
  const handleRef = useRef<StreamHandle | null>(null);
  const followRef = useRef(follow);
  followRef.current = follow;

  const scrollBottom = useCallback(() => {
    const el = scrollRef.current;
    if (el && followRef.current) el.scrollTop = el.scrollHeight;
  }, []);

  useEffect(() => {
    let stopped = false;
    setBuffer(EMPTY_LOG_BUFFER);
    setConnected(false);
    setReconnecting(false);
    setLastReceived(null);
    setEnded(false);
    setHttpError(null);
    setStreamError(null);
    const handle = streamEvents(url, {
      onEvent: (ev) => {
        if (stopped) return;
        if (ev.event === "end") {
          stopped = true;
          setBuffer(finishLogBuffer);
          setEnded(true);
          setReconnecting(false);
          setConnected(false);
          handleRef.current?.close();
          return;
        }
        const chunks = parseLogChunks(ev.data);
        if (ev.event !== "snapshot" && chunks.some((chunk) => chunk.data)) setLastReceived(Date.now());
        setBuffer((prev) => chunks.reduce(
          (next, chunk) => appendLogChunk(next, chunk.data, chunk.stream),
          ev.event === "snapshot" ? EMPTY_LOG_BUFFER : prev,
        ));
      },
      onState: (s) => {
        if (stopped) return;
        setReconnecting(s.reconnecting);
        setConnected(s.connected);
        setHttpError(s.status ?? null);
        setStreamError(s.error ?? null);
        if (s.error) {
          stopped = true;
          handleRef.current?.close();
        }
      },
    });
    handleRef.current = handle;
    if (stopped) handle.close();
    return () => {
      stopped = true;
      handle.close();
      handleRef.current = null;
    };
  }, [url]);

  // Keep the tail pinned while following.
  useLayoutEffect(() => {
    scrollBottom();
  }, [lines, pending, follow, scrollBottom]);

  if (httpError === 404) {
    return (
      <div className={cn("lmw-panel flex flex-col items-center justify-center gap-1 px-4", height, className)}>
        <AlertCircle className="h-4 w-4 text-muted" aria-hidden />
        <p className="text-xs text-muted">no logs for this workload yet</p>
      </div>
    );
  }

  return (
    <div className={cn("lmw-panel flex w-full min-w-0 max-w-full flex-col overflow-hidden", className)}>
      <div className="flex flex-wrap items-center gap-2 border-b border-hairline px-3 py-1.5">
        <span className="lmw-label">logs</span>
        {summarize ? <span className="font-mono text-[11px] text-muted">{operatorView ? "operator view" : "raw diagnostics"}</span> : null}
        {streamError ? (
          <span role="status" className="font-mono text-[11px] text-destructive">
            {httpError === 401 ? "session expired — sign in to reconnect" : streamError}
          </span>
        ) : reconnecting ? (
          <span className="inline-flex items-center gap-1 font-mono text-[11px] text-primary">
            <span className="h-1.5 w-1.5 rounded-full bg-primary blink" aria-hidden />
            reconnecting
          </span>
        ) : ended ? (
          <span className="font-mono text-[11px] text-muted">stream ended</span>
        ) : (
          <span className="font-mono text-[11px] text-muted" title="Last received includes raw log output hidden by the operator filter.">
            {!active ? "source inactive" : connected ? "connected" : "connecting"}
            {lastReceived !== null ? <> · last received <time dateTime={new Date(lastReceived).toISOString()}>{new Date(lastReceived).toLocaleTimeString()}</time></> : active ? " · waiting for new log output" : " · retained log output"}
          </span>
        )}
        <div className="ml-auto flex items-center gap-1">
          {summarize ? (
            <button
              type="button"
              onClick={() => setRawDiagnostics(!rawDiagnostics)}
              aria-pressed={rawDiagnostics}
              className={cn(
                "rounded border border-hairline px-2 py-0.5 font-mono text-[11px] control",
                rawDiagnostics ? "text-foreground" : "text-muted hover:text-foreground",
              )}
            >
              Raw diagnostics
            </button>
          ) : null}
          <button
            type="button"
            onClick={() => setFollow(!follow)}
            aria-pressed={follow}
            aria-label={follow ? "Pause follow" : "Resume follow"}
            className="inline-flex items-center gap-1 rounded border border-hairline px-2 py-0.5 font-mono text-[11px] text-muted control hover:text-foreground"
          >
            {follow ? <Pause className="h-3 w-3" aria-hidden /> : <Play className="h-3 w-3" aria-hidden />}
            {follow ? "follow" : "paused"}
          </button>
          <button
            type="button"
            onClick={() => setWrap(!wrap)}
            aria-pressed={wrap}
            aria-label={wrap ? "Disable word wrap" : "Enable word wrap"}
            className={cn(
              "rounded border border-hairline px-2 py-0.5 font-mono text-[11px] control",
              wrap ? "text-foreground" : "text-muted hover:text-foreground",
            )}
          >
            wrap
          </button>
        </div>
      </div>
      {summarize ? (
        <p className="border-b border-hairline px-3 py-2 text-xs text-muted">
          {operatorView
            ? `${buffer.hidden.toLocaleString()} diagnostic lines hidden. Warnings, errors, summaries and other output remain visible.`
            : "Unfiltered output, including scheduler diagnostics and health/metrics polls."}
          {" "}Starts with recent output; up to 2,000 lines retained per view. Stored logs are unchanged.
        </p>
      ) : null}
      <div
        ref={scrollRef}
        className={cn("max-w-full overflow-auto bg-background/60 p-3 font-mono text-xs leading-5", height)}
        role="log"
        aria-live={follow ? "polite" : "off"}
      >
        {lines.length === 0 && pending.length === 0 ? (
          <span className="text-muted">{operatorView && buffer.hidden > 0 ? "No operator output yet — switch to Raw diagnostics to see hidden lines." : "— no output —"}</span>
        ) : (
          lines.map((line, i) => (
            <div key={i} className={wrap ? "whitespace-pre-wrap break-all" : "whitespace-pre"}>
              {line || "\u00A0"}
            </div>
          ))
        )}
        {pending.map((line, i) => <div key={`pending-${i}`} className={wrap ? "whitespace-pre-wrap break-all" : "whitespace-pre"}>{line}</div>)}
      </div>
    </div>
  );
}
