import { Link } from "react-router";
import { useNodes, useLatestNodeTelemetry, useDeployments, useLatestServingTelemetry } from "~/lib/queries";
import { EmptyState } from "~/components/empty-state";
import { StatusDot } from "~/components/status-dot";
import { bytes } from "~/lib/format";
import { useSession } from "~/lib/api/session";
import { NodeCard } from "~/components/fleet/node-card";
import { deploymentsOnNode, type NodeDeploymentRow } from "~/lib/telemetry";
import type { Deployment, NodeTelemetrySample, ServingTelemetrySample } from "~/lib/api";
import { cn } from "~/lib/utils";
import { StatTile } from "~/components/stat-tile";

function latestMap(samples: NodeTelemetrySample[] | undefined): Map<string, NodeTelemetrySample> {
  const m = new Map<string, NodeTelemetrySample>();
  for (const s of samples ?? []) m.set(s.node_id, s);
  return m;
}

function servingMap(samples: ServingTelemetrySample[] | undefined): Map<string, ServingTelemetrySample> {
  const m = new Map<string, ServingTelemetrySample>();
  for (const s of samples ?? []) m.set(s.deployment_id, s);
  return m;
}

function DeploymentRow({ d, sample }: { d: NodeDeploymentRow; sample?: ServingTelemetrySample }) {
  const p = sample?.payload;
  // Prefer live serving identity, then the persisted deployment engine/model,
  // and only render "Not reported" when both sources are absent.
  const backend = p?.backend ?? d.engine;
  const model = p?.model_id ?? d.endpointModel;
  const identity =
    backend && model
      ? `${backend} · ${model}`
      : backend ?? model ?? "Not reported";
  return (
    <div className="flex items-center justify-between gap-2 rounded border border-hairline px-2 py-1.5">
      <span className="flex min-w-0 items-center gap-2">
        <StatusDot state={d.observedState} />
        <span className="truncate font-mono text-[11px] text-foreground">
          <Link to={`/serving/deployments/${d.deploymentId}`} className="hover:underline">
            {d.rankZero
              ? d.recipeName ?? d.deploymentId.slice(0, 8)
              : `${d.recipeName ?? "deployment"} · rank ${d.rank} worker`}
          </Link>
        </span>
      </span>
      <span className="flex shrink-0 items-center gap-2">
        {d.recipeDigest ? (
          <Link to={`/library/recipes/${d.recipeDigest}`} className="font-mono text-[10px] text-faint hover:underline">
            recipe →
          </Link>
        ) : null}
        {d.rankZero ? (
          <span className="tnum whitespace-nowrap font-mono text-[10px] text-muted">
            {identity}
            <span className="ml-2">
              {typeof p?.generation_tps === "number" ? `${p.generation_tps.toFixed(1)} tok/s` : ""}
              {typeof p?.requests_running === "number" || typeof p?.requests_waiting === "number"
                ? ` · r${p?.requests_running ?? 0}/w${p?.requests_waiting ?? 0}`
                : ""}
            </span>
          </span>
        ) : (
          <span className="font-mono text-[10px] text-faint">{p?.available ? "serving" : "offline"}</span>
        )}
      </span>
    </div>
  );
}

