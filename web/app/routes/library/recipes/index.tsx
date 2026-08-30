import { useMemo, useState } from "react";
import { useCheckRecipeUpdates, useRecipeRepositories } from "~/lib/queries";
import { ImportRecipeDialog } from "~/components/dialogs/import-recipe-dialog";
import { PlanDeploymentDialog } from "~/components/dialogs/plan-deployment-dialog";
import { RecipeUpdateDialog } from "~/components/dialogs/recipe-update-dialog";
import { shortDigest } from "~/lib/format";
import type { Recipe, RecipeRepository } from "~/lib/api";
import { toast } from "sonner";
import "./catalog.css";

const SOURCE_SEAL: Record<string, string> = {
  catalog: "◈",
  oci: "◉",
  git: "⎇",
  local: "▣",
};


function repositoryDisplayName(repository: RecipeRepository): string {
  return repository.current_recipe?.name || repository.source_url;
}

export default function RecipesRoute() {
  const repositoriesQuery = useRecipeRepositories();
  const checkUpdates = useCheckRecipeUpdates();
  const [search, setSearch] = useState("");
  const [importOpen, setImportOpen] = useState(false);
  const [planOpen, setPlanOpen] = useState(false);
  const [updateOpen, setUpdateOpen] = useState(false);
  const [selectedRecipeDigest, setSelectedRecipeDigest] = useState<string>();
  const [selectedRepositoryId, setSelectedRepositoryId] = useState<string>();

  const visibleRepositories = useMemo(() => {
    const term = search.trim().toLowerCase();
    return [...(repositoriesQuery.data ?? [])]
      .filter((repository) => repository.current_recipe)
      .filter((repository) => {
        if (!term) return true;
        const recipe = repository.current_recipe;
        return [
          recipe?.name,
          repository.source_url,
          repository.source_path,
          recipe?.digest,
        ]
          .filter(Boolean)
          .join(" ")
          .toLowerCase()
          .includes(term);
      })
      .sort((left, right) => {
        const leftName = repositoryDisplayName(left).toLowerCase();
        const rightName = repositoryDisplayName(right).toLowerCase();
        return leftName < rightName ? -1 : leftName > rightName ? 1 : left.id.localeCompare(right.id);
      });
  }, [repositoriesQuery.data, search]);

  const refreshUpdates = async () => {
    try {
      const statuses = await checkUpdates.mutateAsync();
      const available = statuses.filter((status) => status.state === "available").length;
      toast.success("Recipe update check complete", {
        description: available > 0 ? `${available} update${available === 1 ? "" : "s"} available` : "All tracked recipes are current",
      });
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Recipe update check failed");
    }
  };

  return (
    <div className="sample-a-catalog">
      <header className="sample-a-mast">
        <div className="sample-a-brand">
          <span className="sample-a-seal sample-a-seal--local" aria-hidden>L</span>
          <div>
            <h1 className="sample-a-title">Recipe catalog</h1>
            <p className="sample-a-subtitle">
              A curated catalog of models, workers, and workloads for your hardware.
            </p>
          </div>
        </div>
        <div className="sample-a-mastrow">
          <label className="sample-a-search">
            <span className="sample-a-searchicon" aria-hidden>⌕</span>
            <span className="sr-only">Search recipes</span>
            <input
              type="search"
              value={search}
              onChange={(event) => setSearch(event.target.value)}
              placeholder="Search by name, source, or digest…"
            />
          </label>
          <button
            type="button"
            className="sample-a-check"
            disabled={checkUpdates.isPending}
            onClick={() => void refreshUpdates()}
          >
            {checkUpdates.isPending ? "Checking updates…" : "Check updates"}
          </button>
          <button type="button" className="sample-a-import" onClick={() => setImportOpen(true)}>
            Import recipe
          </button>
        </div>
      </header>

      <div className="sample-a-shell">
        {repositoriesQuery.isPending ? (
          <div className="sample-a-banner" role="status">
            <span className="sample-a-bannericon" aria-hidden>…</span>
            <div>
              <p className="sample-a-bannertitle">Loading recipes</p>
              <p className="sample-a-bannerbody">Contacting the recipe registry…</p>
            </div>
          </div>
        ) : repositoriesQuery.isError ? (
          <div className="sample-a-banner" role="alert">
            <span className="sample-a-bannericon" aria-hidden>!</span>
            <div>
              <p className="sample-a-bannertitle">Cannot load recipes</p>
              <p className="sample-a-bannerbody">
                {repositoriesQuery.error instanceof Error
                  ? repositoriesQuery.error.message
                  : "The recipe service did not respond."}
              </p>
              <button type="button" className="lmw-link mt-2 text-xs" onClick={() => void repositoriesQuery.refetch()}>
                Retry
              </button>
            </div>
          </div>
        ) : (repositoriesQuery.data ?? []).length === 0 ? (
          <div className="sample-a-banner" role="status">
            <span className="sample-a-bannericon" aria-hidden>∅</span>
            <div>
              <p className="sample-a-bannertitle">No recipes installed</p>
              <p className="sample-a-bannerbody">Import a recipe to add it to this catalog.</p>
            </div>
          </div>
        ) : visibleRepositories.length === 0 ? (
          <div className="sample-a-banner" role="status">
            <span className="sample-a-bannericon" aria-hidden>∅</span>
            <div>
              <p className="sample-a-bannertitle">No recipes match</p>
              <p className="sample-a-bannerbody">Clear the search to widen the result set.</p>
            </div>
          </div>
        ) : (
          <section aria-labelledby="sample-a-grid-title">
            <header className="sample-a-gridhead">
              <h2 id="sample-a-grid-title" className="sample-a-gridtitle">All recipes</h2>
            </header>
            <div className="sample-a-grid">
              {visibleRepositories.map((repository) => {
                const recipe = repository.current_recipe as Recipe;
                const sourceType = "git";
                const nodeCount = recipe.compatibility?.nodeCount;
                const fabric = recipe.compatibility?.fabric;
                const transport = typeof fabric?.transport === "string" ? fabric.transport : "";
                const installedCount = repository.installed_devices.length;
                const compatibility = nodeCount
                  ? `${nodeCount} ${nodeCount === 1 ? "node" : "nodes"}${transport ? ` · ${transport === "roce" ? "RDMA" : transport.toUpperCase()} fabric` : ""}`
                  : "Compatibility not reported";

                return (
                  <div key={repository.id} className="sample-a-card-shell">
                    <button
                      type="button"
                      className="sample-a-card"
                      aria-label={`Plan launch for ${recipe.name}`}
                      onClick={() => {
                        setSelectedRecipeDigest(recipe.digest);
                        setPlanOpen(true);
                      }}
                    >
                      <span className="sample-a-cardtop">
                        <span
                          className={`sample-a-seal sample-a-seal--${sourceType}`}
                          aria-label={`${sourceType} source`}
                        >
                          {SOURCE_SEAL[sourceType] ?? "·"}
                        </span>
                        <span className="sample-a-cardflags">
                          {repository.update_available ? (
                            <span className="sample-a-update-state is-available">Update available</span>
                          ) : repository.observed_head_commit ? (
                            <span className="sample-a-update-state is-current">Up to date</span>
                          ) : null}
                          {installedCount > 0 ? (
                            <span className="sample-a-install-state is-installed">
                              Recipe ready on {installedCount} {installedCount === 1 ? "node" : "nodes"}
                            </span>
                          ) : (
                            <span className="sample-a-install-state is-not-installed">Recipe package not cached</span>
                          )}
                        </span>
                      </span>
                      <span className="sample-a-cardname">{recipe.name}</span>
                      <span className="sample-a-carddesc">
                        {recipe.description || "No description provided for this installed recipe."}
                      </span>
                      <span className="sample-a-compat">{compatibility}</span>
                      <span className="sample-a-updrow">
                        <span className="sample-a-upd">Plan launch →</span>
                      </span>
                      <span className="sample-a-meta">
                        {recipe.version} · {shortDigest(recipe.digest)} · {recipe.license || "License not reported"}
                      </span>
                    </button>
                    {installedCount > 0 && repository.update_available && repository.update_supported ? (
                      <button
                        type="button"
                        className="sample-a-update-action"
                        onClick={() => {
                          setSelectedRepositoryId(repository.id);
                          setUpdateOpen(true);
                        }}
                      >
                        Update recipe
                      </button>
                    ) : null}
                  </div>
                );
              })}
            </div>
          </section>
        )}
      </div>

      <ImportRecipeDialog
        open={importOpen}
        onOpenChange={setImportOpen}
        onPlan={(recipe) => {
          setSelectedRecipeDigest(recipe.digest);
          setPlanOpen(true);
        }}
      />
      <PlanDeploymentDialog
        open={planOpen}
        onOpenChange={setPlanOpen}
        initialRecipeDigest={selectedRecipeDigest}
      />
      <RecipeUpdateDialog
        open={updateOpen}
        onOpenChange={setUpdateOpen}
        repositoryId={selectedRepositoryId}
      />
    </div>
  );
}
