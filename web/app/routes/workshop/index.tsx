import { Link } from "react-router";
import { Cpu, Network, Radio, Server } from "lucide-react";
import { EmptyState } from "~/components/empty-state";
import { StatTile } from "~/components/stat-tile";
import { StatusDot } from "~/components/status-dot";
import { useDeployments, useFabrics, useNodes } from "~/lib/queries";
import { bytes, shortId } from "~/lib/format";

export default function WorkshopRoute() {
  const nodesQ = useNodes();
  const fabricsQ = useFabrics();
  const deploymentsQ = useDeployments();
  const nodes = nodesQ.data ?? [];
  const fabrics = fabricsQ.data ?? [];
  const deployments = deploymentsQ.data ?? [];
  const accelerators = nodes.flatMap((node) => node.inventory?.accelerators ?? []);
  const memory = accelerators.reduce((total, accelerator) => total + (accelerator.memory_bytes ?? 0), 0);
  const liveEndpoints = deployments.filter((deployment) => deployment.observed_state === "healthy" && deployment.endpoint).length;
  const readyFabrics = fabrics.filter((fabric) => fabric.state === "ok").length;
  const error = nodesQ.error ?? fabricsQ.error ?? deploymentsQ.error;

  return (
    <main className="grid gap-4" aria-labelledby="workshop-title">
      <header className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <p className="lmw-label text-info">live systems bench</p>
          <h1 id="workshop-title" className="mt-1 font-display text-3xl font-semibold tracking-tight">Workshop</h1>
          <p className="mt-1 max-w-2xl text-sm text-muted">Physical topology, accelerator inventory, and serving endpoints in one operational surface.</p>
        </div>
        <span className="font-mono text-[11px] uppercase tracking-wider text-faint">controller view · live inventory</span>
      </header>

      <section className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4" aria-label="Workshop instruments">
        <StatTile label="nodes online" value={`${nodes.filter((node) => node.status === "online").length}/${nodes.length}`} tone={nodes.some((node) => node.status !== "online") ? "warn" : nodes.length ? "ok" : "muted"} sub={<Server className="size-3" />} />
        <StatTile label="accelerators" value={String(accelerators.length)} tone={accelerators.length ? "info" : "muted"} sub={<Cpu className="size-3" />} />
        <StatTile label="aggregate vram" value={bytes(memory)} tone={memory ? "info" : "muted"} sub="reported inventory" />
        <StatTile label="live endpoints" value={String(liveEndpoints)} tone={liveEndpoints ? "ok" : "muted"} sub={`${readyFabrics}/${fabrics.length} fabrics ready`} />
      </section>

      {error ? <EmptyState title="Workshop instruments unavailable" detail={error instanceof Error ? error.message : undefined} onRetry={() => { void nodesQ.refetch(); void fabricsQ.refetch(); void deploymentsQ.refetch(); }} /> : null}

      <section className="grid gap-4 xl:grid-cols-[minmax(0,1.6fr)_minmax(20rem,1fr)]">
        <div className="lmw-panel min-h-[30rem] overflow-hidden">
          <header className="lmw-panel-head"><Network className="size-3.5 text-info" /><h2 className="lmw-label">physical topology</h2><span className="ml-auto font-mono text-[11px] text-faint">{fabrics.length} fabrics</span></header>
          {nodes.length === 0 ? <EmptyState className="m-3" title="No topology to render" hint="Enroll a node agent, then connect nodes with a fabric." /> : (
            <div className="relative grid min-h-[27rem] place-items-center overflow-auto bg-[radial-gradient(circle_at_center,rgba(77,107,69,0.08),transparent_58%)] p-6">
              <svg className="pointer-events-none absolute inset-0 h-full w-full" aria-hidden>
                {fabrics.flatMap((fabric, fabricIndex) => fabric.members.slice(1).map((member, memberIndex) => {
                  const from = nodes.findIndex((node) => node.id === fabric.members[memberIndex]);
                  const to = nodes.findIndex((node) => node.id === member);
                  if (from < 0 || to < 0) return null;
                  const x1 = 15 + ((from * 29) % 70); const y1 = 20 + ((from * 37) % 65);
                  const x2 = 15 + ((to * 29) % 70); const y2 = 20 + ((to * 37) % 65);
                  return <line key={`${fabric.id}-${member}`} x1={`${x1}%`} y1={`${y1}%`} x2={`${x2}%`} y2={`${y2}%`} stroke={fabric.state === "ok" ? "rgb(77 107 69 / .65)" : "rgb(170 115 39 / .6)"} strokeWidth="2" strokeDasharray={fabricIndex % 2 ? "5 5" : undefined} />;
                }))}
              </svg>
              <div className="relative h-[25rem] min-w-[42rem] w-full">
                {nodes.map((node, index) => {
                  const nodeAccelerators = node.inventory?.accelerators ?? [];
                  const left = 8 + ((index * 29) % 76); const top = 7 + ((index * 37) % 72);
                  return <Link key={node.id} to={`/fleet/nodes/${node.id}`} className="absolute w-44 -translate-x-1/2 -translate-y-1/2 border border-hairline bg-surface/95 p-3 shadow-lg backdrop-blur hover:border-primary" style={{ left: `${left}%`, top: `${top}%` }}>
                    <div className="flex items-center gap-2"><StatusDot state={node.status} pulse={node.status === "online"} /><strong className="truncate text-sm">{node.display_name}</strong></div>
                    <div className="mt-2 grid grid-cols-2 gap-1 font-mono text-[10px] text-muted"><span>{nodeAccelerators.length} GPU</span><span>{node.inventory?.arch ?? "unknown"}</span><span className="col-span-2 truncate">{nodeAccelerators[0]?.name ?? "no accelerator"}</span></div>
                  </Link>;
                })}
              </div>
            </div>
          )}
        </div>

        <div className="grid content-start gap-4">
          <div className="lmw-panel">
            <header className="lmw-panel-head"><Radio className="size-3.5 text-success" /><h2 className="lmw-label">serving instruments</h2></header>
            {deployments.length === 0 ? <p className="p-4 font-mono text-xs text-faint">no deployments</p> : <ul>{deployments.map((deployment) => <li key={deployment.id} className="border-b border-hairline/60 p-3 last:border-0"><div className="flex items-center gap-2"><StatusDot state={deployment.observed_state} /><Link className="truncate text-sm font-medium hover:text-primary" to={`/serving/deployments/${deployment.id}`}>{deployment.recipe_name}@{deployment.profile}</Link></div><div className="mt-1.5 flex justify-between font-mono text-[10px] text-muted"><span>{deployment.endpoint?.model ?? shortId(deployment.id)}</span><span>{deployment.endpoint ? `${deployment.endpoint.host ?? "0.0.0.0"}:${deployment.endpoint.port}` : "no endpoint"}</span></div></li>)}</ul>}
          </div>
          <div className="lmw-panel">
            <header className="lmw-panel-head"><h2 className="lmw-label">fabric matrix</h2></header>
            {fabrics.length === 0 ? <p className="p-4 font-mono text-xs text-faint">no fabrics</p> : <ul>{fabrics.map((fabric) => <li key={fabric.id} className="flex items-center gap-3 border-b border-hairline/60 p-3 last:border-0"><StatusDot state={fabric.state} /><Link to={`/fleet/fabrics/${fabric.id}`} className="text-sm font-medium hover:text-primary">{fabric.name}</Link><span className="ml-auto font-mono text-[10px] text-muted">{fabric.transport} · {fabric.members.length} nodes</span></li>)}</ul>}
          </div>
        </div>
      </section>
    </main>
  );
}
