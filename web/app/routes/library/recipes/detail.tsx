import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useLocation, useNavigate, useSearchParams } from "react-router";
import { Button } from "~/components/ui/button";
import { SourceForm } from "~/components/recipes/source-form";
import { ChangeEditor } from "~/components/recipes/change-editor";
import { RecipeDevices } from "~/components/recipes/recipe-devices";
import { RecipeLaunchSummary } from "~/components/recipes/recipe-launch-summary";
import { RecipeOverview } from "~/components/recipes/recipe-overview";
import { RecipeSettings } from "~/components/recipes/recipe-settings";
import { DeploymentSettings } from "~/components/recipes/deployment-settings";
import { RecipeUpdateButton } from "~/components/recipes/recipe-update-button";
import { recipeDisplayName } from "~/components/recipes/presentation";
import { runtimeDefaults, type RuntimeSelection } from "~/components/recipes/launch-options";
import { PlanDeploymentDialog } from "~/components/dialogs/plan-deployment-dialog";
import { ArrowLeft, ExternalLink, FileCode, HelpCircle, Play, Settings2 } from "lucide-react";
import { draftPath, recipePath } from "~/components/recipes/links";
import { RecipeError, record, records } from "~/components/recipes/workflow";
import * as api from "~/lib/api";
import { qk, useDeployment, useRecipe, useRecipeDraft, useRecipeDrafts, useRecipeRepositories, useRecipeRepository } from "~/lib/queries";
import { useTailPathParam } from "~/lib/path-param";

