import { describe, expect, it } from "vitest";
import {
  NDJSONEventDecoder,
  reconstructActiveInvocations,
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
