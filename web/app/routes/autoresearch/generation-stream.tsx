import { AlertTriangle, CirclePause, CirclePlay, OctagonX, RotateCw } from "lucide-react";
import { Button } from "~/components/ui/button";
import type { Run } from "~/lib/api";
import type { ActiveInvocation, AutoResearchEvent } from "./events";

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

export function GenerationStream({
  run,
  events,
  active,
  reconnecting,
  streamError,
  onControl,
  controlPending,
}: {
  run?: Run;
  events: AutoResearchEvent[];
  active: ActiveInvocation[];
  reconnecting: boolean;
  streamError: string | null;
  onControl: (action: "pause" | "resume" | "stop") => void;
  controlPending: boolean;
}) {
  let inputTokens = 0;
  let outputTokens = 0;
  let cost = 0;
  for (const event of events) {
    if (event.type !== "agent.usage") continue;
    inputTokens += Number(event.payload.input_tokens ?? 0);
    outputTokens += Number(event.payload.output_tokens ?? 0);
    cost += Number(event.payload.cost_usd ?? 0);
  }
  const transcript = events.filter((event) => eventText(event) !== "").slice(-120);
  const current = active.find((item) => !item.advisor) ?? active[0];
  const canPause = run?.state === "running";
  const canResume = run?.state === "paused";
  const canStop = Boolean(run && !["succeeded", "failed", "cancelled", "interrupted"].includes(run.state));

  return (
    <section className="lmw-panel flex min-h-[480px] flex-col overflow-hidden">
      <header className="lmw-panel-head">
        <h2 className="lmw-label">generation stream</h2>
        {reconnecting ? <span className="ml-auto inline-flex items-center gap-1 font-mono text-[10px] text-warn"><RotateCw className="h-3 w-3 animate-spin motion-reduce:animate-none" aria-hidden /> reconnecting</span> : (
          <span className="ml-auto font-mono text-[10px] text-faint">{run?.state ?? "idle"}</span>
        )}
      </header>
      <div className="grid grid-cols-3 divide-x divide-hairline border-b border-hairline bg-raised/35">
        <div className="px-3 py-2"><span className="lmw-label block">input</span><strong className="font-mono text-sm tnum">{inputTokens.toLocaleString()}</strong></div>
        <div className="px-3 py-2"><span className="lmw-label block">output</span><strong className="font-mono text-sm tnum">{outputTokens.toLocaleString()}</strong></div>
        <div className="px-3 py-2"><span className="lmw-label block">cost</span><strong className="font-mono text-sm tnum">${cost.toFixed(3)}</strong></div>
      </div>
      <div className="border-b border-hairline px-3 py-3">
        {current ? (
          <div className="flex items-center gap-3">
            <span className="grid h-8 w-8 place-items-center rounded-sm border border-primary/50 bg-primary/10 font-mono text-xs text-primary">⌁</span>
            <div className="min-w-0"><p className="truncate font-display text-sm font-medium">{current.role}</p><p className="truncate font-mono text-[10px] text-faint">{current.backend} · {current.model}</p></div>
            <span className="ml-auto h-2 w-2 rounded-full bg-ok shadow-[0_0_10px_currentColor]" aria-label="active" />
          </div>
        ) : <p className="font-mono text-xs text-faint">No invocation active.</p>}
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto bg-[#11191c] px-3 py-3 font-mono text-[11px] leading-5 text-[#b8c3bd]" role="log" aria-live="polite">
        {transcript.length === 0 ? <p className="text-[#71817c]">normalized model and tool events will appear here</p> : transcript.map((event) => (
          <div key={event.event_id} className={event.type === "error" || event.type === "decision.required" ? "text-[#e7b579]" : event.type === "advisor.note" ? "text-[#bca8ef]" : undefined}>
            <span className="mr-2 text-[#53645f]">{new Date(event.timestamp).toLocaleTimeString([], { hour12: false })}</span>
            {eventText(event)}
          </div>
        ))}
        {streamError ? <p className="mt-2 inline-flex items-center gap-1 text-[#e79078]"><AlertTriangle className="h-3 w-3" aria-hidden /> {streamError}</p> : null}
      </div>
      <footer className="flex flex-wrap items-center gap-2 border-t border-hairline px-3 py-2">
        <Button size="sm" variant="outline" disabled={!canPause || controlPending} onClick={() => onControl("pause")}><CirclePause aria-hidden /> pause</Button>
        <Button size="sm" variant="outline" disabled={!canResume || controlPending} onClick={() => onControl("resume")}><CirclePlay aria-hidden /> resume</Button>
        <Button size="sm" variant="destructive" disabled={!canStop || controlPending} onClick={() => onControl("stop")}><OctagonX aria-hidden /> stop</Button>
        <span className="ml-auto font-mono text-[10px] text-faint">{events.length} events</span>
      </footer>
    </section>
  );
}
