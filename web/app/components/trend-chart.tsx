import { useEffect, useMemo, useRef } from "react";
import uPlot from "uplot";
import "uplot/dist/uPlot.min.css";

export interface TrendSeries {
  label: string;
  color: string;
  /** [x=epochSeconds, y] points, ascending x. */
  points: [number, number][];
}

function align(series: TrendSeries[]): uPlot.AlignedData {
  const xs = [
    ...new Set(series.flatMap((s) => s.points.map((p) => p[0]))),
  ].sort((a, b) => a - b);
  return [
    xs,
    ...series.map((s) => {
      const byX = new Map(s.points);
      return xs.map((x) => (byX.has(x) ? (byX.get(x) as number) : null));
    }),
  ];
}

/**
 * Minimal uPlot panel for trends. The plot is created once per series
 * definition (labels/colors) and polling updates are pushed through setData
 * rather than destroying/recreating the chart every five seconds; it is
 * destroyed only on unmount or when the series definition changes.
 */
export function TrendChart({
  series,
  height = 180,
  yLabel,
  valueFormat,
  yFixed,
  ariaLabel,
}: {
  series: TrendSeries[];
  height?: number;
  yLabel?: string;
  valueFormat?: (v: number) => string;
  /** Optional fixed y-range [min, max]; omit for auto-scale. */
  yFixed?: [number, number];
  ariaLabel?: string;
}) {
  const hostRef = useRef<HTMLDivElement>(null);
  const chartRef = useRef<uPlot | null>(null);

  // Stable identity: recreate only when the series shape changes.
  const defKey = useMemo(() => series.map((s) => `${s.label}:${s.color}`).join("|"), [series]);
  const heightClass = height === 220 ? "h-[220px]" : "h-[180px]";
   const data = useMemo(() => align(series), [series]);
  // Has-data drives both the empty placeholder and the plot's create/destroy
  // lifecycle so an initially empty chart mounts uPlot only when async sample
  // data arrives, and cleans it up when it returns to empty.
  const hasData = series.length > 0 && series.some((s) => s.points.length > 0);

  useEffect(() => {
    const el = hostRef.current;
    if (!el) return;
    const fmt = valueFormat ?? ((v: number) => String(v));
    const scales: uPlot.Scales = yFixed
      ? { time: { time: true }, y: { min: yFixed[0], max: yFixed[1] } }
      : { time: { time: true } };
    const chart = new uPlot(
      {
        id: `trend-${defKey.slice(0, 8)}`,
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
        scales,
        axes: [
          {
            stroke: "#5b6368",
            font: "10px 'Commit Mono', monospace",
            grid: { stroke: "#e0d8c8", width: 0.5 },
            ticks: { width: 0 },
            values: (_u: uPlot, vals: (number | null)[]) =>
              vals.map((v) => (v == null ? "" : new Date(v * 1000).toISOString().slice(11, 16))),
          },
          {
            stroke: "#5b6368",
            font: "10px 'Commit Mono', monospace",
            grid: { stroke: "#e0d8c8", width: 0.5 },
            ticks: { width: 0 },
            label: yLabel ?? "",
            values: (_u: uPlot, vals: (number | null)[]) => vals.map((v) => (v == null ? "" : fmt(v))),
          },
        ],
        legend: { show: true },
      },
      data,
      el,
    );
    chartRef.current = chart;
    const ro = new ResizeObserver(() => {
      chart.setSize({ width: el.clientWidth || 480, height });
    });
    ro.observe(el);
    return () => {
      ro.disconnect();
      chart.destroy();
      chartRef.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [defKey, height, yLabel, valueFormat, yFixed?.[0], yFixed?.[1], hasData]);

  // Push polling updates without recreating the plot.
  useEffect(() => {
    chartRef.current?.setData(data);
  }, [data]);

    if (!hasData) {
    return (
      <div className={`flex ${heightClass} items-center justify-center rounded border border-hairline bg-background/40 px-4 text-xs text-muted`}>
        no data
      </div>
    );
  }
  return (
    <div
      ref={hostRef}
      className={`w-full ${heightClass}`}
      aria-label={ariaLabel ?? yLabel ?? "trend chart"}
      role="img"
    />
  );
}
