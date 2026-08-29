import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useNavigate } from "react-router";
import {
  Command as CommandIcon,
  FileText,
  FolderPlus,
  Gauge,
  MousePointerClick,
  Rocket,
  Server,
  type LucideIcon,
} from "lucide-react";
import { useDeployments, useRuns } from "~/lib/queries";
import { isRunTerminal, shortId, stateInfo, TONE_TEXT } from "~/lib/format";
import { cn } from "~/lib/utils";
export type DialogId = "enroll" | "import-recipe" | "plan-deployment" | "benchmark";


interface PaletteItem {
  id: string;
  label: string;
  hint?: string;
  icon: LucideIcon;
  group: "actions" | "logs" | "navigate";
  onSelect: () => void;
  tone?: string;
}

const LOG_CAP = 8;

/**
 * Ctrl/Cmd-K palette: operator actions (enroll, install, plan, benchmark),
 * open-logs shortcuts for recent active runs and live deployments, and
 * navigation to any visible section.
 */
export function CommandPalette({
  open,
  onOpenChange,
  actions,
  sections,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  actions: Record<DialogId, () => void>;
  sections: { id: string; label: string; route: string }[];
}) {
  const navigate = useNavigate();
  const [query, setQuery] = useState("");
  const [index, setIndex] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLUListElement>(null);

  const { data: runsData } = useRuns({});
  const { data: deployments } = useDeployments();

  useEffect(() => {
    if (open) {
      setQuery("");
      setIndex(0);
      const t = setTimeout(() => inputRef.current?.focus(), 30);
      return () => clearTimeout(t);
    }
  }, [open]);

  const items = useMemo<PaletteItem[]>(() => {
    const act = (id: string, label: string, icon: LucideIcon): PaletteItem => ({
      id,
      label,
      icon,
      group: "actions",
      onSelect: () => actions[id as DialogId](),
    });
    const base: PaletteItem[] = [
      act("enroll", "Enroll node", Server),
      act("import-recipe", "Install recipe", FolderPlus),
      act("plan-deployment", "Launch deployment", Rocket),
      act("benchmark", "Run benchmark", Gauge),
    ];
    const logItems: PaletteItem[] = [];
    const runs = (runsData?.items ?? []).filter((r) => !isRunTerminal(r.state)).slice(0, LOG_CAP);
    for (const r of runs) {
      logItems.push({
        id: `log-run-${r.id}`,
        label: `Open logs — run ${shortId(r.id)}`,
        hint: `${r.module}/${r.kind}`,
        icon: FileText,
        group: "logs",
        tone: TONE_TEXT[stateInfo(r.state).tone],
        onSelect: () => navigate(`/runs/${r.id}`),
      });
    }
    const live = (deployments ?? []).filter((d) => d.desired_state === "running").slice(0, LOG_CAP);
    for (const d of live) {
      logItems.push({
        id: `log-dep-${d.id}`,
        label: `Open logs — ${d.recipe_name}`,
        hint: d.profile,
        icon: FileText,
        group: "logs",
        tone: TONE_TEXT[stateInfo(d.observed_state).tone],
        onSelect: () => navigate(`/serving/deployments/${d.id}`),
      });
    }
    const navItems: PaletteItem[] = sections.map((s) => ({
      id: `nav-${s.id}`,
      label: `Go to ${s.label}`,
      hint: s.route,
      icon: MousePointerClick,
      group: "navigate",
      onSelect: () => navigate(s.route),
    }));
    return [...base, ...logItems, ...navItems];
  }, [actions, sections, runsData, deployments, navigate]);

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return items;
    return items.filter((i) => `${i.label} ${i.hint ?? ""}`.toLowerCase().includes(q));
  }, [items, query]);

  useEffect(() => {
    setIndex(0);
  }, [query]);

  useEffect(() => {
    const el = listRef.current?.children[index] as HTMLElement | undefined;
    el?.scrollIntoView({ block: "nearest" });
  }, [index]);

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setIndex((i) => Math.min(i + 1, filtered.length - 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setIndex((i) => Math.max(i - 1, 0));
    } else if (e.key === "Enter") {
      e.preventDefault();
      const item = filtered[index];
      if (item) {
        onOpenChange(false);
        item.onSelect();
      }
    }
  };

  if (!open) return null;

  const renderGroup = (group: PaletteItem["group"], label: string): ReactNode => {
    const groupItems = filtered.filter((i) => i.group === group);
    if (groupItems.length === 0) return null;
    return (
      <>
        <li className="px-3 pb-1 pt-3">
          <span className="lmw-label">{label}</span>
        </li>
        {groupItems.map((item) => {
          const idx = filtered.indexOf(item);
          const Icon = item.icon;
          return (
            <li key={item.id}>
              <button
                type="button"
                data-palette-item
                onClick={() => {
                  onOpenChange(false);
                  item.onSelect();
                }}
                onMouseEnter={() => setIndex(idx)}
                className={cn(
                  "flex w-full items-center gap-2.5 rounded px-3 py-2 text-left text-sm control",
                  idx === index ? "bg-raised text-foreground" : "text-foreground/80",
                )}
              >
                <Icon className={cn("h-4 w-4 shrink-0", item.tone ?? "text-muted")} aria-hidden />
                <span className="truncate">{item.label}</span>
                {item.hint ? (
                  <span className="ml-auto shrink-0 font-mono text-[11px] text-muted">{item.hint}</span>
                ) : null}
              </button>
            </li>
          );
        })}
      </>
    );
  };

  return (
    <div
      className="fixed inset-0 z-50 flex items-start justify-center bg-background/70 p-4 pt-[12vh] backdrop-blur-[2px]"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onOpenChange(false);
      }}
      role="dialog"
      aria-modal="true"
      aria-label="Command palette"
    >
      <div className="lmw-panel w-full max-w-lg overflow-hidden shadow-2xl shadow-black/50">
        <div className="flex items-center gap-2 border-b border-hairline px-3">
          <CommandIcon className="h-4 w-4 text-muted" aria-hidden />
          <input
            ref={inputRef}
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            onKeyDown={onKeyDown}
            placeholder="Type a command or destination…"
            aria-label="Command palette search"
            className="w-full bg-transparent py-3 text-sm text-foreground outline-none placeholder:text-muted"
          />
          <kbd className="rounded border border-hairline px-1.5 py-0.5 font-mono text-[10px] text-muted">esc</kbd>
        </div>
        <ul ref={listRef} className="max-h-80 overflow-auto p-2" role="listbox" aria-label="Commands">
          {renderGroup("actions", "actions")}
          {renderGroup("logs", "open logs")}
          {renderGroup("navigate", "navigate")}
          {filtered.length === 0 ? (
            <li className="px-3 py-6 text-center text-xs text-muted">no matches</li>
          ) : null}
        </ul>
      </div>
    </div>
  );
}
