import { Link } from "react-router";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "~/components/ui/table";
import { useDeployments, useNodes } from "~/lib/queries";
import { StatusDot } from "~/components/status-dot";
import { EmptyState } from "~/components/empty-state";
import { relativeTime } from "~/lib/format";
import { useSession } from "~/lib/api/session";
import type { Deployment, Node } from "~/lib/api";

type DeploymentPlacement = NonNullable<Deployment["placements"]>[number];
type FleetRow = {
  node: Node;
  deployment?: Deployment;
  placement?: DeploymentPlacement;
};

export default function NodesRoute() {
  const nodesQuery = useNodes();
  const deploymentsQuery = useDeployments();
  const session = useSession();
  const nodes = nodesQuery.data ?? [];
  const deployments = deploymentsQuery.data ?? [];

  const rows = nodes.flatMap<FleetRow>((node) => {
    const servingRows: FleetRow[] = deployments.flatMap((deployment) =>
      (deployment.placements ?? [])
        .filter((placement) => placement.node_id === node.id)
        .map((placement) => ({ node, deployment, placement })),
    );
    return servingRows.length > 0
      ? servingRows
      : [{ node, deployment: undefined, placement: undefined }];
  });

  const pending = nodesQuery.isPending || deploymentsQuery.isPending;
  const queryError = nodesQuery.error ?? deploymentsQuery.error;

  return (
    <div className="grid gap-4">
      <section className="lmw-panel overflow-hidden">
        <header className="lmw-panel-head flex items-center gap-3">
          <div>
            <p className="lmw-label">Fleet registry</p>
            <h1 className="font-display text-2xl font-semibold">Devices and workloads</h1>
          </div>
          <span className="font-mono text-[11px] text-muted">{nodes.length} enrolled</span>
        </header>

        {pending ? (
          <p className="px-3 py-10 text-center font-mono text-xs text-muted">Loading fleet workloads…</p>
        ) : queryError ? (
          <EmptyState
            className="m-3"
            title="Cannot load fleet workloads"
            detail={queryError instanceof Error ? queryError.message : undefined}
            onRetry={() => {
              void nodesQuery.refetch();
              void deploymentsQuery.refetch();
            }}
          />
        ) : nodes.length === 0 ? (
          <EmptyState
            className="m-3"
            title="No nodes enrolled"
            hint="Create an enrollment token and run the generated lmw-agent install command on each host."
          />
        ) : (
          <div className="overflow-x-auto">
            <Table aria-label="Fleet workloads">
              <TableHeader>
                <TableRow>
                  <TableHead>Device</TableHead>
                  <TableHead>Model</TableHead>
                  <TableHead>Recipe</TableHead>
                  <TableHead>Engine</TableHead>
                  <TableHead>Workload</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map(({ node, deployment, placement }) => {
                  const accelerators = node.inventory?.accelerators ?? [];
                  const engine = deployment?.engine === "vllm"
                    ? "vLLM"
                    : deployment?.engine === "sglang"
                      ? "SGLang"
                      : deployment?.engine || "Not reported";
                  return (
                    <TableRow key={`${node.id}-${deployment?.id ?? "idle"}-${placement?.rank ?? "idle"}`}>
                      <TableCell className="min-w-64">
                        <div className="flex items-start gap-2">
                          <StatusDot state={node.status} pulse={node.status === "online"} />
                          <div className="min-w-0">
                            <Link to={`/fleet/nodes/${node.id}`} className="lmw-link font-medium">
                              {node.display_name}
                            </Link>
                            <p className="mt-0.5 font-mono text-[10px] text-muted">
                              {accelerators.length > 0
                                ? `${accelerators.length}× ${accelerators[0].name}`
                                : "No accelerator reported"}
                              {placement ? ` · rank ${placement.rank}` : ""}
                            </p>
                            <p className="font-mono text-[10px] text-muted">
                              {node.status} · last seen {relativeTime(node.last_heartbeat)}
                            </p>
                            {node.status === "pending" && session ? (
                              <Link
                                to={`/fleet/nodes/${node.id}`}
                                className="control mt-1 inline-flex rounded border border-warn/50 bg-warn/10 px-2 py-0.5 font-mono text-[10px] text-warn hover:bg-warn/20"
                              >
                                Review
                              </Link>
                            ) : null}
                          </div>
                        </div>
                      </TableCell>
                      <TableCell>{deployment?.endpoint?.model || "Not reported"}</TableCell>
                      <TableCell>
                        {deployment ? (
                          <Link to={`/library/recipes/${deployment.recipe_digest}`} className="lmw-link">
                            {deployment.recipe_name || deployment.recipe_digest}
                          </Link>
                        ) : (
                          "Not reported"
                        )}
                      </TableCell>
                      <TableCell>{deployment ? engine : "Not reported"}</TableCell>
                      <TableCell>
                        <span className={deployment ? "font-medium text-foreground" : "text-muted"}>
                          {deployment?.observed_state ?? "idle"}
                        </span>
                      </TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          </div>
        )}
      </section>
    </div>
  );
}
