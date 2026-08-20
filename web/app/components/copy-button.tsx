import { useCallback, useEffect, useRef, useState } from "react";
import { Check, Copy } from "lucide-react";
import { cn } from "~/lib/utils";

/** Mono chip that copies its value to the clipboard with a check flash. */
export function CopyButton({
  value,
  label,
  className,
}: {
  value: string;
  label?: string;
  className?: string;
}) {
  const [copied, setCopied] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(
    () => () => {
      if (timer.current) clearTimeout(timer.current);
    },
    [],
  );
  const onCopy = useCallback(async () => {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(true);
      timer.current = setTimeout(() => setCopied(false), 1500);
    } catch {
      // clipboard unavailable (non-secure context); leave silent
    }
  }, [value]);
  return (
    <button
      type="button"
      onClick={onCopy}
      title={`Copy ${label ?? "value"}`}
      aria-label={`Copy ${label ?? "value"}`}
      className={cn(
        "inline-flex max-w-full items-center gap-1.5 rounded border border-hairline bg-raised px-2 py-1 font-mono text-xs text-foreground control hover:border-primary/60",
        className,
      )}
    >
      <span className="truncate">{value}</span>
      {copied ? (
        <Check className="h-3.5 w-3.5 shrink-0 text-ok" aria-hidden />
      ) : (
        <Copy className="h-3.5 w-3.5 shrink-0 text-muted" aria-hidden />
      )}
    </button>
  );
}
