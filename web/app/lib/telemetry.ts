// Pure selectors aggregating live node/serving telemetry for the Fleet
// overview and node detail. Kept framework-free so unit tests exercise the
// exact math and thresholds the components render.
import type { Deployment, Node, NodePayload } from "~/lib/api";

export type TelemetryRange = "15m" | "1h" | "24h" | "7d";
export const TELEMETRY_RANGES: TelemetryRange[] = ["15m", "1h", "24h", "7d"];


/** A deployment still needs operator attention unless it is fully stopped. */
export function isCurrentDeployment(deployment: Deployment): boolean {
  return !(deployment.desired_state === "stopped" && deployment.observed_state === "stopped");
}

export interface RangePolicy {
  resolution: "5s" | "1m";
  windowMs: number;
  limit: number;
}

/** 15m/1h query 5s; 24h/7d query 1m; the 7d request uses limit 10080. */
export function rangePolicy(range: TelemetryRange): RangePolicy {
  switch (range) {
    case "15m":
      return { resolution: "5s", windowMs: 15 * 60_000, limit: 1000 };
    case "1h":
      return { resolution: "5s", windowMs: 60 * 60_000, limit: 2000 };
    case "24h":
      return { resolution: "1m", windowMs: 24 * 60 * 60_000, limit: 1440 };
    case "7d":
      return { resolution: "1m", windowMs: 7 * 24 * 60 * 60_000, limit: 10080 };
  }
}

/** Node is live when enrolled/online and its heartbeat is recent. */
export function isLiveNode(node: Node | undefined | null, now: number = Date.now()): boolean {
  if (!node || node.status !== "online") return false;
  const hb = Date.parse(node.last_heartbeat ?? "");
  if (Number.isNaN(hb)) return false;
  return now - hb < 30_000;
}

/** A sample is fresh only when at most 15s old AND the state is live. */
export function isFreshSample(tsSeconds: number, live: boolean, now: number = Date.now()): boolean {
  return live && tsSeconds > 0 && now - tsSeconds * 1000 <= 15_000;
}

export interface FilesystemAgg {
  mountPath: string;
  usedBytes: number;
  totalBytes: number;
  pct: number;
}

export interface AggregatedNodePayload {
  cpuUsage?: number;
  cpuCores?: number;
  memUsed?: number;
  memTotal?: number;
  gpuUtilMax?: number;
  gpuTempMax?: number;
  gpuMemUsed?: number;
  gpuMemTotal?: number;
  gpuPower?: number;
  gpuPowerLimit?: number;
  netRxRate?: number;
  netTxRate?: number;
  uptime?: number;
  filesystems: FilesystemAgg[];
}

/**
 * Aggregate a node payload deterministically: sums accelerator memory/power,
 * uses the maximum accelerator utilization and temperature, and uses the
 * aggregate host network rates.
 */
export function aggregateNodePayload(p: NodePayload | undefined): AggregatedNodePayload {
  const out: AggregatedNodePayload = { filesystems: [] };
  if (!p) return out;
  out.cpuUsage = p.cpu?.usage_percent;
  out.cpuCores = p.cpu?.cores;
  out.memUsed = p.memory?.used_bytes;
  out.memTotal = p.memory?.total_bytes;
  out.netRxRate = p.network?.rx_bytes_per_second;
  out.netTxRate = p.network?.tx_bytes_per_second;
  if (typeof p.uptime_seconds === "number") out.uptime = p.uptime_seconds;
  let maxUtil: number | undefined;
  let maxTemp: number | undefined;
  let sumMemUsed = 0;
  let sumMemTotal = 0;
  let sumPower = 0;
  let sumLimit = 0;
  for (const a of p.accelerators ?? []) {
    if (typeof a.utilization_percent === "number") {
      maxUtil = a.utilization_percent > (maxUtil ?? -1) ? a.utilization_percent : maxUtil;
    }
    if (typeof a.temperature_c === "number") {
      maxTemp = a.temperature_c > (maxTemp ?? -1) ? a.temperature_c : maxTemp;
    }
    sumMemUsed += a.memory_used_bytes ?? 0;
    sumMemTotal += a.memory_total_bytes ?? 0;
    sumPower += a.power_mw ?? 0;
    sumLimit += a.power_limit_mw ?? 0;
  }
  if (maxUtil !== undefined) out.gpuUtilMax = maxUtil;
  if (maxTemp !== undefined) out.gpuTempMax = maxTemp;
  out.gpuMemUsed = sumMemUsed;
  out.gpuMemTotal = sumMemTotal;
  out.gpuPower = sumPower;
  out.gpuPowerLimit = sumLimit;
  for (const fs of p.filesystems ?? []) {
    out.filesystems.push({
      mountPath: fs.mount_path ?? "",
      usedBytes: fs.used_bytes ?? 0,
      totalBytes: fs.total_bytes ?? 0,
      pct: (fs.total_bytes ?? 0) > 0 ? ((fs.used_bytes ?? 0) / (fs.total_bytes ?? 1)) * 100 : 0,
    });
  }
  out.filesystems.sort((a, b) => b.pct - a.pct);
  return out;
}

