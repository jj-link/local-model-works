import { useCallback, useEffect, useRef, useState } from "react";
import { AlertCircle, Pause, Play } from "lucide-react";
import { cn } from "~/lib/utils";
import { streamEvents, type StreamHandle } from "~/lib/api/sse";

const MAX_LINES = 2000;

/**
 * SSE log pane. Follows the tail by default (toggle), caps rendered
 * lines at ~2000, supports wrap toggle, and shows a reconnecting
 * indicator when the stream is re-established with Last-Event-ID.
 */
export function LogPane({
  url,
  active,
  className,
  height = "h-80",
}: {
  url: string;
  /** Whether the source is still producing output. */
  active: boolean;
  className?: string;
  height?: string;
}) {
  const [lines, setLines] = useState<string[]>([]);
  const [follow, setFollow] = useState(true);
  const [wrap, setWrap] = useState(false);
  const [reconnecting, setReconnecting] = useState(false);
  const [ended, setEnded] = useState(false);
  const [httpError, setHttpError] = useState<number | null>(null);
  const scrollRef = useRef<HTMLDivElement>(null);
  const handleRef = useRef<StreamHandle | null>(null);
  const followRef = useRef(follow);
  followRef.current = follow;

  const scrollBottom = useCallback(() => {
    const el = scrollRef.current;
    if (el && followRef.current) el.scrollTop = el.scrollHeight;
  }, []);

  useEffect(() => {
    setLines([]);
    setEnded(false);
    setHttpError(null);
    const handle = streamEvents(url, {
      onEvent: (ev) => {
        if (ev.event === "end") {
          setEnded(true);
          setReconnecting(false);
          handleRef.current?.close();
          return;
        }
        let chunk = ev.data;
        try {
          const parsed: unknown = JSON.parse(ev.data);
          if (typeof parsed === "string") chunk = parsed;
        } catch {
          // raw text frame
        }
        setLines((prev) => {
          const next = [...prev, ...chunk.split("\n")];
          return next.length > MAX_LINES ? next.slice(next.length - MAX_LINES) : next;
        });
      },
      onState: (s) => {
        setReconnecting(s.reconnecting);
        if (s.status === 404) {
          setHttpError(404);
          handleRef.current?.close();
        }
      },
    });
    handleRef.current = handle;
    const t = setTimeout(scrollBottom, 0);
    return () => {
      clearTimeout(t);
      handle.close();
      handleRef.current = null;
    };
  }, [url, active, scrollBottom]);

  // Keep the tail pinned while following.
  useEffect(() => {
    scrollBottom();
  }, [lines, scrollBottom]);

  if (httpError === 404) {
    return (
      <div className={cn("lmw-panel flex flex-col items-center justify-center gap-1 px-4", height, className)}>
        <AlertCircle className="h-4 w-4 text-muted" aria-hidden />
        <p className="text-xs text-muted">no logs for this workload yet</p>
      </div>
    );
  }

  return (
    <div className={cn("lmw-panel flex flex-col overflow-hidden", className)}>
      <div className="flex items-center gap-2 border-b border-hairline px-3 py-1.5">
        <span className="lmw-label">logs</span>
        {reconnecting ? (
          <span className="inline-flex items-center gap-1 font-mono text-[11px] text-primary">
            <span className="h-1.5 w-1.5 rounded-full bg-primary blink" aria-hidden />
            reconnecting
          </span>
        ) : ended ? (
          <span className="font-mono text-[11px] text-muted">stream ended</span>
        ) : null}
        <div className="ml-auto flex items-center gap-1">
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
      <div
        ref={scrollRef}
        className={cn("overflow-auto bg-background/60 p-3 font-mono text-xs leading-5", height)}
        role="log"
        aria-live={follow ? "polite" : "off"}
      >
        {lines.length === 0 ? (
          <span className="text-muted">— no output —</span>
        ) : (
          lines.map((line, i) => (
            <div key={i} className={wrap ? "whitespace-pre-wrap break-all" : "whitespace-pre"}>
              {line || "\u00A0"}
            </div>
          ))
        )}
      </div>
    </div>
  );
}