export default function NodesRoute() {
  const now = Date.now();
  const { data: nodes, isPending, isError, error, refetch } = useNodes();
  const nodesQ = useLatestNodeTelemetry();
  const deploymentsQ = useDeployments();
  const servingQ = useLatestServingTelemetry();
  const session = useSession();

  const nodeList = nodes ?? [];
  const latest = latestMap(nodesQ.data);
  const deployments = deploymentsQ.data ?? [];
  const latestServing = servingMap(servingQ.data);
  const pendingNodes = nodeList.filter((n) => n.status === "pending");
  const onlineCount = nodeList.filter((n) => n.status === "online").length;

  let accelCount = 0;
  let accelMem = 0;
  for (const n of nodeList) {
    for (const a of n.inventory?.accelerators ?? []) {
      accelCount += 1;
      accelMem += a.memory_bytes ?? 0;
    }
  }
  const degraded = nodeList.filter((n) => n.status === "degraded" || n.status === "offline").length;
  const activeServing = deployments.filter(
    (d) => d.desired_state === "running" && d.endpoint?.host,
  ).length;

  if (isPending) {
    return <p className="font-mono text-xs text-faint">loading nodes…</p>;
  }
  if (isError) {
    return (
      <EmptyState
        className="m-3"
        title="Cannot load nodes"
        detail={error instanceof Error ? error.message : undefined}
        onRetry={() => void refetch()}
      />
    );
  }
  return (
    <div className="grid gap-4">
      <section className="lmw-panel overflow-hidden">
        <header className="lmw-panel-head flex items-center gap-3">
          <div>
            <p className="lmw-label">Fleet registry</p>
            <h1 className="font-display text-2xl font-semibold">Devices and workloads</h1>
          </div>
          <span className="font-mono text-[11px] text-muted">{nodeList.length} enrolled</span>
        </header>
      </section>
      <div className="grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-5">
        <StatTile label="online / total" value={`${onlineCount}/${nodeList.length}`} tone="ok" />
        <StatTile label="accelerators / mem" value={`${accelCount}`} sub={bytes(accelMem)} tone="info" />
        <StatTile label="degraded / offline" value={`${degraded}`} tone={degraded > 0 ? "warn" : "ok"} />
        <StatTile label="pending approval" value={`${pendingNodes.length}`} tone={pendingNodes.length ? "warn" : "ok"} />
        <StatTile label="serving endpoints" value={`${activeServing}`} tone={activeServing ? "ok" : "muted"} />
      </div>

      {nodesQ.isError ? (
        <EmptyState
          className="m-0"
          title="Monitoring unavailable"
          detail="Latest node telemetry could not be loaded; showing inventory only."
          onRetry={() => void nodesQ.refetch()}
        />
      ) : null}

      <div className={cn("grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3")}>
        {nodeList.map((n) => (
          <div key={n.id} className="flex flex-col gap-2">
            <NodeCard
              node={n}
              sample={latest.get(n.id)}
              now={now}
              action={
                session && n.status === "pending" ? (
                  <Link
                    to={`/fleet/nodes/${n.id}`}
                    className="control rounded border border-warn/60 bg-warn/10 px-2 py-0.5 font-mono text-[11px] text-warn hover:bg-warn/20"
                  >
                    review →
                  </Link>
                ) : undefined
              }
            />
            <DeploymentRows
              deployments={deployments}
              nodeId={n.id}
              latestServing={latestServing}
              servingError={servingQ.isError}
            />
          </div>
        ))}
      </div>

      {pendingNodes.length > 0 ? (
        <p className="font-mono text-[11px] text-warn">
          {pendingNodes.length} node{pendingNodes.length === 1 ? "" : "s"} awaiting approval — review their
          reported inventory before they join the schedulable fleet.
        </p>
      ) : (
        <p className="font-mono text-[10px] text-faint">live: every 5s</p>
      )}
    </div>
  );
}

function DeploymentRows({
  deployments,
  nodeId,
  latestServing,
  servingError,
}: {
  deployments: Deployment[];
  nodeId: string;
  latestServing: Map<string, ServingTelemetrySample>;
  servingError: boolean;
}) {
  const rows = deploymentsOnNode(deployments, nodeId).slice(0, 3);
  if (rows.length === 0) {
    if (servingError) {
      return <p className="font-mono text-[10px] text-warn">service metrics unavailable</p>;
    }
    return <p className="font-mono text-[10px] text-faint">No managed deployments</p>;
  }
  return (
    <div className="flex flex-col gap-1.5">
      {rows.map((d) => (
        <DeploymentRow key={`${d.deploymentId}-${d.rank}`} d={d} sample={latestServing.get(d.deploymentId)} />
      ))}
      {servingError ? (
        <p className="font-mono text-[10px] text-warn">service metrics unavailable</p>
      ) : null}
    </div>
  );
}
