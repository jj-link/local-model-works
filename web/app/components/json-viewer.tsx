import { useMemo, useState, type ReactNode } from "react";
import { ChevronDown, ChevronRight } from "lucide-react";
import { cn } from "~/lib/utils";

/* ------------------------------------------------------------------ */
/* Highlighted pre (JSON or YAML token colorizer, display-only)        */
/* ------------------------------------------------------------------ */

const JSON_TOKEN =
  /("(\\u[a-fA-F0-9]{4}|\\[^u]|[^\\"])*"(\s*:)?|\b(true|false|null)\b|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)/g;

function highlightJson(text: string): ReactNode[] {
  const out: ReactNode[] = [];
  let last = 0;
  let m: RegExpExecArray | null;
  let key = 0;
  JSON_TOKEN.lastIndex = 0;
  while ((m = JSON_TOKEN.exec(text)) !== null) {
    if (m.index > last) out.push(text.slice(last, m.index));
    const tok = m[0];
    let cls = "text-muted";
    if (m[1] !== undefined) {
      cls = m[3] !== undefined ? "text-primary" : "text-ink/90"; // key vs string
    } else if (m[4] !== undefined) {
      cls = "text-accent";
    } else {
      cls = "text-violet";
    }
    out.push(
      <span key={key++} className={cls}>
        {tok}
      </span>,
    );
    last = m.index + tok.length;
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}

const YAML_LINE = /^(\s*(?:-\s+)?)([^:\s][^:]*):(\s+(.*))?$/;

function highlightYaml(text: string): ReactNode[] {
  return text.split("\n").map((line, i) => {
    if (/^\s*#/.test(line)) {
      return (
        <span key={i} className="text-muted/70">
          {line}
          {"\n"}
        </span>
      );
    }
    const m = YAML_LINE.exec(line);
    if (!m) {
      return (
        <span key={i}>
          {line}
          {"\n"}
        </span>
      );
    }
    const rest = m[4] ?? "";
    const restCls = /^".*"$/.test(rest) ? "text-ink/90" : /^(true|false|null)$/i.test(rest) ? "text-accent" : /^-?\d/.test(rest) ? "text-violet" : "text-ink/90";
    return (
      <span key={i}>
        <span className="text-muted">{m[1]}</span>
        <span className="text-primary">
          {m[2]}
          :
        </span>
        {m[3] ? <span className={restCls}>{rest}</span> : null}
        {"\n"}
      </span>
    );
  });
}

export function HighlightedPre({
  text,
  language = "json",
  maxLines,
  className,
}: {
  text: string;
  language?: "json" | "yaml";
  maxLines?: number;
  className?: string;
}) {
  const nodes = useMemo(
    () => (language === "yaml" ? highlightYaml(text) : highlightJson(text)),
    [text, language],
  );
  const lines = text.split("\n").length;
  return (
    <pre
      className={cn(
        "overflow-auto rounded border border-hairline bg-background/60 p-3 font-mono text-xs leading-relaxed",
        className,
      )}
      style={maxLines ? { maxHeight: `${maxLines * 1.55}em` } : undefined}
      aria-label={`${language} document`}
    >
      <code>{nodes}</code>
      {maxLines && lines > maxLines ? (
        <span className="text-muted">… {lines - maxLines} more lines</span>
      ) : null}
    </pre>
  );
}

/* ------------------------------------------------------------------ */
/* Expandable JSON tree                                                */
/* ------------------------------------------------------------------ */

function TreeNode({ name, value, depth }: { name?: string; value: unknown; depth: number }) {
  const [open, setOpen] = useState(depth < 2);
  const isObj = value !== null && typeof value === "object";
  const label = name !== undefined ? (
    <span className="text-primary">
      {JSON.stringify(name)}
      <span className="text-muted">: </span>
    </span>
  ) : null;

  if (!isObj) {
    let cls = "text-ink/90";
    let shown: string;
    if (typeof value === "string") shown = JSON.stringify(value);
    else if (value === null) {
      cls = "text-accent";
      shown = "null";
    } else if (typeof value === "number") {
      cls = "text-violet";
      shown = String(value);
    } else if (typeof value === "boolean") {
      cls = "text-accent";
      shown = String(value);
    } else shown = String(value);
    return (
      <div className="whitespace-pre-wrap break-all">
        {label}
        <span className={cls}>{shown}</span>
      </div>
    );
  }

  const entries = Array.isArray(value)
    ? value.map((v, i) => [String(i), v] as const)
    : Object.entries(value as Record<string, unknown>);
  return (
    <div>
      <button
        type="button"
        aria-expanded={open}
        onClick={() => setOpen(!open)}
        className="inline-flex items-center gap-1 text-left text-muted hover:text-foreground control"
      >
        {open ? (
          <ChevronDown className="h-3.5 w-3.5" aria-hidden />
        ) : (
          <ChevronRight className="h-3.5 w-3.5" aria-hidden />
        )}
        {label}
        <span className="text-muted">
          {Array.isArray(value) ? `[${entries.length}]` : `{${entries.length}}`}
        </span>
      </button>
      {open ? (
        <div className="ml-5 flex flex-col gap-0.5 border-l border-hairline pl-3">
          {entries.map(([k, v]) => (
            <TreeNode key={k} name={k} value={v} depth={depth + 1} />
          ))}
        </div>
      ) : null}
    </div>
  );
}

export function JsonTree({ value, className }: { value: unknown; className?: string }) {
  return (
    <div className={cn("font-mono text-xs leading-relaxed", className)}>
      <TreeNode value={value} depth={0} />
    </div>
  );
}
