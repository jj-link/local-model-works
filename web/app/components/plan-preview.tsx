import { Link } from "react-router";
import { AlertTriangle, ArrowRight } from "lucide-react";
import { Badge } from "~/components/ui/badge";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "~/components/ui/table";
import type { DeploymentPlan, Node } from "~/lib/api";
import { bytes, endpointLabel, shortId } from "~/lib/format";
import { DiagnosticsList } from "~/components/diagnostics-list";

/**
 * Deployment plan preview: placement table, endpoint, artifact transfers,
 * risk chips, conflicts, and diagnostics. `ready` gates creation.
 */
export function PlanPreview({
  plan,
  nodes,
  className,
}: {
  plan: DeploymentPlan;
  nodes: Node[];
  className?: string;
}) {
  const nodeName = (id: string) => nodes.find((n) => n.id === id)?.display_name ?? shortId(id);
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
            <TableHead className="w-12">rank</TableHead>
            <TableHead>node</TableHead>
            <TableHead className="w-28">accelerator</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {plan.placements.length === 0 ? (
            <TableRow>
              <TableCell colSpan={3} className="py-6 text-center font-mono text-xs text-faint">
                No placement available
              </TableCell>
            </TableRow>
          ) : (
            plan.placements.map((p) => (
              <TableRow key={p.rank}>
                <TableCell className="font-mono text-xs tnum">{p.rank}</TableCell>
                <TableCell>
                  <Link to={`/fleet/nodes/${p.node_id}`} className="text-foreground underline-offset-2 hover:underline control">
                    {nodeName(p.node_id)}
                  </Link>
                  <span className="ml-2 font-mono text-[11px] text-muted">{shortId(p.node_id)}</span>
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

      {plan.transfers && plan.transfers.length > 0 ? (
        <div className="mt-3">
          <p className="lmw-label mb-1.5">artifact preparation</p>
          <ul className="flex flex-col gap-1">
            {plan.transfers.map((t, i) => (
              <li key={`${t.artifact_id}-${i}`} className="flex flex-wrap items-center gap-2 rounded border border-hairline bg-raised px-3 py-2 font-mono text-xs">
                <span className="max-w-56 truncate text-ink/90" title={t.identity}>{t.identity}</span>
                <span className="inline-flex items-center gap-1 text-muted">
                  {nodeName(t.source_node)}
                  <ArrowRight className="h-3 w-3" aria-hidden />
                  {nodeName(t.dest_node)}
                </span>
                <span className="ml-auto text-muted">
                  {bytes(t.bytes)} · {t.network ?? "network"}
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
