import { Link, useParams } from "react-router";
import { toast } from "sonner";
import { Button } from "~/components/ui/button";
import { JsonTree } from "~/components/json-viewer";
import { LogPane } from "~/components/log-pane";
import { StatusDot } from "~/components/status-dot";
import { EmptyState } from "~/components/empty-state";
import { useRun, useCancelRun } from "~/lib/queries";
import { runLogsUrl } from "~/lib/api";
import { duration, isRunTerminal, relativeTime } from "~/lib/format";

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section className="lmw-panel">
      <header className="lmw-panel-head">
        <h2 className="lmw-label">{title}</h2>
      </header>
      {children}
    </section>
  );
}

export default function RunDetailRoute() {
  const { id } = useParams();
  const { data: run, isPending, isError, error, refetch } = useRun(id);
  const cancel = useCancelRun();

  if (isPending) {
    return <p className="py-10 text-center font-mono text-xs text-faint">loading run…</p>;
  }
  if (isError) {
    return (
      <EmptyState
        title="Cannot load run"
        detail={error instanceof Error ? error.message : undefined}
        onRetry={() => void refetch()}
      />
    );
  }
  if (!run) return null;

  const terminal = isRunTerminal(run.state);

  return (
    <div className="grid gap-4">
      <div className="lmw-panel">
        <header className="lmw-panel-head">
          <div className="flex items-center gap-2.5">
            <h1 className="lmw-label">run</h1>
            <span className="font-mono text-sm">{run.id}</span>
          </div>
          <div className="flex items-center gap-3">
            <StatusDot state={run.state} pulse={!terminal} />
            <span className="font-mono text-xs text-muted">
              {run.module} · {run.kind}
            </span>
            {run.error_code ? (
              <span className="font-mono text-xs text-fault">{run.error_code}</span>
            ) : null}
          </div>
          <div className="ml-auto flex items-center gap-2">
            {run.deployment_id ? (
              <Link
                to={`/serving/deployments/${run.deployment_id}`}
                className="control rounded border border-hairline px-2.5 py-1 font-mono text-xs text-muted hover:text-foreground"
              >
                view deployment
              </Link>
            ) : null}
            {!terminal ? (
              <Button
                size="sm"
                variant="outline"
                disabled={cancel.isPending}
                onClick={() =>
                  cancel
                    .mutateAsync(run.id)
                    .then(() => toast.success("Cancellation requested"))
                    .catch((e) => toast.error(e instanceof Error ? e.message : "cancel failed"))
                }
              >
                {cancel.isPending ? "cancelling…" : "cancel"}
              </Button>
            ) : null}
          </div>
        </header>

        <div className="grid gap-4 p-3 md:grid-cols-2">
          <div className="grid gap-1 font-mono text-xs">
            <p className="lmw-label mb-1">schedule</p>
            <p>
              <span className="text-muted">created</span>{" "}
              <span>{run.created_at ? relativeTime(run.created_at) : "—"}</span>
            </p>
            <p>
              <span className="text-muted">started</span>{" "}
              <span>{run.started_at ? relativeTime(run.started_at) : "—"}</span>
            </p>
            <p>
              <span className="text-muted">finished</span>{" "}
              <span>{run.finished_at ? relativeTime(run.finished_at) : "—"}</span>
            </p>
            <p>
              <span className="text-muted">duration</span>{" "}
              <span>{duration(run.started_at, run.finished_at)}</span>
            </p>
          </div>
          <div className="grid gap-1 font-mono text-xs">
            <p className="lmw-label mb-1">resources</p>
            {run.resources?.nodes && run.resources.nodes.length > 0 ? (
              <p>
                <span className="text-muted">nodes</span>{" "}
                <span>{run.resources.nodes.map((n) => n.slice(0, 8)).join(", ")}</span>
              </p>
            ) : null}
            {run.resources?.accelerators && run.resources.accelerators.length > 0 ? (
              <p>
                <span className="text-muted">accelerators</span>{" "}
                <span>{run.resources.accelerators.join(", ")}</span>
              </p>
            ) : null}
            {run.resources?.fabrics && run.resources.fabrics.length > 0 ? (
              <p>
                <span className="text-muted">fabrics</span>{" "}
                <span>{run.resources.fabrics.join(", ")}</span>
              </p>
            ) : null}
            {!run.resources ||
            (run.resources.nodes?.length ?? 0) === 0 &&
              (run.resources.accelerators?.length ?? 0) === 0 &&
              (run.resources.fabrics?.length ?? 0) === 0 ? (
              <p className="text-faint">none reported</p>
            ) : null}
            {run.legacy_identity ? (
              <p className="text-faint">
                <span className="text-muted">legacy</span> {run.legacy_identity}
              </p>
            ) : null}
          </div>
        </div>
      </div>

      {run.error_code || run.error_message ? (
        <Section title="error">
          <div className="grid gap-1 p-3 font-mono text-xs">
            {run.error_code ? <p className="text-fault">{run.error_code}</p> : null}
            {run.error_message ? <p className="text-foreground">{run.error_message}</p> : null}
          </div>
        </Section>
      ) : null}

      <div className="grid gap-4 xl:grid-cols-2">
        <Section title="input">
          <div className="max-h-96 overflow-auto p-3">
            {run.input ? (
              <JsonTree value={run.input} />
            ) : (
              <p className="py-4 text-center font-mono text-xs text-faint">no input</p>
            )}
          </div>
        </Section>
        <Section title={`output${terminal ? "" : " (pending)"}`}>
          <div className="max-h-96 overflow-auto p-3">
            {run.output ? (
              <JsonTree value={run.output} />
            ) : (
              <p className="py-4 text-center font-mono text-xs text-faint">
                {terminal ? "no output" : "validated output appears when the run finishes"}
              </p>
            )}
          </div>
        </Section>
      </div>

      <Section title={`logs${terminal ? "" : " (live)"}`}>
        <div className="p-3">
          <LogPane url={runLogsUrl(run.id)} active={!terminal} />
        </div>
      </Section>
    </div>
  );
}
