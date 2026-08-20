import type { ReactNode } from "react";
import { AlertTriangle, Inbox } from "lucide-react";
import { cn } from "~/lib/utils";
import { ApiError } from "~/lib/api/client";

/** Hairline panel with an icon, title, and optional hint/actions. */
export function EmptyState({
  title,
  hint,
  detail,
  icon,
  action,
  onRetry,
  className,
}: {
  title: string;
  hint?: string;
  detail?: string;
  icon?: ReactNode;
  action?: ReactNode;
  onRetry?: () => void;
  className?: string;
}) {
  return (
    <div className={cn("lmw-panel flex flex-col items-center justify-center gap-2 px-6 py-10 text-center", className)}>
      <span className="text-muted" aria-hidden>
        {icon ?? <Inbox className="h-6 w-6" />}
      </span>
      <p className="font-display text-base font-semibold tracking-wide text-foreground">{title}</p>
      {hint ? <p className="max-w-sm text-xs text-muted">{hint}</p> : null}
      {detail ? (
        <p className="max-w-md break-all font-mono text-[11px] text-faint">{detail}</p>
      ) : null}
      {onRetry ? (
        <button
          type="button"
          onClick={onRetry}
          className="control mt-1 rounded border border-hairline px-2.5 py-1 font-mono text-xs text-muted hover:text-foreground"
        >
          retry
        </button>
      ) : null}
      {action ? <div className="mt-2">{action}</div> : null}
    </div>
  );
}

/** Error panel that surfaces the API's stable code + message. */
export function ErrorState({
  error,
  className,
}: {
  error: unknown;
  className?: string;
}) {
  const apiErr = error instanceof ApiError ? error : null;
  const message =
    apiErr?.message ?? (error instanceof Error ? error.message : "Unexpected error");
  return (
    <div
      role="alert"
      className={cn(
        "flex flex-col items-center justify-center gap-2 border border-fault/40 bg-fault/5 px-6 py-8 text-center",
        className,
      )}
    >
      <AlertTriangle className="h-5 w-5 text-fault" aria-hidden />
      <p className="text-sm text-foreground">{message}</p>
      {apiErr && apiErr.code ? (
        <p className="font-mono text-[11px] text-fault">{apiErr.code}</p>
      ) : null}
    </div>
  );
}

/** Loading placeholder: hairline panel with a muted label. */
export function LoadingPanel({ label = "loading", className }: { label?: string; className?: string }) {
  return (
    <div className={cn("lmw-panel flex items-center justify-center px-6 py-10", className)}>
      <span className="lmw-label blink">{label}…</span>
    </div>
  );
}
