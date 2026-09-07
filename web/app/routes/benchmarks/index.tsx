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
import { useBenchmarkResults, useBenchmarkCatalog } from "~/lib/queries";
import type { BenchmarkLanguageResult } from "~/lib/api";
import { EmptyState } from "~/components/empty-state";
import { BenchmarkDialog, BENCHMARK_LANGUAGES } from "~/components/dialogs/benchmark-dialog";
import { TrendChart, type TrendSeries } from "~/components/trend-chart";
import { number, shortId, wallClock } from "~/lib/format";

const LANGUAGES = ["all", ...BENCHMARK_LANGUAGES];

export default function BenchmarksRoute() {
  const { data: results, isPending, isError, error, refetch } = useBenchmarkResults();
  const { data: catalog } = useBenchmarkCatalog();
  const [language, setLanguage] = useState("all");
  const [benchmarkId, setBenchmarkId] = useState("all");
  const [dialogOpen, setDialogOpen] = useState(false);

  // Flatten run results into language rows (legacy one-shot shape).
  const rows = useMemo(() => {
    const list: (BenchmarkLanguageResult & { benchmark_id: string; benchmark_version: string; pass_at_1: number | null; verifier_pass_rate: number | null })[] = [];
    for (const r of results ?? []) {
      for (const l of r.result.language_results ?? []) {
        list.push({
          ...l,
          benchmark_id: r.result.benchmark_id,
          benchmark_version: r.result.benchmark_version,
          pass_at_1: r.result.pass_at_1 ?? null,
          verifier_pass_rate: r.result.verifier_pass_rate ?? null,
        });
      }
    }
    return list.sort((a, b) => (b.created_at ?? "").localeCompare(a.created_at ?? ""));
  }, [results]);

  const benchmarkOptions = useMemo(
    () => [...new Set((results ?? []).map((r) => r.result.benchmark_id))].sort(),
    [results],
  );

  const filtered = useMemo(
    () =>
      rows.filter((r) => {
        if (language !== "all" && r.language !== language) return false;
        if (benchmarkId !== "all" && r.benchmark_id !== benchmarkId) return false;
        return true;
      }),
    [rows, language, benchmarkId],
  );

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
          <header className="lmw-panel-head flex-wrap">
            <h1 className="lmw-title">
              <Gauge className="size-4" aria-hidden /> Benchmark results
            </h1>
            <div className="ml-auto flex flex-wrap items-center gap-2">
              <Select value={language} onValueChange={setLanguage}>
                <SelectTrigger className="w-32" aria-label="Language filter">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {LANGUAGES.map((lang) => (
                    <SelectItem key={lang} value={lang}>
                      {lang}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <Select value={benchmarkId} onValueChange={setBenchmarkId}>
                <SelectTrigger className="w-40" aria-label="Benchmark filter">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="all">all benchmarks</SelectItem>
                  {benchmarkOptions.map((id) => (
                    <SelectItem key={id} value={id}>
                      {id}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <Button size="sm" className="ml-auto" onClick={() => setDialogOpen(true)}>
                <Gauge aria-hidden /> run benchmark
              </Button>
            </div>
          </header>

          {isPending ? (
            <p className="px-3 py-8 text-center font-mono text-xs text-faint">loading results...</p>
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
              hint={
                catalog?.benchmarks?.length
                  ? "Launch a pinned benchmark from the catalog: terminal-bench or the six-language grader."
                  : "Run a benchmark against a healthy deployment: six oneshot languages, deterministic prompts, per-language grading."
              }
            />
          ) : (
            <div className="overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Time</TableHead>
                    <TableHead>Run</TableHead>
                    <TableHead>Benchmark</TableHead>
                    <TableHead>Language</TableHead>
                    <TableHead className="text-right">tok/s</TableHead>
                    <TableHead className="text-right">pass@1</TableHead>
                    <TableHead className="text-right">verifier</TableHead>
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
                      <TableCell className="font-mono text-xs">
                        {r.benchmark_id}@{r.benchmark_version}
                      </TableCell>
                      <TableCell className="font-mono text-xs">{r.language}</TableCell>
                      <TableCell className="text-right font-mono text-xs tnum text-violet">
                        {r.tokens_per_second != null ? number(r.tokens_per_second, 1) : "—"}
                      </TableCell>
                      <TableCell className="text-right font-mono text-xs tnum text-muted">
                        {r.pass_at_1 != null ? `${Math.round(r.pass_at_1 * 100)}%` : "—"}
                      </TableCell>
                      <TableCell className="text-right font-mono text-xs tnum text-muted">
                        {r.verifier_pass_rate != null ? `${Math.round(r.verifier_pass_rate * 100)}%` : "—"}
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
