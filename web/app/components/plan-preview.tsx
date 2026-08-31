import { Link } from "react-router";
import { AlertTriangle, ArrowRight, Box, HardDrive, SlidersHorizontal } from "lucide-react";
import { Badge } from "~/components/ui/badge";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "~/components/ui/table";
import type { DeploymentPlan, Fabric, Node } from "~/lib/api";
import { bytes, endpointLabel, shortId } from "~/lib/format";
import { DiagnosticsList } from "~/components/diagnostics-list";

/**
 * Deployment plan preview: placement table, endpoint, artifact transfers,
 * risk chips, conflicts, and diagnostics. `ready` gates creation.
 */
export function PlanPreview({
  plan,
  nodes,
  fabric,
  className,
}: {
  plan: DeploymentPlan;
  nodes: Node[];
  fabric?: Fabric;
  className?: string;
}) {
  const nodeName = (id: string) => nodes.find((n) => n.id === id)?.display_name ?? shortId(id);
  // Nodes whose plan diagnostics flag a read-only cache root get inline
  // repair guidance on the storage card.
  const readonlyRoots: Record<string, true> = {};
  for (const d of plan.diagnostics ?? []) {
    if (d.code === "storage.cache_root_readonly" && d.resource) {
      readonlyRoots[d.resource.replace(/^node:/, "")] = true;
    }
  }
  return (
    <div className={className}>
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <Badge variant={plan.ready ? "default" : "destructive"}>
          {plan.ready ? "plan ready" : "plan blocked"}
        </Badge>
        <span className="font-mono text-xs text-muted">
          {plan.recipe_name}
          {plan.recipe_version ? `@${plan.recipe_version}` : ""} · profile{" "}
          <span className="text-foreground">{plan.profile}</span>
        </span>
        {plan.endpoint ? (
          <span className="font-mono text-xs text-accent">
            endpoint {endpointLabel(plan.endpoint)}
            {plan.endpoint.model ? ` · ${plan.endpoint.model}` : ""}
          </span>
        ) : null}
      </div>

      <Table>
        <TableHeader>
          <TableRow>
            <TableHead className="w-24">role</TableHead>
            <TableHead>node</TableHead>
            <TableHead>fabric wiring</TableHead>
            <TableHead className="w-28">accelerator</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {plan.placements.length === 0 ? (
            <TableRow>
              <TableCell colSpan={4} className="py-6 text-center font-mono text-xs text-faint">
                No placement available
              </TableCell>
            </TableRow>
          ) : (
            plan.placements.map((p) => (
              <TableRow key={p.rank}>
                <TableCell className="font-mono text-xs tnum">{p.rank === 0 ? "Head · API" : `Worker ${p.rank}`}</TableCell>
                <TableCell>
                  <Link to={`/fleet/nodes/${p.node_id}`} className="text-foreground underline-offset-2 hover:underline control">
                    {nodeName(p.node_id)}
                  </Link>
                  <span className="ml-2 font-mono text-[11px] text-muted">{shortId(p.node_id)}</span>
                </TableCell>
                <TableCell className="font-mono text-[11px]">
                  {(() => {
                    const binding = fabric?.bindings.find((item) => item.node_id === p.node_id);
                    return binding ? (
                      <>
                        <span className="block">{binding.interface_name} · {binding.address}</span>
                        <span className="text-muted">{binding.rdma_device ? `${binding.rdma_device} · GID ${binding.gid_index ?? "—"}` : fabric?.transport}</span>
                      </>
                    ) : <span className="text-fault">binding missing</span>;
                  })()}
                </TableCell>
                <TableCell className="font-mono text-xs tnum">
                  {p.accelerator_index}
                  {p.accelerator_uuid ? <span className="text-muted"> · {p.accelerator_uuid.slice(0, 8)}</span> : null}
                </TableCell>
              </TableRow>
            ))
          )}
        </TableBody>
      </Table>

      {fabric ? (
        <div className="mt-3 flex flex-wrap items-center gap-2 rounded border border-hairline bg-raised px-3 py-2 text-xs">
          <span className="lmw-label">cluster fabric</span>
          <Link to={`/fleet/fabrics/${fabric.id}`} className="control font-display font-semibold underline-offset-2 hover:underline">
            {fabric.name}
          </Link>
          <span className="font-mono text-muted">{fabric.transport} · {fabric.state}</span>
        </div>
      ) : plan.placements.length > 1 ? (
        <div className="mt-3 rounded border border-fault/40 bg-fault/5 px-3 py-2 text-xs text-fault">
          No healthy fabric covers this placement. <Link to="/fleet/fabrics" className="underline">Configure fabric wiring</Link>.
        </div>
      ) : null}

      {plan.images && plan.images.length > 0 ? (
        <div className="mt-3">
          <p className="lmw-label mb-1.5">immutable images</p>
          <ul className="grid gap-1 sm:grid-cols-2">
            {plan.images.map((image) => (
              <li key={image.node_id} className="rounded border border-hairline bg-raised px-3 py-2 text-xs">
                <span className="flex items-center gap-1.5 font-display font-semibold">
                  <Box className="h-3.5 w-3.5 text-primary" aria-hidden />
                  {nodeName(image.node_id)}
                  <span className="ml-auto font-mono text-[10px] font-normal text-ok">verify / pull</span>
                </span>
                <p className="mt-1 truncate font-mono text-[10px] text-muted" title={image.reference}>
                  {image.reference}
                </p>
              </li>
            ))}
          </ul>
        </div>
      ) : null}

      {plan.storage && plan.storage.length > 0 ? (
        <div className="mt-3">
          <p className="lmw-label mb-1.5">storage preflight</p>
          <ul className="grid gap-1 sm:grid-cols-2">
            {plan.storage.map((storage) => (
              <li key={storage.node_id} className="rounded border border-hairline bg-raised px-3 py-2 text-xs">
                <span className="flex items-center gap-1.5 font-display font-semibold">
                  <HardDrive className="h-3.5 w-3.5 text-primary" aria-hidden />
                  {nodeName(storage.node_id)}
                  <span className={`ml-auto font-mono text-[10px] font-normal ${storage.sufficient ? "text-ok" : "text-fault"}`}>
                    {!storage.known ? "capacity unavailable" : storage.sufficient ? "capacity ready" : "space required"}
                  </span>
                </span>
                <p className="mt-1 font-mono text-[10px] text-muted">
                  {storage.required_bytes > 0 ? `${bytes(storage.required_bytes)} download` : "artifacts already cached"}
                  {storage.known ? ` · ${bytes(storage.available_bytes ?? 0)} available` : ""}
                </p>
                {storage.cache_root ? <p className="truncate font-mono text-[10px] text-faint" title={storage.cache_root}>{storage.cache_root}</p> : null}
                {readonlyRoots.has(storage.node_id) ? (
                  <p className="mt-1.5 rounded border border-fault/40 bg-fault/5 px-2 py-1.5 text-[11px] text-fault">
                    The node agent cannot write to this cache root. On the node, add
                    <code className="mx-1 font-mono">ReadWritePaths={storage.cache_root}</code>
                    to a <code className="font-mono">local-model-works-agent.service.d</code> drop-in, run
                    <code className="mx-1 font-mono">sudo systemctl daemon-reload && sudo systemctl restart local-model-works-agent</code>
                    , then re-plan. <Link to={`/fleet/nodes/${storage.node_id}`} className="underline">Node details</Link>
                  </p>
                ) : null}
              </li>
            ))}
          </ul>
        </div>
      ) : null}

      {plan.host_preparation && plan.host_preparation.length > 0 ? (
        <div className="mt-3">
          <p className="lmw-label mb-1.5">host memory preparation</p>
          <ul className="grid gap-1 sm:grid-cols-2">
            {plan.host_preparation.map((host) => (
              <li key={host.node_id} className={`rounded border px-3 py-2 text-xs ${
                host.require_swap && (host.swap_total_bytes ?? 0) === 0
                  ? "border-fault/40 bg-fault/5"
                  : "border-warn/35 bg-warn/5"
              }`}>
                <span className="flex items-center gap-1.5 font-display font-semibold">
                  <SlidersHorizontal className="h-3.5 w-3.5 text-warn" aria-hidden />
                  {nodeName(host.node_id)}
                </span>
                <p className="mt-1 text-muted">
                  {host.swappiness_target != null
                    ? `vm.swappiness ${host.swappiness_current} → ${host.swappiness_target}`
                    : `vm.swappiness ${host.swappiness_current}`}
                  {host.require_swap
                    ? (host.swap_total_bytes ?? 0) > 0
                      ? ` · swap enabled (${bytes(host.swap_total_bytes ?? 0)})`
                      : " · swap disabled"
                    : ""}
                  {host.drop_page_cache ? " · drop page cache before model load" : ""}
                </p>
                <p className="mt-1 font-mono text-[10px] text-faint" title={host.helper_image}>
                  bounded privileged helper · immutable image
                </p>
              </li>
            ))}
          </ul>
        </div>
      ) : null}

      {plan.transfers && plan.transfers.length > 0 ? (
        <div className="mt-3">
          <p className="lmw-label mb-1.5">artifact preparation</p>
          <ul className="flex flex-col gap-1">
            {plan.transfers.map((t, i) => (
              <li key={`${t.artifact_id}-${i}`} className="flex flex-wrap items-center gap-2 rounded border border-hairline bg-raised px-3 py-2 font-mono text-xs">
                <span className="max-w-56 truncate text-ink/90" title={t.identity}>{t.identity}</span>
                <span className="inline-flex items-center gap-1 text-muted">
                  {t.source_node === "origin" ? "Hugging Face" : nodeName(t.source_node)}
                  <ArrowRight className="h-3 w-3" aria-hidden />
                  {t.dest_node === "all" ? "selected nodes" : nodeName(t.dest_node)}
                </span>
                <span className="ml-auto text-muted">
                  {t.bytes ? bytes(t.bytes) : "size calculating"} · {t.network ?? "resumable download"}
                </span>
              </li>
            ))}
          </ul>
        </div>
      ) : null}

      {plan.risks && plan.risks.length > 0 ? (
        <div className="mt-3">
          <p className="lmw-label mb-1.5">risks</p>
          <div className="flex flex-wrap gap-1.5">
            {plan.risks.map((r) => (
              <Badge key={r} variant="outline" className="border-primary/40 text-primary">
                <AlertTriangle className="mr-1 h-3 w-3" aria-hidden />
                {r}
              </Badge>
            ))}
          </div>
        </div>
      ) : null}

      {plan.conflicts && plan.conflicts.length > 0 ? (
        <div className="mt-3">
          <p className="lmw-label mb-1.5">conflicts</p>
          <ul className="flex flex-col gap-1">
            {plan.conflicts.map((c) => (
              <li key={c.resource} className="rounded border border-fault/40 bg-fault/5 px-3 py-2 text-xs">
                <span className="text-fault">{c.resource}</span>
                <span className="text-muted"> — occupied by </span>
                {c.deployment_id ? (
                  <Link
                    to={`/serving/deployments/${c.deployment_id}`}
                    className="control font-mono text-foreground underline-offset-2 hover:underline"
                  >
                    {c.occupied_by}
                  </Link>
                ) : (
                  <span className="font-mono text-foreground">{c.occupied_by}</span>
                )}
              </li>
            ))}
          </ul>
        </div>
      ) : null}

      {plan.diagnostics && plan.diagnostics.length > 0 ? (
        <div className="mt-3">
          <p className="lmw-label mb-1.5">diagnostics</p>
          <DiagnosticsList diagnostics={plan.diagnostics} />
        </div>
      ) : null}
    </div>
  );
}
