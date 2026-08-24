import { useCallback, useEffect, useMemo, useState } from "react";
import { Link, NavLink, Outlet, useLocation } from "react-router";
import {
  Boxes,
  BookOpen,
  Braces,
  Command as CommandIcon,
  Gauge,
  History,
  LogOut,
  Menu,
  Radio,
  Server,
  Settings2,
  Workflow,
  type LucideIcon,
} from "lucide-react";
import { logout, type Session } from "~/lib/api/session";
import { useModules, useSystemInfo } from "~/lib/queries";
import { cn } from "~/lib/utils";
import { shortId } from "~/lib/format";
import { TooltipProvider } from "~/components/ui/tooltip";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "~/components/ui/dropdown-menu";
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
  SheetTrigger,
} from "~/components/ui/sheet";
import { Toaster } from "~/components/ui/sonner";
import { CommandPalette, type DialogId } from "~/components/command-palette";
import { EnrollDialog } from "~/components/dialogs/enroll-dialog";
import { ImportRecipeDialog } from "~/components/dialogs/import-recipe-dialog";
import { PlanDeploymentDialog } from "~/components/dialogs/plan-deployment-dialog";
import { BenchmarkDialog } from "~/components/dialogs/benchmark-dialog";

interface NavSection {
  id: string;
  label: string;
  route: string;
  icon: LucideIcon;
}

const MODULE_SECTIONS: Record<string, NavSection> = {
  fleet: { id: "fleet", label: "Fleet", route: "/fleet", icon: Server },
  library: { id: "library", label: "Library", route: "/library", icon: BookOpen },
  serving: { id: "serving", label: "Serving", route: "/serving", icon: Radio },
  benchmarks: { id: "benchmarks", label: "Benchmarks", route: "/benchmarks", icon: Gauge },
  runs: { id: "runs", label: "Runs", route: "/runs", icon: History },
  "coding-traces": { id: "coding-traces", label: "Coding Traces", route: "/coding-traces", icon: Braces },
  settings: { id: "settings", label: "Settings", route: "/settings", icon: Settings2 },
};

const ALWAYS_SECTIONS: NavSection[] = [
  { id: "workshop", label: "Workshop", route: "/", icon: Workflow },
  { id: "runs", label: "Runs", route: "/runs", icon: History },
  { id: "modules", label: "Modules", route: "/modules", icon: Boxes },
];

/**
 * Authenticated layout: fixed nav rail (full at ≥1024px, icons at
 * 768–1023px, off-canvas below 768px), topbar, outlet, command palette,
 * global dialogs, toasts.
 */
