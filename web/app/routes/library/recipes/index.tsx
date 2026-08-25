import { useMemo, useState } from "react";
import { Link } from "react-router";
import { Download, Search, SlidersHorizontal } from "lucide-react";
import { Badge } from "~/components/ui/badge";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import { useRecipes } from "~/lib/queries";
import { EmptyState } from "~/components/empty-state";
import { ImportRecipeDialog } from "~/components/dialogs/import-recipe-dialog";
import { PlanDeploymentDialog } from "~/components/dialogs/plan-deployment-dialog";
import { shortDigest, wallClock } from "~/lib/format";
import type { Recipe } from "~/lib/api";
import { cn } from "~/lib/utils";

const SOURCE_SEAL: Record<string, string> = {
  catalog: "CA",
  oci: "OC",
  git: "GT",
  local: "LC",
};

const TRUST_ORDER: Record<Recipe["trust_state"], number> = {
  verified: 0,
  local: 1,
  untrusted: 2,
};

type SortMode = "name" | "newest" | "trust";

function recipeDisplayName(recipe: Recipe): string {
  return recipe.display_name || recipe.name;
}

function compareDigest(left: Recipe, right: Recipe): number {
  return left.digest < right.digest ? -1 : left.digest > right.digest ? 1 : 0;
}

export default function RecipesRoute() {
  const { data, isPending, isError, error, refetch } = useRecipes();
  const [importOpen, setImportOpen] = useState(false);
  const [planOpen, setPlanOpen] = useState(false);
  const [selectedRecipeDigest, setSelectedRecipeDigest] = useState<string>();
  const [search, setSearch] = useState("");
  const [trustFilters, setTrustFilters] = useState<Recipe["trust_state"][]>([]);
  const [sourceFilters, setSourceFilters] = useState<string[]>([]);
  const [nodeCount, setNodeCount] = useState("");
  const [sort, setSort] = useState<SortMode>("name");

  const sourceTypes = useMemo(
    () =>
      Array.from(new Set((data ?? []).map((recipe) => recipe.source?.type).filter(Boolean)))
        .sort() as string[],
    [data],
  );
  const nodeCounts = useMemo(
    () =>
      Array.from(
        new Set(
          (data ?? [])
            .map((recipe) => recipe.compatibility?.nodeCount)
            .filter((count): count is number => typeof count === "number" && count > 0),
        ),
      ).sort((left, right) => left - right),
    [data],
  );

  const visibleRecipes = useMemo(() => {
    const term = search.trim().toLowerCase();
    const selectedNodeCount = nodeCount ? Number(nodeCount) : null;
    return (data ?? [])
      .filter((recipe) => {
        const searchable = [
          recipe.name,
          recipe.display_name,
          recipe.source?.remote,
          recipe.source?.type,
          recipe.digest,
        ]
          .filter(Boolean)
          .join(" ")
          .toLowerCase();
        if (term && !searchable.includes(term)) return false;
        if (trustFilters.length > 0 && !trustFilters.includes(recipe.trust_state)) return false;
        if (sourceFilters.length > 0 && (!recipe.source?.type || !sourceFilters.includes(recipe.source.type))) return false;
        if (selectedNodeCount !== null && recipe.compatibility?.nodeCount !== selectedNodeCount) return false;
        return true;
      })
      .sort((left, right) => {
        if (sort === "newest") {
          const installedDelta = Date.parse(right.installed_at) - Date.parse(left.installed_at);
          if (installedDelta !== 0) return installedDelta;
          return compareDigest(left, right);
        }
        if (sort === "trust") {
          const trustDelta = TRUST_ORDER[left.trust_state] - TRUST_ORDER[right.trust_state];
          return trustDelta || compareDigest(left, right);
        }
        const leftName = recipeDisplayName(left).toLowerCase();
        const rightName = recipeDisplayName(right).toLowerCase();
        if (leftName !== rightName) return leftName < rightName ? -1 : 1;
        return compareDigest(left, right);
      });
  }, [data, nodeCount, search, sort, sourceFilters, trustFilters]);

  return (
    <div className="grid gap-4">
      <section className="lmw-panel overflow-hidden">
        <header className="lmw-panel-head flex flex-wrap items-center gap-3">
          <div>
            <p className="lmw-label">Recipe library</p>
            <h1 className="font-display text-2xl font-semibold">Installed catalog</h1>
          </div>
          <span className="font-mono text-[11px] text-muted">{(data ?? []).length} installed</span>
          <Button size="sm" className="ml-auto" onClick={() => setImportOpen(true)}>
            <Download aria-hidden /> Import recipe
          </Button>
        </header>

        {isPending ? (
          <p className="px-3 py-10 text-center font-mono text-xs text-muted">Loading recipes…</p>
        ) : isError ? (
          <EmptyState
            className="m-3"
            title="Cannot load recipes"
            detail={error instanceof Error ? error.message : undefined}
            onRetry={() => void refetch()}
          />
        ) : (data ?? []).length === 0 ? (
          <EmptyState
            className="m-3"
            title="No recipes installed"
            hint="Import from a signed catalog, an OCI reference, a pinned Git commit, or a local path."
            action={<Button onClick={() => setImportOpen(true)}>Import recipe</Button>}
          />
        ) : (
          <>
            <div className="grid gap-3 border-b border-hairline bg-raised p-3 lg:grid-cols-[minmax(16rem,1fr)_auto_auto]">
              <label className="relative block">
                <span className="sr-only">Search recipes</span>
                <Search className="pointer-events-none absolute left-2.5 top-1/2 h-4 w-4 -translate-y-1/2 text-muted" aria-hidden />
                <Input
                  value={search}
                  onChange={(event) => setSearch(event.target.value)}
                  placeholder="Search name, source, or digest"
                  className="pl-8"
                />
              </label>
              <label className="flex items-center gap-2 text-xs text-muted">
                <span>Nodes</span>
                <select
                  aria-label="Node count"
                  value={nodeCount}
                  onChange={(event) => setNodeCount(event.target.value)}
                  className="control h-8 rounded-md border border-hairline bg-card px-2 text-sm text-foreground"
                >
                  <option value="">All</option>
                  {nodeCounts.map((count) => (
                    <option key={count} value={count}>{count}</option>
                  ))}
                </select>
              </label>
              <label className="flex items-center gap-2 text-xs text-muted">
                <span>Sort</span>
                <select
                  aria-label="Sort recipes"
                  value={sort}
                  onChange={(event) => setSort(event.target.value as SortMode)}
                  className="control h-8 rounded-md border border-hairline bg-card px-2 text-sm text-foreground"
                >
                  <option value="name">Name</option>
                  <option value="newest">Newest</option>
                  <option value="trust">Trust</option>
                </select>
              </label>
              <div className="flex flex-wrap items-center gap-1.5 lg:col-span-3">
                <SlidersHorizontal className="mr-1 h-3.5 w-3.5 text-muted" aria-hidden />
                {(["verified", "local", "untrusted"] as const).map((trust) => {
                  const selected = trustFilters.includes(trust);
                  return (
                    <button
                      key={trust}
                      type="button"
                      aria-pressed={selected}
                      aria-label={`Trust ${trust}`}
                      onClick={() =>
                        setTrustFilters((current) =>
                          selected ? current.filter((value) => value !== trust) : [...current, trust],
                        )
                      }
                      className={cn(
                        "control rounded-full border px-2.5 py-1 font-mono text-[11px]",
                        selected
                          ? "border-primary/40 bg-primary/10 text-primary"
                          : "border-hairline bg-card text-muted hover:text-foreground",
                      )}
                    >
                      {trust}
                    </button>
                  );
                })}
                <span className="mx-1 h-4 w-px bg-hairline" aria-hidden />
                {sourceTypes.map((source) => {
                  const selected = sourceFilters.includes(source);
                  return (
                    <button
                      key={source}
                      type="button"
                      aria-pressed={selected}
                      aria-label={`Source ${source}`}
                      onClick={() =>
                        setSourceFilters((current) =>
                          selected ? current.filter((value) => value !== source) : [...current, source],
                        )
                      }
                      className={cn(
                        "control rounded-full border px-2.5 py-1 font-mono text-[11px]",
                        selected
                          ? "border-primary/40 bg-primary/10 text-primary"
                          : "border-hairline bg-card text-muted hover:text-foreground",
                      )}
                    >
                      {source}
                    </button>
                  );
                })}
              </div>
            </div>

            {visibleRecipes.length === 0 ? (
              <EmptyState
                className="m-3"
                title="No recipes match"
                hint="Adjust the search or selected facets."
              />
            ) : (
              <div className="grid grid-cols-1 gap-3 p-3 md:grid-cols-2 xl:grid-cols-3">
                {visibleRecipes.map((recipe) => {
                  const fabric = recipe.compatibility?.fabric;
                  const fabricTransport = typeof fabric?.transport === "string" ? fabric.transport : "Not required";
                  const engine = recipe.engine === "vllm" ? "vLLM" : recipe.engine === "sglang" ? "SGLang" : recipe.engine || "Not reported";
                  return (
                    <article key={recipe.digest} className="lmw-panel flex min-h-72 flex-col overflow-hidden">
                      <div className="flex items-start gap-3 border-b border-hairline bg-raised px-4 py-3">
                        <span className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full border border-hairline bg-card font-mono text-[10px] font-semibold text-primary" aria-label={`${recipe.source?.type ?? "unknown"} source`}>
                          {SOURCE_SEAL[recipe.source?.type ?? ""] ?? "—"}
                        </span>
                        <div className="min-w-0 flex-1">
                          <Link to={`/library/recipes/${recipe.digest}`} className="lmw-link font-display text-lg font-semibold leading-tight">
                            {recipeDisplayName(recipe)}
                          </Link>
                          <p className="mt-1 truncate font-mono text-[10px] text-muted" title={recipe.source?.remote ?? recipe.source?.reference}>
                            {recipe.source?.type ?? "unknown source"}{recipe.source?.remote ? ` · ${recipe.source.remote}` : ""}
                          </p>
                        </div>
                        <Badge
                          variant="outline"
                          className={cn(
                            recipe.trust_state === "verified" && "border-ok/40 text-ok",
                            recipe.trust_state === "local" && "border-warn/40 text-warn",
                            recipe.trust_state === "untrusted" && "border-fault/40 text-fault",
                          )}
                        >
                          {recipe.trust_state}
                        </Badge>
                      </div>

                      <div className="flex flex-1 flex-col gap-4 p-4">
                        {recipe.description ? (
                          <p className="line-clamp-3 text-sm leading-5 text-muted">{recipe.description}</p>
                        ) : null}
                        <dl className="grid grid-cols-2 gap-x-4 gap-y-3 text-sm">
                          <div><dt className="lmw-label">Model</dt><dd className="mt-0.5">{recipe.model || "Not reported"}</dd></div>
                          <div><dt className="lmw-label">Engine</dt><dd className="mt-0.5">{engine}</dd></div>
                          <div><dt className="lmw-label">Nodes</dt><dd className="mt-0.5">{recipe.compatibility?.nodeCount ?? "Not reported"}</dd></div>
                          <div><dt className="lmw-label">Fabric</dt><dd className="mt-0.5 uppercase">{fabricTransport}</dd></div>
                          <div><dt className="lmw-label">Version</dt><dd className="mt-0.5 font-mono text-xs">{recipe.version}{recipe.version_count && recipe.version_count > 1 ? ` · ${recipe.version_count} installed` : ""}</dd></div>
                          <div><dt className="lmw-label">License</dt><dd className="mt-0.5">{recipe.license || "Not reported"}</dd></div>
                        </dl>
                        <div className="mt-auto flex items-end justify-between gap-3 border-t border-hairline pt-3">
                          <div className="min-w-0 font-mono text-[10px] text-muted">
                            <p className="truncate" title={recipe.digest}>{shortDigest(recipe.digest)}</p>
                            <p title={recipe.installed_at}>Installed {wallClock(recipe.installed_at)}</p>
                          </div>
                          <Button
                            size="sm"
                            variant="outline"
                            onClick={() => {
                              setSelectedRecipeDigest(recipe.digest);
                              setPlanOpen(true);
                            }}
                          >
                            Choose hardware
                          </Button>
                        </div>
                      </div>
                    </article>
                  );
                })}
              </div>
            )}
          </>
        )}
      </section>

      <ImportRecipeDialog open={importOpen} onOpenChange={setImportOpen} />
      <PlanDeploymentDialog
        open={planOpen}
        onOpenChange={setPlanOpen}
        initialRecipeDigest={selectedRecipeDigest}
      />
    </div>
  );
}
