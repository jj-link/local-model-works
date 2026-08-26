import { useEffect, useMemo, useRef, useState } from "react";
import { runLogsUrl } from "~/lib/api";
import { streamEvents, type StreamHandle } from "~/lib/api/sse";

export type AutoResearchEventType =
  | "run.status"
  | "phase.changed"
  | "agent.started"
  | "agent.text.delta"
  | "agent.tool.started"
  | "agent.tool.finished"
  | "agent.usage"
  | "agent.finished"
  | "advisor.started"
  | "advisor.note"
  | "advisor.finished"
  | "artifact.changed"
  | "decision.required"
  | "error";

export interface AutoResearchEvent {
  version: 1;
  event_id: string;
  run_id: string;
  invocation_id?: string;
  parent_invocation_id?: string;
  node_id?: string;
  timestamp: string;
  type: AutoResearchEventType;
  payload: Record<string, unknown>;
}

export interface AutoResearchUsageSummary {
  inputTokens: number;
  outputTokens: number;
  totalTokens: number;
  costUsd: number | null;
  outputRate: number | null;
  contextPercent: number | null;
}

function finitePayloadNumber(payload: Record<string, unknown>, key: string): number | null {
  const value = payload[key];
  return typeof value === "number" && Number.isFinite(value) ? value : null;
}

export function summarizeAutoResearchUsage(events: AutoResearchEvent[]): AutoResearchUsageSummary {
  const cumulative = new Map<string, { inputTokens: number; outputTokens: number; costUsd: number | null }>();
  let outputRate: number | null = null;
  let contextPercent: number | null = null;

  for (const event of events) {
    if (event.type !== "agent.usage") continue;
    const key = event.invocation_id ?? event.event_id;
    const previous = cumulative.get(key) ?? { inputTokens: 0, outputTokens: 0, costUsd: null };
    const inputTokens = finitePayloadNumber(event.payload, "input_tokens");
    const outputTokens = finitePayloadNumber(event.payload, "output_tokens");
    const costUsd = finitePayloadNumber(event.payload, "cost_usd");
    cumulative.set(key, {
      inputTokens: inputTokens === null ? previous.inputTokens : Math.max(previous.inputTokens, inputTokens),
      outputTokens: outputTokens === null ? previous.outputTokens : Math.max(previous.outputTokens, outputTokens),
      costUsd: costUsd === null ? previous.costUsd : Math.max(previous.costUsd ?? 0, costUsd),
    });
    outputRate = finitePayloadNumber(event.payload, "output_rate") ?? outputRate;
    contextPercent = finitePayloadNumber(event.payload, "context_percent") ?? contextPercent;
  }

  let inputTokens = 0;
  let outputTokens = 0;
  let costUsd: number | null = null;
  for (const usage of cumulative.values()) {
    inputTokens += usage.inputTokens;
    outputTokens += usage.outputTokens;
    if (usage.costUsd !== null) costUsd = (costUsd ?? 0) + usage.costUsd;
  }

  return {
    inputTokens,
    outputTokens,
    totalTokens: inputTokens + outputTokens,
    costUsd,
    outputRate,
    contextPercent,
  };
}

export class NDJSONEventDecoder {
  private buffer = "";

  push(chunk: string): AutoResearchEvent[] {
    this.buffer += chunk;
    const events: AutoResearchEvent[] = [];
    for (;;) {
      const newline = this.buffer.indexOf("\n");
      if (newline < 0) break;
      const line = this.buffer.slice(0, newline).trim();
      this.buffer = this.buffer.slice(newline + 1);
      if (line === "") continue;
      const value: unknown = JSON.parse(line);
      if (!isAutoResearchEvent(value)) throw new Error("Invalid AutoResearch event envelope");
      events.push(value);
    }
    return events;
  }

  reset(): void {
    this.buffer = "";
  }
}

