import { useSyncExternalStore } from "react";
import type { QueryClient } from "@tanstack/react-query";
import { eventsUrl } from "~/lib/api";
import { streamEvents, type SseEvent, type StreamHandle } from "~/lib/api/sse";

export interface WireEvent {
  id: number;
  type: string;
  payload: Record<string, unknown> | null;
  receivedAt: number;
}

const MAX_EVENTS = 200;
const INVALIDATE_DEBOUNCE_MS = 1000;

const EVENT_TO_QUERY: Record<string, string[]> = {
  node: ["nodes"],
  deployment: ["deployments"],
  run: ["runs", "benchmarks"],
  transfer: ["transfers"],
  recipe: ["recipes"],
  artifact: ["artifacts"],
  fabric: ["fabrics"],
  enrollment: ["enrollment-tokens"],
};

/**
 * Process-wide SSE feed for GET /events. One connection; the ring buffer
 * backs the dashboard activity panel, and event types drive TanStack
 * Query invalidation (debounced).
 */
class EventStream {
  private events: WireEvent[] = [];
  private listeners = new Set<() => void>();
  private handle: StreamHandle | null = null;
  private queryClient: QueryClient | null = null;
  private lastInvalidate = 0;
  private nextId = 1;

  bindQueryClient(qc: QueryClient): void {
    this.queryClient = qc;
  }

  start(): void {
    if (this.handle) return;
    this.handle = streamEvents(eventsUrl(), { onEvent: (ev) => this.ingest(ev) });
  }

  stop(): void {
    this.handle?.close();
    this.handle = null;
  }

  private ingest(ev: SseEvent): void {
    let payload: Record<string, unknown> | null = null;
    try {
      const p: unknown = JSON.parse(ev.data);
      if (p && typeof p === "object") payload = p as Record<string, unknown>;
    } catch {
      payload = null;
    }
    const type =
      ev.event !== "message" ? ev.event : ((payload?.type as string | undefined) ?? "event");
    const id = ev.id && Number.isFinite(Number(ev.id)) ? Number(ev.id) : this.nextId++;
    const w: WireEvent = { id, type, payload, receivedAt: Date.now() };
    this.events = [w, ...this.events].slice(0, MAX_EVENTS);
    this.invalidate(type);
    for (const l of this.listeners) l();
  }

  private invalidate(type: string): void {
    if (!this.queryClient) return;
    const now = Date.now();
    if (now - this.lastInvalidate < INVALIDATE_DEBOUNCE_MS) return;
    this.lastInvalidate = now;
    const keys = EVENT_TO_QUERY[type.split(".")[0]];
    if (!keys) return;
    const qc = this.queryClient;
    for (const k of keys) void qc.invalidateQueries({ queryKey: [k] });
  }

  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };

  getSnapshot = (): WireEvent[] => this.events;
}

export const eventStream = new EventStream();

export function useEventFeed(): WireEvent[] {
  return useSyncExternalStore(eventStream.subscribe, eventStream.getSnapshot, eventStream.getSnapshot);
}

/** Events whose payload references a node id. */
export function eventsForNode(events: WireEvent[], nodeId: string): WireEvent[] {
  return events.filter(
    (e) => e.payload && (e.payload.node_id === nodeId || e.payload.deployment_id !== undefined),
  );
}
