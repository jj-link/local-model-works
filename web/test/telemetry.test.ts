import { describe, expect, it } from "vitest";
import {
  rangePolicy,
  isLiveNode,
  isFreshSample,
  aggregateNodePayload,
  utilizationTone,
  temperatureTone,
  deploymentsOnNode,
} from "~/lib/telemetry";
import type { Deployment, Node, NodePayload } from "~/lib/api";

const NOW = 1_800_000_000_000;

function node(over: Partial<Node> = {}): Node {
  return {
    id: "n1",
    status: "online",
    display_name: "n1",
    created_at: new Date(NOW).toISOString(),
    last_heartbeat: new Date(NOW).toISOString(),
    inventory: { hostname: "h", os: "linux", arch: "x86_64", docker: { ok: true, version: "v" }, accelerators: [] },
    ...over,
  };
}

describe("rangePolicy", () => {
  it("maps UI ranges to resolution and 7d uses limit 10080", () => {
    expect(rangePolicy("15m")).toEqual({ resolution: "5s", windowMs: 900_000, limit: 1000 });
    expect(rangePolicy("1h").resolution).toBe("5s");
    expect(rangePolicy("24h").resolution).toBe("1m");
    expect(rangePolicy("7d")).toMatchObject({ resolution: "1m", limit: 10080 });
  });
});

describe("freshness", () => {
  it("requires online state and a recent timestamp", () => {
    expect(isLiveNode(node(), NOW)).toBe(true);
    expect(isLiveNode(node({ status: "offline" }), NOW)).toBe(false);
    expect(isLiveNode(node({ last_heartbeat: new Date(NOW - 120_000).toISOString() }), NOW)).toBe(false);
    expect(isFreshSample(Math.floor(NOW / 1000) - 5, true, NOW)).toBe(true);
    expect(isFreshSample(Math.floor(NOW / 1000) - 20, true, NOW)).toBe(false);
    expect(isFreshSample(Math.floor(NOW / 1000), false, NOW)).toBe(false);
  });
});

describe("aggregateNodePayload", () => {
  it("sums memory/power and takes max utilization/temperature across GPUs", () => {
    const p: NodePayload = {
      cpu: { usage_percent: 12 },
      network: { rx_bytes_per_second: 500, tx_bytes_per_second: 300 },
      accelerators: [
        { index: 0, utilization_percent: 40, memory_used_bytes: 100, memory_total_bytes: 1000, temperature_c: 50, power_mw: 10, power_limit_mw: 100 },
        { index: 1, utilization_percent: 70, memory_used_bytes: 200, memory_total_bytes: 1000, temperature_c: 80, power_mw: 20, power_limit_mw: 100 },
      ],
    };
    const agg = aggregateNodePayload(p);
    expect(agg.gpuUtilMax).toBe(70);
    expect(agg.gpuTempMax).toBe(80);
    expect(agg.gpuMemUsed).toBe(300);
    expect(agg.gpuMemTotal).toBe(2000);
    expect(agg.gpuPower).toBe(30);
    expect(agg.gpuPowerLimit).toBe(200);
    expect(agg.cpuUsage).toBe(12);
    expect(agg.netRxRate).toBe(500);
  });

  it("renders unavailable for missing capacity, zero for valid zeroes", () => {
    const agg = aggregateNodePayload({ cpu: { usage_percent: 0 }, accelerators: [{ index: 0, utilization_percent: 0, memory_used_bytes: 0, memory_total_bytes: 0 }] });
    expect(agg.gpuMemTotal).toBe(0);
    expect(agg.cpuUsage).toBe(0);
    expect(agg.memTotal).toBeUndefined();
  });
});

describe("thresholds", () => {
  it("utilization warns above 60 and faults above 85", () => {
    expect(utilizationTone(50)).toBe("ok");
    expect(utilizationTone(61)).toBe("warn");
    expect(utilizationTone(90)).toBe("fault");
  });
  it("temperature warns above 65 and faults above 85", () => {
    expect(temperatureTone(60)).toBe("ok");
    expect(temperatureTone(70)).toBe("warn");
    expect(temperatureTone(90)).toBe("fault");
  });
});

describe("deploymentsOnNode", () => {
  const deps: Deployment[] = [
    { id: "d-worker", desired_state: "running", placements: [{ node_id: "node-a", rank: 1 }] } as Deployment,
    { id: "d-zero", desired_state: "running", placements: [{ node_id: "node-a", rank: 0 }] } as Deployment,
    { id: "d-stopped", desired_state: "stopped", observed_state: "stopped", placements: [{ node_id: "node-a", rank: 0 }] } as Deployment,
    { id: "d-other", desired_state: "running", placements: [{ node_id: "node-b", rank: 0 }] } as Deployment,
  ];
  it("keeps only current placements on the target node", () => {
    const rows = deploymentsOnNode(deps, "node-a");
    const ids = rows.map((r) => r.deploymentId);
    expect(ids.sort()).toEqual(["d-worker", "d-zero"].sort());
  });
  it("sorts desired-running rank-zero services first", () => {
    const rows = deploymentsOnNode(deps, "node-a");
    expect(rows[0].deploymentId).toBe("d-zero");
    expect(rows[0].rankZero).toBe(true);
  });
  it("projects recipe digest, engine, and endpoint model joins", () => {
    const joined: Deployment = {
      id: "d-join",
      recipe_digest: "sha256:recipe-join",
      recipe_name: "Join Recipe",
      engine: "vllm",
      desired_state: "running",
      placements: [{ node_id: "node-a", rank: 0 }],
      endpoint: { host: "h", port: 1, model: "Join Model" },
    } as unknown as Deployment;
    const [row] = deploymentsOnNode([joined], "node-a");
    expect(row.deploymentId).toBe("d-join");
    expect(row.recipeName).toBe("Join Recipe");
    expect(row.recipeDigest).toBe("sha256:recipe-join");
    expect(row.engine).toBe("vllm");
    expect(row.endpointModel).toBe("Join Model");
  });
});
