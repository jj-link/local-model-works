import { useEffect, useMemo, useRef } from "react";
import uPlot from "uplot";
import "uplot/dist/uPlot.min.css";

export interface TrendSeries {
  label: string;
  color: string;
  /** [x=epochSeconds, y] points, ascending x. */
  points: [number, number][];
}

/**
 * Minimal uPlot panel for benchmark trends. Static data, no drag;
 * destroyed and recreated when the series identity changes.
 */
export function TrendChart({
  series,
  height = 180,
  yLabel,
  valueFormat,
}: {
  series: TrendSeries[];
  height?: number;
  yLabel?: string;
  valueFormat?: (v: number) => string;
}) {
  const hostRef = useRef<HTMLDivElement>(null);
  const dataKey = useMemo(
    () => series.map((s) => `${s.label}:${s.points.length}:${s.points.at(-1)?.[1] ?? ""}`).join("|"),
    [series],
  );
  const heightClass = height === 220 ? "h-[220px]" : "h-[180px]";


  useEffect(() => {
    const el = hostRef.current;
    if (!el) return;
    if (series.length === 0 || series.every((s) => s.points.length === 0)) return;
    const xs = series.flatMap((s) => s.points.map((p) => p[0])).sort((a, b) => a - b);
    const xMin = xs[0];
    const xMax = xs[xs.length - 1];
    const data = [
      xs,
      ...series.map((s) => {
        const byX = new Map(s.points);
        return xs.map((x) => byX.get(x) ?? null);
      }),
    ] as uPlot.AlignedData;
    const fmt = valueFormat ?? ((v: number) => String(v));
    const chart = new uPlot(
      {
        id: `trend-${dataKey.slice(0, 8)}`,
        data,
        height,
        width: el.clientWidth || 480,
        series: [
          { label: "time", stroke: "transparent", width: 0 },
          ...series.map((s) => ({
            label: s.label,
            stroke: s.color,
            width: 1.5,
            fill: `${s.color}1f`,
            points: { size: 2.5, stroke: s.color, width: 1 },
          })),
        ],
        scales: {
          time: {
            time: true,
            min: xMin,
            max: xMax,
            range: (_u: uPlot, min: number, max: number) => [min, max],
          },
        },
        axes: [
          {
            stroke: "#919a9d",
            font: "10px 'Commit Mono', monospace",
            grid: { stroke: "#313a3e", width: 0.5 },
            ticks: { width: 0 },
            values: (_self: uPlot, vals: (number | null)[]) =>
              vals.map((v) => (v == null ? "" : new Date(v * 1000).toISOString().slice(11, 16))),
          },
          {
            stroke: "#919a9d",
            font: "10px 'Commit Mono', monospace",
            grid: { stroke: "#313a3e", width: 0.5 },
            ticks: { width: 0 },
            label: yLabel ?? "",
            values: (_self: uPlot, vals: (number | null)[]) =>
              vals.map((v) => (v == null ? "" : fmt(v))),
          },
        ],
        legend: { show: true },
      },
      data,
      el,
    );
    const ro = new ResizeObserver(() => {
      chart.setSize({ width: el.clientWidth || 480, height });
    });
    ro.observe(el);
    return () => {
      ro.disconnect();
      chart.destroy();
    };
  }, [dataKey, series, height, yLabel, valueFormat]);

  if (series.length === 0 || series.every((s) => s.points.length === 0)) {
    return (
      <div className={`flex ${heightClass} items-center justify-center rounded border border-hairline bg-background/40 px-4 text-xs text-muted`}>
        no data
      </div>
    );
  }
  return <div ref={hostRef} className={`w-full ${heightClass}`} aria-label={yLabel ?? "trend chart"} role="img" />;
}
