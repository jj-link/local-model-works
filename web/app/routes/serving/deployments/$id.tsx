import { useState } from "react";
import { Link, useNavigate } from "react-router";
import { toast } from "sonner";
import { MessageSquare, Play, Power, RotateCcw, ShieldCheck, Trash2 } from "lucide-react";
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
  useStartDeployment,
  useDeleteDeployment,
  useNodes,
  useRun,
} from "~/lib/queries";
import { StatusDot } from "~/components/status-dot";
import { EmptyState } from "~/components/empty-state";
import { DiagnosticsList } from "~/components/diagnostics-list";
import { LogPane } from "~/components/log-pane";
import { CopyButton } from "~/components/copy-button";
import { ConfirmDialog } from "~/components/dialogs/confirm-dialog";
import { deploymentLogsUrl } from "~/lib/api";
import { bytes, endpointUrl, relativeTime, shortId } from "~/lib/format";

function Section({ title, children, className }: { title: string; children: React.ReactNode; className?: string }) {
  return (
    <section className={`lmw-panel min-w-0 ${className ?? ""}`}>
      <header className="lmw-panel-head">
        <h2 className="lmw-label">{title}</h2>
      </header>
      {children}
    </section>
  );
}

const LOG_ACTIVE = new Set(["preparing", "starting", "healthy", "degraded", "stopping"]);

type RankProgress = {
  rank: number;
  role?: string;
  node_name?: string;
  phase?: string;
  artifact?: string;
  current_file?: string;
  bytes_done?: number;
  bytes_total?: number;
  files_done?: number;
  files_total?: number;
  message?: string;
};

const PHASE_LABELS: Record<string, string> = {
  queued: "Queued",
  recipe_package: "Installing recipe",
  artifact_queued: "Preparing download",
  metadata: "Reading artifact manifest",
  downloading: "Downloading",
  validating: "Verifying snapshot",
  complete: "Artifact ready",
  artifacts_ready: "Artifacts ready",
  pulling_image: "Pulling runtime image",
  creating_container: "Creating container",
  preparing_host: "Preparing host memory",
  starting_container: "Starting runtime",
  health_check: "Waiting for health",
  waiting_for_node: "Waiting for node",
  waiting_for_workers: "Waiting for workers",
  healthy: "Healthy",
  failed: "Failed",
  stopped: "Stopped",
};

function rankProgress(value: unknown): RankProgress[] {
  if (!value || typeof value !== "object") return [];
  const rows = (value as Record<string, unknown>).ranks;
  return Array.isArray(rows) ? rows.filter((row): row is RankProgress => Boolean(row && typeof row === "object")) : [];
}

