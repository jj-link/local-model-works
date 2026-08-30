import { describe, expect, it, vi } from "vitest";
import {
  NDJSONEventDecoder,
  reconstructActiveInvocations,
  summarizeAutoResearchUsage,
  storeRunEvents,
  type AutoResearchEvent,
} from "~/routes/autoresearch/events";

function event(
  id: string,
  type: AutoResearchEvent["type"],
  invocationId: string,
  nodeId: string,
  payload: Record<string, unknown>,
): AutoResearchEvent {
  return {
    version: 1,
    event_id: id,
    run_id: "run-1",
    invocation_id: invocationId,
    node_id: nodeId,
    timestamp: "2026-08-23T12:00:00Z",
    type,
    payload,
  };
}

describe("NDJSONEventDecoder", () => {
  it("buffers arbitrary chunk boundaries without losing events", () => {
    const decoder = new NDJSONEventDecoder();
    const first = event("e1", "agent.started", "primary-1", "experiment.coder", { role: "experiment-coder", model: "qwen" });
    const second = event("e2", "agent.text.delta", "primary-1", "experiment.coder", { delta: "result" });
    const wire = `${JSON.stringify(first)}\n${JSON.stringify(second)}\n`;
    expect(decoder.push(wire.slice(0, 17))).toEqual([]);
    expect(decoder.push(wire.slice(17, wire.length - 3))).toEqual([first]);
    expect(decoder.push(wire.slice(wire.length - 3))).toEqual([second]);
  });

  it("reconstructs concurrent primaries and a separate advisor lane", () => {
    const events = [
      event("e1", "agent.started", "primary-1", "experiment.coder", { role: "experiment-coder", backend: "codex", model: "qwen" }),
      event("e2", "agent.started", "primary-2", "paper.writer", { role: "paper-writer", backend: "claude", model: "sonnet" }),
      { ...event("e3", "advisor.started", "advisor-1", "aux:advisor", { role: "experiment-coder", backend: "claude", model: "haiku" }), parent_invocation_id: "primary-1" },
    ];
    const active = reconstructActiveInvocations(events);
    expect(active).toHaveLength(3);
    expect(active.filter((item) => !item.advisor).map((item) => item.nodeId)).toEqual(["experiment.coder", "paper.writer"]);
    expect(active.find((item) => item.advisor)?.parentId).toBe("primary-1");

    events.push(event("e4", "agent.finished", "primary-1", "experiment.coder", {}));
    expect(reconstructActiveInvocations(events).map((item) => item.id)).toEqual(["primary-2", "advisor-1"]);
  });
});

describe("summarizeAutoResearchUsage", () => {
  it("uses cumulative invocation totals without double-counting repeated frames", () => {
    const events = [
      event("u1", "agent.usage", "primary-1", "experiment.coder", { input_tokens: 100, output_tokens: 20, cost_usd: 0.1 }),
      event("u2", "agent.usage", "primary-1", "experiment.coder", { input_tokens: 100, output_tokens: 20, cost_usd: 0.1 }),
      event("u3", "agent.usage", "primary-1", "experiment.coder", { input_tokens: 120, output_tokens: 35, cost_usd: 0.16, output_rate: 18.5 }),
      event("u4", "agent.usage", "primary-2", "paper.writer", { input_tokens: 50, output_tokens: 10, cost_usd: 0.04, context_percent: 42 }),
    ];

    expect(summarizeAutoResearchUsage(events)).toEqual({
      inputTokens: 170,
      outputTokens: 45,
      totalTokens: 215,
      costUsd: 0.2,
      outputRate: 18.5,
      contextPercent: 42,
    });
  });

  it("keeps unavailable values null and ignores malformed numeric payloads", () => {
    const events = [
      event("u1", "agent.usage", "primary-1", "experiment.coder", {
        input_tokens: "100",
        output_tokens: Number.NaN,
        cost_usd: "free",
        output_rate: Infinity,
        context_percent: null,
      }),
    ];

    expect(summarizeAutoResearchUsage(events)).toEqual({
      inputTokens: 0,
      outputTokens: 0,
      totalTokens: 0,
      costUsd: null,
      outputRate: null,
      contextPercent: null,
    });
  });
});

describe("AutoResearch event replay storage", () => {
  it("does not interrupt event handling when sessionStorage rejects writes", () => {
    const setItem = vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new DOMException("full", "QuotaExceededError");
    });
    expect(() => storeRunEvents("run-1", {
      lastEventId: "e1",
      events: [event("e1", "agent.text.delta", "primary-1", "idea.creator", { delta: "still rendered" })],
    })).not.toThrow();
    setItem.mockRestore();
  });

  it("caps persisted replay data by event count and serialized bytes", () => {
    const events = Array.from({ length: 2_000 }, (_, index) =>
      event(`large-${index}`, "agent.text.delta", "primary-1", "idea.creator", { delta: "x".repeat(2_000) }));
    storeRunEvents("large-run", { lastEventId: "large-1999", events });
    const raw = sessionStorage.getItem("autoresearch.events.large-run");
    expect(raw).not.toBeNull();
    expect(new TextEncoder().encode(raw ?? "").byteLength).toBeLessThanOrEqual(2 * 1024 * 1024);
    expect((JSON.parse(raw ?? "{}").events as unknown[]).length).toBeLessThanOrEqual(1_500);
  });
});
