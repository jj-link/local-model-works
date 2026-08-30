// Single source of truth for human state labels, semantic tones, and
// value formatting. Tones map 1:1 to CSS palette colors.

export type Tone = "ok" | "warn" | "fault" | "info" | "muted" | "violet";

export interface StateInfo {
  label: string;
  tone: Tone;
}

const STATES: Record<string, StateInfo> = {
  // node status
  online: { label: "online", tone: "ok" },
  pending: { label: "pending", tone: "warn" },
  offline: { label: "offline", tone: "fault" },
  degraded: { label: "degraded", tone: "warn" },
  // run states
  queued: { label: "queued", tone: "muted" },
  planning: { label: "planning", tone: "info" },
  waiting: { label: "waiting", tone: "info" },
  running: { label: "running", tone: "info" },
  verifying: { label: "verifying", tone: "warn" },
  succeeded: { label: "succeeded", tone: "ok" },
  failed: { label: "failed", tone: "fault" },
  cancelling: { label: "cancelling", tone: "warn" },
  cancelled: { label: "cancelled", tone: "muted" },
  interrupted: { label: "interrupted", tone: "warn" },
  // deployment observed state
  unknown: { label: "unknown", tone: "muted" },
  preparing: { label: "preparing", tone: "info" },
  starting: { label: "starting", tone: "info" },
  healthy: { label: "healthy", tone: "ok" },
  stopping: { label: "stopping", tone: "warn" },
  stopped: { label: "stopped", tone: "muted" },
  // fabric state
  ok: { label: "ok", tone: "ok" },
  incomplete: { label: "incomplete", tone: "warn" },
  // transfer / placement / validation
  transferring: { label: "transferring", tone: "info" },
  validating: { label: "validating", tone: "warn" },
  complete: { label: "complete", tone: "ok" },
  valid: { label: "valid", tone: "ok" },
  invalid: { label: "invalid", tone: "fault" },
  // diagnostic severity
  warning: { label: "warning", tone: "warn" },
  error: { label: "error", tone: "fault" },
  info: { label: "info", tone: "info" },
};

export function stateInfo(state: string | null | undefined): StateInfo {
  if (!state) return { label: "unknown", tone: "muted" };
  return STATES[state] ?? { label: state, tone: "muted" };
}

export const TERMINAL_RUN_STATES = new Set<string>([
  "succeeded",
  "failed",
  "cancelled",
  "interrupted",
]);

export function isRunTerminal(state: string | null | undefined): boolean {
  return state === null || state === undefined || TERMINAL_RUN_STATES.has(state);
}

export const TONE_TEXT: Record<Tone, string> = {
  ok: "text-ok",
  warn: "text-primary",
  fault: "text-fault",
  info: "text-accent",
  muted: "text-muted",
  violet: "text-violet",
};

export const TONE_BG: Record<Tone, string> = {
  ok: "bg-ok",
  warn: "bg-primary",
  fault: "bg-fault",
  info: "bg-accent",
  muted: "bg-muted",
  violet: "bg-violet",
};

/* ------------------------------------------------------------------ */
/* time                                                                */
/* ------------------------------------------------------------------ */

const pad2 = (n: number) => String(n).padStart(2, "0");

/** "42s ago" / "5m ago" / "3h ago" / "2d ago" / "2026-08-01" */
export function relativeTime(iso: string | null | undefined, now: number = Date.now()): string {
  if (!iso) return "—";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "—";
  const d = Math.max(0, now - t);
  const s = Math.floor(d / 1000);
  if (s < 5) return "just now";
  if (s < 60) return `${s}s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  const days = Math.floor(h / 24);
  if (days < 14) return `${days}d ago`;
  return new Date(t).toISOString().slice(0, 10);
}

/** "38s" / "12m 04s" / "1h 02m" between two ISO timestamps. */
export function duration(fromIso: string | null | undefined, toIso?: string | null): string {
  if (!fromIso) return "—";
  const a = Date.parse(fromIso);
  if (Number.isNaN(a)) return "—";
  const b = toIso ? Date.parse(toIso) : Date.now();
  if (Number.isNaN(b)) return "—";
  const s = Math.max(0, Math.round((b - a) / 1000));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${pad2(s % 60)}s`;
  const h = Math.floor(m / 60);
  return `${h}h ${pad2(m % 60)}m`;
}

