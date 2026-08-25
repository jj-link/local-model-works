import { useCallback, useEffect, useMemo, useState } from "react";
import { Link, Outlet, useLocation } from "react-router";
import { Command as CommandIcon, FileText, LogOut, Menu } from "lucide-react";
import { logout, type Session } from "~/lib/api/session";
import { useModules, useSystemInfo } from "~/lib/queries";
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
import {
  AppNav,
  buildAppNavigation,
  flattenAppNavigation,
  isAppNavItemActive,
} from "~/components/app-nav";

/**
 * Authenticated layout: Sample A grouped rail (full at ≥1024px, icons at
 * 768–1023px, off-canvas below 768px), sticky topbar, outlet, command
 * palette, global dialogs, account controls, and toasts.
 */
export function AppShell({ session }: { session: Session | null }) {
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [dialog, setDialog] = useState<DialogId | null>(null);
  const [mobileNavOpen, setMobileNavOpen] = useState(false);
  const { data: modules } = useModules();
  const { data: sys } = useSystemInfo();
  const pathname = useLocation().pathname;

  const enabledModuleIds = useMemo(
    () => new Set((modules ?? []).map((module) => module.id)),
    [modules],
  );
  const navigation = useMemo(
    () => buildAppNavigation(enabledModuleIds),
    [enabledModuleIds],
  );
  const navItems = useMemo(() => flattenAppNavigation(navigation), [navigation]);
  const currentItem = useMemo(
    () =>
      [...navItems]
        .sort((left, right) => right.path.length - left.path.length)
        .find((item) => isAppNavItemActive(pathname, item.path)),
    [navItems, pathname],
  );

  const onKey = useCallback((event: KeyboardEvent) => {
    if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "k") {
      event.preventDefault();
      setPaletteOpen((open) => !open);
    }
  }, []);

  useEffect(() => {
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onKey]);

  const closeDialog = () => setDialog(null);
  const openDialog = useCallback((id: DialogId) => setDialog(id), []);

  const signOut = async () => {
    await logout();
    window.location.assign("/login");
  };

  const brand = (
    <Link
      to="/"
      className="flex h-[58px] items-center gap-2.5 border-b border-hairline px-3 max-lg:justify-center"
      aria-label="Local Model Works — overview"
    >
      <span
        aria-hidden
        className="flex h-7 w-7 shrink-0 items-center justify-center rounded-sm border border-primary/40 bg-primary/10 font-display text-sm font-bold text-primary"
      >
        LM
      </span>
      <span className="hidden min-w-0 flex-col leading-tight lg:flex">
        <span className="font-display text-[0.95rem] font-semibold tracking-[0.08em] text-foreground">
          LOCAL MODEL WORKS
        </span>
        <span className="font-mono text-[10px] text-muted">operator console</span>
      </span>
    </Link>
  );

  const railFooter = (
    <div className="border-t border-hairline px-3 py-3">
      <DropdownMenu>
        <DropdownMenuTrigger
          aria-label="Account menu"
          className="control flex w-full items-center gap-2 rounded-md border border-hairline bg-raised px-2 py-1.5 hover:border-primary/50 max-lg:w-auto max-lg:border-0 max-lg:bg-transparent max-lg:p-1"
        >
          <span className="flex h-6 w-6 items-center justify-center rounded-sm bg-primary/10 font-display text-xs font-semibold text-primary">
            {(session?.username ?? "op").slice(0, 1).toUpperCase()}
          </span>
          <span className="hidden min-w-0 flex-1 truncate text-left font-mono text-xs text-foreground lg:inline">
            {session?.username ?? "operator"}
          </span>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="start" className="w-60">
          <DropdownMenuLabel className="font-mono text-xs">
            {session?.username ?? "operator"}
          </DropdownMenuLabel>
          <DropdownMenuSeparator />
          {sys ? (
            <DropdownMenuItem disabled className="justify-between font-mono text-[11px] text-muted">
              <span>server</span>
              <span>{sys.version} · {shortId(sys.commit)}</span>
            </DropdownMenuItem>
          ) : null}
          <DropdownMenuSeparator />
          <DropdownMenuItem asChild>
            <a href="/licenses/Atkinson-Hyperlegible-OFL.txt" target="_blank" rel="noreferrer">
              <FileText className="mr-2 h-4 w-4" aria-hidden />
              Atkinson license
            </a>
          </DropdownMenuItem>
          <DropdownMenuItem asChild>
            <a href="/licenses/Commit-Mono-OFL.txt" target="_blank" rel="noreferrer">
              <FileText className="mr-2 h-4 w-4" aria-hidden />
              Commit Mono license
            </a>
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem onClick={() => void signOut()}>
            <LogOut className="mr-2 h-4 w-4" aria-hidden />
            Sign out
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
    </div>
  );

  return (
    <TooltipProvider delayDuration={200}>
      <div className="lmw-bg min-h-screen">
        <aside className="fixed inset-y-0 left-0 z-30 hidden w-16 flex-col border-r border-hairline bg-panel md:flex lg:w-[220px]">
          {brand}
          <AppNav compact groups={navigation} pathname={pathname} onNavigate={() => undefined} />
          {railFooter}
        </aside>

        <Sheet open={mobileNavOpen} onOpenChange={setMobileNavOpen}>
          <SheetContent side="left" className="w-72 p-0">
            <div className="flex h-full flex-col">
              <Link
                to="/"
                className="flex h-[58px] items-center gap-2.5 border-b border-hairline px-3"
                aria-label="Local Model Works — overview"
              >
                <span
                  aria-hidden
                  className="flex h-7 w-7 shrink-0 items-center justify-center rounded-sm border border-primary/40 bg-primary/10 font-display text-sm font-bold text-primary"
                >
                  LM
                </span>
                <span className="flex min-w-0 flex-col leading-tight">
                  <span className="font-display text-[0.95rem] font-semibold tracking-[0.08em] text-foreground">
                    LOCAL MODEL WORKS
                  </span>
                  <span className="font-mono text-[10px] text-muted">operator console</span>
                </span>
              </Link>
              <AppNav
                groups={navigation}
                pathname={pathname}
                onNavigate={() => setMobileNavOpen(false)}
              />
              {railFooter}
            </div>
            <SheetHeader className="sr-only">
              <SheetTitle>Navigation</SheetTitle>
            </SheetHeader>
          </SheetContent>
          <SheetTrigger aria-label="Open navigation" className="hidden" />
        </Sheet>

        <div className="flex min-h-screen flex-col md:pl-16 lg:pl-[220px]">
          <header className="sticky top-0 z-20 flex h-[49px] items-center gap-3 border-b border-hairline bg-background/95 px-4 backdrop-blur-sm">
            <button
              type="button"
              onClick={() => setMobileNavOpen(true)}
              aria-label="Open navigation"
              className="control rounded-md border border-hairline bg-panel p-1.5 text-foreground md:hidden"
            >
              <Menu className="h-4 w-4" aria-hidden />
            </button>
            <h1 className="font-display text-base font-semibold text-foreground">
              {currentItem?.label ?? "Overview"}
            </h1>
            <div className="ml-auto flex items-center gap-2">
              <button
                type="button"
                onClick={() => setPaletteOpen(true)}
                className="control hidden items-center gap-2 rounded-md border border-hairline bg-panel px-2.5 py-1.5 text-xs text-muted hover:border-primary/50 hover:text-foreground sm:flex"
                aria-label="Open command palette"
              >
                <CommandIcon className="h-3.5 w-3.5" aria-hidden />
                commands
                <kbd className="rounded border border-hairline px-1 font-mono text-[10px]">⌘K</kbd>
              </button>
              <button
                type="button"
                onClick={() => setPaletteOpen(true)}
                className="control rounded-md border border-hairline bg-panel p-1.5 text-muted hover:text-foreground sm:hidden"
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
          sections={navItems.map((item) => ({
            id: item.id,
            label: item.label,
            route: item.path,
          }))}
        />

        {dialog === "enroll" ? <EnrollDialog open onOpenChange={(open) => !open && closeDialog()} /> : null}
        {dialog === "import-recipe" ? (
          <ImportRecipeDialog open onOpenChange={(open) => !open && closeDialog()} />
        ) : null}
        {dialog === "plan-deployment" ? (
          <PlanDeploymentDialog open onOpenChange={(open) => !open && closeDialog()} />
        ) : null}
        {dialog === "benchmark" ? (
          <BenchmarkDialog open onOpenChange={(open) => !open && closeDialog()} />
        ) : null}

        <Toaster theme="light" position="bottom-right" richColors closeButton />
      </div>
    </TooltipProvider>
  );
}
