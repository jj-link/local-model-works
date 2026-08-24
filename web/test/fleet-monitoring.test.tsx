import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { describe, expect, it } from "vitest";
import { NodeCard } from "~/components/fleet/node-card";
import type { Node, NodeTelemetrySample } from "~/lib/api";

const NOW = Date.now();

function onlineNode(over: Partial<Node> = {}): Node {
  return {
    id: "n1",
    display_name: "online-node",
    status: "online",
    last_heartbeat: new Date(NOW).toISOString(),
    inventory: { accelerators: [{ index: 0, vendor: "NVIDIA", name: "RTX", memory_bytes: 1048576, features: [] }] },
    ...over,
  };
}

function sample(over: Partial<NodeTelemetrySample["payload"]> = {}, ts = Math.floor(NOW / 1000) - 2): NodeTelemetrySample {
  return {
    node_id: "n1",
    ts,
    payload: {
      cpu: { usage_percent: 0, cores: 8, load1: 0 },
      memory: { used_bytes: 0, total_bytes: 0, swap_used_bytes: 0 },
      uptime_seconds: 3600,
      accelerators: [{ index: 0, utilization_percent: 0, memory_used_bytes: 0, memory_total_bytes: 0 }],
      ...over,
    },
  };
}

function renderCard(node: Node, nodeSample?: NodeTelemetrySample) {
  return render(
    <MemoryRouter>
      <NodeCard node={node} sample={nodeSample} now={NOW} />
    </MemoryRouter>,
  );
}

describe("NodeCard unavailable-vs-zero", () => {
  it("renders a dash for missing capacity and 0% for a valid zero utilization", () => {
    renderCard(onlineNode(), sample());
    // Valid zero utilization renders as 0%.
    expect(screen.getAllByText("0%").length).toBeGreaterThan(0);
    // Missing capacity (0 total memory) renders the RAM meter emptied (max=1).
    const ramMeter = screen.getByRole("meter", { name: "ram" });
    expect(ramMeter.getAttribute("aria-valuemax")).toBe("1");
    expect(ramMeter.getAttribute("aria-valuenow")).toBe("0");
  });

  it("labels a stale node and shows its last values", () => {
    renderCard(onlineNode({ last_heartbeat: new Date(NOW - 10 * 60_000).toISOString() }), sample(undefined, Math.floor(NOW / 1000) - 600));
    expect(screen.getByText(/sample/)).toBeTruthy();
  });

  it("shows a review action for a pending node with no health inference", () => {
    renderCard(
      onlineNode({ id: "pending", display_name: "pending-node", status: "pending" }),
      // Pending cards receive no telemetry sample.
      undefined,
    );
    expect(screen.getByText("awaiting approval")).toBeTruthy();
    expect(screen.queryByText("0%")).toBeNull();
  });
});
