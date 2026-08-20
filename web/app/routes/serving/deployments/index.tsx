import { useState } from "react";
import { Link } from "react-router";
import { Radio } from "lucide-react";
import { Button } from "~/components/ui/button";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "~/components/ui/table";
import { useDeployments } from "~/lib/queries";
import { StatusDot } from "~/components/status-dot";
import { EmptyState } from "~/components/empty-state";
import { PlanDeploymentDialog } from "~/components/dialogs/plan-deployment-dialog";
import { endpointLabel, relativeTime, shortDigest } from "~/lib/format";

export default function DeploymentsRoute() {
  const { data, isPending, isError, error, refetch } = useDeployments();
  const [planOpen, setPlanOpen] = useState(false);

  return (
    <div className="grid gap-4">
      <div className="lmw-panel">
        <header className="lmw-panel-head">
          <h1 className="lmw-label">deployments</h1>
          <span className="font-mono text-[11px] text-faint">{(data ?? []).length} total</span>
          <Button size="sm" className="ml-auto" onClick={() => setPlanOpen(true)}>
            <Radio aria-hidden /> plan deployment
          </Button>
        </header>

        {isPending ? (
          <p className="px-3 py-8 text-center font-mono text-xs text-faint">loading deployments…</p>
        ) : isError ? (
          <EmptyState
            className="m-3"
            title="Cannot load deployments"
            detail={error instanceof Error ? error.message : undefined}
            onRetry={() => void refetch()}
          />
        ) : (data ?? []).length === 0 ? (
          <EmptyState
            className="m-3"
            title="No deployments"
            hint="Plan a deployment from an installed recipe to place a model on the fleet."
          />
        ) : (
          <div className="overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Deployment</TableHead>
                  <TableHead>Recipe</TableHead>
                  <TableHead>Profile</TableHead>
                  <TableHead>Endpoint</TableHead>
                  <TableHead>Desired</TableHead>
                  <TableHead>Observed</TableHead>
                  <TableHead>Created</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {(data ?? []).map((d) => (
                  <TableRow key={d.id}>
                    <TableCell>
                      <Link
                        to={`/serving/deployments/${d.id}`}
                        className="control font-medium hover:text-foreground"
                      >
                        {d.recipe_name ?? d.recipe_digest.slice(0, 12)}
                      </Link>
                    </TableCell>
                    <TableCell>
                      <Link to={`/library/recipes/${d.recipe_digest}`} className="control font-mono text-xs text-muted hover:text-foreground">
                        {shortDigest(d.recipe_digest)}
                      </Link>
                    </TableCell>
                    <TableCell className="font-mono text-xs text-muted">{d.profile}</TableCell>
                    <TableCell className="font-mono text-xs text-accent">
                      {d.endpoint ? endpointLabel(d.endpoint) : "—"}
                    </TableCell>
                    <TableCell>
                      <StatusDot state={d.desired_state === "running" ? "online" : "stopped"} />
                    </TableCell>
                    <TableCell>
                      <StatusDot state={d.observed_state} pulse={d.observed_state === "starting" || d.observed_state === "preparing"} />
                    </TableCell>
                    <TableCell className="font-mono text-[11px] text-faint">
                      {d.created_at ? relativeTime(d.created_at) : "—"}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        )}
      </div>

      <PlanDeploymentDialog open={planOpen} onOpenChange={setPlanOpen} />
    </div>
  );
}
