import { useMemo, useState } from "react";
import { Link } from "react-router";
import { toast } from "sonner";
import { Button } from "~/components/ui/button";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "~/components/ui/select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "~/components/ui/table";
import { useRuns, useCancelRun, useModules } from "~/lib/queries";
import { StatusDot } from "~/components/status-dot";
import { EmptyState } from "~/components/empty-state";
import { duration, isRunTerminal, relativeTime, shortId } from "~/lib/format";

const RUN_STATES = [
  "queued",
  "planning",
  "waiting",
  "running",
  "verifying",
  "succeeded",
  "failed",
  "cancelling",
  "cancelled",
  "interrupted",
] as const;

export default function RunsRoute() {
  const [module, setModule] = useState("all");
  const [state, setState] = useState("all");
  const { data: modules } = useModules();
  const { data, isPending, isError, error, refetch } = useRuns({
    module: module === "all" ? undefined : module,
    state: state === "all" ? undefined : state,
  });
  const cancel = useCancelRun();
  const items = useMemo(() => data?.items ?? [], [data]);
  const active = useMemo(
    () => items.filter((r) => !isRunTerminal(r.state)).length,
    [items],
  );

  return (
    <div className="grid gap-4">
      <div className="lmw-panel">
        <header className="lmw-panel-head">
          <h1 className="lmw-label">runs</h1>
          <span className="font-mono text-[11px] text-faint">
            {items.length} shown · {active} active
          </span>
          <Select value={module} onValueChange={setModule}>
            <SelectTrigger className="h-7 w-36 font-mono text-xs" aria-label="Filter by module">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">all modules</SelectItem>
              {(modules ?? []).map((m) => (
                <SelectItem key={m.id} value={m.id}>
                  {m.id}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <Select value={state} onValueChange={setState}>
            <SelectTrigger className="h-7 w-32 font-mono text-xs" aria-label="Filter by state">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">all states</SelectItem>
              {RUN_STATES.map((s) => (
                <SelectItem key={s} value={s}>
                  {s}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </header>

        {isPending ? (
          <p className="px-3 py-8 text-center font-mono text-xs text-faint">loading runs…</p>
        ) : isError ? (
          <EmptyState
            className="m-3"
            title="Cannot load runs"
            detail={error instanceof Error ? error.message : undefined}
            onRetry={() => void refetch()}
          />
        ) : items.length === 0 ? (
          <EmptyState
            className="m-3"
            title="No runs"
            hint="Every long action — deployments, benchmarks, transfers, migrations — produces a run with a typed input, validated output, and a resumable log."
          />
        ) : (
          <div className="overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Run</TableHead>
                  <TableHead>Module</TableHead>
                  <TableHead>Kind</TableHead>
                  <TableHead>State</TableHead>
                  <TableHead>Started</TableHead>
                  <TableHead className="text-right">Duration</TableHead>
                  <TableHead>Deployment</TableHead>
                  <TableHead />
                </TableRow>
              </TableHeader>
              <TableBody>
                {items.map((r) => (
                  <TableRow key={r.id}>
                    <TableCell>
                      <Link to={`/runs/${r.id}`} className="control font-mono text-xs hover:text-foreground">
                        {shortId(r.id)}
                      </Link>
                    </TableCell>
                    <TableCell className="font-mono text-xs text-muted">{r.module}</TableCell>
                    <TableCell className="font-mono text-xs text-faint">{r.kind}</TableCell>
                    <TableCell>
                      <StatusDot state={r.state} pulse={!isRunTerminal(r.state)} />
                    </TableCell>
                    <TableCell className="font-mono text-[11px] text-muted">
                      {r.started_at ? relativeTime(r.started_at) : relativeTime(r.created_at)}
                    </TableCell>
                    <TableCell className="text-right font-mono text-xs tnum text-muted">
                      {duration(r.started_at, r.finished_at)}
                    </TableCell>
                    <TableCell>
                      {r.deployment_id ? (
                        <Link
                          to={`/serving/deployments/${r.deployment_id}`}
                          className="control font-mono text-xs text-muted hover:text-foreground"
                        >
                          {shortId(r.deployment_id)}
                        </Link>
                      ) : (
                        <span className="text-faint">—</span>
                      )}
                    </TableCell>
                    <TableCell>
                      {!isRunTerminal(r.state) ? (
                        <Button
                          size="sm"
                          variant="outline"
                          aria-label={`Cancel run ${shortId(r.id)}`}
                          disabled={cancel.isPending}
                          onClick={() =>
                            cancel
                              .mutateAsync(r.id)
                              .then(() => toast.success("Cancellation requested"))
                              .catch((e) => toast.error(e instanceof Error ? e.message : "cancel failed"))
                          }
                        >
                          cancel
                        </Button>
                      ) : null}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        )}
      </div>
    </div>
  );
}
