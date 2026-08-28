import { useEffect, useMemo, useState, type CSSProperties } from "react";
import { Plus, Save, X } from "lucide-react";
import { cn } from "~/lib/utils";
import workflowImage from "../../../../third_party/agon/figures/figure_xp.png";
import type { ActiveInvocation } from "./events";
import {
  effectiveFallbacks,
  effectiveProvider,
  GRAPH_ROLE_BY_NODE,
  providerKey,
  providerLabel,
  withConfiguredProviders,
  type ProviderOption,
  type ProviderRef,
} from "./routing";

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
  { id: "paper-area-chair", node: "paper.area-chair", label: "Paper area chair", glyph: "AC", x: 81.7, y: 36.3, size: 5.8 },
  { id: "paper-rhetorician", node: "paper.rhetorician", label: "Paper rhetorician", glyph: "RH", x: 78.3, y: 46, size: 5.8 },
  { id: "paper-killer", node: "paper.killer", label: "Paper killer reviewer", glyph: "KR", x: 85.4, y: 45.6, size: 5.8 },
  { id: "paper-writer", node: "paper.writer", label: "Paper writer", glyph: "PW", x: 81.7, y: 57.7, size: 5.8 },
  { id: "paper-evidence", node: "paper.evidence", label: "Evidence auditor", glyph: "A1", x: 75.39, y: 72.15, size: 5.8 },
  { id: "paper-citations", node: "paper.citations", label: "Citation auditor", glyph: "A2", x: 72.86, y: 79.08, size: 5.8 },
  { id: "paper-reproducibility", node: "paper.reproducibility", label: "Reproducibility auditor", glyph: "A3", x: 77.95, y: 79.08, size: 5.8 },
  { id: "paper-reviewer", node: "paper.reviewer", label: "Paper reviewer", glyph: "RV", x: 88.96, y: 74.53, size: 5.8 },
  { id: "deep-idea-review", node: "idea.reviewer", label: "Deep literature idea reviewer", glyph: "IR", x: 20.9, y: 71.5, size: 5.8 },
  { id: "deep-proposal-review", node: "proposal.reviewer", label: "Deep literature proposal reviewer", glyph: "PR", x: 17.9, y: 78.8, size: 5.8 },
  { id: "deep-experiment-review", node: "experiment.reviewer", label: "Deep literature experiment reviewer", glyph: "ER", x: 23.3, y: 78.8, size: 5.8 },
  { id: "deep-readers", node: "literature.reader", label: "Deep literature readers", glyph: "N×", x: 36.4, y: 68.8, size: 5.8 },
  { id: "deep-coordinator", node: "literature.reader", label: "Deep literature coordinator", glyph: "DL", x: 36.4, y: 79, size: 5.8 },
  { id: "deep-idea-refiner", node: "idea.refiner", label: "Deep literature idea refiner", glyph: "IF", x: 52.4, y: 71.5, size: 5.8 },
  { id: "deep-proposal-refiner", node: "proposal.refiner", label: "Deep literature proposal refiner", glyph: "PF", x: 49.5, y: 79, size: 5.8 },
  { id: "deep-scientist", node: "experiment.scientist", label: "Deep literature experiment scientist", glyph: "ES", x: 54.5, y: 79, size: 5.8 },
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

export interface WorkflowRouting {
  roles: Record<string, ProviderRef>;
  fallbacks: Record<string, ProviderRef[]>;
  defaults: Record<string, ProviderRef>;
  providers: ProviderOption[];
  saving: boolean;
  error: string | null;
  onSave: (role: string, primary: ProviderRef | undefined, fallbackOverride: ProviderRef[] | undefined) => void;
}

