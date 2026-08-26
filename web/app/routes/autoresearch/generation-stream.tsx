import { AlertTriangle, RotateCw } from "lucide-react";
import type { Run } from "~/lib/api";
import type {
  ActiveInvocation,
  AutoResearchEvent,
  AutoResearchUsageSummary,
} from "./events";

function eventText(event: AutoResearchEvent): string {
  if (event.type === "agent.text.delta") return String(event.payload.delta ?? event.payload.text ?? "");
  if (event.type === "advisor.note") return `[${String(event.payload.severity ?? "note")}] ${String(event.payload.note ?? event.payload.text ?? "")}`;
  if (event.type === "agent.tool.started") return `tool → ${String(event.payload.name ?? "unknown")}`;
  if (event.type === "agent.tool.finished") return `tool ✓ ${String(event.payload.name ?? "unknown")}`;
  if (event.type === "artifact.changed") return `artifact Δ ${String(event.payload.path ?? "workspace")}`;
  if (event.type === "decision.required") return `decision required · ${String(event.payload.message ?? event.payload.gate ?? "human review")}`;
  if (event.type === "error") return `error · ${String(event.payload.message ?? event.payload.code ?? "unknown")}`;
  return "";
}

interface TimelineEntry {
  id: string;
  role: string;
  timestamp: string;
  state: "queued" | "running" | "completed" | "failed";
}

function invocationTimeline(events: AutoResearchEvent[]): TimelineEntry[] {
  const entries = new Map<string, TimelineEntry>();
  for (const event of events) {
    const id = event.invocation_id;
    if (!id || event.type.startsWith("advisor.")) continue;
    const previous = entries.get(id);
    if (event.type === "agent.started") {
      entries.set(id, {
        id,
        role: String(event.payload.role ?? previous?.role ?? event.node_id ?? "auxiliary"),
        timestamp: event.timestamp,
        state: event.payload.state === "queued" ? "queued" : "running",
      });
    } else if (event.type === "agent.finished") {
      entries.set(id, {
        id,
        role: String(event.payload.role ?? previous?.role ?? event.node_id ?? "auxiliary"),
        timestamp: event.timestamp,
        state: event.payload.state === "failed" || event.payload.error ? "failed" : "completed",
      });
    } else if (event.type === "error" && previous) {
      entries.set(id, { ...previous, timestamp: event.timestamp, state: "failed" });
    }
  }
  return [...entries.values()]
    .sort((left, right) => Date.parse(right.timestamp) - Date.parse(left.timestamp))
    .slice(0, 3);
}

function metric(value: number | null, suffix = ""): string {
  return value === null ? "—" : `${value.toLocaleString(undefined, { maximumFractionDigits: 2 })}${suffix}`;
}

export function GenerationStream({
  run,
  events,
  active,
  usage,
  reconnecting,
  streamError,
  onStop,
  controlPending,
}: {
  run?: Run;
  events: AutoResearchEvent[];
  active: ActiveInvocation[];
  usage: AutoResearchUsageSummary;
  reconnecting: boolean;
  streamError: string | null;
  onStop: () => void;
  controlPending: boolean;
}) {
  const transcript = events.filter((event) => eventText(event) !== "").slice(-120);
  const current = active.filter((item) => !item.advisor).at(-1);
  const timeline = invocationTimeline(events);
  const canStop = Boolean(run && ["queued", "planning", "running", "paused", "waiting", "verifying"].includes(run.state));
  const status = reconnecting ? "reconnecting" : run?.state ?? "idle";

  return (
    <aside className="arf-panel arf-stream-panel" aria-label="Generation stream">
      <header className="arf-panel-head">
        <div className="arf-panel-title">
          <h2>Generation stream</h2>
          <span className="arf-panel-kicker">Tokens</span>
        </div>
        <div className="arf-stream-head-status">
          {reconnecting ? <RotateCw className="h-3 w-3 animate-spin motion-reduce:animate-none" aria-hidden /> : <i />}
          <span>{status}</span>
        </div>
      </header>
      <div className="arf-stream-summary">
        <div className="arf-stream-metric"><label>Total tokens</label><strong>{usage.totalTokens.toLocaleString()}</strong></div>
        <div className="arf-stream-metric"><label>Output rate</label><strong>{metric(usage.outputRate, " t/s")}</strong></div>
        <div className="arf-stream-metric"><label>Context</label><strong>{metric(usage.contextPercent, "%")}</strong></div>
      </div>
      <div className="arf-stream-body">
        <div className="arf-active-agent">
          <div className="arf-agent-glyph"><span>⌁</span></div>
          {current ? (
            <div>
              <h3>{current.role}</h3>
              <p>{current.model || "—"} · {current.backend || "—"}</p>
            </div>
          ) : (
            <div>
              <h3>No invocation active</h3>
              <p>Waiting for primary execution</p>
            </div>
          )}
          <div className="arf-token-rate">{metric(usage.outputRate, " t/s")}</div>
        </div>
        <div className="arf-transcript" role="log" aria-live="polite">
          {transcript.length === 0 ? <div className="arf-transcript-line arf-meta">Normalized model and tool events will appear here</div> : transcript.map((event, index) => {
            const tone = event.type === "error" || event.type === "decision.required"
              ? "arf-accent"
              : event.type === "agent.tool.started" || event.type === "agent.tool.finished"
                ? "arf-tool"
                : event.type === "agent.text.delta"
                  ? "arf-output"
                  : "arf-meta";
            return (
              <div key={event.event_id} className={`arf-transcript-line ${tone}`} style={{ animationDelay: `${Math.min(index, 8) * 0.05}s` }}>
                <span>{new Date(event.timestamp).toLocaleTimeString([], { hour12: false })} </span>
                {eventText(event)}
              </div>
            );
          })}
          {streamError ? <p className="arf-inline-error" role="alert"><AlertTriangle className="inline h-3 w-3" aria-hidden /> {streamError}</p> : null}
        </div>
        {timeline.length ? <div className="arf-timeline" aria-label="Recent invocation lifecycle">
          {timeline.map((entry) => (
            <div className="arf-timeline-row" key={entry.id}>
              <span>{new Date(entry.timestamp).toLocaleTimeString([], { hour12: false })}</span>
              <b>{entry.role}</b>
              <em>{entry.state}</em>
            </div>
          ))}
        </div> : null}
      </div>
      <footer className="arf-stream-footer">
        <span>{events.length.toLocaleString()} events retained</span>
        {canStop ? <button type="button" className="arf-stop-button" disabled={controlPending} onClick={onStop}>Stop run</button> : null}
      </footer>
    </aside>
  );
}
