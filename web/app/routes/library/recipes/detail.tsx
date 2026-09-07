import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link, useLocation, useNavigate, useSearchParams } from "react-router";
import { Button } from "~/components/ui/button";
import { SourceForm } from "~/components/recipes/source-form";
import { ChangeEditor } from "~/components/recipes/change-editor";
import { RecipeDevices } from "~/components/recipes/recipe-devices";
import { draftPath, recipePath } from "~/components/recipes/links";
import { RecipeError, record } from "~/components/recipes/workflow";
import * as api from "~/lib/api";
import { qk, useDeployment, useRecipe, useRecipeDraft, useRecipeDrafts, useRecipeRepositories, useRecipeRepository } from "~/lib/queries";
import { useTailPathParam } from "~/lib/path-param";

type Section = "configuration" | "updates" | "devices";
export function RecipePage() {
  const { pathname } = useLocation();
  const identity = useTailPathParam();
  const mode = pathname === "/library/recipes/new" ? "new" : pathname.startsWith("/library/recipes/repositories/") ? "repository" : "package";
  return <RecipeIdentityPage key={`${mode}:${identity}`} identity={identity} mode={mode} />;
}
function RecipeIdentityPage({ identity, mode }: { identity: string; mode: "new" | "repository" | "package" }) {
  const [search, setSearch] = useSearchParams();
  const navigate = useNavigate(); const location = useLocation(); const client = useQueryClient();
  const repositories = useRecipeRepositories();
  const repositoryQuery = useRecipeRepository(mode === "repository" ? identity : undefined);
  const selectedVersion = search.get("version");
  const digest = mode === "package" ? identity : mode === "repository" ? selectedVersion || repositoryQuery.data?.current_recipe?.digest : undefined;
  const recipe = useRecipe(digest);
  const repository = mode === "repository" ? repositoryQuery.data : repositories.data?.find((item) => item.versions.some((version) => version.recipe.digest === digest));
  const draftId = search.get("draft") || ""; const draft = useRecipeDraft(draftId);
  const drafts = useRecipeDrafts(repository ? { repository_id: repository.id } : {});
  const deploymentId = search.get("deployment") || undefined; const deployment = useDeployment(deploymentId);
  const sectionValue = search.get("section"); const section: Section = sectionValue === "updates" || sectionValue === "devices" ? sectionValue : "configuration";
  const check = useMutation({ mutationFn: () => api.checkRecipeRepositoryUpdates(repository!.id), retry: false,
    onSuccess: async () => { await client.invalidateQueries({ queryKey: qk.recipeRepository(repository!.id) }); await client.invalidateQueries({ queryKey: qk.recipeRepositories }); } });
  const startChange = useMutation({ mutationFn: (kind: "repair" | "update") => api.createRecipeChange(digest!, { kind,
    expected_current_digest: repository?.current_recipe?.digest || digest!, ...(kind === "update" && repository?.observed_head_commit ? { expected_head_commit: repository.observed_head_commit } : {}), ...(deploymentId ? { deployment_id: deploymentId } : {}) }), retry: false,
    onSuccess: (result, kind) => { const next = new URLSearchParams(search); next.set("draft", result.draft_id); next.set("section", kind === "update" ? "updates" : "configuration"); setSearch(next); void client.invalidateQueries({ queryKey: qk.recipeDrafts }); } });
  const openChange = (kind: "repair" | "update") => startChange.mutate(kind);
  const saved = async (savedDigest: string) => {
    await client.invalidateQueries({ queryKey: qk.recipes });
    let linked = repositories.data || [];
    try { linked = await api.listRecipeRepositories(); client.setQueryData(qk.recipeRepositories, linked); } catch { /* The exact saved package URL remains valid without a repository lookup. */ }
    navigate(recipePath(savedDigest, linked));
  };
  const savedDigests = repository?.versions.map((version) => version.recipe.digest) || [];
  const invalidVersion = Boolean(mode === "repository" && selectedVersion && repositoryQuery.data && !savedDigests.includes(selectedVersion));
  const associated = !draft.data ? true : mode === "new" ? !draft.data.change_context?.repository_id && !draft.data.change_context?.base_recipe_digest
    : mode === "repository" ? draft.data.change_context?.repository_id === identity || Boolean(draft.data.package_digest && savedDigests.includes(draft.data.package_digest)) || Boolean(draft.data.change_context?.base_recipe_digest && savedDigests.includes(draft.data.change_context.base_recipe_digest))
      : draft.data.change_context?.base_recipe_digest === digest || draft.data.package_digest === digest;
  const invalidDeployment = Boolean(deployment.data && deployment.data.recipe_digest !== digest);
  const work = (drafts.data || []).filter((item) => item.state !== "installed" && (repository ? item.change_context?.repository_id === repository.id : digest ? item.change_context?.base_recipe_digest === digest || item.package_digest === digest : !item.change_context?.repository_id && !item.change_context?.base_recipe_digest));
  const title = recipe.data?.name || repository?.current_recipe?.name || repository?.source_url || (mode === "new" ? "Add from GitHub" : "Recipe");
  const sourceRemote = String(record(draft.data?.source).remote || "").replace(/(?:\.git)?\/?$/, "");
  const existingSource = mode === "new" && sourceRemote ? repositories.data?.find((item) => item.source_url.replace(/(?:\.git)?\/?$/, "") === sourceRemote) : undefined;
  const existingRecipeURL = existingSource ? `/library/recipes/repositories/${encodeURIComponent(existingSource.id)}` : undefined;
  return <main className="mx-auto max-w-7xl space-y-5 p-4 md:p-6">
    <header className="space-y-3 border-b border-rule pb-4"><Link to="/library/recipes" className="text-sm underline">Recipe catalog</Link><div className="flex flex-wrap items-start justify-between gap-3"><div><h1 className="break-words font-display text-3xl font-semibold">{title}</h1><p className="mt-2 max-w-3xl text-sm text-muted">{recipe.data?.description || "Save configuration separately from downloading files and running models."}</p></div><Link className="text-sm underline" to={`/settings/ai-assistance?return=${encodeURIComponent(location.pathname + location.search)}`}>AI assistance settings</Link></div>
      <nav className="flex flex-wrap gap-2" aria-label="Recipe sections">{(["configuration", "updates", "devices"] as const).map((value) => <Button key={value} variant={section === value ? "default" : "outline"} aria-current={section === value ? "page" : undefined} disabled={mode === "new" && value !== "configuration"} onClick={() => { const next = new URLSearchParams(search); next.set("section", value); setSearch(next); }}>{value === "configuration" ? "Configuration" : value === "updates" ? "Updates" : "Devices"}</Button>)}</nav>
    </header>
    {repositories.isError ? <RecipeError error={repositories.error} retry={() => void repositories.refetch()} /> : null}
    {mode === "repository" && repositoryQuery.isError ? <RecipeError error={repositoryQuery.error} retry={() => void repositoryQuery.refetch()} /> : null}
    {digest && recipe.isError ? <RecipeError error={recipe.error} retry={() => void recipe.refetch()} /> : null}
    {draftId && draft.isError ? <RecipeError error={draft.error} retry={() => void draft.refetch()} /> : null}
    {deploymentId && deployment.isError ? <RecipeError error={deployment.error} retry={() => void deployment.refetch()} /> : null}
    {startChange.isError ? <RecipeError error={startChange.error} /> : null}
    {invalidVersion ? <RecipeError error="This version is not saved in the selected recipe repository." /> : null}
    {invalidDeployment ? <RecipeError error="The selected deployment does not use this exact saved recipe. Open its recorded recipe version before requesting help." /> : null}
    {draft.data && !associated ? <section role="alert" className="space-y-2 border border-fault/40 p-3"><p>This saved work belongs to another recipe. It was not loaded into this page.</p><Link className="underline" to={draftPath(draft.data)}>Open its recipe page</Link></section> : null}
    {(mode === "repository" && repositoryQuery.isPending) || (digest && recipe.isPending) || (draftId && draft.isPending) ? <p className="text-sm text-muted">Loading saved recipe work…</p> : null}
    {mode === "new" && !draftId ? <SourceForm onCreated={(id) => navigate(`/library/recipes/new?draft=${encodeURIComponent(id)}`)} /> : null}

    {section === "updates" && repository && !invalidVersion ? <section className="control space-y-3 p-4"><div className="flex flex-wrap items-center justify-between gap-3"><h2 className="font-display text-xl font-semibold">Upstream updates</h2><Button variant="outline" disabled={check.isPending} onClick={() => check.mutate()}>{check.isPending ? "Checking source…" : "Check for updates"}</Button></div><p className="text-sm text-muted">Checking and saving an update do not require any device and never replace running models.</p><dl className="grid gap-2 text-sm md:grid-cols-2"><div><dt className="lmw-label">Tracked branch or tag</dt><dd>{repository.tracking_ref || "Default branch"}</dd></div><div><dt className="lmw-label">Last check</dt><dd>{repository.head_checked_at || "Not checked"}</dd></div><div><dt className="lmw-label">Saved source</dt><dd className="break-all font-mono text-xs">{repository.installed_commit || "Not recorded"}</dd></div><div><dt className="lmw-label">Observed upstream revision</dt><dd className="break-all font-mono text-xs">{repository.observed_head_commit || "Not checked"}</dd></div></dl>
      {repository.head_check_error ? <RecipeError error={repository.head_check_error} retry={() => check.mutate()} /> : repository.update_available ? <p className="text-sm">An upstream revision is available for review.</p> : repository.observed_head_commit ? <p className="text-sm text-muted">No newer source revision was found at the recorded check.</p> : null}
      {!repository.update_supported ? <p className="text-sm text-warning">{repository.update_diagnostic || "Source updates are unavailable. Saved configuration can still be repaired."}</p> : null}
      {check.isError ? <RecipeError error={check.error} retry={() => check.mutate()} /> : null}
      <Button disabled={!digest || !repository.update_supported || !repository.observed_head_commit || !repository.update_available || Boolean(repository.head_check_error) || startChange.isPending || Boolean(draftId) || invalidDeployment} onClick={() => openChange("update")}>Review upstream changes</Button>
    </section> : null}
    {section === "updates" && !repository && recipe.data ? <p className="text-sm text-muted">This saved package has no tracked GitHub source. Its configuration can be repaired without reconstructing an upstream revision.</p> : null}

    {draft.data && associated && !invalidVersion && !invalidDeployment ? <div hidden={section === "devices"}><ChangeEditor key={draft.data.id} initialDraft={draft.data} section={section === "updates" ? "updates" : "configuration"} onSaved={(value) => void saved(value)} existingRecipeURL={existingRecipeURL} /></div> : null}
    {section === "configuration" && recipe.data && !draftId && !invalidVersion && !invalidDeployment ? <section className="control space-y-4 p-4"><h2 className="font-display text-xl font-semibold">Saved configuration</h2><p className="text-sm text-muted">Version {recipe.data.version}. Configuration is saved independently of running workloads.</p>{deploymentId ? <p className="text-sm">Failure context: <Link className="underline" to={`/serving/deployments/${deploymentId}`}>{deploymentId}</Link>. The first help action prepares editable work from this exact saved digest; no logs are sent without consent.</p> : null}<Button disabled={startChange.isPending || Boolean(deploymentId && !deployment.data)} onClick={() => openChange("repair")}>Help fix this configuration</Button><details><summary className="cursor-pointer text-sm">Saved manifest, permissions and helpers</summary><pre className="mt-3 max-h-[36rem] overflow-auto text-xs">{JSON.stringify(recipe.data.manifest, null, 2)}</pre></details><details><summary className="cursor-pointer text-sm">Technical source details</summary><pre className="mt-2 overflow-auto text-xs">{JSON.stringify({ digest, source: recipe.data.source, permissions: recipe.data.permissions, high_risk: recipe.data.high_risk }, null, 2)}</pre></details></section> : null}
    {section === "devices" && recipe.data && !invalidVersion && !invalidDeployment ? <RecipeDevices key={recipe.data.digest} recipe={recipe.data} repositoryId={repository?.id} /> : null}
    {mode === "repository" && repository && !repository.current_recipe ? <p className="text-sm text-warning">This repository has no current saved version. Retained versions and editable work are listed below.</p> : null}

    <section className="space-y-3"><h2 className="font-display text-lg font-semibold">Saved in-progress work</h2>{drafts.isError ? <RecipeError error={drafts.error} retry={() => void drafts.refetch()} /> : drafts.isPending ? <p className="text-sm text-muted">Loading saved work…</p> : !work.length ? <p className="text-sm text-muted">No unfinished changes for this recipe.</p> : work.map((item) => <Link className="control block space-y-1 p-3 hover:bg-raised" key={item.id} to={draftPath(item)}><span className="block font-medium">{String(record(item.manifest).metadata && record(record(item.manifest).metadata).name || record(item.source).remote || "Saved recipe work")}</span><span className="text-xs text-muted">{item.change_context?.kind || "addition"} · {item.state} · {item.updated_at}</span></Link>)}</section>
    {repository?.versions.length ? <section className="space-y-3"><h2 className="font-display text-lg font-semibold">Saved versions</h2>{repository.versions.map((version) => <Link className="control block p-3 text-sm hover:bg-raised" key={version.recipe.digest} to={`/library/recipes/packages/${encodeURIComponent(version.recipe.digest)}`}>{version.recipe.name} · {version.recipe.version}{version.recipe.digest === repository.current_recipe?.digest ? " · Current catalog version" : " · Retained version"}<span className="mt-1 block text-xs text-muted">Saved {version.installed_at}</span></Link>)}</section> : null}
  </main>;
}
export default RecipePage;