export function AppShell({ session }: { session: Session | null }) {
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [dialog, setDialog] = useState<DialogId | null>(null);
  const [mobileNavOpen, setMobileNavOpen] = useState(false);
  const { data: modules } = useModules();
  const { data: sys } = useSystemInfo();

  const sections = useMemo<NavSection[]>(() => {
    const ordered: NavSection[] = [];
    for (const m of [...(modules ?? [])].sort((a, b) => (a.nav?.order ?? 0) - (b.nav?.order ?? 0))) {
      const s = MODULE_SECTIONS[m.id];
      if (s) ordered.push(s);
    }
    const all: NavSection[] = [ALWAYS_SECTIONS[0], ...ordered];
    if (!ordered.some((s) => s.id === "runs")) all.push(ALWAYS_SECTIONS[1]);
    all.push(ALWAYS_SECTIONS[2]);
    return all;
  }, [modules]);

  const onKey = useCallback(
    (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setPaletteOpen((o) => !o);
      }
    },
    [],
  );

  useEffect(() => {
    if (typeof window !== "undefined") {
      window.addEventListener("keydown", onKey);
      return () => window.removeEventListener("keydown", onKey);
    }
    return undefined;
  }, [onKey]);

  const closeDialog = () => setDialog(null);
  const openDialog = useCallback((id: DialogId) => setDialog(id), []);

  const signOut = async () => {
    await logout();
    window.location.assign("/login");
  };

  const pathname = useLocation().pathname;
  const nav = (
    <nav aria-label="Primary" className="flex flex-1 flex-col gap-0.5 px-2">
      {sections.map((s) => {
        const Icon = s.icon;
        const active =
          s.route === "/"
            ? pathname === "/"
            : pathname.startsWith(s.route);
        return (
          <NavLink
            key={s.id}
            to={s.route}
            end={s.route === "/"}
            onClick={() => setMobileNavOpen(false)}
            aria-current={active ? "page" : undefined}
            className={cn(
              "control flex items-center gap-2.5 rounded px-2.5 py-2 text-sm max-lg:justify-center",
              active
                ? "bg-raised text-primary"
                : "text-foreground/75 hover:bg-raised/60 hover:text-foreground",
            )}
            title={s.label}
          >
            <Icon className="h-4 w-4 shrink-0" aria-hidden />
            <span className="hidden font-display text-[15px] font-medium tracking-wide lg:inline">
              {s.label}
            </span>
          </NavLink>
        );
      })}
    </nav>
  );

  const railFooter = (
    <div className="border-t border-hairline px-3 py-3">
      <div className="flex items-center gap-2 max-lg:justify-center">
        <DropdownMenu>
          <DropdownMenuTrigger
            aria-label="Account menu"
            className="control flex w-full items-center gap-2 rounded border border-hairline bg-raised px-2 py-1.5 hover:border-primary/50 max-lg:w-auto max-lg:border-0 max-lg:bg-transparent max-lg:p-1"
          >
            <span className="flex h-6 w-6 items-center justify-center rounded bg-primary/15 font-display text-xs font-semibold text-primary">
              {(session?.username ?? "op").slice(0, 1).toUpperCase()}
            </span>
            <span className="hidden min-w-0 flex-1 truncate text-left font-mono text-xs text-foreground lg:inline">
              {session?.username ?? "operator"}
            </span>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="start" className="w-56">
            <DropdownMenuLabel className="font-mono text-xs">
              {session?.username ?? "operator"}
            </DropdownMenuLabel>
            <DropdownMenuSeparator />
            {sys ? (
              <DropdownMenuItem disabled className="justify-between font-mono text-[11px] text-muted">
                <span>server</span>
                <span>
                  {sys.version} · {shortId(sys.commit)}
                </span>
              </DropdownMenuItem>
            ) : null}
            <DropdownMenuItem onClick={() => void signOut()}>
              <LogOut className="mr-2 h-4 w-4" aria-hidden />
              Sign out
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
    </div>
  );

  const brand = (
    <Link
      to="/"
      className="flex items-center gap-2.5 border-b border-hairline px-3 py-3.5 max-lg:justify-center"
      aria-label="Local Model Works — workshop"
    >
      <span
        aria-hidden
        className="flex h-7 w-7 shrink-0 items-center justify-center rounded border border-primary/50 bg-primary/10 font-display text-sm font-bold text-primary"
      >
        LM
      </span>
      <span className="hidden min-w-0 flex-col leading-tight lg:flex">
        <span className="font-display text-[15px] font-semibold tracking-[0.08em] text-foreground">
          LOCAL MODEL WORKS
        </span>
        <span className="font-mono text-[10px] text-muted">operator console</span>
      </span>
    </Link>
  );

  const currentSection =
    sections.find((s) =>
      s.route === "/" ? pathname === "/" : pathname.startsWith(s.route),
    )?.label ?? "Workshop";

  return (
    <TooltipProvider delayDuration={200}>
      <div className="lmw-bg min-h-screen">
        {/* desktop rail */}
        <aside className="fixed inset-y-0 left-0 z-30 hidden w-16 flex-col border-r border-hairline bg-panel md:flex lg:w-[220px]">
          {brand}
          {nav}
          {railFooter}
        </aside>

        {/* mobile off-canvas nav */}
        <Sheet open={mobileNavOpen} onOpenChange={setMobileNavOpen}>
          <SheetContent side="left" className="w-72 p-0">
            <div className="flex h-full flex-col">
              {brand}
              {nav}
              {railFooter}
            </div>
            <SheetHeader className="sr-only">
              <SheetTitle>Navigation</SheetTitle>
            </SheetHeader>
          </SheetContent>
          <SheetTrigger aria-label="Open navigation" className="hidden" />
        </Sheet>

        {/* main column */}
        <div className="flex min-h-screen flex-col md:pl-16 lg:pl-[220px]">
          <header className="sticky top-0 z-20 flex items-center gap-3 border-b border-hairline bg-background/90 px-4 py-2.5 backdrop-blur-sm">
            <button
              type="button"
              onClick={() => setMobileNavOpen(true)}
              aria-label="Open navigation"
              className="control rounded border border-hairline p-1.5 text-foreground md:hidden"
            >
              <Menu className="h-4 w-4" aria-hidden />
            </button>
            <h1 className="lmw-label !text-foreground/90">{currentSection}</h1>
            <div className="ml-auto flex items-center gap-2">
              <button
                type="button"
                onClick={() => setPaletteOpen(true)}
                className="control hidden items-center gap-2 rounded border border-hairline bg-raised px-2.5 py-1.5 text-xs text-muted hover:border-primary/50 hover:text-foreground sm:flex"
                aria-label="Open command palette"
              >
                <CommandIcon className="h-3.5 w-3.5" aria-hidden />
                commands
                <kbd className="rounded border border-hairline px-1 font-mono text-[10px]">⌘K</kbd>
              </button>
              <button
                type="button"
                onClick={() => setPaletteOpen(true)}
                className="control rounded border border-hairline p-1.5 text-muted hover:text-foreground sm:hidden"
                aria-label="Open command palette"
              >
                <CommandIcon className="h-4 w-4" aria-hidden />
              </button>
            </div>
          </header>
          <main id="main-content" className="mx-auto w-full max-w-[1440px] flex-1 px-4 py-4 lg:px-6">
            <Outlet />
          </main>
        </div>

        <CommandPalette
          open={paletteOpen}
          onOpenChange={setPaletteOpen}
          actions={{
            enroll: () => openDialog("enroll"),
            "import-recipe": () => openDialog("import-recipe"),
            "plan-deployment": () => openDialog("plan-deployment"),
            benchmark: () => openDialog("benchmark"),
          }}
          sections={sections.map(({ id, label, route }) => ({ id, label, route }))}
        />

        {dialog === "enroll" ? <EnrollDialog open onOpenChange={(o) => !o && closeDialog()} /> : null}
        {dialog === "import-recipe" ? (
          <ImportRecipeDialog open onOpenChange={(o) => !o && closeDialog()} />
        ) : null}
        {dialog === "plan-deployment" ? (
          <PlanDeploymentDialog open onOpenChange={(o) => !o && closeDialog()} />
        ) : null}
        {dialog === "benchmark" ? (
          <BenchmarkDialog open onOpenChange={(o) => !o && closeDialog()} />
        ) : null}

        <Toaster theme="dark" position="bottom-right" richColors closeButton />
      </div>
    </TooltipProvider>
  );
}
