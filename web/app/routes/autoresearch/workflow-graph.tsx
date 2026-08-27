import type { CSSProperties, ReactNode } from "react";
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

function stageClass(hotspot: Hotspot): string {
  if (hotspot.node.startsWith("idea.")) return "arf-stage-idea";
  if (hotspot.node.startsWith("proposal.")) return "arf-stage-proposal";
  if (hotspot.node.startsWith("experiment.")) return "arf-stage-experiment";
  if (hotspot.node.startsWith("paper.")) return "arf-stage-paper";
  return "arf-stage-literature";
}

function roleLabel(hotspot: Hotspot): string {
  if (hotspot.id === "deep-readers") return "lit-readers";
  if (hotspot.id === "deep-coordinator" || hotspot.id.endsWith("deep-lit")) return "deep-lit";
  return hotspot.node.split(".").at(-1)?.replaceAll("-", " ") ?? hotspot.label;
}

export function WorkflowGraph({ active, paused = false, emptyState }: { active: ActiveInvocation[]; paused?: boolean; emptyState?: ReactNode }) {
  const primary = active.filter((invocation) => !invocation.advisor);
  const advisors = active.filter((invocation) => invocation.advisor);
  const represented = new Set(HOTSPOTS.map((hotspot) => hotspot.node));
  const auxiliary = primary.filter((invocation) => !invocation.nodeId || !represented.has(invocation.nodeId));

  return (
    <section className="arf-panel arf-flow-panel" aria-label="Live Agon workflow">
      <header className="arf-panel-head">
        <div className="arf-panel-title">
          <h2>Research topology</h2>
          <span className="arf-panel-kicker">Live execution graph</span>
        </div>
        <div className="arf-legend" aria-label="Execution state legend">
          <span><i /> waiting</span>
          <span><i className="arf-live" /> generating</span>
        </div>
      </header>
      <div className="arf-flow-canvas">
        <div className="arf-chart-reference">
          <img src={workflowImage} alt="Agon workflow from topic radar through idea, proposal, experiment, deep literature, and paper factories" />
          {HOTSPOTS.map((hotspot) => {
            const invocation = primary.find((item) => item.nodeId === hotspot.node);
            const model = invocation?.model || "—";
            const state = invocation ? (paused ? "paused" : "generating") : "waiting";
            return (
              <button
                key={hotspot.id}
                type="button"
                aria-label={`${hotspot.label} — ${model} — ${state}`}
                title={invocation ? `${hotspot.label} · ${invocation.backend}/${model}` : `${hotspot.label} · idle`}
                className={cn(
                  "arf-chart-node",
                  stageClass(hotspot),
                  invocation && "arf-live",
                  invocation && paused && "arf-paused",
                )}
                style={{
                  "--x": `${hotspot.x}%`,
                  "--y": `${hotspot.y}%`,
                  "--size": `${hotspot.size ?? 7}%`,
                } as CSSProperties}
              >
                <span className="arf-node-shell">
                  <span className="arf-node-avatar">{hotspot.glyph}</span>
                  <span className="arf-node-role">{roleLabel(hotspot)}</span>
                  <span className="arf-node-model">{model}</span>
                </span>
              </button>
            );
          })}
          {emptyState ? <div className="arf-topology-empty">{emptyState}</div> : null}
        </div>
      </div>
      <footer className="arf-chart-attribution">
        <span>Exact Agon workflow · interactive model/status overlay</span>
        <a href="https://github.com/AutoResearch-Factory/Agon" target="_blank" rel="noreferrer">Source: AutoResearch-Factory/Agon</a>
        <span className="arf-execution-lanes">
          <span><strong>Auxiliary:</strong> {auxiliary.length ? auxiliary.map((item) => `${item.role} · ${item.model}`).join(", ") : "none active"}</span>
          <span><strong>Advisors:</strong> {advisors.length ? advisors.map((item) => `${item.role} · ${item.model}`).join(", ") : "off"}</span>
        </span>
      </footer>
    </section>
  );
}
