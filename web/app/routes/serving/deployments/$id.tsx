import { Link } from "react-router";
import { toast } from "sonner";
import { Power, ShieldCheck } from "lucide-react";
import { Button } from "~/components/ui/button";
import { useTailPathParam } from "~/lib/path-param";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "~/components/ui/table";
import {
  useDeployment,
  useVerifyDeployment,
  useStopDeployment,
  useNodes,
} from "~/lib/queries";
import { StatusDot } from "~/components/status-dot";
import { EmptyState } from "~/components/empty-state";
import { DiagnosticsList } from "~/components/diagnostics-list";
import { LogPane } from "~/components/log-pane";
import { CopyButton } from "~/components/copy-button";
import { ConfirmDialog } from "~/components/dialogs/confirm-dialog";
import { deploymentLogsUrl } from "~/lib/api";
import { endpointUrl, relativeTime, shortId } from "~/lib/format";

function Section({ title, children, className }: { title: string; children: React.ReactNode; className?: string }) {
  return (
    <section className={`lmw-panel ${className ?? ""}`}>
      <header className="lmw-panel-head">
        <h2 className="lmw-label">{title}</h2>
      </header>
      {children}
    </section>
  );
}

const LOG_ACTIVE = new Set(["preparing", "starting", "healthy", "degraded", "stopping"]);

