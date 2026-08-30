import { useState } from "react";
import { Instrument } from "~/components/instrument";
import { TrendChart, type TrendSeries } from "~/components/trend-chart";
import { MetricBar } from "~/components/fleet/metric-bar";
import { EmptyState } from "~/components/empty-state";
import { useNodeTelemetry } from "~/lib/queries";
import { bytes } from "~/lib/format";
import {
  aggregateNodePayload,
  isFreshSample,
  TELEMETRY_RANGES,
  type TelemetryRange,
  type Tone,
} from "~/lib/telemetry";
import { DeploymentMonitor } from "~/components/fleet/deployment-monitor";
import type { Node, NodeTelemetrySample } from "~/lib/api";
import { cn } from "~/lib/utils";

const POW = "#ffb000";
const TEAL = "#5ed6d0";
const GREEN = "#76c66b";

type Axis = "util" | "bytes" | "power" | "temp" | "pct";

function seriesFor(
  samples: NodeTelemetrySample[],
  pick: (s: NodeTelemetrySample) => [number, number] | null,
): TrendSeries[] {
  const points: [number, number][] = [];
  for (const s of samples) {
    const v = pick(s);
    if (v) points.push(v);
  }
  return points.length
    ? [{ label: "value", color: TEAL, points }]
    : [];
}

function fmtAxis(axis: Axis): (v: number) => string {
  switch (axis) {
    case "util":
    case "pct":
      return (v) => `${v.toFixed(0)}%`;
    case "bytes":
      return (v) => bytes(v);
    case "power":
      return (v) => `${(v / 1000).toFixed(0)}W`;
    case "temp":
      return (v) => `${v.toFixed(0)}°C`;
  }
}

export function NodeMonitor({ node }: { node: Node }) {
  const [range, setRange] = useState<TelemetryRange>("1h");
  const samples = useNodeTelemetry(node.id, range);

  const latestSample = samples.data?.at(-1);
  const agg = aggregateNodePayload(latestSample?.payload);
  const live = isFreshSample(latestSample?.ts ?? 0, node.status === "online");

  const cpuSeries = seriesFor(samples.data ?? [], (s) => {
    if (typeof s.payload.cpu?.usage_percent !== "number") return null;
    return [s.ts, s.payload.cpu.usage_percent];
  });
  const memSeries = seriesFor(samples.data ?? [], (s) => {
    if (typeof s.payload.memory?.used_bytes !== "number") return null;
    return [s.ts, s.payload.memory.used_bytes];
  });
  const netRx = seriesFor(samples.data ?? [], (s) => {
    if (typeof s.payload.network?.rx_bytes_per_second !== "number") return null;
    return [s.ts, s.payload.network.rx_bytes_per_second];
  }).map((s) => ({ ...s, label: "rx", color: TEAL }));
  const netTx = seriesFor(samples.data ?? [], (s) => {
    if (typeof s.payload.network?.tx_bytes_per_second !== "number") return null;
    return [s.ts, s.payload.network.tx_bytes_per_second];
  }).map((s) => ({ ...s, label: "tx", color: GREEN }));

  // One utilization/temperature/power series per accelerator index.
  const indexCount = (samples.data ?? []).reduce(
    (m, s) => Math.max(m, (s.payload.accelerators ?? []).reduce((n, a) => Math.max(n, (a.index ?? 0) + 1), 0)),
    0,
  );
  const perIndex: Record<string, TrendSeries[]> = {};
  for (let i = 0; i < indexCount; i++) {
    const util = seriesFor(samples.data ?? [], (s) => {
      const a = (s.payload.accelerators ?? []).find((x) => x.index === i);
      if (!a || typeof a.utilization_percent !== "number") return null;
      return [s.ts, a.utilization_percent];
    }).map((s) => ({ ...s, color: i === 0 ? POW : i === 1 ? TEAL : GREEN }));
    perIndex[`gpu${i} util`] = util;
  }

  if (samples.isError) {
    return <EmptyState title="Monitoring query failed" detail="Retry the node telemetry fetch." onRetry={() => void samples.refetch()} />;
  }

  return (
    <section className="lmw-panel grid gap-4 p-4">
      <header className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="lmw-label">live resources</h2>
        <RangeControl value={range} onChange={setRange} />
      </header>

      {node.status === "pending" ? (
        <p className="font-mono text-xs text-muted">
          This node is pending approval; live resource telemetry will appear once it is approved and reporting
          samples.
        </p>
      ) : !latestSample ? (
        <p className="font-mono text-xs text-faint">waiting for telemetry…</p>
      ) : (
        <>
          <div className="flex flex-wrap gap-4">
            <Instrument label="cpu" value={agg.cpuUsage ?? 0} max={100} unit="%" display={pct(agg.cpuUsage)} tone={tone(agg.cpuUsage, 60, 85)} />
            <Instrument label="ram" value={ramPct(agg.memUsed, agg.memTotal)} max={100} unit="%" display={memDisplay(agg.memUsed, agg.memTotal)} tone="info" />
            <Instrument label="gpu util" value={agg.gpuUtilMax ?? 0} max={100} unit="%" display={pct(agg.gpuUtilMax)} tone={tone(agg.gpuUtilMax, 60, 85)} />
            <Instrument label="gpu temp" value={agg.gpuTempMax ?? 0} max={100} unit="°C" display={agg.gpuTempMax != null ? `${agg.gpuTempMax}°C` : "—"} tone={tone(agg.gpuTempMax, 65, 85)} />
          </div>

          <div className="grid gap-3 md:grid-cols-2">
            <TrendChart series={cpuSeries} yLabel="cpu %" valueFormat={fmtAxis("util")} ariaLabel="cpu utilization" yFixed={[0, 100]} />
            <TrendChart series={memSeries.concat(memSeries.length ? [] : [])} yLabel="ram" valueFormat={fmtAxis("bytes")} ariaLabel="system memory" />
          </div>
          <div className="grid gap-3 md:grid-cols-2">
            {Object.entries(perIndex).map(([label, series]) => (
              <TrendChart key={label} series={series} yLabel={label} valueFormat={fmtAxis("util")} ariaLabel={label} yFixed={[0, 100]} />
            ))}
            <TrendChart series={[...netRx, ...netTx]} yLabel="net B/s" valueFormat={fmtAxis("bytes")} ariaLabel="network rates" />
          </div>

          <div className="grid grid-cols-2 gap-x-3 gap-y-1 font-mono text-[10px] text-muted md:grid-cols-4">
            <KV label="sum gpu power" value={agg.gpuPower ? `${(agg.gpuPower / 1000).toFixed(0)} W` : "—"} />
            <KV label="sum power limit" value={agg.gpuPowerLimit ? `${(agg.gpuPowerLimit / 1000).toFixed(0)} W` : "—"} />
            <KV label="uptime" value={agg.uptime != null ? fmtUptime(agg.uptime) : "—"} />
            <KV label="swap" value={agg.swapTotal != null ? (agg.swapTotal > 0 ? `${bytes(agg.swapUsed ?? 0)} / ${bytes(agg.swapTotal)}` : "disabled") : "—"} />
            <KV label="vm.swappiness" value={agg.swappiness != null ? String(agg.swappiness) : "—"} />
            <KV label="sample age" value={live ? "fresh" : latestSample.ts ? `${Math.round((Date.now() - latestSample.ts * 1000) / 1000)}s` : "—"} />
          </div>

          {agg.filesystems.length ? (
            <CurrentTable title="filesystems">
              {agg.filesystems.map((fs) => (
                <MetricBar
                  key={fs.mountPath}
                  label={fs.mountPath}
                  value={fs.usedBytes}
                  max={fs.totalBytes}
                  display={fs.totalBytes ? `${bytes(fs.usedBytes)} / ${bytes(fs.totalBytes)}` : "—"}
                  tone={fs.pct > 85 ? "fault" : fs.pct > 60 ? "warn" : "ok"}
                />
              ))}
            </CurrentTable>
          ) : null}

          <CurrentTables sample={latestSample} />
        </>
      )}
      {node.status !== "pending" ? <DeploymentMonitor nodeId={node.id} range={range} /> : null}
    </section>
  );
}

