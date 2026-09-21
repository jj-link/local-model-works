import { useMemo, useState } from "react";
import { Link, useNavigate } from "react-router";
import { useCheckRecipeUpdates, useDeployments, useRecipeDrafts, useRecipeRepositories, useRecipes } from "~/lib/queries";
import { ImportRecipeDialog } from "~/components/dialogs/import-recipe-dialog";
import { RecipeError, record } from "~/components/recipes/workflow";
import { draftPath, recipePath } from "~/components/recipes/links";
import { recipeDisplayName } from "~/components/recipes/presentation";
import type { Recipe, RecipeRepository } from "~/lib/api";
import "./catalog.css";

type Card = { id: string; path: string; recipe?: Recipe; repository?: RecipeRepository; digests: string[] };
export default function RecipesRoute() {
  const navigate = useNavigate();
  const repositories = useRecipeRepositories(); const recipes = useRecipes(); const drafts = useRecipeDrafts(); const deployments = useDeployments();
  const checkUpdates = useCheckRecipeUpdates();
  const [search, setSearch] = useState(""); const [importOpen, setImportOpen] = useState(false);
  const cards = useMemo(() => {
    const linked = new Set<string>();
    const out: Card[] = (repositories.data || []).map((repository) => {
      const digests = repository.versions.map((version) => version.recipe.digest);
      if (repository.current_recipe) digests.push(repository.current_recipe.digest);
      digests.forEach((digest) => linked.add(digest));
      return { id: repository.id, path: `/library/recipes/repositories/${encodeURIComponent(repository.id)}`, recipe: repository.current_recipe, repository, digests };
    });
    for (const recipe of recipes.data || []) if (!linked.has(recipe.digest)) { linked.add(recipe.digest); out.push({ id: recipe.digest, path: recipePath(recipe.digest), recipe, digests: [recipe.digest] }); }
    const query = search.toLowerCase().trim();
    return out.filter((card) => !query || [card.recipe?.name, card.recipe?.description, card.recipe?.version, card.repository?.source_url, ...card.digests].some((value) => value?.toLowerCase().includes(query)));
  }, [repositories.data, recipes.data, search]);
  const pending = (drafts.data || []).filter((draft) => (!draft.change_context?.repository_id && !draft.change_context?.base_recipe_digest) && (draft.state !== "installed" || !recipes.data?.some((recipe) => recipe.digest === draft.package_digest)));
  return <div className="sample-a-catalog"><header className="sample-a-mast"><div className="sample-a-brand"><span className="sample-a-seal sample-a-seal--local" aria-hidden>L</span><div><h1 className="sample-a-title">Recipe catalog</h1><p className="sample-a-subtitle">Review configuration, save versions, download files, and run models as separate actions.</p></div></div><div className="sample-a-mastrow"><label className="sample-a-search"><span className="sr-only">Search recipes</span><input type="search" value={search} onChange={(event) => setSearch(event.target.value)} placeholder="Search name, source, or saved version…" /></label><button type="button" className="sample-a-check" disabled={checkUpdates.isPending} onClick={() => checkUpdates.mutate()}>{checkUpdates.isPending ? "Checking sources…" : "Check for updates"}</button><Link className="sample-a-import" to="/library/recipes/new">Add from GitHub</Link><button type="button" className="sample-a-check" onClick={() => setImportOpen(true)}>Import existing package</button></div></header>
    <div className="sample-a-shell space-y-5">
      {[repositories, recipes, drafts, deployments].map((query, index) => query.isError ? <RecipeError key={index} error={query.error} retry={() => void query.refetch()} /> : null)}
      {checkUpdates.isError ? <RecipeError error={checkUpdates.error} /> : null}
      {repositories.isPending || recipes.isPending ? <p className="sample-a-banner" role="status">Loading saved recipes…</p> : null}
      {!repositories.isPending && !recipes.isPending && !cards.length ? <div className="sample-a-banner"><div><p className="sample-a-bannertitle">{search ? "No recipes match" : "No saved recipes"}</p><p className="sample-a-bannerbody">{search ? "Clear the search to see all saved recipes." : "Add from GitHub to inspect a source, or import an existing package."}</p></div></div> : null}
      <section aria-labelledby="recipe-grid-title"><header className="sample-a-gridhead"><h2 id="recipe-grid-title" className="sample-a-gridtitle">Saved recipes</h2></header><div className="sample-a-grid">{cards.map((card) => { const repository = card.repository; const recipe = card.recipe; const sourceType = repository ? "git" : recipe?.source?.type || "local"; const running = (deployments.data || []).filter((deployment) => card.digests.includes(deployment.recipe_digest) && deployment.desired_state === "running"); return <div className="sample-a-card-shell" key={card.id}><Link className="sample-a-card" to={card.path} aria-label={`Open ${recipe ? recipeDisplayName(recipe) : repository?.source_url || "saved recipe"}`}><span className="sample-a-cardtop"><span className={`sample-a-seal sample-a-seal--${sourceType}`} aria-label={`${sourceType} source`}>{sourceType === "git" ? "G" : "P"}</span><span className="sample-a-cardflags">{repository?.head_check_error ? <span className="sample-a-update-state">Update check failed</span> : repository?.update_available ? <span className="sample-a-update-state is-available">Update available for review</span> : repository?.observed_head_commit ? <span className="sample-a-update-state is-current">Source checked</span> : null}<span className="sample-a-install-state">Files: check on recipe page</span><span className="sample-a-install-state">Running: {deployments.isError ? "Unknown" : deployments.isPending ? "Loading…" : running.length}</span></span></span><span className="sample-a-cardname">{recipe ? recipeDisplayName(recipe) : repository?.source_url || "Saved recipe"}</span><span className="sample-a-carddesc">{recipe?.description || (recipe ? "Saved configuration; device files and running versions are tracked separately." : "No current saved version. Open to recover retained versions and editable work.")}</span><span className="sample-a-compat">{recipe?.compatibility?.nodeCount ? `${recipe.compatibility.nodeCount} device${recipe.compatibility.nodeCount === 1 ? "" : "s"} required to run` : "Run requirements available in configuration"}</span><span className="sample-a-updrow"><span className="sample-a-upd">Open recipe</span></span><span className="sample-a-meta">{recipe?.version || "No current version"} · {recipe?.license || "License not reported"}</span></Link></div>; })}</div></section>
      {pending.length > 0 ? <section className="space-y-3" aria-labelledby="unfinished-recipes-title"><h2 id="unfinished-recipes-title" className="sample-a-gridtitle">Unfinished additions</h2><p className="text-sm text-muted">Failed inspections and unfinished additions remain recoverable. In-progress updates and repairs are listed on their recipe pages.</p>{pending.map((draft) => <Link key={draft.id} className="control block space-y-1 p-3 hover:bg-raised" to={draftPath(draft)}><span className="block font-medium">{String(record(record(draft.manifest).metadata).name || record(draft.source).remote || "Saved addition")}</span><span className="text-xs text-muted">{draft.state} · {draft.updated_at}</span></Link>)}</section> : null}
    </div>
    <ImportRecipeDialog open={importOpen} onOpenChange={setImportOpen} onImported={(recipe) => navigate(recipePath(recipe.digest, repositories.data))} />
  </div>;
}
