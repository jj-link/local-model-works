import { Link } from "react-router";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "~/components/ui/table";
import { useDeployments, useNodes, useRecipes, useRuns } from "~/lib/queries";
import { StatTile } from "~/components/stat-tile";
import { StatusDot } from "~/components/status-dot";
import { EmptyState } from "~/components/empty-state";
import { bytes, relativeTime } from "~/lib/format";
import type { RunState } from "~/lib/api";
import { isCurrentDeployment } from "~/lib/telemetry";


const ACTIVE_RUN_STATES: RunState[] = [
  "queued",
  "planning",
  "waiting",
  "running",
  "verifying",
  "cancelling",
];

export default function DashboardRoute() {
  const nodesQ = useNodes();
  const deploymentsQ = useDeployments();
  const recipesQ = useRecipes();
  const runsQ = useRuns();

  const nodes = nodesQ.data ?? [];
  const deployments = deploymentsQ.data ?? [];
  const activeDeployments = deployments.filter(isCurrentDeployment);
  const recipes = recipesQ.data ?? [];
  const runs = runsQ.data?.items ?? [];

  const nodesOnline = nodes.filter((n) => n.status === "online").length;
  const depHealthy = activeDeployments.filter((d) => d.observed_state === "healthy").length;
  const activeRuns = runs.filter((r) => ACTIVE_RUN_STATES.includes(r.state)).length;
  const trusted = recipes.filter((r) => r.trust_state !== "untrusted").length;

  const error = nodesQ.error ?? deploymentsQ.error ?? recipesQ.error ?? runsQ.error;

  return (
    <div className="grid gap-4">
      {/* Instrument row */}
      <section className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4" aria-label="Fleet instruments">
        <StatTile
          label="nodes online"
          value={`${nodesOnline}/${nodes.length}`}
          sub={<Link to="/fleet/nodes" className="text-faint hover:text-foreground">fleet →</Link>}
          tone={nodes.length === 0 ? "muted" : nodesOnline === nodes.length ? "ok" : nodesOnline === 0 ? "fault" : "warn"}
        />
        <StatTile
          label="deployments healthy"
          value={`${depHealthy}/${activeDeployments.length}`}
          sub={<Link to="/serving/deployments" className="text-faint hover:text-foreground">serving →</Link>}
          tone={activeDeployments.length === 0 ? "muted" : depHealthy === activeDeployments.length ? "ok" : "warn"}
        />
        <StatTile
          label="active runs"
          value={String(activeRuns)}
          sub={<Link to="/runs" className="text-faint hover:text-foreground">runs →</Link>}
          tone={activeRuns > 0 ? "info" : "muted"}
        />
        <StatTile
          label="recipes launchable"
          value={`${trusted}/${recipes.length}`}
          sub={<Link to="/library/recipes" className="text-faint hover:text-foreground">library →</Link>}
          tone={recipes.length === 0 ? "muted" : "info"}
        />
      </section>

      {error ? (
        <EmptyState
          title="Cannot reach the control plane"
          hint="The API is not responding. Check that lmw-server is running and the dev proxy target is correct."
          detail={error instanceof Error ? error.message : undefined}
          onRetry={() => {
            void nodesQ.refetch();
            void deploymentsQ.refetch();
            void recipesQ.refetch();
            void runsQ.refetch();
          }}
        />
      ) : null}

      <section className="grid gap-4 xl:grid-cols-3">
        {/* Active deployments */}
        <div className="lmw-panel xl:col-span-2 min-h-64">
          <header className="lmw-panel-head">
            <h2 className="lmw-label">active deployments</h2>
          </header>
          {activeDeployments.length === 0 ? (
            <EmptyState
              className="m-3"
              title="No active deployments"
              hint="Plan a deployment from an installed recipe to put a model on the fleet."
            />
          ) : (
            <div className="overflow-x-auto">
              <Table aria-label="Active deployments">
                <TableHeader>
                  <TableRow>
                    <TableHead>Deployment</TableHead>
                    <TableHead>Model</TableHead>
                    <TableHead>Endpoint</TableHead>
                    <TableHead>Observed</TableHead>
                    <TableHead>Desired</TableHead>
                    <TableHead>Updated</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {activeDeployments.map((d) => (
                    <TableRow key={d.id}>
                      <TableCell>
                        <Link
                          to={`/serving/deployments/${d.id}`}
                          className="font-mono text-xs text-foreground hover:text-primary"
                        >
                          {d.recipe_name}@{d.profile}
                        </Link>
                      </TableCell>
                      <TableCell className="font-mono text-xs text-muted">
                        {d.endpoint?.model ?? <span className="text-faint">—</span>}
                      </TableCell>
                      <TableCell className="font-mono text-xs text-muted">
                        {d.endpoint ? `${d.endpoint.host ?? "0.0.0.0"}:${d.endpoint.port}` : <span className="text-faint">—</span>}
                      </TableCell>
                      <TableCell>
                        <StatusDot state={d.observed_state} />
                      </TableCell>
                      <TableCell>
                        <StatusDot state={d.desired_state === "running" ? "healthy" : "stopped"} label={d.desired_state} />
                      </TableCell>
                      <TableCell className="font-mono text-xs text-muted">
                        {relativeTime(d.updated_at)}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          )}
        </div>

        {/* Fleet */}
        <div className="lmw-panel min-h-64">
          <header className="lmw-panel-head">
            <h2 className="lmw-label">fleet</h2>
          </header>
          {nodes.length === 0 ? (
            <EmptyState
              className="m-3"
              title="No nodes enrolled"
              hint="Enroll a node agent to bring hardware into the fleet."
            />
          ) : (
            <ul>
              {nodes.map((n) => {
                const accels = n.inventory?.accelerators ?? [];
                const memTotal = accels.reduce((a, g) => a + (g.memory_bytes ?? 0), 0);
                return (
                  <li key={n.id} className="border-b border-hairline/60 px-3 py-2.5 last:border-b-0">
                    <div className="flex items-center gap-2">
                      <StatusDot state={n.status} pulse={n.status === "online"} />
                      <Link
                        to={`/fleet/nodes/${n.id}`}
                        className="text-sm font-medium text-foreground hover:text-primary"
                      >
                        {n.display_name}
                      </Link>
                      <span className="ml-auto font-mono text-[11px] text-faint">
                        {relativeTime(n.last_heartbeat)}
                      </span>
                    </div>
                    <div className="mt-1.5 flex items-center gap-3 pl-5 font-mono text-[11px] text-muted">
                      <span>
                        {accels.length > 0
                          ? `${accels.length}× ${accels[0].vendor ?? "?"} ${accels[0].name ?? ""}`
                          : "no accelerators"}
                      </span>
                      {accels.length > 0 ? (
                        <span className="tabular-nums">{bytes(memTotal)} vram</span>
                      ) : null}
                    </div>
                  </li>
                );
              })}
            </ul>
          )}
        </div>
      </section>

    </div>
  );
}
