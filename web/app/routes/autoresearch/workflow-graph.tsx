import { Activity, ShieldCheck } from "lucide-react";
import { cn } from "~/lib/utils";
import workflowImage from "../../../../third_party/agon/figures/figure_xp.png";
import type { ActiveInvocation } from "./events";

interface Hotspot {
  id: string;
  node: string;
  label: string;
  glyph: string;
  x: number;
  y: number;
  size?: number;
}

const HOTSPOTS: Hotspot[] = [
  { id: "idea-creator", node: "idea.creator", label: "Idea creator", glyph: "IC", x: 10.1, y: 54.8 },
  { id: "idea-reviewer", node: "idea.reviewer", label: "Idea reviewer", glyph: "IR", x: 17.8, y: 54.8 },
  { id: "idea-refiner", node: "idea.refiner", label: "Idea refiner", glyph: "IF", x: 13.4, y: 42.1 },
  { id: "idea-deep-lit", node: "literature.reader", label: "Idea deep literature", glyph: "DL", x: 21.5, y: 41.5 },
  { id: "proposal-deep-lit", node: "literature.reader", label: "Proposal deep literature", glyph: "DL", x: 35.2, y: 40 },
  { id: "proposal-refiner", node: "proposal.refiner", label: "Proposal refiner", glyph: "PF", x: 30.8, y: 54.8 },
  { id: "proposal-reviewer", node: "proposal.reviewer", label: "Proposal reviewer", glyph: "PR", x: 39.4, y: 54.8 },
  { id: "experiment-deep-lit", node: "literature.reader", label: "Experiment deep literature", glyph: "DL", x: 55.3, y: 34.6 },
  { id: "experiment-auditor", node: "experiment.auditor", label: "Experiment auditor", glyph: "EA", x: 53.7, y: 44 },
  { id: "experiment-scientist", node: "experiment.scientist", label: "Experiment scientist", glyph: "ES", x: 47.2, y: 55 },
  { id: "experiment-coder", node: "experiment.coder", label: "Experiment coder", glyph: "EC", x: 56.9, y: 51.3 },
  { id: "experiment-reviewer", node: "experiment.reviewer", label: "Experiment reviewer", glyph: "ER", x: 62.8, y: 55.2 },
  { id: "paper-area-chair", node: "paper.area-chair", label: "Paper area chair", glyph: "AC", x: 81.7, y: 36.3, size: 5.2 },
  { id: "paper-rhetorician", node: "paper.rhetorician", label: "Paper rhetorician", glyph: "RH", x: 78.3, y: 46, size: 5.2 },
  { id: "paper-killer", node: "paper.killer", label: "Paper killer reviewer", glyph: "KR", x: 85.4, y: 45.6, size: 5.2 },
  { id: "paper-writer", node: "paper.writer", label: "Paper writer", glyph: "PW", x: 81.7, y: 57.7, size: 5.2 },
  { id: "paper-evidence", node: "paper.evidence", label: "Evidence auditor", glyph: "A1", x: 75.39, y: 72.15, size: 5.2 },
  { id: "paper-citations", node: "paper.citations", label: "Citation auditor", glyph: "A2", x: 72.86, y: 79.08, size: 5.2 },
  { id: "paper-reproducibility", node: "paper.reproducibility", label: "Reproducibility auditor", glyph: "A3", x: 77.95, y: 79.08, size: 5.2 },
  { id: "paper-reviewer", node: "paper.reviewer", label: "Paper reviewer", glyph: "RV", x: 88.96, y: 74.53, size: 5.2 },
  { id: "deep-idea-review", node: "idea.reviewer", label: "Deep literature idea reviewer", glyph: "IR", x: 20.9, y: 71.5, size: 5.2 },
  { id: "deep-proposal-review", node: "proposal.reviewer", label: "Deep literature proposal reviewer", glyph: "PR", x: 17.9, y: 78.8, size: 5.2 },
  { id: "deep-experiment-review", node: "experiment.reviewer", label: "Deep literature experiment reviewer", glyph: "ER", x: 23.3, y: 78.8, size: 5.2 },
  { id: "deep-readers", node: "literature.reader", label: "Deep literature readers", glyph: "N×", x: 36.4, y: 68.8, size: 5.2 },
  { id: "deep-coordinator", node: "literature.reader", label: "Deep literature coordinator", glyph: "DL", x: 36.4, y: 79, size: 5.2 },
  { id: "deep-idea-refiner", node: "idea.refiner", label: "Deep literature idea refiner", glyph: "IF", x: 52.4, y: 71.5, size: 5.2 },
  { id: "deep-proposal-refiner", node: "proposal.refiner", label: "Deep literature proposal refiner", glyph: "PF", x: 49.5, y: 79, size: 5.2 },
  { id: "deep-scientist", node: "experiment.scientist", label: "Deep literature experiment scientist", glyph: "ES", x: 54.5, y: 79, size: 5.2 },
];

