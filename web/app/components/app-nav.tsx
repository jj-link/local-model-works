import { useEffect, useRef, useState } from "react";
import { NavLink } from "react-router";
import {
  ChartNoAxesCombined,
  ChevronDown,
  FileCode2,
  Gauge,
  History,
  LayoutDashboard,
  Library,
  MessageSquare,
  Network,
  PackageOpen,
  Radio,
  Server,
  Waypoints,
  type LucideIcon,
} from "lucide-react";
import { cn } from "~/lib/utils";
import { ROADMAP_PAGES } from "~/roadmap-pages";

export type AppNavItem = {
  id: string;
  label: string;
  path: string;
  icon: LucideIcon;
};

export type AppNavGroup = {
  id: string;
  label: string;
  icon: LucideIcon;
  grouped?: boolean;
  items: AppNavItem[];
};

export function buildAppNavigation(enabledModuleIds: ReadonlySet<string>): AppNavGroup[] {
  const groups: AppNavGroup[] = [
    {
      id: "workshop",
      label: "Workshop",
      icon: LayoutDashboard,
      grouped: true,
      items: [
        { id: "overview", label: "Overview", path: "/", icon: LayoutDashboard },
      ],
    },
  ];

  if (enabledModuleIds.has("fleet")) {
    groups.push({
      id: "fleet",
      label: "Fleet",
      icon: Server,
      items: [
        { id: "fleet-nodes", label: "Nodes", path: "/fleet/nodes", icon: Server },
        { id: "fleet-fabrics", label: "Fabrics", path: "/fleet/fabrics", icon: Network },
      ],
    });
  }
  if (enabledModuleIds.has("library")) {
    groups.push({
      id: "recipes",
      label: "Recipes",
      icon: Library,
      items: [
        { id: "recipe-catalog", label: "Catalog", path: "/library/recipes", icon: Library },
        { id: "recipe-builder", label: "Recipe Builder", path: "/library/builder", icon: FileCode2 },
        { id: "profiles-sharing", ...ROADMAP_PAGES.profiles, icon: Library },
      ],
    });
  }
  groups.push({
    id: "knowledge",
    label: ROADMAP_PAGES.knowledge.label,
    icon: PackageOpen,
    items: [{ id: "knowledge-route", ...ROADMAP_PAGES.knowledge, icon: PackageOpen }],
  });
  if (enabledModuleIds.has("serving")) {
    groups.push({
      id: "serving",
      label: "Serving",
      icon: Radio,
      items: [{ id: "deployments", label: "Serving", path: "/serving/deployments", icon: Radio }],
    });
  }
  groups.push({
    id: "benchmarks",
    label: "Benchmarks",
    icon: Gauge,
    grouped: true,
    items: [
      ...(enabledModuleIds.has("benchmarks")
        ? [{ id: "benchmarks-overview", label: "Overview", path: "/benchmarks", icon: ChartNoAxesCombined }]
        : []),
      { id: "community-leaderboard", ...ROADMAP_PAGES.leaderboard, icon: Gauge },
    ],
  });
  groups.push({
    id: "research",
    label: "Research",
    icon: ChartNoAxesCombined,
    grouped: true,
    items: [
      { id: "autoresearch", ...ROADMAP_PAGES.autoresearch, icon: ChartNoAxesCombined },
      { id: "experiment-builder", ...ROADMAP_PAGES.experiments, icon: FileCode2 },
      { id: "workflow-builder", ...ROADMAP_PAGES.workflows, icon: Waypoints },
    ],
  });
  groups.push(
    {
      id: "scheduled",
      label: ROADMAP_PAGES.scheduled.label,
      icon: History,
      items: [{ id: "scheduled-route", ...ROADMAP_PAGES.scheduled, icon: History }],
    },
    {
      id: "usage",
      label: ROADMAP_PAGES.usage.label,
      icon: Gauge,
      items: [{ id: "usage-route", ...ROADMAP_PAGES.usage, icon: Gauge }],
    },
    {
      id: "fine-tuning",
      label: ROADMAP_PAGES.fineTuning.label,
      icon: Server,
      items: [{ id: "fine-tuning-route", ...ROADMAP_PAGES.fineTuning, icon: Server }],
    },
    {
      id: "projects",
      label: ROADMAP_PAGES.projects.label,
      icon: PackageOpen,
      items: [{ id: "projects-route", ...ROADMAP_PAGES.projects, icon: PackageOpen }],
    },
  );
  if (enabledModuleIds.has("chat")) {
    groups.push({
      id: "chat",
      label: "Chat",
      icon: MessageSquare,
      items: [{ id: "chat-route", label: "Chat", path: "/chat", icon: MessageSquare }],
    });
  }
  return groups;
}