function RoutingPopover({ hotspot, routing, onClose }: { hotspot: Hotspot; routing: WorkflowRouting; onClose: () => void }) {
  const role = GRAPH_ROLE_BY_NODE[hotspot.node];
  const effective = effectiveProvider(role, routing.roles, routing.defaults);
  const effectiveFallback = useMemo(() => effectiveFallbacks(role, routing.fallbacks), [role, routing.fallbacks]);
  const [primaryKey, setPrimaryKey] = useState(routing.roles[role] ? providerKey(routing.roles[role]) : "inherit");
  const [inheritFallbacks, setInheritFallbacks] = useState(effectiveFallback.inherited);
  const [fallbackKeys, setFallbackKeys] = useState(effectiveFallback.refs.map(providerKey));
  const aliases = HOTSPOTS.filter((item) => item.node === hotspot.node).length;
  const options = useMemo(() => withConfiguredProviders(
    routing.providers,
    [effective.ref, ...effectiveFallback.refs, routing.roles[role]],
  ), [effective.ref, effectiveFallback.refs, role, routing.providers, routing.roles]);
  const byKey = useMemo(() => new Map(options.map((option) => [option.key, option.ref])), [options]);

  useEffect(() => {
    setPrimaryKey(routing.roles[role] ? providerKey(routing.roles[role]) : "inherit");
    setInheritFallbacks(effectiveFallback.inherited);
    setFallbackKeys(effectiveFallback.refs.map(providerKey));
  }, [effectiveFallback.inherited, effectiveFallback.refs, role, routing.roles]);

  const selectedKeys = fallbackKeys.filter(Boolean);
  const duplicateFallback = !inheritFallbacks && new Set(selectedKeys).size !== selectedKeys.length;
  const primaryConflict = !inheritFallbacks && primaryKey !== "inherit" && selectedKeys.includes(primaryKey);
  const validationError = duplicateFallback
    ? "A provider can appear only once in the fallback chain."
    : primaryConflict
      ? "Primary provider cannot also be a fallback."
      : null;

  return (
    <div className={cn("arf-routing-popover", hotspot.x > 68 && "arf-routing-popover-left", hotspot.y > 64 && "arf-routing-popover-up")} role="dialog" aria-label={`Model assignment for ${hotspot.label}`} onKeyDown={(event) => event.key === "Escape" && onClose()}>
      <header>
        <div><strong>{hotspot.label}</strong><span>{role}</span></div>
        <button type="button" aria-label="Close model assignment" onClick={onClose}><X aria-hidden /></button>
      </header>
      <label>Primary provider
        <select aria-label={`${role} primary provider`} value={primaryKey} onChange={(event) => setPrimaryKey(event.target.value)}>
          <option value="inherit">Inherit project/module default</option>
          {options.map((option) => <option key={option.key} value={option.key} disabled={!option.available && option.key !== primaryKey}>{option.label}{option.available ? "" : " (unavailable)"}</option>)}
        </select>
      </label>
      <p className="arf-routing-effective">Effective: {effective.ref ? providerLabel(effective.ref) : "unassigned"} · {effective.source.replaceAll("-", " ")}</p>
      {role !== "default" ? <label className="arf-routing-check"><input type="checkbox" checked={inheritFallbacks} onChange={(event) => setInheritFallbacks(event.target.checked)} /> Inherit project fallback chain</label> : null}
      {!inheritFallbacks ? <div className="arf-routing-fallbacks">
        <span>Fallback chain</span>
        {fallbackKeys.map((key, index) => <div key={`${index}-${key}`}>
          <select aria-label={`${role} fallback ${index + 1}`} value={key} onChange={(event) => setFallbackKeys((current) => current.map((item, itemIndex) => itemIndex === index ? event.target.value : item))}>
            <option value="">Select provider</option>
            {options.map((option) => <option key={option.key} value={option.key} disabled={!option.available && option.key !== key}>{option.label}{option.available ? "" : " (unavailable)"}</option>)}
          </select>
          <button type="button" aria-label={`Remove fallback ${index + 1}`} onClick={() => setFallbackKeys((current) => current.filter((_, itemIndex) => itemIndex !== index))}><X aria-hidden /></button>
        </div>)}
        <button type="button" className="arf-routing-add" onClick={() => setFallbackKeys((current) => [...current, ""])}><Plus aria-hidden /> Add fallback</button>
      </div> : null}
      {aliases > 1 ? <p className="arf-routing-notice">Shared assignment: this role appears at {aliases} topology points.</p> : null}
      {validationError ? <p className="arf-inline-error" role="alert">{validationError}</p> : null}
      {routing.error ? <p className="arf-inline-error" role="alert">{routing.error}</p> : null}
      <footer>
        <span>Applies to the next invocation.</span>
        <button type="button" disabled={routing.saving || Boolean(validationError)} onClick={() => routing.onSave(
          role,
          primaryKey === "inherit" ? undefined : byKey.get(primaryKey),
          inheritFallbacks ? undefined : fallbackKeys.map((key) => byKey.get(key)).filter((ref): ref is ProviderRef => Boolean(ref)),
        )}><Save aria-hidden /> {routing.saving ? "Saving…" : "Save"}</button>
      </footer>
    </div>
  );
}

export function WorkflowGraph({ active, paused = false, routing }: { active: ActiveInvocation[]; paused?: boolean; routing?: WorkflowRouting }) {
  const [openHotspot, setOpenHotspot] = useState<string | null>(null);
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
            const routingEnabled = Boolean(routing && GRAPH_ROLE_BY_NODE[hotspot.node]);
            return (
              <div
                key={hotspot.id}
                className={cn(
                  "arf-chart-hotspot",
                  stageClass(hotspot),
                  invocation && "arf-live",
                  invocation && paused && "arf-paused",
                  openHotspot === hotspot.id && "arf-routing-open",
                )}
                style={{
                  "--x": `${hotspot.x}%`,
                  "--y": `${hotspot.y}%`,
                  "--size": `${hotspot.size ?? 7}%`,
                } as CSSProperties}
              >
                <button
                  type="button"
                  aria-label={`${hotspot.label} — ${model} — ${state}${routingEnabled ? " — configure model" : ""}`}
                  aria-expanded={routingEnabled ? openHotspot === hotspot.id : undefined}
                  title={invocation ? `${hotspot.label} · ${invocation.backend}/${model}` : `${hotspot.label} · idle`}
                  className="arf-chart-node"
                  onClick={() => routingEnabled && setOpenHotspot((current) => current === hotspot.id ? null : hotspot.id)}
                >
                  <span className="arf-node-shell">
                    <span className="arf-node-avatar">{hotspot.glyph}</span>
                    <span className="arf-node-role">{roleLabel(hotspot)}</span>
                    <span className="arf-node-model">{model}</span>
                  </span>
                </button>
                {routing && openHotspot === hotspot.id ? <RoutingPopover hotspot={hotspot} routing={routing} onClose={() => setOpenHotspot(null)} /> : null}
              </div>
            );
          })}
        </div>
      </div>
      <footer className="arf-chart-attribution">
        <span>Exact Agon workflow · click a role to assign its model</span>
        <a href="https://github.com/AutoResearch-Factory/Agon" target="_blank" rel="noreferrer">Source: AutoResearch-Factory/Agon</a>
        <span className="arf-execution-lanes">
          <span><strong>Auxiliary:</strong> {auxiliary.length ? auxiliary.map((item) => `${item.role} · ${item.model}`).join(", ") : "none active"}</span>
          <span><strong>Advisors:</strong> {advisors.length ? advisors.map((item) => `${item.role} · ${item.model}`).join(", ") : "off"}</span>
        </span>
      </footer>
    </section>
  );
}