type Section = "overview" | "configuration" | "updates" | "devices" | "assistance" | "technical";
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
  const [providerId, setProviderId] = useState("");
  const [instruction, setInstruction] = useState("");
  const [initialAssistance, setInitialAssistance] = useState<{ draftId: string; providerId: string; instruction: string }>();
  const sectionValue = search.get("section");
  const section: Section = sectionValue === "configuration" || sectionValue === "updates" || sectionValue === "devices" || sectionValue === "assistance" || sectionValue === "technical" ? sectionValue : deploymentId ? "configuration" : "overview";
  const [runOpen, setRunOpen] = useState(false);
  const [runNodes, setRunNodes] = useState<string[]>([]);
  const [editedInputs, setEditedInputs] = useState<{ digest?: string; parameters: Record<string, unknown>; selection: RuntimeSelection }>({ parameters: {}, selection: { variants: {} } });
  const runInputs = useMemo(() => editedInputs.digest === digest ? editedInputs : { digest, ...runtimeDefaults(record(recipe.data?.manifest)) }, [digest, editedInputs, recipe.data?.manifest]);
  const showSection = (value: Section) => {
    const next = new URLSearchParams(search);
    next.set("section", value);
    next.delete("draft");
    if (value === "overview") next.delete("deployment");
    setSearch(next);
  };
  const openRun = (nodeIds: string[] = []) => { setRunNodes(nodeIds); setRunOpen(true); };
  const providerCatalog = useQuery({ queryKey: ["recipe-assistant", "providers"], queryFn: ({ signal }) => api.listRecipeAssistantProviders({ signal }), enabled: Boolean(digest && !draftId && section === "assistance") });
  const providers = providerCatalog.data?.providers || [];
  const selectedProvider = providers.find((provider) => provider.id === providerId);
  const check = useMutation({ mutationFn: () => api.checkRecipeRepositoryUpdates(repository!.id), retry: false,
    onSuccess: async () => { await client.invalidateQueries({ queryKey: qk.recipeRepository(repository!.id) }); await client.invalidateQueries({ queryKey: qk.recipeRepositories }); } });
  const startChange = useMutation({ mutationFn: ({ kind }: { kind: "repair" | "update"; assistance?: { providerId: string; instruction: string } }) => api.createRecipeChange(digest!, { kind,
    expected_current_digest: repository?.current_recipe?.digest || digest!, ...(kind === "update" && repository?.observed_head_commit ? { expected_head_commit: repository.observed_head_commit } : {}), ...(deploymentId ? { deployment_id: deploymentId } : {}) }), retry: false,
    onSuccess: (result, { kind, assistance }) => { setInitialAssistance(assistance ? { draftId: result.draft_id, ...assistance } : undefined); const next = new URLSearchParams(search); next.set("draft", result.draft_id); next.set("section", kind === "update" ? "updates" : "technical"); setSearch(next); void client.invalidateQueries({ queryKey: qk.recipeDrafts }); } });
  const openChange = (kind: "repair" | "update", assistance?: { providerId: string; instruction: string }) => startChange.mutate({ kind, assistance });
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
  const identityRecipe = recipe.data || repository?.current_recipe;
  const title = identityRecipe ? recipeDisplayName(identityRecipe) : mode === "new" ? "Add from GitHub" : "Recipe";
  const ready = Boolean(recipe.data && !invalidVersion && !invalidDeployment && associated);
  const source = String(repository?.source_url || record(recipe.data?.source).remote || record(record(record(recipe.data?.manifest).metadata).source).url || "");
  let upstream: URL | undefined;
  try { const url = new URL(source); if (url.protocol === "https:" || url.protocol === "http:") upstream = url; } catch { /* Non-browser source locators remain available in technical details. */ }
  return <main className="mx-auto max-w-6xl space-y-6 p-4 md:p-6">
    <header className="space-y-5 border-b border-rule pb-5">
      <Link to="/library/recipes" className="inline-flex items-center gap-1.5 text-sm text-muted hover:text-ink"><ArrowLeft className="size-4" aria-hidden />Recipe catalog</Link>
      <div className="space-y-3">
        <h1 className="break-words font-display text-3xl font-semibold">{title}</h1>
        {identityRecipe ? <div className="flex flex-wrap items-center gap-x-4 gap-y-2 text-sm text-muted">
          <span className="rounded-md bg-raised px-2 py-1 font-medium text-ink">Version {identityRecipe.version}</span>
          {identityRecipe.engine ? <span>{identityRecipe.engine === "sglang" ? "SGLang" : identityRecipe.engine === "vllm" ? "vLLM" : identityRecipe.engine}</span> : null}
          {identityRecipe.compatibility?.nodeCount ? <span>{identityRecipe.compatibility.nodeCount} device{identityRecipe.compatibility.nodeCount === 1 ? "" : "s"} required</span> : null}
          {upstream ? <a href={upstream.href} target="_blank" rel="noopener noreferrer" className="inline-flex items-center gap-1 underline underline-offset-4">Upstream{upstream.hostname === "github.com" ? ` · ${upstream.pathname.split("/")[1]}` : ""}<ExternalLink className="size-3.5" aria-hidden /></a> : null}
          {repository?.current_recipe && recipe.data?.digest !== repository.current_recipe.digest ? <span>Retained version</span> : null}
        </div> : null}
      </div>
      {mode !== "new" ? <div className="flex flex-wrap gap-2">
        <Button disabled={!ready} onClick={() => openRun()}><Play className="size-4" aria-hidden />Run on device</Button>
        {digest ? <RecipeUpdateButton recipeDigest={digest} repositoryId={repository?.id} /> : null}
        <Button variant={section === "configuration" && !draftId ? "secondary" : "outline"} disabled={!ready} onClick={() => showSection("configuration")}><Settings2 className="size-4" aria-hidden />Settings</Button>
        {section !== "overview" || draftId ? <Button variant="ghost" onClick={() => showSection("overview")}>Overview</Button> : null}
      </div> : <p className="text-sm text-muted">Review a repository before saving its recipe.</p>}
    </header>
    {repositories.isError ? <RecipeError error={repositories.error} retry={() => void repositories.refetch()} /> : null}
    {mode === "repository" && repositoryQuery.isError ? <RecipeError error={repositoryQuery.error} retry={() => void repositoryQuery.refetch()} /> : null}
    {digest && recipe.isError ? <RecipeError error={recipe.error} retry={() => void recipe.refetch()} /> : null}
    {draftId && draft.isError ? <RecipeError error={draft.error} retry={() => void draft.refetch()} /> : null}
    {deploymentId && deployment.isError ? <RecipeError error={deployment.error} retry={() => void deployment.refetch()} /> : null}
    {startChange.isError ? <RecipeError error={startChange.error} /> : null}
    {invalidVersion ? <RecipeError error="This version is not saved in the selected recipe repository." /> : null}
    {invalidDeployment ? <RecipeError error="The selected deployment does not use this exact saved recipe. Open its recorded recipe version before configuring it." /> : null}
    {draft.data && !associated ? <section role="alert" className="space-y-2 border border-fault/40 p-3"><p>This saved work belongs to another recipe. It was not loaded into this page.</p><Link className="underline" to={draftPath(draft.data)}>Open its recipe page</Link></section> : null}
    {(mode === "repository" && repositoryQuery.isPending) || (digest && recipe.isPending) || (draftId && draft.isPending) ? <p className="text-sm text-muted">Loading saved recipe work…</p> : null}
    {mode === "new" && !draftId ? <SourceForm onCreated={(id) => navigate(`/library/recipes/new?draft=${encodeURIComponent(id)}`)} /> : null}

    {section === "updates" && repository && !invalidVersion ? <section className="control space-y-4 p-5"><h2 className="font-display text-xl font-semibold">Updates</h2><div className="flex flex-wrap gap-x-6 gap-y-2 text-sm text-muted"><span>Tracking {repository.tracking_ref || "default branch"}</span><span>Last checked: {repository.head_checked_at ? new Date(repository.head_checked_at).toLocaleString() : "Not yet checked"}</span></div>
      {repository.head_check_error ? <RecipeError error={repository.head_check_error} retry={() => check.mutate()} /> : repository.update_available ? <p className="font-medium">An upstream update is available.</p> : repository.observed_head_commit ? <p className="font-medium">Source is up to date.</p> : <p className="text-sm text-muted">Check the source to look for a newer revision.</p>}
      {!repository.update_supported ? <p className="text-sm text-warning">{repository.update_diagnostic || "Source updates are unavailable. Saved settings can still be edited."}</p> : null}
      {check.isError ? <RecipeError error={check.error} retry={() => check.mutate()} /> : null}
      <Button disabled={!digest || !repository.update_supported || !repository.observed_head_commit || !repository.update_available || Boolean(repository.head_check_error) || startChange.isPending || Boolean(draftId) || invalidDeployment} onClick={() => openChange("update")}>Review upstream changes</Button>
      <p className="text-xs text-muted">Reviewing or saving an update does not change running models.</p>
    </section> : null}
    {section === "updates" && !repository && recipe.data ? <p className="text-sm text-muted">This package has no tracked upstream source.</p> : null}

    {draft.data && associated && !invalidVersion && !invalidDeployment ? <ChangeEditor key={draft.data.id} initialDraft={draft.data} initialAssistance={initialAssistance?.draftId === draft.data.id ? initialAssistance : undefined} section={section === "updates" ? "updates" : "configuration"} onSaved={(value) => void saved(value)} /> : null}
    {recipe.data && ready && !draftId && section === "overview" ? <RecipeOverview recipe={recipe.data} repository={repository} onRun={openRun} onManageDevices={() => showSection("devices")} onConfigure={(selected) => navigate(`${repository ? `/library/recipes/repositories/${encodeURIComponent(repository.id)}?version=${encodeURIComponent(selected.recipe_digest)}&` : `/library/recipes/packages/${encodeURIComponent(selected.recipe_digest)}?`}section=configuration&deployment=${encodeURIComponent(selected.id)}`)} /> : null}
    {recipe.data && ready && !draftId && section === "configuration" && deploymentId ? <DeploymentSettings deploymentID={deploymentId} repositoryId={repository?.id} /> : null}
    {recipe.data && ready && !draftId && (section === "overview" || section === "configuration" && !deploymentId) ? <RecipeSettings recipe={recipe.data} parameters={runInputs.parameters} selection={runInputs.selection} onParametersChange={(parameters) => setEditedInputs((current) => ({ ...(current.digest === digest ? current : runInputs), parameters }))} onSelectionChange={(selection) => setEditedInputs((current) => ({ ...(current.digest === digest ? current : runInputs), selection }))} onReset={() => {
      const manifest = record(recipe.data?.manifest);
      const defaults = runtimeDefaults(manifest);
      const supportedParameters = new Set(records(manifest.parameters).map((parameter) => String(parameter.name)));
      const supportedArtifacts = new Set(records(manifest.artifacts).map((artifact) => String(artifact.name)));
      setEditedInputs({ digest, parameters: { ...Object.fromEntries(Object.entries(runInputs.parameters).filter(([name]) => !supportedParameters.has(name))), ...defaults.parameters }, selection: { ...defaults.selection, variants: { ...Object.fromEntries(Object.entries(runInputs.selection.variants).filter(([name]) => !supportedArtifacts.has(name))), ...defaults.selection.variants } } });
    }} onRun={() => openRun()} /> : null}
    {section === "technical" && recipe.data && ready && !draftId ? <><RecipeLaunchSummary recipe={recipe.data} /><Button variant="outline" disabled={startChange.isPending || Boolean(deploymentId && !deployment.data)} onClick={() => openChange("repair")}>Edit saved recipe definition</Button></> : null}
    {section === "assistance" && recipe.data && !draftId && ready ? <section className="control max-w-3xl space-y-4 p-5">
        <div className="flex flex-wrap items-center justify-between gap-3"><h2 className="font-display text-xl font-semibold">Ask AI about a change</h2><Link className="text-sm underline" to={`/settings/ai-assistance?return=${encodeURIComponent(location.pathname + location.search)}`}>Provider settings</Link></div>
        <p className="text-sm text-muted">Describe what you need. You'll review the source and destination before anything is sent.</p>
        {deploymentId ? <p className="text-sm">Troubleshooting <Link className="underline" to={`/serving/deployments/${deploymentId}`}>this deployment</Link>. Sharing logs requires your approval.</p> : null}
        {providerCatalog.isError ? <RecipeError error={providerCatalog.error} retry={() => void providerCatalog.refetch()} /> : null}
        <label className="block space-y-1 text-sm">Assistant model / provider<select className="control w-full bg-panel p-2" disabled={providerCatalog.isPending || providerCatalog.isError || startChange.isPending} value={selectedProvider ? providerId : ""} onChange={(event) => setProviderId(event.target.value)}><option value="">{providerCatalog.isPending ? "Loading assistants…" : "Choose an assistant"}</option>{providers.map((provider) => <option key={provider.id} value={provider.id}>{provider.label}{provider.model && !provider.label.includes(provider.model) ? ` · ${provider.model}` : ""}</option>)}</select></label>
        {providerCatalog.isSuccess && !providers.length ? <p className="text-sm text-warning">No assistants are available. Choose a running model in <Link className="underline" to="/serving">Serving</Link> or configure a provider in <Link className="underline" to={`/settings/ai-assistance?return=${encodeURIComponent(location.pathname + location.search)}`}>AI assistance settings</Link>. Manual editing is still available.</p> : null}
        {providerId && !selectedProvider && providerCatalog.isSuccess ? <p className="text-sm text-warning">The selected assistant is no longer available. Choose another assistant.</p> : null}
        <label className="block space-y-1 text-sm">What should change?<textarea className="control min-h-24 w-full bg-panel p-2" disabled={startChange.isPending} value={instruction} onChange={(event) => setInstruction(event.target.value)} placeholder="Describe the setting or launch behavior you want to change." /></label>
        <Button disabled={startChange.isPending || !selectedProvider || providerCatalog.isError || !instruction.trim() || Boolean(deploymentId && !deployment.data)} onClick={() => openChange("repair", { providerId, instruction })}>{startChange.isPending ? "Preparing editable work…" : "Review request and source"}</Button>
    </section> : null}
    {section === "devices" && recipe.data && !invalidVersion && !invalidDeployment ? <RecipeDevices key={recipe.data.digest} recipe={recipe.data} repositoryId={repository?.id} /> : null}
    {mode === "repository" && repository && !repository.current_recipe ? <p className="text-sm text-warning">This repository has no current saved version. Retained versions and editable work are listed below.</p> : null}

    {drafts.isError ? <RecipeError error={drafts.error} retry={() => void drafts.refetch()} /> : null}
    {work.length ? <section className="space-y-3"><div className="flex items-center justify-between gap-3"><h2 className="text-sm font-medium">Unfinished changes ({work.length})</h2>{section !== "assistance" ? <Button variant="ghost" size="sm" onClick={() => showSection("assistance")}>View all</Button> : null}</div><div className="space-y-2">{(section === "assistance" ? work : work.slice(0, 2)).map((item) => <Link className="control flex flex-wrap items-center justify-between gap-2 px-3 py-2 text-sm hover:bg-raised" key={item.id} to={draftPath(item)}><span>{item.change_context?.kind === "update" ? "Upstream update" : item.change_context?.kind === "repair" ? "Recipe change" : "New recipe"}</span><span className="text-xs text-muted">{item.state} · Continue</span></Link>)}</div></section> : null}
    {repository?.versions.length && (section === "updates" || !repository.current_recipe) ? <section className="control space-y-3 p-5"><h2 className="font-display text-lg font-semibold">Saved versions</h2><div className="divide-y divide-rule">{repository.versions.map((version) => <Link className="flex flex-wrap items-center justify-between gap-2 py-3 text-sm hover:text-muted" key={version.recipe.digest} to={`/library/recipes/packages/${encodeURIComponent(version.recipe.digest)}`}><span className="font-medium">{version.recipe.version}{version.recipe.digest === repository.current_recipe?.digest ? <span className="ml-2 rounded bg-raised px-2 py-1 text-xs font-normal">Current</span> : null}</span><span className="text-xs text-muted">{new Date(version.installed_at).toLocaleString()}</span></Link>)}</div></section> : null}
    {recipe.data && ready && !draftId ? <footer className="flex flex-wrap gap-x-5 gap-y-2 border-t border-rule pt-4">
      <Button variant="ghost" size="sm" onClick={() => showSection("assistance")}><HelpCircle className="size-4" aria-hidden />Ask AI about a change</Button>
      <Button variant="ghost" size="sm" onClick={() => showSection("technical")}><FileCode className="size-4" aria-hidden />Technical details</Button>
      {repository && section !== "updates" ? <Button variant="ghost" size="sm" onClick={() => showSection("updates")}>Version history</Button> : null}
    </footer> : null}
    {recipe.data && ready ? <PlanDeploymentDialog key={recipe.data.digest} open={runOpen} onOpenChange={setRunOpen} initialRecipeDigest={recipe.data.digest} initialNodeIds={runNodes} initialParameters={runInputs.parameters} initialSelection={runInputs.selection} /> : null}
  </main>;
}
export default RecipePage;
