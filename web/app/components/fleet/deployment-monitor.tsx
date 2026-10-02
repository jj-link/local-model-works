import { useDeployments, useServingTelemetry, useLatestServingTelemetry } from "~/lib/queries";
import { StatusDot } from "~/components/status-dot";
import { EmptyState } from "~/components/empty-state";
import { TrendChart, type TrendSeries } from "~/components/trend-chart";
import { deploymentsOnNode, isFreshSample, TELEMETRY_RANGES, type TelemetryRange } from "~/lib/telemetry";
import type { ServingTelemetrySample } from "~/lib/api";
import { Link } from "react-router";
import { useState } from "react";
import { cn } from "~/lib/utils";
import type { UseQueryResult } from "@tanstack/react-query";

const TEAL = "#5ed6d0";
const GREEN = "#76c66b";
const AMBER = "#ffb000";

// Stable formatter identities let uPlot update data without remounting on every poll.
const formatRate = (value: number) => value.toFixed(1);
const formatCount = (value: number) => value.toFixed(0);
const formatPercent = (value: number) => `${(value * 100).toFixed(1)}%`;

function histSeries(
  samples: ServingTelemetrySample[],
  pick: (p: ServingTelemetrySample["payload"]) => number | null | undefined,
): TrendSeries[] {
  const points: [number, number][] = [];
  for (const s of samples) {
    if (!s.payload.available) continue;
    const v = pick(s.payload);
    if (v == null || !Number.isFinite(v)) continue;
    points.push([s.ts, v]);
  }
  return points.length ? [{ label: "value", color: TEAL, points }] : [];
}

/** Service panels for every deployment placed on this node. */
export function DeploymentMonitor({ nodeId, range }: { nodeId: string; range: TelemetryRange }) {
  const { data: deployments } = useDeployments();
  const latestQuery = useLatestServingTelemetry();

  const rows = deploymentsOnNode(deployments ?? [], nodeId);
  if (rows.length === 0) return null;

  return (
    <section className="lmw-panel grid gap-3 p-4">
      <h2 className="lmw-label">serving</h2>
      {rows.map((d) =>
        d.rankZero ? (
          <RankZeroPanel key={d.deploymentId} deploymentId={d.deploymentId} range={range} latestQuery={latestQuery} recipeName={d.recipeName} observedState={d.observedState} />
        ) : (
          <WorkerRow key={`${d.deploymentId}-${d.rank}`} rank={d.rank} recipeName={d.recipeName} observedState={d.observedState} />
        ),
      )}
    </section>
  );
}