function isAutoResearchEvent(value: unknown): value is AutoResearchEvent {
  if (!value || typeof value !== "object") return false;
  const event = value as Partial<AutoResearchEvent>;
  return event.version === 1 && typeof event.event_id === "string" && typeof event.run_id === "string" &&
    typeof event.timestamp === "string" && typeof event.type === "string" && Boolean(event.payload) &&
    typeof event.payload === "object";
}

export interface ActiveInvocation {
  id: string;
  role: string;
  backend: string;
  model: string;
  nodeId?: string;
  advisor: boolean;
  parentId?: string;
}

export function reconstructActiveInvocations(events: AutoResearchEvent[]): ActiveInvocation[] {
  const active = new Map<string, ActiveInvocation>();
  for (const event of events) {
    const id = event.invocation_id;
    if (!id) continue;
    if (event.type === "agent.started" || event.type === "advisor.started") {
      active.set(id, {
        id,
        role: String(event.payload.role ?? "auxiliary"),
        backend: String(event.payload.backend ?? ""),
        model: String(event.payload.model ?? ""),
        nodeId: event.node_id,
        advisor: event.type === "advisor.started",
        parentId: event.parent_invocation_id,
      });
    }
    if (event.type === "agent.finished" || event.type === "advisor.finished") active.delete(id);
  }
  return [...active.values()];
}

interface StoredRunEvents {
  lastEventId: string | null;
  events: AutoResearchEvent[];
}

function readStoredEvents(runId: string): StoredRunEvents {
  if (typeof sessionStorage === "undefined") return { lastEventId: null, events: [] };
  try {
    const raw = sessionStorage.getItem(`autoresearch.events.${runId}`);
    if (!raw) return { lastEventId: null, events: [] };
    const parsed = JSON.parse(raw) as StoredRunEvents;
    return {
      lastEventId: typeof parsed.lastEventId === "string" ? parsed.lastEventId : null,
      events: Array.isArray(parsed.events) ? parsed.events.filter(isAutoResearchEvent) : [],
    };
  } catch {
    return { lastEventId: null, events: [] };
  }
}

export function useAutoResearchEvents(runId: string | undefined, active: boolean) {
  const initial = useMemo(() => runId ? readStoredEvents(runId) : { lastEventId: null, events: [] }, [runId]);
  const [events, setEvents] = useState<AutoResearchEvent[]>(initial.events);
  const [reconnecting, setReconnecting] = useState(false);
  const [ended, setEnded] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const handleRef = useRef<StreamHandle | null>(null);

  useEffect(() => {
    setEvents(initial.events);
    setEnded(false);
    setError(null);
    if (!runId || !active) return;
    const decoder = new NDJSONEventDecoder();
    let lastEventId = initial.lastEventId;
    const handle = streamEvents(runLogsUrl(runId), {
      lastEventId,
      onEvent: (frame) => {
        if (frame.id) lastEventId = frame.id;
        if (frame.event === "end") {
          setEnded(true);
          setReconnecting(false);
          handleRef.current?.close();
          return;
        }
        let chunk = frame.data;
        try {
          const parsed: unknown = JSON.parse(frame.data);
          if (typeof parsed === "string") chunk = parsed;
        } catch {
          // A log frame may already contain raw NDJSON.
        }
        try {
          const decoded = decoder.push(chunk);
          if (decoded.length === 0) return;
          setEvents((previous) => {
            const known = new Set(previous.map((event) => event.event_id));
            const merged = [...previous, ...decoded.filter((event) => !known.has(event.event_id))].slice(-1500);
            sessionStorage.setItem(`autoresearch.events.${runId}`, JSON.stringify({ lastEventId, events: merged }));
            return merged;
          });
        } catch (reason) {
          decoder.reset();
          setError(reason instanceof Error ? reason.message : "Malformed AutoResearch event stream");
        }
      },
      onState: (state) => setReconnecting(state.reconnecting),
    });
    handleRef.current = handle;
    return () => {
      handle.close();
      handleRef.current = null;
    };
  }, [active, initial, runId]);

  return {
    events,
    activeInvocations: useMemo(() => reconstructActiveInvocations(events), [events]),
    reconnecting,
    ended,
    error,
  };
}
