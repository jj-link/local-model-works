import { Badge } from "~/components/ui/badge";
import type { Diagnostic } from "~/lib/api";
import { stateInfo, TONE_TEXT } from "~/lib/format";

const SEV_CLASS: Record<string, string> = {
  info: "border-accent/40 text-accent",
  warning: "border-primary/40 text-primary",
  error: "border-fault/40 text-fault",
};

/** Diagnostics list: severity chip + stable code + message. */
export function DiagnosticsList({ diagnostics }: { diagnostics: Diagnostic[] | undefined }) {
  if (!diagnostics || diagnostics.length === 0) {
    return <p className="font-mono text-xs text-muted">no diagnostics</p>;
  }
  return (
    <ul className="flex flex-col gap-2">
      {diagnostics.map((d, i) => {
        const info = stateInfo(d.severity);
        return (
          <li
            key={`${d.code}-${i}`}
            className="flex items-start gap-2 rounded border border-hairline bg-raised px-3 py-2"
          >
            <Badge variant="outline" className={`shrink-0 ${SEV_CLASS[d.severity] ?? SEV_CLASS.info}`}>
              {info.label}
            </Badge>
            <div className="min-w-0">
              <p className="break-words text-xs text-foreground">{d.message}</p>
              <p className={`font-mono text-[11px] ${TONE_TEXT[info.tone]}`}>
                {d.code}
                {d.resource ? <span className="text-muted"> · {d.resource}</span> : null}
              </p>
            </div>
          </li>
        );
      })}
    </ul>
  );
}
