import { cn } from "~/lib/utils";
import { stateInfo, TONE_BG } from "~/lib/format";

const DOT_SHAPE: Record<string, string> = {
  ok: "",
  warn: "border-2 border-current bg-transparent",
  muted: "border-2 border-current bg-transparent",
  info: "",
  fault: "",
  violet: "",
};

/**
 * Status indicator: a shape+color dot plus a text label. Color is never
 * the only signal: ok=filled, warn/muted=hollow ring, fault=filled.
 */
export function StatusDot({
  state,
  label,
  className,
  pulse,
}: {
  state: string | null | undefined;
  label?: string;
  className?: string;
  pulse?: boolean;
}) {
  const info = stateInfo(state);
  return (
    <span className={cn("inline-flex items-center gap-1.5 whitespace-nowrap", className)}>
      <span
        aria-hidden
        className={cn(
          "h-2 w-2 shrink-0 rounded-full",
          TONE_BG[info.tone],
          DOT_SHAPE[info.tone],
          pulse && "blink",
        )}
      />
      <span className={cn("text-xs", info.tone === "muted" ? "text-muted" : "text-foreground")}>
        {label ?? info.label}
      </span>
    </span>
  );
}