/** Deployment detail: placements, endpoint, live logs, diagnostics, metadata. */
export default function DeploymentDetailRoute() {
  const id = useTailPathParam();
  const navigate = useNavigate();
  const { data: d, isPending, isError, error, refetch } = useDeployment(id);
  const { data: run } = useRun(d?.run_id ?? undefined);
  const verify = useVerifyDeployment();
  const stop = useStopDeployment();
  const start = useStartDeployment();
  const del = useDeleteDeployment();
  const { data: nodes } = useNodes();
  const [logRank, setLogRank] = useState<number | undefined>(0);

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
  const fullyStopped = d.desired_state === "stopped" && d.observed_state === "stopped";
  const retryStop = d.desired_state === "stopped" && d.observed_state !== "stopped";
  const runFailed = run?.state === "failed";
  const progressRows = rankProgress(run?.progress);

  return (
    <div className="grid min-w-0 gap-4">
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
          <div className="flex flex-wrap items-center gap-3">
            <div className="flex items-center gap-1.5">
              <span className="lmw-label">desired</span>
              <StatusDot state={d.desired_state === "running" ? "online" : "stopped"} label={d.desired_state} />
            </div>
            <div className="flex items-center gap-1.5">
              <span className="lmw-label">observed</span>
              <StatusDot state={d.observed_state} label={d.observed_state} pulse={LOG_ACTIVE.has(d.observed_state)} />
            </div>
          </div>
          <div className="ml-auto flex flex-wrap items-center gap-2">
            {d.observed_state === "healthy" ? (
              <Button
                size="sm"
                variant="outline"
                disabled={verify.isPending}
                onClick={() =>
                  verify
                    .mutateAsync(d.id)
                    .then((result) => toast.success("Verification started", { description: `observed: ${result.observed_state}` }))
                    .catch((cause) => toast.error(cause instanceof Error ? cause.message : "verify failed"))
                }
              >
                <ShieldCheck aria-hidden /> {verify.isPending ? "Verifying…" : "Verify"}
              </Button>
            ) : null}
            {fullyStopped ? (
              <ConfirmDialog
                title={runFailed ? "Restart deployment" : "Start deployment"}
                description={`${runFailed ? "Restart" : "Start"} ${d.recipe_name ?? d.id}? Leases are re-acquired and the workload is dispatched as a new run.`}
                confirmLabel={runFailed ? "restart" : "start"}
                onConfirm={async () => {
                  try {
                    const result = await start.mutateAsync(d.id);
                    toast.success(runFailed ? "Deployment restarting" : "Deployment starting", {
                      description: `observed: ${result.observed_state}`,
                    });
                  } catch (cause) {
                    toast.error(cause instanceof Error ? cause.message : "start failed");
                    throw cause;
                  }
                }}
              >
                <span className="inline-flex items-center gap-1.5">
                  {runFailed ? <RotateCcw aria-hidden /> : <Play aria-hidden />}
                  {runFailed ? "Restart" : "Start"}
                </span>
              </ConfirmDialog>
            ) : null}
            {d.desired_state === "running" || retryStop ? (
              <ConfirmDialog
                title={retryStop ? "Retry stop" : "Stop deployment"}
                description={`${retryStop ? "Retry stopping" : "Stop"} ${d.recipe_name ?? d.id}? Active artifact downloads are canceled; completed and partial cache data remain available for resume. Placements remain reserved until every workload rank is confirmed down.`}
                confirmLabel={retryStop ? "retry stop" : "stop"}
                tone="destructive"
                onConfirm={async () => {
                  try {
                    const result = await stop.mutateAsync(d.id);
                    toast.success(retryStop ? "Stop retried" : "Deployment stopping", {
                      description: `observed: ${result.observed_state}`,
                    });
                  } catch (cause) {
                    toast.error(cause instanceof Error ? cause.message : "stop failed");
                    throw cause;
                  }
                }}
              >
                <span className="inline-flex items-center gap-1.5">
                  <Power aria-hidden /> {retryStop ? "Retry stop" : "Stop"}
                </span>
              </ConfirmDialog>
            ) : null}
            {fullyStopped ? (
              <ConfirmDialog
                title="Delete stopped deployment"
                description={`Delete ${d.recipe_name ?? d.id}? This removes the deployment and its run records. Verified model and image caches remain available for future launches.`}
                confirmLabel="delete deployment"
                tone="destructive"
                onConfirm={async () => {
                  try {
                    await del.mutateAsync(d.id);
                    toast.success("Deployment deleted");
                    navigate("/serving/deployments");
                  } catch (cause) {
                    toast.error(cause instanceof Error ? cause.message : "delete failed");
                    throw cause;
                  }
                }}
              >
                <span className="inline-flex items-center gap-1.5">
                  <Trash2 aria-hidden /> Delete
                </span>
              </ConfirmDialog>
            ) : null}
          </div>
        </header>

        <div className="grid gap-4 p-3 lg:grid-cols-2">
          <div className="min-w-0">
            <p className="lmw-label mb-2">endpoint</p>
            {d.endpoint ? (
              <div className="grid gap-2 font-mono text-xs">
                <div className="flex min-w-0 items-center gap-2 rounded border border-hairline bg-raised/40 px-2 py-1.5">
                  <span className="min-w-0 truncate text-accent" title={url}>{url}</span>
                  <CopyButton value={url} label="copy url" className="ml-auto" />
                  {d.observed_state === "healthy" ? (
                    <Button size="sm" variant="outline" onClick={() => navigate(`/chat?deployment=${d.id}`)}>
                      <MessageSquare aria-hidden /> Open Chat
                    </Button>
                  ) : null}
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

      {progressRows.length > 0 ? (
        <Section title="launch progress">
          <div className="grid gap-3 p-3 md:grid-cols-2">
            {progressRows.sort((left, right) => left.rank - right.rank).map((progress) => {
              const total = Number(progress.bytes_total ?? 0);
              const done = Number(progress.bytes_done ?? 0);
              const percent = total > 0 ? Math.min(100, Math.round((done / total) * 100)) : 0;
              return (
                <article key={progress.rank} className="overflow-hidden rounded border border-hairline">
                  <header className="flex items-center gap-2 border-b border-hairline bg-raised px-3 py-2">
                    <span className="rounded border border-primary/40 px-1.5 py-0.5 font-mono text-[10px] text-primary">R{progress.rank}</span>
                    <span className="font-display text-sm font-semibold">{progress.rank === 0 ? "Head · API" : `Worker ${progress.rank}`}</span>
                    <span className="ml-auto font-mono text-[11px] text-muted">{progress.node_name}</span>
                  </header>
                  <div className="grid gap-2 p-3">
                    <div className="flex items-center justify-between gap-3">
                      <span className="text-sm">{PHASE_LABELS[progress.phase ?? ""] ?? progress.phase ?? "Preparing"}</span>
                      {total > 0 ? <span className="font-mono text-xs tnum">{percent}%</span> : null}
                    </div>
                    {total > 0 ? (
                      <>
                        <div className="h-1.5 overflow-hidden rounded-full bg-hairline" role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={percent}>
                          <div className="h-full rounded-full bg-primary transition-[width] duration-500" style={{ width: `${percent}%` }} />
                        </div>
                        <p className="font-mono text-[11px] text-muted">
                          {bytes(done)} / {bytes(total)}
                          {progress.files_total ? ` · ${progress.files_done ?? 0}/${progress.files_total} files` : ""}
                        </p>
                      </>
                    ) : null}
                    {progress.artifact ? <p className="truncate font-mono text-[11px]" title={progress.artifact}>{progress.artifact}</p> : null}
                    {progress.current_file ? <p className="truncate font-mono text-[11px] text-muted" title={progress.current_file}>{progress.current_file}</p> : null}
                    {progress.message ? <p className={`text-xs ${progress.phase === "failed" ? "text-fault" : "text-muted"}`}>{progress.message}</p> : null}
                  </div>
                </article>
              );
            })}
          </div>
        </Section>
      ) : null}

      {runFailed && d.run_id ? (
        <Section title="Last failure" className="border-fault/50">
          <div className="grid gap-2 p-3 font-mono text-xs">
            <p>
              <span className="text-muted">error code</span>{" "}
              <span className="text-fault">{run?.error_code ?? "workload.failed"}</span>
            </p>
            <p className="max-w-4xl whitespace-pre-wrap text-foreground">
              {run?.error_message ?? "The serving workload stopped unexpectedly."}
            </p>
            <Link
              to={`/runs/${d.run_id}`}
              className="control w-fit font-medium text-primary underline-offset-2 hover:underline"
            >
              View crash logs
            </Link>
          </div>
        </Section>
      ) : null}

      <div className="grid gap-4 xl:grid-cols-2">
        <Section title="placements">
          {(d.placements ?? []).length === 0 ? (
            <p className="px-3 py-6 text-center font-mono text-xs text-faint">no placements reported</p>
          ) : (
            <div className="overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Role</TableHead>
                    <TableHead>Node</TableHead>
                    <TableHead>Accelerator</TableHead>
                    <TableHead>Container</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {(d.placements ?? []).map((p) => (
                    <TableRow key={`${p.node_id}-${p.rank}`}>
                      <TableCell className="font-mono text-xs">{p.rank === 0 ? "Head · API" : `Worker ${p.rank}`}</TableCell>
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
        <div className="border-b border-hairline px-3 py-2">
          <div className="flex flex-wrap gap-1" role="group" aria-label="Log rank">
            <Button size="sm" variant={logRank === undefined ? "secondary" : "ghost"} onClick={() => setLogRank(undefined)}>All ranks</Button>
            {(d.placements ?? []).map((placement) => (
              <Button
                key={placement.rank}
                size="sm"
                variant={logRank === placement.rank ? "secondary" : "ghost"}
                onClick={() => setLogRank(placement.rank)}
              >
                {placement.rank === 0 ? "Head · R0" : `Worker · R${placement.rank}`}
              </Button>
            ))}
          </div>
        </div>
        <div className="min-w-0 p-3">
          <LogPane key={d.run_id ?? "none"} url={deploymentLogsUrl(d.id, logRank)} active={LOG_ACTIVE.has(d.observed_state)} />
        </div>
      </Section>
    </div>
  );
}
