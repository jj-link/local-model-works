import type { Tone } from "~/lib/format";

const TONE_STROKE: Record<Tone, string> = {
  ok: "#4d6b45",
  warn: "#aa7327",
  fault: "#8f2d2d",
  info: "#225ea8",
  muted: "#5b6368",
  violet: "#225ea8",
};

/**
 * Radial instrument gauge (SVG): 270° arc, value needle-free fill,
 * mono readout in the center. Pure display.
 */
export function Instrument({
  label,
  value,
  max,
  unit,
  display,
  tone = "info",
  size = 132,
}: {
  label: string;
  value: number;
  max: number;
  unit?: string;
  display?: string;
  tone?: Tone;
  size?: number;
}) {
  const pct = max > 0 ? Math.min(1, Math.max(0, value / max)) : 0;
  const r = 52;
  const c = 2 * Math.PI * r;
  const arc = 0.75 * c; // 270°
  const stroke = TONE_STROKE[tone];
  return (
    <div className="flex flex-col items-center gap-2">
      <svg
        width={size}
        height={size}
        viewBox="0 0 128 128"
        role="img"
        aria-label={`${label}: ${display ?? `${value} of ${max}`}${unit ? ` ${unit}` : ""}`}
      >
        <g transform="rotate(135 64 64)">
          <circle
            cx="64"
            cy="64"
            r={r}
            fill="none"
            stroke="#e0d8c8"
            strokeWidth="7"
            strokeDasharray={`${arc} ${c}`}
            strokeLinecap="butt"
          />
          <circle
            cx="64"
            cy="64"
            r={r}
            fill="none"
            stroke={stroke}
            strokeWidth="7"
            strokeDasharray={`${arc * pct} ${c}`}
            strokeLinecap="butt"
            className="control"
          />
        </g>
        <text
          x="64"
          y="60"
          textAnchor="middle"
          className="tnum"
          fill="#20272b"
          fontSize="20"
          fontFamily="Commit Mono, monospace"
          fontWeight="500"
        >
          {display ?? String(value)}
        </text>
        {unit ? (
          <text x="64" y="78" textAnchor="middle" fill="#5b6368" fontSize="10" fontFamily="Commit Mono, monospace">
            {unit}
          </text>
        ) : null}
      </svg>
      <span className="lmw-label">{label}</span>
    </div>
  );
}
