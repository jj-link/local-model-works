import type { ReactNode } from "react";
import { cn } from "~/lib/utils";
import type { Tone } from "~/lib/format";
import { TONE_TEXT } from "~/lib/format";

/** Compact stat tile for the workshop strip: big condensed number + label. */
export function StatTile({
  label,
  value,
  sub,
  tone = "muted",
  className,
}: {
  label: string;
  value: string;
  sub?: ReactNode;
  tone?: Tone;
  className?: string;
}) {
  return (
    <div className={cn("lmw-panel flex flex-col gap-1 px-4 py-3", className)}>
      <span className="lmw-label">{label}</span>
      <span
        className={cn(
          "font-display text-3xl font-semibold leading-none tnum",
          TONE_TEXT[tone],
        )}
      >
        {value}
      </span>
      {sub ? <span className="font-mono text-[11px] text-muted">{sub}</span> : null}
    </div>
  );
}
