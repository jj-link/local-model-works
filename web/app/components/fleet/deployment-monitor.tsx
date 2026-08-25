import { useDeployments, useServingTelemetry, useLatestServingTelemetry } from "~/lib/queries";
import { StatusDot } from "~/components/status-dot";
import { EmptyState } from "~/components/empty-state";
import { TrendChart, type TrendSeries } from "~/components/trend-chart";
import { deploymentsOnNode } from "~/lib/telemetry";
import type { TelemetryRange } from "~/lib/telemetry";
import type { ServingTelemetrySample } from "~/lib/api";
import { Link } from "react-router";

const TEAL = "#5ed6d0";
const GREEN = "#76c66b";
const AMBER = "#ffb000";

function histSeries(
  samples: ServingTelemetrySample[],
  pick: (p: ServingTelemetrySample["payload"]) => number | null | undefined,
): TrendSeries[] {
  const points: [number, number][] = [];
  for (const s of samples) {
    const v = pick(s.payload);
    if (v == null || !Number.isFinite(v)) continue;
    points.push([s.ts, v]);
  }
  return points.length ? [{ label: "value", color: TEAL, points }] : [];
}

/** Service panels for every deployment placed on this node. */
export function DeploymentMonitor({ nodeId, range }: { nodeId: string; range: TelemetryRange }) {
  const { data: deployments } = useDeployments();
  const { data: latestServing } = useLatestServingTelemetry();
  const latestMap = new Map<string, ServingTelemetrySample>();
  for (const s of latestServing ?? []) latestMap.set(s.deployment_id, s);

  const rows = deploymentsOnNode(deployments ?? [], nodeId);
  if (rows.length === 0) return null;

  return (
    <section className="lmw-panel grid gap-3 p-4">
      <h2 className="lmw-label">serving</h2>
      {rows.map((d) =>
        d.rankZero ? (
          <RankZeroPanel key={d.deploymentId} deploymentId={d.deploymentId} range={range} latest={latestMap.get(d.deploymentId)?.payload} recipeName={d.recipeName} observedState={d.observedState} />
        ) : (
          <WorkerRow key={`${d.deploymentId}-${d.rank}`} rank={d.rank} recipeName={d.recipeName} observedState={d.observedState} />
        ),
      )}
    </section>
  );
}

function WorkerRow({ rank, recipeName, observedState }: { rank: number; recipeName?: string; observedState?: string }) {
  return (
    <div className="flex items-center justify-between rounded border border-hairline px-2 py-1.5">
      <span className="font-mono text-[11px] text-muted">{recipeName ?? "deployment"} · rank {rank} worker</span>
      <StatusDot state={observedState} />
    </div>
  );
}

function RankZeroPanel({
  deploymentId,
  range,
  latest,
  recipeName,
  observedState,
}: {
  deploymentId: string;
  range: TelemetryRange;
  latest?: ServingTelemetrySample["payload"];
  recipeName?: string;
  observedState?: string;
}) {
  const samples = useServingTelemetry(deploymentId, range);
  const p = latest;
  return (
    <div className="rounded border border-hairline p-3">
      <header className="flex flex-wrap items-center justify-between gap-2">
        <Link to={`/serving/deployments/${deploymentId}`} className="font-mono text-[12px] font-medium text-foreground hover:text-primary">
          {recipeName ?? deploymentId.slice(0, 8)}
        </Link>
        <StatusDot state={observedState} />
      </header>

      <div className="mt-2 grid gap-x-4 gap-y-1 font-mono text-[11px] text-muted sm:grid-cols-2 lg:grid-cols-4">
        {p ? (
          <>
            <Metric label="model" value={p.model_id ?? p.backend ?? "—"} />
            <Metric label="gen" value={num(p.generation_tps, "tok/s")} />
            <Metric label="prefill" value={num(p.prefill_tps, "tok/s")} />
            <Metric label="load" value={`r${p.requests_running ?? 0} / w${p.requests_waiting ?? 0} · ${activeSlots(p)}`} />
            <Metric label="kv cache" value={ratio(p.kv_cache_usage_ratio)} />
            <Metric label="prefix hit" value={ratio(p.prefix_cache_hit_ratio)} />
            <Metric label="ttft/e2e/itl p95" value={`${sec(p.ttft_p95_seconds)} / ${sec(p.e2e_p95_seconds)} / ${sec(p.itl_p95_seconds)}`} />
            <Metric label="preempts / spec" value={`${p.preemptions_total ?? 0} / ${ratio(p.spec_acceptance_ratio)}`} />
          </>
        ) : (
          <p className="font-mono text-xs text-faint">waiting for telemetry…</p>
        )}
      </div>

      {samples.isError ? (
        <EmptyState title="Serving history unavailable" detail="Retry the service telemetry fetch." onRetry={() => void samples.refetch()} />
      ) : samples.data && samples.data.length ? (
        <div className="mt-3 grid gap-3 md:grid-cols-2">
          <TrendChart
            series={[
              ...histSeries(samples.data, (x) => x.generation_tps).map((s) => ({ ...s, label: "gen", color: TEAL })),
              ...histSeries(samples.data, (x) => x.prefill_tps).map((s) => ({ ...s, label: "prefill", color: AMBER })),
            ]}
            yLabel="tok/s"
            valueFormat={(v) => v.toFixed(1)}
            ariaLabel="generation and prefill throughput"
          />
          <TrendChart
            series={histSeries(samples.data, (x) => x.requests_running).map((s) => ({ ...s, label: "running", color: GREEN }))}
            yLabel="requests"
            valueFormat={(v) => v.toFixed(0)}
            ariaLabel="request load"
          />
        </div>
      ) : null}
    </div>
  );
}

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <span className="flex items-baseline justify-between gap-2">
      <span className="lmw-label">{label}</span>
      <span className="tnum text-foreground">{value}</span>
    </span>
  );
}
function num(v: number | null | undefined, unit: string): string {
  return v == null ? "—" : `${v.toFixed(1)} ${unit}`;
}
function ratio(v: number | null | undefined): string {
  return v == null ? "—" : `${(v * 100).toFixed(0)}%`;
}
function sec(v: number | null | undefined): string {
  return v == null ? "—" : v < 1 ? `${(v * 1000).toFixed(0)}ms` : `${v.toFixed(2)}s`;
}
function activeSlots(p: ServingTelemetrySample["payload"]): string {
  if (p.slots_total) return `${p.slots_active ?? 0}/${p.slots_total}`;
  return `${p.slots_active ?? 0} active`;
}
