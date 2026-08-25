import { useMemo, useState } from "react";
import { Link } from "react-router";
import { Gauge } from "lucide-react";
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
import { useBenchmarkResults, useBenchmarkRuns } from "~/lib/queries";
import { EmptyState } from "~/components/empty-state";
import { BenchmarkDialog, BENCHMARK_LANGUAGES } from "~/components/dialogs/benchmark-dialog";
import { TrendChart, type TrendSeries } from "~/components/trend-chart";
import { number, shortId, wallClock } from "~/lib/format";

const LANGUAGES = ["all", ...BENCHMARK_LANGUAGES];

export default function BenchmarksRoute() {
  const { data: results, isPending, isError, error, refetch } = useBenchmarkResults();
  const { data: runs } = useBenchmarkRuns();
  const [language, setLanguage] = useState("all");
  const [deployment, setDeployment] = useState("all");
  const [dialogOpen, setDialogOpen] = useState(false);

  const deploymentOf = useMemo(() => {
    const m = new Map<string, string>();
    for (const r of runs ?? []) if (r.deployment_id) m.set(r.id, r.deployment_id);
    return m;
  }, [runs]);

  const deployments = useMemo(() => {
    const set = new Set<string>();
    for (const r of results ?? []) {
      const d = deploymentOf.get(r.run_id);
      if (d) set.add(d);
    }
    return [...set].sort();
  }, [results, deploymentOf]);

  const filtered = useMemo(() => {
    const list = [...(results ?? [])].sort(
      (a, b) => (b.created_at ?? "").localeCompare(a.created_at ?? ""),
    );
    return list.filter((r) => {
      if (language !== "all" && r.language !== language) return false;
      if (deployment !== "all") {
        const d = deploymentOf.get(r.run_id);
        if (d !== deployment) return false;
      }
      return true;
    });
  }, [results, language, deployment, deploymentOf]);

  const series: TrendSeries[] = useMemo(() => {
    if (language === "all") return [];
    const pts: [number, number][] = filtered
      .filter((r) => typeof r.tokens_per_second === "number" && r.created_at)
      .map(
        (r) =>
          [
            Math.floor(new Date(r.created_at as string).getTime() / 1000),
            r.tokens_per_second as number,
          ] as [number, number],
      );
    return [{ label: `${language} tokens/s`, color: "#225ea8", points: pts }];
  }, [filtered, language]);

  return (
    <div className="grid gap-4">
      <div className="grid gap-4 lg:grid-cols-[1fr_360px]">
        <div className="lmw-panel">
          <header className="lmw-panel-head">
            <h1 className="lmw-label">benchmark results</h1>
            <span className="font-mono text-[11px] text-faint">{filtered.length} rows</span>
            <Select value={deployment} onValueChange={setDeployment}>
              <SelectTrigger className="h-7 w-40 font-mono text-xs" aria-label="Filter by deployment">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all">all deployments</SelectItem>
                {deployments.map((d) => (
                  <SelectItem key={d} value={d}>
                    {shortId(d)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Select value={language} onValueChange={setLanguage}>
              <SelectTrigger className="h-7 w-32 font-mono text-xs" aria-label="Filter by language">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {LANGUAGES.map((l) => (
                  <SelectItem key={l} value={l}>
                    {l}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Button size="sm" className="ml-auto" onClick={() => setDialogOpen(true)}>
              <Gauge aria-hidden /> run benchmark
            </Button>
          </header>

          {isPending ? (
            <p className="px-3 py-8 text-center font-mono text-xs text-faint">loading results…</p>
          ) : isError ? (
            <EmptyState
              className="m-3"
              title="Cannot load benchmark results"
              detail={error instanceof Error ? error.message : undefined}
              onRetry={() => void refetch()}
            />
          ) : filtered.length === 0 ? (
            <EmptyState
              className="m-3"
              title="No benchmark results"
              hint="Run a benchmark against a healthy deployment: six oneshot languages, deterministic prompts, per-language grading."
            />
          ) : (
            <div className="overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Time</TableHead>
                    <TableHead>Run</TableHead>
                    <TableHead>Model</TableHead>
                    <TableHead>Language</TableHead>
                    <TableHead className="text-right">Tokens/s</TableHead>
                    <TableHead className="text-right">p50</TableHead>
                    <TableHead className="text-right">p90</TableHead>
                    <TableHead className="text-right">OK</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {filtered.slice(0, 100).map((r) => (
                    <TableRow key={`${r.run_id}-${r.language}`}>
                      <TableCell className="font-mono text-[11px] text-faint">
                        {r.created_at ? wallClock(r.created_at) : "—"}
                      </TableCell>
                      <TableCell>
                        <Link to={`/runs/${r.run_id}`} className="control font-mono text-xs hover:text-foreground">
                          {shortId(r.run_id)}
                        </Link>
                      </TableCell>
                      <TableCell className="max-w-40 truncate font-mono text-xs text-muted">
                        {r.model ?? r.endpoint ?? "—"}
                      </TableCell>
                      <TableCell className="font-mono text-xs">{r.language}</TableCell>
                      <TableCell className="text-right font-mono text-xs tnum text-violet">
                        {r.tokens_per_second != null ? number(r.tokens_per_second, 1) : "—"}
                      </TableCell>
                      <TableCell className="text-right font-mono text-xs tnum text-muted">
                        {r.latency_ms?.p50 != null ? `${number(r.latency_ms.p50)}ms` : "—"}
                      </TableCell>
                      <TableCell className="text-right font-mono text-xs tnum text-muted">
                        {r.latency_ms?.p90 != null ? `${number(r.latency_ms.p90)}ms` : "—"}
                      </TableCell>
                      <TableCell className="text-right font-mono text-xs tnum">
                        {r.successes != null && r.requests != null ? (
                          <span className={r.successes === r.requests ? "text-ok" : "text-warn"}>
                            {r.successes}/{r.requests}
                          </span>
                        ) : (
                          "—"
                        )}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          )}
        </div>

        <div className="lmw-panel">
          <header className="lmw-panel-head">
            <h2 className="lmw-label">throughput trend</h2>
            <span className="font-mono text-[11px] text-faint">{language}</span>
          </header>
          <div className="p-3">
            {series.length === 0 ? (
              <p className="py-10 text-center font-mono text-xs text-faint">
                select a language to chart tokens/s over time
              </p>
            ) : (
              <TrendChart series={series} height={220} yLabel="tokens/s" valueFormat={(v) => number(v, 1)} />
            )}
          </div>
        </div>
      </div>

      <BenchmarkDialog open={dialogOpen} onOpenChange={setDialogOpen} />
    </div>
  );
}