export type Tone = "ok" | "warn" | "fault";

/** Utilization/occupancy: warning above 60%, fault above 85%. */
export function utilizationTone(pct: number): Tone {
  if (pct > 85) return "fault";
  if (pct > 60) return "warn";
  return "ok";
}

/** Temperature: warning above 65°C, fault above 85°C. */
export function temperatureTone(c: number): Tone {
  if (c > 85) return "fault";
  if (c > 65) return "warn";
  return "ok";
}

/** Bytes-per-second rate tone: warning above 60% of 10 Gbps link, fault 85%. */
export function rateTone(rate: number): Tone {
  const pct = (rate / (10 * 1_000 * 1_000_000)) * 100;
  if (pct > 85) return "fault";
  if (pct > 60) return "warn";
  return "ok";
}

export interface DeploymentHost {
  deploymentId: string;
  nodeId: string;
  rank: number;
}

/** Expand every deployment placement into (deploymentId, nodeId, rank) rows. */
export function deploymentHosts(deployments: Deployment[]): DeploymentHost[] {
  const out: DeploymentHost[] = [];
  for (const d of deployments) {
    for (const pl of d.placements ?? []) {
      if (!pl.node_id) continue;
      out.push({ deploymentId: d.id, nodeId: pl.node_id, rank: pl.rank ?? 0 });
    }
  }
  return out;
}
export interface NodeDeploymentRow {
  deploymentId: string;
  recipeName?: string;
  recipeDigest?: string;
  engine?: string;
  endpointModel?: string;
  observedState?: string;
  desiredState?: string;
  rank: number;
  rankZero: boolean;
}
/**
 * Current deployments placed on a node, sorted with desired-running rank-zero
 * services first, then by recipe_name / id.
 */
export function deploymentsOnNode(deployments: Deployment[], nodeId: string): NodeDeploymentRow[] {
  const rows: NodeDeploymentRow[] = [];
  for (const d of deployments) {
    if (!isCurrentDeployment(d)) continue;
    for (const pl of d.placements ?? []) {
      if (pl.node_id !== nodeId) continue;
      rows.push({
        deploymentId: d.id,
        recipeName: d.recipe_name,
        recipeDigest: d.recipe_digest,
        engine: d.engine,
        endpointModel: d.endpoint?.model,
        observedState: d.observed_state,
        desiredState: d.desired_state,
        rank: pl.rank ?? 0,
        rankZero: (pl.rank ?? 0) === 0,
      });
    }
  }
  rows.sort((a, b) => {
    const aRankZero = a.rankZero && a.desiredState === "running" ? 0 : 1;
    const bRankZero = b.rankZero && b.desiredState === "running" ? 0 : 1;
    if (aRankZero !== bRankZero) return aRankZero - bRankZero;
    const an = a.recipeName ?? a.deploymentId;
    const bn = b.recipeName ?? b.deploymentId;
    if (an !== bn) return an < bn ? -1 : 1;
    return a.deploymentId < b.deploymentId ? -1 : 1;
  });
  return rows;
}
