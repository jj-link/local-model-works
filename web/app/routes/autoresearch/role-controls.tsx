import { useEffect, useState } from "react";
import { Save, Shield } from "lucide-react";
import { Button } from "~/components/ui/button";
import type { AutoResearchProject } from "~/lib/api";
import { useUpdateAutoResearchProject } from "~/lib/queries";

const ROLES = [
  "idea-creator", "idea-refiner", "idea-reviewer", "proposal-refiner", "proposal-reviewer",
  "deep-lit-reader", "experiment-scientist", "experiment-coder", "experiment-auditor", "experiment-reviewer",
  "paper-writer", "paper-auditor-evidence", "paper-auditor-citations", "paper-auditor-reproducibility",
  "paper-rhetorician", "paper-reviewer", "paper-killer-reviewer", "paper-area-chair",
] as const;

type AdvisorConfig = { enabled: boolean; backlog: "off" | 1 | 3 | 5 };

export function RoleControls({ project }: { project: AutoResearchProject }) {
  const [advisors, setAdvisors] = useState<Record<string, AdvisorConfig>>({});
  const [maxRounds, setMaxRounds] = useState(project.config.paper_max_rounds);
  const update = useUpdateAutoResearchProject();

  useEffect(() => {
    const next: Record<string, AdvisorConfig> = {};
    for (const role of ROLES) {
      const configured = project.config.advisors?.[role];
      next[role] = { enabled: configured?.enabled ?? false, backlog: configured?.backlog ?? 1 };
    }
    setAdvisors(next);
    setMaxRounds(project.config.paper_max_rounds);
  }, [project]);

  return (
    <section className="lmw-panel overflow-hidden">
      <header className="lmw-panel-head"><h2 className="lmw-label inline-flex items-center gap-1"><Shield className="h-3.5 w-3.5" aria-hidden /> role supervision</h2><span className="ml-auto font-mono text-[10px] text-faint">advisors never veto or mutate artifacts</span></header>
      <div className="grid gap-px bg-hairline sm:grid-cols-2 xl:grid-cols-3">
        {ROLES.map((role) => {
          const config = advisors[role] ?? { enabled: false, backlog: 1 as const };
          return (
            <div key={role} className="flex items-center gap-2 bg-panel px-3 py-2">
              <label className="flex min-w-0 flex-1 cursor-pointer items-center gap-2 font-mono text-[10px]">
                <input type="checkbox" checked={config.enabled} onChange={(event) => setAdvisors((previous) => ({ ...previous, [role]: { ...config, enabled: event.target.checked } }))} />
                <span className="truncate">{role}</span>
              </label>
              <select aria-label={`${role} advisor backlog`} disabled={!config.enabled} className="h-7 rounded border border-hairline bg-background px-1 font-mono text-[10px] disabled:opacity-40" value={config.backlog} onChange={(event) => setAdvisors((previous) => ({ ...previous, [role]: { ...config, backlog: event.target.value === "off" ? "off" : Number(event.target.value) as 1 | 3 | 5 } }))}>
                <option value="off">off</option><option value={1}>1</option><option value={3}>3</option><option value={5}>5</option>
              </select>
            </div>
          );
        })}
      </div>
      <footer className="flex flex-wrap items-end gap-3 border-t border-hairline p-3">
        <label className="font-mono text-[10px] text-faint">paper round cap
          <input type="number" min={1} max={20} value={maxRounds} onChange={(event) => setMaxRounds(Math.max(1, Number(event.target.value)))} className="mt-1 block h-8 w-24 rounded border border-hairline bg-background px-2 font-mono text-xs text-foreground" />
        </label>
        <Button className="ml-auto" size="sm" disabled={update.isPending} onClick={() => update.mutate({ id: project.id, version: project.version, body: { config: { ...project.config, paper_max_rounds: maxRounds, advisors } } })}><Save aria-hidden /> save next-invocation settings</Button>
      </footer>
    </section>
  );
}