export function WorkflowGraph({ active }: { active: ActiveInvocation[] }) {
  const primary = active.filter((invocation) => !invocation.advisor);
  const advisors = active.filter((invocation) => invocation.advisor);
  const represented = new Set(HOTSPOTS.map((hotspot) => hotspot.node));
  const auxiliary = primary.filter((invocation) => !invocation.nodeId || !represented.has(invocation.nodeId));

  return (
    <section className="lmw-panel overflow-hidden" aria-label="Live Agon workflow">
      <header className="lmw-panel-head">
        <h2 className="lmw-label">factory graph</h2>
        <span className="ml-auto inline-flex items-center gap-1 font-mono text-[10px] text-faint">
          <Activity className="h-3 w-3" aria-hidden /> {primary.length} primary · {advisors.length} advisor
        </span>
      </header>
      <div className="relative aspect-[1.78/1] min-h-[310px] overflow-hidden bg-[#eeece5]">
        <img src={workflowImage} alt="Agon research factory workflow" className="absolute inset-0 h-full w-full object-contain opacity-80" />
        {HOTSPOTS.map((hotspot) => {
          const invocation = primary.find((item) => item.nodeId === hotspot.node);
          return (
            <button
              key={hotspot.id}
              type="button"
              aria-label={`${hotspot.label}${invocation ? ` — ${invocation.model} active` : " — idle"}`}
              title={invocation ? `${hotspot.label} · ${invocation.backend}/${invocation.model}` : hotspot.label}
              className={cn(
                "absolute grid -translate-x-1/2 -translate-y-1/2 place-items-center rounded-sm border font-mono text-[clamp(6px,0.65vw,10px)] font-semibold transition-[box-shadow,border-color,background] motion-reduce:transition-none",
                invocation
                  ? "z-10 border-primary bg-primary text-primary-foreground shadow-[0_0_0_3px_color-mix(in_srgb,var(--primary)_25%,transparent),0_0_24px_color-mix(in_srgb,var(--primary)_65%,transparent)]"
                  : "border-foreground/30 bg-background/90 text-foreground/80 hover:border-primary/70",
              )}
              style={{ left: `${hotspot.x}%`, top: `${hotspot.y}%`, width: `${hotspot.size ?? 6.2}%`, aspectRatio: "1" }}
            >
              {hotspot.glyph}
            </button>
          );
        })}
      </div>
      <div className="grid gap-px border-t border-hairline bg-hairline sm:grid-cols-2">
        <div className="bg-panel px-3 py-2">
          <p className="lmw-label mb-1">auxiliary execution rail</p>
          <div className="flex min-h-6 flex-wrap gap-1.5">
            {auxiliary.length === 0 ? <span className="font-mono text-[10px] text-faint">no auxiliary role active</span> : auxiliary.map((item) => (
              <span key={item.id} className="rounded border border-primary/40 bg-primary/10 px-2 py-1 font-mono text-[10px] text-primary">
                {item.role} · {item.model}
              </span>
            ))}
          </div>
        </div>
        <div className="bg-panel px-3 py-2">
          <p className="lmw-label mb-1 inline-flex items-center gap-1"><ShieldCheck className="h-3 w-3" aria-hidden /> advisor lane</p>
          <div className="flex min-h-6 flex-wrap gap-1.5">
            {advisors.length === 0 ? <span className="font-mono text-[10px] text-faint">advisors off</span> : advisors.map((item) => (
              <span key={item.id} className="rounded border border-violet/40 bg-violet/10 px-2 py-1 font-mono text-[10px] text-violet">
                {item.role} · {item.model}
              </span>
            ))}
          </div>
        </div>
      </div>
    </section>
  );
}
