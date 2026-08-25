import { TONE_BG, type Tone } from "~/lib/format";
import { cn } from "~/lib/utils";

/**
 * Labeled textual value plus a tone-colored fill. Exposes role="meter" with
 * aria-valuemin/max/now so the value is never conveyed by color alone.
 */
export function MetricBar({
  label,
  value,
  max = 100,
  display,
  tone = "ok",
}: {
  label: string;
  /** Logarithmic-free numeric value for the meter; 0 fills nothing. */
  value: number;
  max?: number;
  /** Human-readable textual value ("34%", "12.4 GiB", "—"). */
  display: string;
  tone?: Tone;
}) {
  const clamped = Number.isFinite(value) ? Math.max(0, Math.min(max, value)) : 0;
  const pct = max > 0 ? (clamped / max) * 100 : 0;
  return (
    <div className="space-y-1">
      <div className="flex items-baseline justify-between gap-2">
        <span className="lmw-label">{label}</span>
        <span className="tnum font-mono text-xs text-foreground">{display}</span>
      </div>
      <div
        role="meter"
        aria-valuemin={0}
        aria-valuemax={max}
        aria-valuenow={clamped}
        aria-label={label}
        className="h-1.5 w-full overflow-hidden rounded bg-hairline"
      >
        <div
          className={cn("h-full rounded transition-[width] duration-500", TONE_BG[tone])}
          style={{ width: `${pct}%` }}
        />
      </div>
    </div>
  );
}
