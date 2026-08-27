import { useMemo, useState } from "react";
import { useCheckRecipeUpdates, useCreateRecipeDraft, useDeployments, useRecipes } from "~/lib/queries";
import { ImportRecipeDialog } from "~/components/dialogs/import-recipe-dialog";
import { PlanDeploymentDialog } from "~/components/dialogs/plan-deployment-dialog";
import { shortDigest } from "~/lib/format";
import type { Recipe } from "~/lib/api";
import { toast } from "sonner";
import { Link, useNavigate } from "react-router";
import "./catalog.css";

const SOURCE_SEAL: Record<string, string> = {
  catalog: "◈",
  oci: "◉",
  git: "⎇",
  local: "▣",
};

const UPDATE_LABEL: Record<string, string> = {
  available: "Update available",
  current: "Up to date",
  error: "Check failed",
};

function recipeDisplayName(recipe: Recipe): string {
  return recipe.display_name || recipe.name;
}

function compareDigest(left: Recipe, right: Recipe): number {
  return left.digest < right.digest ? -1 : left.digest > right.digest ? 1 : 0;
}

export default function RecipesRoute() {
  const navigate = useNavigate();
  const recipesQuery = useRecipes();
  const deploymentsQuery = useDeployments();
  const checkUpdates = useCheckRecipeUpdates();
  const createDraft = useCreateRecipeDraft();
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

  const queueUpdate = async (recipe: Recipe) => {
    const update = recipe.update;
    if (!update?.candidate_revision) return;
    try {
      await createDraft.mutateAsync({
        remote: update.remote,
        revision: update.candidate_revision,
        ...(update.path ? { path: update.path } : {}),
        base_recipe_digest: recipe.digest,
      });
      toast.success("Recipe update queued", {
        description: `${recipeDisplayName(recipe)} · ${update.candidate_revision.slice(0, 12)}`,
      });
      navigate("/library/builder");
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "Recipe update failed");
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
              Installed serving recipes, hardware compatibility, and upstream update status.
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
                  <article
                    key={recipe.digest}
                    className="sample-a-card"
                    aria-label={`Recipe ${recipeDisplayName(recipe)}`}
                  >
                    <span className="sample-a-cardtop">
                      <span
                        className={`sample-a-seal sample-a-seal--${sourceType}`}
                        aria-label={`${sourceType} source`}
                      >
                        {SOURCE_SEAL[sourceType] ?? "·"}
                      </span>
                      <span className="sample-a-cardflags">
                        {recipe.update ? (
                          <span className={`sample-a-update-state is-${recipe.update.state}`}>
                            {UPDATE_LABEL[recipe.update.state] ?? recipe.update.state}
                          </span>
                        ) : null}
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
                    </span>
                    <Link className="sample-a-cardname" to={`/library/recipes/${recipe.digest}`}>
                      {recipeDisplayName(recipe)}
                    </Link>
                    <span className="sample-a-carddesc">
                      {recipe.description || "No description provided for this installed recipe."}
                    </span>
                    <span className="sample-a-compat">{compatibility}</span>
                    <div className="sample-a-updrow">
                      {recipe.update?.state === "available" && recipe.update.candidate_revision ? (
                        <button
                          type="button"
                          className="sample-a-update-action"
                          disabled={createDraft.isPending}
                          onClick={() => void queueUpdate(recipe)}
                        >
                          {createDraft.isPending ? "Queuing update…" : "Update recipe →"}
                        </button>
                      ) : null}
                      <button
                        type="button"
                        className="sample-a-upd"
                        onClick={() => {
                          setSelectedRecipeDigest(recipe.digest);
                          setPlanOpen(true);
                        }}
                      >
                        Choose hardware →
                      </button>
                    </div>
                    <span className="sample-a-meta">
                      {recipe.version} · {shortDigest(recipe.digest)} · {recipe.license || "License not reported"}
                    </span>
                  </article>
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