export function isAppNavItemActive(pathname: string, path: string): boolean {
  return path === "/" ? pathname === "/" : pathname === path || pathname.startsWith(`${path}/`);
}

export function flattenAppNavigation(groups: AppNavGroup[]): AppNavItem[] {
  return groups.flatMap((group) => group.items);
}

export function AppNav({
  groups,
  pathname,
  onNavigate,
  compact = false,
}: {
  groups: AppNavGroup[];
  pathname: string;
  onNavigate: () => void;
  compact?: boolean;
}) {
  const [expanded, setExpanded] = useState<Record<string, boolean>>(() =>
    Object.fromEntries(groups.filter((group) => group.items.length > 1).map((group) => [group.id, true])),
  );
  const previousPathname = useRef(pathname);

  useEffect(() => {
    if (previousPathname.current === pathname) return;
    previousPathname.current = pathname;
    setExpanded((current) => {
      const next = { ...current };
      for (const group of groups) {
        if (group.items.some((item) => isAppNavItemActive(pathname, item.path))) {
          next[group.id] = true;
        }
      }
      return next;
    });
  }, [groups, pathname]);

  return (
    <nav aria-label="Primary" className="flex flex-1 flex-col gap-1 overflow-y-auto px-2 py-2">
      {groups.map((group) => {
        const parentActive = group.items.some((item) => isAppNavItemActive(pathname, item.path));
        if (!group.grouped && group.items.length === 1) {
          const item = group.items[0];
          const Icon = item.icon;
          return (
            <NavLink
              key={group.id}
              to={item.path}
              end={item.path === "/"}
              onClick={onNavigate}
              className={({ isActive }) =>
                cn(
                  "control flex min-h-9 items-center gap-2.5 rounded-md px-2.5 py-2 text-sm",
                  compact && "max-lg:justify-center",
                  isActive || parentActive
                    ? "bg-primary/10 text-primary"
                    : "text-foreground/75 hover:bg-raised hover:text-foreground",
                )
              }
              title={item.label}
            >
              <Icon className="h-4 w-4 shrink-0" aria-hidden />
              <span className={cn("font-medium", compact && "hidden lg:inline")}>{item.label}</span>
            </NavLink>
          );
        }

        const open = expanded[group.id] ?? true;
        const GroupIcon = group.icon;
        return (
          <section key={group.id} aria-label={group.label} data-nav-group={group.id}>
            <button
              type="button"
              aria-expanded={open}
              onClick={() => setExpanded((current) => ({ ...current, [group.id]: !open }))}
              className={cn(
                "control flex min-h-9 w-full items-center gap-2.5 rounded-md px-2.5 py-2 text-left text-sm",
                compact && "max-lg:justify-center",
                parentActive
                  ? "bg-raised text-foreground"
                  : "text-foreground/75 hover:bg-raised hover:text-foreground",
              )}
              title={group.label}
            >
              <GroupIcon className={cn("h-4 w-4 shrink-0", parentActive && "text-primary")} aria-hidden />
              <span className={cn("flex-1 font-display text-[0.95rem] font-semibold", compact && "hidden lg:inline")}>{group.label}</span>
              <ChevronDown
                className={cn("h-3.5 w-3.5 transition-transform", compact && "hidden lg:block", open && "rotate-180")}
                aria-hidden
              />
            </button>
            {open ? (
              <div className={cn("mt-0.5 grid gap-0.5 ml-3 border-l border-hairline pl-2", compact && "max-lg:ml-0 max-lg:border-l-0 max-lg:pl-0")}>
                {group.items.map((item) => {
                  const Icon = item.icon;
                  return (
                    <NavLink
                      key={item.id}
                      to={item.path}
                      end={item.path === "/"}
                      onClick={onNavigate}
                      className={({ isActive }) =>
                        cn(
                          "control flex min-h-8 items-center gap-2 rounded-md px-2 py-1.5 text-sm",
                          compact && "max-lg:justify-center",
                          isActive
                            ? "bg-primary/10 text-primary"
                            : "text-muted hover:bg-raised hover:text-foreground",
                        )
                      }
                      title={item.label}
                    >
                      <Icon className="h-3.5 w-3.5 shrink-0" aria-hidden />
                      <span className={cn(compact && "hidden lg:inline")}>{item.label}</span>
                    </NavLink>
                  );
                })}
              </div>
            ) : null}
          </section>
        );
      })}
    </nav>
  );
}