export function wallClock(iso: string | null | undefined): string {
  if (!iso) return "—";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "—";
  const d = new Date(t);
  return `${d.toISOString().slice(0, 10)} ${pad2(d.getUTCHours())}:${pad2(d.getUTCMinutes())}:${pad2(d.getUTCSeconds())}Z`;
}

/* ------------------------------------------------------------------ */
/* quantities                                                          */
/* ------------------------------------------------------------------ */

const BINARY_UNITS = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];

export function bytes(n: number | null | undefined): string {
  if (n === null || n === undefined || Number.isNaN(n)) return "—";
  let v = n;
  let u = 0;
  while (v >= 1024 && u < BINARY_UNITS.length - 1) {
    v /= 1024;
    u += 1;
  }
  return `${v >= 100 ? Math.round(v) : v.toFixed(1)} ${BINARY_UNITS[u]}`;
}

export function number(n: number | null | undefined, digits = 0): string {
  if (n === null || n === undefined || Number.isNaN(n)) return "—";
  return n.toLocaleString("en-US", {
    maximumFractionDigits: digits,
    minimumFractionDigits: digits,
  });
}

export function shortId(id: string | null | undefined): string {
  if (!id) return "—";
  return id.length > 8 ? `${id.slice(0, 8)}…` : id;
}

export function shortDigest(d: string | null | undefined): string {
  if (!d) return "—";
  return d.length > 12 ? `${d.slice(0, 12)}…` : d;
}

/* ------------------------------------------------------------------ */
/* endpoints                                                           */
/* ------------------------------------------------------------------ */

export interface EndpointShape {
  host?: string;
  port?: number;
  path?: string;
  model?: string;
}

export function endpointUrl(ep: EndpointShape | null | undefined): string {
  if (!ep || !ep.host || !ep.port) return "—";
  return `http://${ep.host}:${ep.port}${ep.path ?? "/"}`;
}

export function endpointLabel(ep: EndpointShape | null | undefined): string {
  if (!ep || !ep.host || !ep.port) return "—";
  return `${ep.host}:${ep.port}`;
}

/* ------------------------------------------------------------------ */
/* YAML rendering (display-only canonical serializer)                  */
/* ------------------------------------------------------------------ */

function yamlScalar(v: unknown): string {
  if (v === null || v === undefined) return "null";
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  const s = String(v);
  if (s === "") return '""';
  if (
    /^[-+.0-9]/.test(s) ||
    /^(true|false|null|yes|no|on|off)$/i.test(s) ||
    /[:#{}[\],&*?|>'"%@`]/.test(s) ||
    s !== s.trim() ||
    s.includes("\n")
  ) {
    return JSON.stringify(s);
  }
  return s;
}

/** Small JSON→YAML serializer for read-only document viewing. */
export function toYaml(value: unknown, indent = 0): string {
  const pad = "  ".repeat(indent);
  if (Array.isArray(value)) {
    if (value.length === 0) return `${pad}[]`;
    return value
      .map((item) => {
        if (item !== null && typeof item === "object") {
          const body = toYaml(item, indent + 1).replace(`${pad}  `, `${pad}- `);
          return body;
        }
        return `${pad}- ${yamlScalar(item)}`;
      })
      .join("\n");
  }
  if (value !== null && typeof value === "object") {
    const entries = Object.entries(value as Record<string, unknown>);
    if (entries.length === 0) return `${pad}{}`;
    return entries
      .map(([k, v]) => {
        if (v !== null && typeof v === "object") {
          return `${pad}${k}:\n${toYaml(v, indent + 1)}`;
        }
        return `${pad}${k}: ${yamlScalar(v)}`;
      })
      .join("\n");
  }
  return `${pad}${yamlScalar(value)}`;
}
