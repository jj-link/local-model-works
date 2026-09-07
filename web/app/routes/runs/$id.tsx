import { Link } from "react-router";
import { toast } from "sonner";
import { Button } from "~/components/ui/button";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "~/components/ui/table";
import { JsonTree } from "~/components/json-viewer";
import { useTailPathParam } from "~/lib/path-param";
import { LogPane } from "~/components/log-pane";
import { StatusDot } from "~/components/status-dot";
import { EmptyState } from "~/components/empty-state";
import {
  useBenchmarkRun,
  useBenchmarkTrials,
  useCancelBenchmark,
  useCancelRun,
  useRun,
} from "~/lib/queries";
import { benchmarkBundleUrl, runLogsUrl } from "~/lib/api";
import { duration, isRunTerminal, number, relativeTime } from "~/lib/format";

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

function pct(v: number | null | undefined): string {
  return v == null ? "—" : `${Math.round(v * 100)}%`;
}

function BenchmarkRunSection({ runId }: { runId: string }) {
  const { data: summary, isPending, isError, refetch } = useBenchmarkRun(runId);
  const { data: trials, isPending: trialsPending, isError: trialsError } = useBenchmarkTrials(runId);
  const cancelBenchmark = useCancelBenchmark();

  if (isPending) {
    return (
      <Section title="benchmark summary">
        <p className="p-3 font-mono text-xs text-faint">loading benchmark summary…</p>
      </Section>
    );
  }
  if (isError || !summary) {
    return (
      <Section title="benchmark summary">
        <div className="p-3">
          <EmptyState
            title="Cannot load benchmark summary"
            onRetry={() => void refetch()}
          />
        </div>
      </Section>
    );
  }

  const result = summary.result;

  return (
    <Section title="benchmark summary">
      <div className="grid gap-3 p-3">
        <div className="flex flex-wrap items-center gap-x-4 gap-y-1 font-mono text-xs">
          <span>
            <span className="text-muted">benchmark</span> {result.benchmark_id}@{result.benchmark_version} ({result.harness})
          </span>
          <span>
            <span className="text-muted">tasks</span> {result.task_count}
          </span>
          <span>
            <span className="text-muted">candidates</span> {result.candidate_count}
          </span>
          <span>
            <span className="text-muted">passed</span> {result.passed_count}
          </span>
          <span>
            <span className="text-muted">pass@1</span> {pct(result.pass_at_1)}
          </span>
          {result.oracle_pass_rate != null ? (
            <span>
              <span className="text-muted">oracle</span> {pct(result.oracle_pass_rate)}
            </span>
          ) : null}
          {result.verifier_pass_rate != null ? (
            <span>
              <span className="text-muted">verifier</span> {pct(result.verifier_pass_rate)}
            </span>
          ) : null}
          <span>
            <span className="text-muted">wall</span> {number(result.wall_seconds, 1)}s
          </span>
          {result.language_results.length > 0 ? (
            <span>
              <span className="text-muted">tokens</span> {number(result.total_tokens)}
            </span>
          ) : null}
        </div>

        {result.language_results.length > 0 ? (
          <div className="overflow-x-auto">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>language</TableHead>
                  <TableHead className="text-right">ok</TableHead>
                  <TableHead className="text-right">tok/s</TableHead>
                  <TableHead className="text-right">p50</TableHead>
                  <TableHead className="text-right">p90</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {result.language_results.map((l) => (
                  <TableRow key={l.language}>
                    <TableCell className="font-mono text-xs">{l.language}</TableCell>
                    <TableCell className="text-right font-mono text-xs tnum">{l.successes}/{l.requests}</TableCell>
                    <TableCell className="text-right font-mono text-xs tnum text-violet">{number(l.tokens_per_second, 1)}</TableCell>
                    <TableCell className="text-right font-mono text-xs tnum">{l.latency_ms?.p50 != null ? `${number(l.latency_ms.p50)}ms` : "—"}</TableCell>
                    <TableCell className="text-right font-mono text-xs tnum">{l.latency_ms?.p90 != null ? `${number(l.latency_ms.p90)}ms` : "—"}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        ) : null}

        {trialsPending ? (
          <p className="font-mono text-[11px] text-faint">loading trials…</p>
        ) : trialsError ? (
          <p className="font-mono text-[11px] text-warn">trials unavailable</p>
        ) : trials != null && trials.length > 0 ? (
          <details>
            <summary className="lmw-label cursor-pointer select-none">trials ({trials.length})</summary>
            <div className="mt-2 overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>task</TableHead>
                    <TableHead className="text-right">candidate</TableHead>
                    <TableHead>official</TableHead>
                    <TableHead className="text-right">verifier</TableHead>
                    <TableHead>selected</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {trials.map((t) => (
                    <TableRow key={`${t.task_id}-${t.candidate_index}`}>
                      <TableCell className="font-mono text-xs">{t.task_id}</TableCell>
                      <TableCell className="text-right font-mono text-xs tnum">{t.candidate_index}</TableCell>
                      <TableCell className="font-mono text-xs">{t.official_pass ? "pass" : "fail"}</TableCell>
                      <TableCell className="text-right font-mono text-xs tnum">{t.verifier_score == null ? "—" : number(t.verifier_score, 2)}</TableCell>
                      <TableCell className="font-mono text-xs">{t.verifier_selected ? "yes" : "no"}</TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          </details>
        ) : null}

        <div className="flex items-center gap-2">
          <a
            href={benchmarkBundleUrl(runId)}
            className="control rounded border border-hairline px-2.5 py-1 font-mono text-xs text-muted hover:text-foreground"
          >
            download bundle
          </a>
          {!isRunTerminal(summary.run.state) ? (
            <Button
              size="sm"
              variant="outline"
              disabled={cancelBenchmark.isPending}
              onClick={() =>
                cancelBenchmark
                  .mutateAsync(runId)
                  .then(() => toast.success("Benchmark cancellation requested"))
                  .catch((e) => toast.error(e instanceof Error ? e.message : "cancel failed"))
              }
            >
              {cancelBenchmark.isPending ? "cancelling…" : "cancel benchmark"}
            </Button>
          ) : null}
        </div>
      </div>
    </Section>
  );
}

export default function RunDetailRoute() {
  const id = useTailPathParam();
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
      {run.module === "benchmarks" ? <BenchmarkRunSection runId={run.id} /> : null}

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
