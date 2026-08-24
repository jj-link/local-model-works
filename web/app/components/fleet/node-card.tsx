import { Link } from "react-router";
import type { ReactNode } from "react";
import { StatusDot } from "~/components/status-dot";
import { bytes, relativeTime } from "~/lib/format";
import { cn } from "~/lib/utils";
import { MetricBar } from "~/components/fleet/metric-bar";
import {
  aggregateNodePayload,
  isLiveNode,
  isFreshSample,
  temperatureTone,
  utilizationTone,
  type Tone,
} from "~/lib/telemetry";
import type { Node, NodeTelemetrySample } from "~/lib/api";

function fmtSeconds(s?: number): string {
  if (typeof s !== "number" || s < 0) return "—";
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m`;
}

export interface NodeCardProps {
  node: Node;
  sample?: NodeTelemetrySample;
  now?: number;
  /** Pending-review action (rendered only for pending nodes). */
  action?: ReactNode;
}

/**
 * Fleet overview card for one node: status/name/hostname, accelerator summary,
 * metric bars for the highest-GPU utilization, summed accelerator memory, RAM,
 * and CPU, plus compact temperature, power, filesystem, network, uptime, agent
 * version, and sample age. Pending cards show only the review action and no
 * health inference; offline/stale cards are dimmed and labeled.
 */
export function NodeCard({ node, sample, now = Date.now(), action }: NodeCardProps) {
  const online = isLiveNode(node, now);
  const fresh = isFreshSample(sample?.ts ?? 0, online, now);
  const agg = aggregateNodePayload(node.status === "pending" ? undefined : sample?.payload);
  const accels = node.inventory?.accelerators ?? [];
  const dimmed = node.status === "offline" || node.status === "pending";
  const accelLabel =
    accels.length > 0 ? `${accels.length}× ${accels[0].vendor ?? ""} ${accels[0].name ?? ""}`.trim() : "none";

  return (
    <article
      className={cn(
        "lmw-panel flex flex-col gap-3 p-4",
        dimmed && "opacity-70",
        node.status === "offline" && "border-hairline",
      )}
    >
      <header className="flex items-start justify-between gap-2">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <Link
              to={`/fleet/nodes/${node.id}`}
              className="font-mono text-sm font-medium text-foreground hover:text-primary"
            >
              {node.display_name}
            </Link>
            <StatusDot state={node.status} pulse={online} />
          </div>
          {node.inventory?.hostname ? (
            <p className="mt-0.5 truncate font-mono text-[10px] text-faint">{node.inventory.hostname}</p>
          ) : null}
        </div>
        <div className="text-right font-mono text-[10px] text-faint">
          {node.status === "pending" ? (
            <span>awaiting approval</span>
          ) : fresh ? (
            <span className="text-ok">fresh</span>
          ) : (
            <span>sample {relativeTime(sampleAge(node))}</span>
          )}
        </div>
      </header>

      {node.status === "pending" ? (
        <footer className="mt-auto">{action}</footer>
      ) : (
        <div className="flex flex-col gap-3">
          <p className="font-mono text-[11px] text-muted">
            {accelLabel} · <span className="text-faint">{bytes(agg.gpuMemTotal)}</span>
          </p>
          <MetricBar
            label="gpu util"
            value={agg.gpuUtilMax ?? 0}
            display={typeof agg.gpuUtilMax === "number" ? `${agg.gpuUtilMax}%` : "—"}
            tone={utilizationTone(agg.gpuUtilMax ?? 0)}
          />
          <MetricBar
            label="gpu mem"
            value={agg.gpuMemUsed ?? 0}
            max={agg.gpuMemTotal && agg.gpuMemTotal > 0 ? agg.gpuMemTotal : 1}
            display={
              agg.gpuMemTotal && agg.gpuMemTotal > 0
                ? `${bytes(agg.gpuMemUsed)} / ${bytes(agg.gpuMemTotal)}`
                : "—"
            }
            tone="info"
          />
          <MetricBar
            label="ram"
            value={agg.memUsed ?? 0}
            max={agg.memTotal && agg.memTotal > 0 ? agg.memTotal : 1}
            display={
              agg.memTotal && agg.memTotal > 0 ? `${bytes(agg.memUsed)} / ${bytes(agg.memTotal)}` : "—"
            }
            tone="info"
          />
          <MetricBar
            label="cpu"
            value={agg.cpuUsage ?? 0}
            display={typeof agg.cpuUsage === "number" ? `${agg.cpuUsage}%` : "—"}
            tone={utilizationTone(agg.cpuUsage ?? 0)}
          />

          <div className="grid grid-cols-2 gap-x-3 gap-y-1 font-mono text-[10px] text-muted">
            <Field
              label="temp"
              value={typeof agg.gpuTempMax === "number" ? `${agg.gpuTempMax}°C` : "—"}
              tone={agg.gpuTempMax != null ? temperatureTone(agg.gpuTempMax) : undefined}
            />
            <Field label="power" value={agg.gpuPower ? `${(agg.gpuPower / 1000).toFixed(0)} W` : "—"} />
            <Field label="net rx/tx" value={formatRate(agg.netRxRate, agg.netTxRate)} />
            <Field label="uptime" value={fmtSeconds(agg.uptime)} />
            <Field label="agent" value={`v${node.agent_version ?? "?"}`} />
            <Field
              label="fs"
              value={agg.filesystems[0] ? `${agg.filesystems[0].pct.toFixed(0)}% ${agg.filesystems[0].mountPath}` : "—"}
            />
          </div>
        </div>
      )}
    </article>
  );
}

function sampleAge(node: Node): string | null {
  return node.last_heartbeat ?? null;
}

function formatRate(rx?: number, tx?: number): string {
  if (typeof rx !== "number" && typeof tx !== "number") return "—";
  return `${rx ?? 0}/${tx ?? 0} B/s`;
}

function Field({ label, value, tone }: { label: string; value: string; tone?: Tone }) {
  const cls =
    tone === "warn" || tone === "fault" ? (tone === "fault" ? "text-fault" : "text-primary") : "text-foreground";
  return (
    <span className="flex items-baseline justify-between gap-2">
      <span className="lmw-label">{label}</span>
      <span className={cls}>{value}</span>
    </span>
  );
}
