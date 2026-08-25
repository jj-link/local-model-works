import { useMemo, useState } from "react";
import { useDeployments, useRecipes } from "~/lib/queries";
import { ImportRecipeDialog } from "~/components/dialogs/import-recipe-dialog";
import { PlanDeploymentDialog } from "~/components/dialogs/plan-deployment-dialog";
import { shortDigest } from "~/lib/format";
import type { Recipe } from "~/lib/api";
import "./catalog.css";

const SOURCE_SEAL: Record<string, string> = {
  catalog: "◈",
  oci: "◉",
  git: "⎇",
  local: "▣",
};

function recipeDisplayName(recipe: Recipe): string {
  return recipe.display_name || recipe.name;
}

function compareDigest(left: Recipe, right: Recipe): number {
  return left.digest < right.digest ? -1 : left.digest > right.digest ? 1 : 0;
}

export default function RecipesRoute() {
  const recipesQuery = useRecipes();
  const deploymentsQuery = useDeployments();
  const [search, setSearch] = useState("");
  const [importOpen, setImportOpen] = useState(false);
  const [planOpen, setPlanOpen] = useState(false);
  const [selectedRecipeDigest, setSelectedRecipeDigest] = useState<string>();

  const visibleRecipes = useMemo(() => {
    const term = search.trim().toLowerCase();
    return [...(recipesQuery.data ?? [])]
      .filter((recipe) => {
        if (!term) return true;
        return [
          recipe.name,
          recipe.display_name,
          recipe.source?.type,
          recipe.source?.remote,
          recipe.digest,
        ]
          .filter(Boolean)
          .join(" ")
          .toLowerCase()
          .includes(term);
      })
      .sort((left, right) => {
        const leftName = recipeDisplayName(left).toLowerCase();
        const rightName = recipeDisplayName(right).toLowerCase();
        if (leftName !== rightName) return leftName < rightName ? -1 : 1;
        return compareDigest(left, right);
      });
  }, [recipesQuery.data, search]);

  return (
    <div className="sample-a-catalog">
      <header className="sample-a-mast">
        <div className="sample-a-brand">
          <span className="sample-a-seal sample-a-seal--local" aria-hidden>L</span>
          <div>
            <h1 className="sample-a-title">Sample A</h1>
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
          <button type="button" className="sample-a-import" onClick={() => setImportOpen(true)}>
            Import recipe
          </button>
        </div>
      </header>

      <div className="sample-a-shell">
        {recipesQuery.isPending ? (
          <div className="sample-a-banner" role="status">
            <span className="sample-a-bannericon" aria-hidden>…</span>
            <div>
              <p className="sample-a-bannertitle">Loading recipes</p>
              <p className="sample-a-bannerbody">Contacting the recipe registry…</p>
            </div>
          </div>
        ) : recipesQuery.isError ? (
          <div className="sample-a-banner" role="alert">
            <span className="sample-a-bannericon" aria-hidden>!</span>
            <div>
              <p className="sample-a-bannertitle">Cannot load recipes</p>
              <p className="sample-a-bannerbody">
                {recipesQuery.error instanceof Error
                  ? recipesQuery.error.message
                  : "The recipe service did not respond."}
              </p>
              <button type="button" className="lmw-link mt-2 text-xs" onClick={() => void recipesQuery.refetch()}>
                Retry
              </button>
            </div>
          </div>
        ) : (recipesQuery.data ?? []).length === 0 ? (
          <div className="sample-a-banner" role="status">
            <span className="sample-a-bannericon" aria-hidden>∅</span>
            <div>
              <p className="sample-a-bannertitle">No recipes installed</p>
              <p className="sample-a-bannerbody">Import a recipe to add it to this catalog.</p>
            </div>
          </div>
        ) : visibleRecipes.length === 0 ? (
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
              {visibleRecipes.map((recipe) => {
                const sourceType = recipe.source?.type ?? "unknown";
                const nodeCount = recipe.compatibility?.nodeCount;
                const fabric = recipe.compatibility?.fabric;
                const transport = typeof fabric?.transport === "string" ? fabric.transport : "";
                const installedNodeIds = new Set(
                  (deploymentsQuery.data ?? [])
                    .filter((deployment) => deployment.recipe_digest === recipe.digest)
                    .flatMap((deployment) => (deployment.placements ?? []).map((placement) => placement.node_id)),
                );
                const installedCount = installedNodeIds.size;
                const compatibility = nodeCount
                  ? `${nodeCount} ${nodeCount === 1 ? "node" : "nodes"}${transport ? ` · ${transport === "roce" ? "RDMA" : transport.toUpperCase()} fabric` : ""}`
                  : "Compatibility not reported";

                return (
                  <button
                    type="button"
                    key={recipe.digest}
                    className="sample-a-card"
                    aria-label={`Choose installation hardware for ${recipeDisplayName(recipe)}`}
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
                      {deploymentsQuery.isPending ? (
                        <span className="sample-a-install-state is-checking">Checking devices</span>
                      ) : deploymentsQuery.isError ? (
                        <span className="sample-a-install-state is-not-installed">Status unavailable</span>
                      ) : installedCount > 0 ? (
                        <span className="sample-a-install-state is-installed">
                          Installed on {installedCount} {installedCount === 1 ? "device" : "devices"}
                        </span>
                      ) : (
                        <span className="sample-a-install-state is-not-installed">Not installed</span>
                      )}
                    </span>
                    <span className="sample-a-cardname">{recipeDisplayName(recipe)}</span>
                    <span className="sample-a-carddesc">
                      {recipe.description || "No description provided for this installed recipe."}
                    </span>
                    <span className="sample-a-compat">{compatibility}</span>
                    <span className="sample-a-updrow">
                      <span className="sample-a-upd">Choose installation hardware →</span>
                    </span>
                    <span className="sample-a-meta">
                      {recipe.version} · {shortDigest(recipe.digest)} · {recipe.license || "License not reported"}
                    </span>
                  </button>
                );
              })}
            </div>
          </section>
        )}
      </div>

      <ImportRecipeDialog open={importOpen} onOpenChange={setImportOpen} />
      <PlanDeploymentDialog
        open={planOpen}
        onOpenChange={setPlanOpen}
        initialRecipeDigest={selectedRecipeDigest}
      />
    </div>
  );
}