function CurrentTables({ sample }: { sample: NodeTelemetrySample }) {
  const processes = (sample.payload.accelerators ?? []).flatMap((a) =>
    (a.processes ?? []).map((p) => ({ index: a.index ?? 0, name: p.name, pid: p.pid, mem: p.used_gpu_memory_bytes })),
  );
  const throttles = (sample.payload.accelerators ?? [])
    .filter((a) => (a.throttle_reasons ?? []).length)
    .map((a) => `GPU${a.index}: ${(a.throttle_reasons ?? []).join(", ")}`);
  if (processes.length === 0 && throttles.length === 0) return null;
  return (
    <div className="grid gap-2">
      {processes.length ? (
        <CurrentTable title="gpu processes">
          {processes.slice(0, 5).map((p) => (
            <KV key={`${p.index}-${p.pid}`} label={p.pid ? `${p.name ?? "?"} #${p.pid}` : `${p.name ?? "?"}`} value={bytes(p.mem)} />
          ))}
        </CurrentTable>
      ) : null}
      {throttles.length ? (
        <CurrentTable title="throttling">
          {throttles.map((t) => (
            <span key={t} className="font-mono text-[11px] text-warn">{t}</span>
          ))}
        </CurrentTable>
      ) : null}
    </div>
  );
}

function CurrentTable({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="rounded border border-hairline p-2">
      <span className="lmw-label">{title}</span>
      <div className="mt-1 flex flex-col gap-1">{children}</div>
    </div>
  );
}

function RangeControl({ value, onChange }: { value: TelemetryRange; onChange: (r: TelemetryRange) => void }) {
  return (
    <div className="flex overflow-hidden rounded border border-hairline">
      {TELEMETRY_RANGES.map((r) => (
        <button
          key={r}
          type="button"
          onClick={() => onChange(r)}
          aria-pressed={r === value}
          className={cn(
            "px-2.5 py-1 font-mono text-[11px]",
            r === value ? "bg-primary text-primary-foreground" : "text-muted hover:bg-raised",
          )}
        >
          {r}
        </button>
      ))}
    </div>
  );
}

function KV({ label, value }: { label: string; value: string }) {
  return (
    <span className="flex items-baseline justify-between gap-2">
      <span className="lmw-label">{label}</span>
      <span className="text-foreground">{value}</span>
    </span>
  );
}

function pct(v?: number): string {
  return typeof v === "number" ? `${v}%` : "—";
}
function ramPct(used?: number, total?: number): number {
  if (typeof used !== "number" || typeof total !== "number" || total <= 0) return 0;
  return Math.round((used / total) * 100);
}
function memDisplay(used?: number, total?: number): string {
  if (typeof used !== "number" || typeof total !== "number" || total <= 0) return "—";
  return `${bytes(used)} / ${bytes(total)}`;
}
function fmtUptime(s: number): string {
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m`;
}
function tone(v: number | undefined, warn: number, fault: number): Tone {
  if (v == null) return "ok";
  if (v > fault) return "fault";
  if (v > warn) return "warn";
  return "ok";
}
