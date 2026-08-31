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
import { isCurrentDeployment } from "~/lib/telemetry";
import type { Deployment } from "~/lib/api";

function DeploymentTable({ deployments, stopped }: { deployments: Deployment[]; stopped: boolean }) {
  if (deployments.length === 0) {
    return (
      <EmptyState
        className="m-3"
        title={stopped ? "No stopped deployments" : "No active deployments"}
        hint={
          stopped
            ? "Stopped deployment history remains here until you delete it."
            : "Launch an installed recipe to place a model on the fleet."
        }
      />
    );
  }

  return (
    <div className="overflow-x-auto">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Deployment</TableHead>
            <TableHead>Recipe</TableHead>
            <TableHead>Model</TableHead>
            <TableHead>Engine</TableHead>
            <TableHead>Endpoint</TableHead>
            <TableHead>Desired</TableHead>
            <TableHead>Observed</TableHead>
            <TableHead>Created</TableHead>
            <TableHead className="text-right">Action</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {deployments.map((deployment) => (
            <TableRow key={deployment.id}>
              <TableCell>
                <Link
                  to={`/serving/deployments/${deployment.id}`}
                  className="control font-medium hover:text-foreground"
                >
                  {deployment.recipe_name ?? deployment.recipe_digest.slice(0, 12)}
                </Link>
              </TableCell>
              <TableCell>
                <span className="font-mono text-xs text-muted">
                  {shortDigest(deployment.recipe_digest)}
                </span>
              </TableCell>
              <TableCell>{deployment.endpoint?.model || "Not reported"}</TableCell>
              <TableCell>
                {deployment.engine === "vllm"
                  ? "vLLM"
                  : deployment.engine === "sglang"
                    ? "SGLang"
                    : deployment.engine || "Not reported"}
              </TableCell>
              <TableCell className="font-mono text-xs text-primary">
                {deployment.endpoint ? endpointLabel(deployment.endpoint) : "—"}
              </TableCell>
              <TableCell>
                <StatusDot state={deployment.desired_state === "running" ? "online" : "stopped"} />
              </TableCell>
              <TableCell>
                <StatusDot
                  state={deployment.observed_state}
                  pulse={deployment.observed_state === "starting" || deployment.observed_state === "preparing"}
                />
              </TableCell>
              <TableCell className="font-mono text-[11px] text-faint">
                {deployment.created_at ? relativeTime(deployment.created_at) : "—"}
              </TableCell>
              <TableCell className="text-right">
                <Link
                  to={`/serving/deployments/${deployment.id}`}
                  className="control font-mono text-xs font-medium text-primary hover:text-foreground"
                >
                  {stopped ? "Start →" : "Manage →"}
                </Link>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}

export default function DeploymentsRoute() {
  const { data, isPending, isError, error, refetch } = useDeployments();
  const [planOpen, setPlanOpen] = useState(false);
  const deployments = data ?? [];
  const activeDeployments = deployments.filter(isCurrentDeployment);
  const stoppedDeployments = deployments.filter((deployment) => !isCurrentDeployment(deployment));

  const content = (stopped: boolean) => {
    if (isPending) {
      return <p className="px-3 py-8 text-center font-mono text-xs text-faint">loading deployments…</p>;
    }
    if (isError) {
      return stopped ? null : (
        <EmptyState
          className="m-3"
          title="Cannot load deployments"
          detail={error instanceof Error ? error.message : undefined}
          onRetry={() => void refetch()}
        />
      );
    }
    return (
      <DeploymentTable
        deployments={stopped ? stoppedDeployments : activeDeployments}
        stopped={stopped}
      />
    );
  };

  return (
    <div className="grid gap-4">
      <section className="lmw-panel">
        <header className="lmw-panel-head">
          <h1 className="lmw-label">Active deployments</h1>
          <span className="font-mono text-[11px] text-faint">{activeDeployments.length} active</span>
          <Button size="sm" className="ml-auto" onClick={() => setPlanOpen(true)}>
            <Radio aria-hidden /> Launch deployment
          </Button>
        </header>
        {content(false)}
      </section>

      <section className="lmw-panel">
        <header className="lmw-panel-head">
          <h2 className="lmw-label">Stopped deployments</h2>
          <span className="font-mono text-[11px] text-faint">{stoppedDeployments.length} stopped</span>
        </header>
        {content(true)}
      </section>

      <PlanDeploymentDialog open={planOpen} onOpenChange={setPlanOpen} />
    </div>
  );
}