/** The same serving panel used on fleet nodes, with its own history range. */
export function DeploymentLiveStats({ deploymentId, recipeName, observedState }: {
  deploymentId: string;
  recipeName?: string;
  observedState?: string;
}) {
  const [range, setRange] = useState<TelemetryRange>("1h");
  const latestQuery = useLatestServingTelemetry();
  return (
    <section className="lmw-panel" aria-label="Live serving statistics">
      <header className="lmw-panel-head flex flex-wrap items-center justify-between gap-2">
        <h2 className="lmw-label">live statistics</h2>
        <div className="flex overflow-hidden rounded border border-hairline" role="group" aria-label="Serving history range">
          {TELEMETRY_RANGES.map((r) => (
            <button
              key={r}
              type="button"
              onClick={() => setRange(r)}
              aria-pressed={r === range}
              className={cn("px-2.5 py-1 font-mono text-[11px]", r === range ? "bg-primary text-primary-foreground" : "text-muted hover:bg-raised")}
            >
              {r}
            </button>
          ))}
        </div>
      </header>
      <div className="p-3">
        <RankZeroPanel deploymentId={deploymentId} range={range} latestQuery={latestQuery} recipeName={recipeName} observedState={observedState} />
      </div>
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
  latestQuery,
  recipeName,
  observedState,
}: {
  deploymentId: string;
  range: TelemetryRange;
  latestQuery: UseQueryResult<ServingTelemetrySample[], Error>;
  recipeName?: string;
  observedState?: string;
}) {
  const samples = useServingTelemetry(deploymentId, range);
  const latest = latestQuery.data?.find((s) => s.deployment_id === deploymentId);
  const p = latest?.payload;
  const tensorFold = (p?.backend ?? samples.data?.at(-1)?.payload.backend) === "tensorfold";
  const promptLabel = tensorFold ? "prompt accounting rate" : "prefill";
  const acceptanceLabel = tensorFold ? "draft acceptance (completed requests)" : "draft acceptance";
  const fresh = latest != null && isFreshSample(latest.ts, observedState === "healthy" || observedState === "degraded");
  return (
    <div className="rounded border border-hairline p-3">
      <header className="flex flex-wrap items-center justify-between gap-2">
        <Link to={`/serving/deployments/${deploymentId}`} className="font-mono text-[12px] font-medium text-foreground hover:text-primary">
          {recipeName ?? deploymentId.slice(0, 8)}
        </Link>
        <StatusDot state={observedState} />
      </header>

      <div className="mt-2 font-mono text-[11px] text-muted" aria-live="polite">
        {latestQuery.isError ? (
          <EmptyState title="Serving telemetry error" detail="Latest statistics could not be refreshed. Any values below are from the last received sample." onRetry={() => void latestQuery.refetch()} />
        ) : latestQuery.isPending ? (
          <p>Loading serving telemetry…</p>
        ) : !latest ? (
          <p>Serving telemetry unavailable · no sample received for this deployment.</p>
        ) : !p?.available ? (
          <p>Serving telemetry unavailable{p?.error_code ? ` · ${p.error_code}` : ""}{p?.error ? ` · ${p.error}` : ""}</p>
        ) : (
          <p>{fresh ? "Live telemetry" : "Stale telemetry · values are from the last received sample"}</p>
        )}
        {latest ? (
          <p className="mt-1">Last sample: <time dateTime={new Date(latest.ts * 1000).toISOString()}>{new Date(latest.ts * 1000).toLocaleString()}</time></p>
        ) : null}
      </div>

      <div className="mt-2 grid gap-x-4 gap-y-1 font-mono text-[11px] text-muted sm:grid-cols-2 lg:grid-cols-4">
        {p?.available ? (
          <>
            <Metric label="model" value={p.model_id ?? p.backend ?? "—"} />
            <Metric label="generation (aggregate)" value={num(p.generation_tps, "tok/s")} />
            <Metric label={promptLabel} value={num(p.prefill_tps, "tok/s")} />
            <Metric label="requests running / waiting" value={`${count(reportedCount(p, "requests_running"))} / ${count(reportedCount(p, "requests_waiting"))}`} />
            <Metric label="slots active / total" value={`${count(reportedCount(p, "slots_active"))} / ${count(reportedCount(p, "slots_total"))}`} />
            <Metric label="kv cache" value={ratio(p.kv_cache_usage_ratio)} />
            <Metric label="prefix hit" value={ratio(p.prefix_cache_hit_ratio)} />
            <Metric label="ttft/e2e/itl p95" value={`${sec(p.ttft_p95_seconds)} / ${sec(p.e2e_p95_seconds)} / ${sec(p.itl_p95_seconds)}`} />
            <Metric label="preemptions" value={count(reportedCount(p, "preemptions_total"))} />
            <Metric label={acceptanceLabel} value={ratio(p.spec_acceptance_ratio)} />
            <Metric label="context length" value={count(p.context_length)} />
          </>
        ) : null}
      </div>
      <p className="mt-2 font-mono text-[11px] text-faint">
        {tensorFold
          ? " TensorFold generation is live aggregate sampled interval token throughput, not individual-request decode speed. Prompt accounting counts prompt tokens from requests completed in the sampled interval, not instantaneous prefill speed. Draft acceptance is cumulative across completed requests."
          : " Throughput is engine-reported; it is not individual-request decode speed."}
        {" "}— means not reported or unsupported.
      </p>

      {samples.isError ? (
        <EmptyState title="Serving history unavailable" detail="Retry the service telemetry fetch." onRetry={() => void samples.refetch()} />
      ) : samples.data && samples.data.length ? (
        <div className="mt-3 grid gap-3 md:grid-cols-2">
          <figure>
            <figcaption className="lmw-label mb-2">{tensorFold ? "generation throughput / prompt accounting rate" : "generation / prefill throughput"}</figcaption>
            <TrendChart
              series={[
                ...histSeries(samples.data, (x) => x.generation_tps).map((s) => ({ ...s, label: "generation", color: TEAL })),
                ...histSeries(samples.data, (x) => x.prefill_tps).map((s) => ({ ...s, label: promptLabel, color: AMBER })),
              ]}
              yLabel="tok/s"
              valueFormat={formatRate}
              ariaLabel={tensorFold ? "generation throughput and prompt accounting rate" : "generation and prefill throughput"}
            />
          </figure>
          <figure>
            <figcaption className="lmw-label mb-2">active requests / slots</figcaption>
            <TrendChart
              series={[
                ...histSeries(samples.data, (x) => reportedCount(x, "requests_running")).map((s) => ({ ...s, label: "requests running", color: GREEN })),
                ...histSeries(samples.data, (x) => reportedCount(x, "slots_active")).map((s) => ({ ...s, label: "active slots", color: TEAL })),
              ]}
              yLabel="count"
              valueFormat={formatCount}
              ariaLabel="active requests and slots"
            />
          </figure>
          <figure>
            <figcaption className="lmw-label mb-2">KV cache usage</figcaption>
            <TrendChart
              series={histSeries(samples.data, (x) => x.kv_cache_usage_ratio).map((s) => ({ ...s, label: "KV cache usage", color: TEAL }))}
              yLabel="ratio"
              yFixed={[0, 1]}
              valueFormat={formatPercent}
              ariaLabel="KV cache usage"
            />
          </figure>
          <figure>
            <figcaption className="lmw-label mb-2">{acceptanceLabel}</figcaption>
            <TrendChart
              series={histSeries(samples.data, (x) => x.spec_acceptance_ratio).map((s) => ({ ...s, label: acceptanceLabel, color: GREEN }))}
              yLabel="ratio"
              yFixed={[0, 1]}
              valueFormat={formatPercent}
              ariaLabel={acceptanceLabel}
            />
          </figure>
        </div>
      ) : (
        <p className="mt-3 font-mono text-[11px] text-faint">{samples.isPending ? "Loading serving history…" : "Serving history unavailable · no samples in this range."}</p>
      )}
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
function count(v: number | null | undefined): string {
  return v == null ? "—" : v.toFixed(0);
}

/** Recover omitempty zeros only for counters that the collector supports. */
function reportedCount(p: ServingTelemetrySample["payload"], key: "requests_running" | "requests_waiting" | "slots_active" | "slots_total" | "preemptions_total"): number | null {
  if (!p.available) return null;
  if (p[key] != null) return p[key];
  if (key === "requests_running" && ["vllm", "sglang", "tensorfold"].includes(p.backend ?? "")) return 0;
  if ((key === "requests_waiting" || key === "preemptions_total") && p.backend === "vllm") return 0;
  if (key === "slots_active" && (["vllm", "sglang", "llamacpp"].includes(p.backend ?? "") || (p.slots_total ?? 0) > 0)) return 0;
  if (key === "slots_total" && p.backend === "llamacpp") return 0;
  return null;
}