/** Deployment detail: placements, endpoint, live logs, diagnostics, metadata. */
export default function DeploymentDetailRoute() {
  const id = useTailPathParam();
  const { data: d, isPending, isError, error, refetch } = useDeployment(id);
  const verify = useVerifyDeployment();
  const stop = useStopDeployment();
  const { data: nodes } = useNodes();

  if (isPending) {
    return <p className="py-10 text-center font-mono text-xs text-faint">loading deployment…</p>;
  }
  if (isError) {
    return (
      <EmptyState
        title="Cannot load deployment"
        detail={error instanceof Error ? error.message : undefined}
        onRetry={() => void refetch()}
      />
    );
  }
  if (!d) return null;

  const nodeName = (nid: string) => nodes?.find((n) => n.id === nid)?.display_name ?? shortId(nid);
  const url = endpointUrl(d.endpoint);

  return (
    <div className="grid gap-4">
      <div className="lmw-panel">
        <header className="lmw-panel-head">
          <div className="flex items-center gap-2.5">
            <h1 className="lmw-label">deployment</h1>
            <span className="font-mono text-sm text-foreground">
              {d.recipe_name ?? shortId(d.recipe_digest)}
              {d.recipe_version ? `@${d.recipe_version}` : ""}
            </span>
            <span className="font-mono text-[11px] text-muted">profile {d.profile}</span>
          </div>
          <div className="flex items-center gap-3">
            <StatusDot state={d.desired_state === "running" ? "online" : "stopped"} label={d.desired_state} />
            <StatusDot state={d.observed_state} pulse={LOG_ACTIVE.has(d.observed_state)} />
          </div>
          <div className="ml-auto flex items-center gap-2">
            <Button
              size="sm"
              variant="outline"
              disabled={verify.isPending}
              onClick={() =>
                verify
                  .mutateAsync(d.id)
                  .then((r) => toast.success("Verification started", { description: `observed: ${r.observed_state}` }))
                  .catch((e) => toast.error(e instanceof Error ? e.message : "verify failed"))
              }
            >
              <ShieldCheck aria-hidden /> {verify.isPending ? "verifying…" : "verify"}
            </Button>
            {d.desired_state === "running" ? (
              <ConfirmDialog
                title="Stop deployment"
                description={`Stop ${d.recipe_name ?? d.id}? The endpoint becomes unavailable; placements are released after the workload exits.`}
                confirmLabel="stop"
                tone="destructive"
                onConfirm={async () => {
                  try {
                    const r = await stop.mutateAsync(d.id);
                    toast.success("Deployment stopping", { description: `observed: ${r.observed_state}` });
                  } catch (e) {
                    toast.error(e instanceof Error ? e.message : "stop failed");
                    throw e;
                  }
                }}
              >
                <span className="inline-flex items-center gap-1.5">
                  <Power aria-hidden /> stop
                </span>
              </ConfirmDialog>
            ) : null}
          </div>
        </header>

        <div className="grid gap-4 p-3 lg:grid-cols-2">
          <div>
            <p className="lmw-label mb-2">endpoint</p>
            {d.endpoint ? (
              <div className="grid gap-2 font-mono text-xs">
                <div className="flex items-center gap-2 rounded border border-hairline bg-raised/40 px-2 py-1.5">
                  <span className="text-accent">{url}</span>
                  <CopyButton value={url} label="copy url" className="ml-auto" />
                </div>
                {d.endpoint.model ? (
                  <p className="text-muted">
                    model <span className="text-foreground">{d.endpoint.model}</span>
                  </p>
                ) : null}
                {d.model_capabilities ? (
                  <p className="text-muted">
                    capabilities{" "}
                    <span className="text-foreground">
                      {Object.entries(d.model_capabilities)
                        .map(([k, v]) => `${k}=${String(v)}`)
                        .join(" · ")}
                    </span>
                  </p>
                ) : null}
              </div>
            ) : (
              <p className="font-mono text-xs text-faint">no endpoint reported</p>
            )}
          </div>

          <div>
            <p className="lmw-label mb-2">metadata</p>
            <div className="grid gap-1 font-mono text-xs">
              <p>
                <span className="text-muted">recipe</span>{" "}
                <Link to={`/library/recipes/${d.recipe_digest}`} className="control hover:text-foreground">
                  {shortId(d.recipe_digest)}
                </Link>
              </p>
              <p>
                <span className="text-muted">id</span> <span>{d.id}</span>
              </p>
              {d.fabric ? (
                <p>
                  <span className="text-muted">fabric</span>{" "}
                  <Link to={`/fleet/fabrics/${d.fabric}`} className="control hover:text-foreground">
                    {shortId(d.fabric)}
                  </Link>
                </p>
              ) : null}
              {d.run_id ? (
                <p>
                  <span className="text-muted">run</span>{" "}
                  <Link to={`/runs/${d.run_id}`} className="control hover:text-foreground">
                    {shortId(d.run_id)}
                  </Link>
                </p>
              ) : null}
              <p>
                <span className="text-muted">created</span>{" "}
                <span>{d.created_at ? relativeTime(d.created_at) : "—"}</span>
              </p>
            </div>
          </div>
        </div>
      </div>

      <div className="grid gap-4 xl:grid-cols-2">
        <Section title="placements">
          {(d.placements ?? []).length === 0 ? (
            <p className="px-3 py-6 text-center font-mono text-xs text-faint">no placements reported</p>
          ) : (
            <div className="overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Rank</TableHead>
                    <TableHead>Node</TableHead>
                    <TableHead>Accelerator</TableHead>
                    <TableHead>Container</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {(d.placements ?? []).map((p) => (
                    <TableRow key={`${p.node_id}-${p.rank}`}>
                      <TableCell className="font-mono text-xs">{p.rank}</TableCell>
                      <TableCell>
                        <Link to={`/fleet/nodes/${p.node_id}`} className="control hover:text-foreground">
                          {p.node_name ?? nodeName(p.node_id)}
                        </Link>
                      </TableCell>
                      <TableCell className="font-mono text-xs text-muted">
                        {p.accelerator_index ?? "—"}
                      </TableCell>
                      <TableCell className="font-mono text-[11px] text-faint">
                        {p.container ? shortId(p.container) : "—"}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          )}
        </Section>

        <Section title="diagnostics">
          <div className="p-3">
            <DiagnosticsList diagnostics={d.diagnostics} />
          </div>
        </Section>
      </div>

      <Section title={`logs${LOG_ACTIVE.has(d.observed_state) ? " (live)" : ""}`}>
        <div className="p-3">
          <LogPane url={deploymentLogsUrl(d.id)} active={LOG_ACTIVE.has(d.observed_state)} />
        </div>
      </Section>
    </div>
  );
}
