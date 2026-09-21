import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate } from "react-router";
import { toast } from "sonner";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "~/components/ui/dialog";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import { useCreateDeployment, useCreateLaunchProfile, useDeleteLaunchProfile, useLaunchProfiles, useNodes, useRecipe, useRecipes, useUpdateLaunchProfile } from "~/lib/queries";
import { planDeployment, type DeploymentPlan, type DeploymentPlanRequest } from "~/lib/api";
import { DeviceSelections, LaunchPlanDetails, RuntimeSelections, runtimeDefaults, type RuntimeSelection } from "~/components/recipes/launch-options";
import { RecipeError, record, records } from "~/components/recipes/workflow";

type Choices = RuntimeSelection & { nodes: string[]; parameters: Record<string, unknown>; profile: string; policy: "require-existing" | "download-missing" };
type Preview = { key: string; request: DeploymentPlanRequest; pending: boolean; plan?: DeploymentPlan; error?: unknown };
const emptyChoices = (): Choices => ({ nodes: [], variants: {}, parameters: {}, profile: "", policy: "download-missing" });

export function PlanDeploymentDialog({ open, onOpenChange, initialRecipeDigest, initialNodeIds, initialSelection, initialParameters }: {
  open: boolean; onOpenChange: (open: boolean) => void; initialRecipeDigest?: string;
  initialNodeIds?: string[]; initialSelection?: RuntimeSelection; initialParameters?: Record<string, unknown>;
}) {
  const navigate = useNavigate();
  const recipes = useRecipes(); const nodes = useNodes();
  const [digest, setDigest] = useState(initialRecipeDigest || "");
  const detail = useRecipe(digest || undefined);
  const profiles = useLaunchProfiles(digest || undefined);
  const createMutation = useCreateDeployment();
  const createProfile = useCreateLaunchProfile(); const updateProfile = useUpdateLaunchProfile(); const deleteProfile = useDeleteLaunchProfile();
  const resetCreate = createMutation.reset;
  const [choices, setChoices] = useState<Choices>(emptyChoices);
  const [configuredDigest, setConfiguredDigest] = useState("");
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [profileName, setProfileName] = useState("");
  const [sourceProfileId, setSourceProfileId] = useState("");
  const [preview, setPreview] = useState<Preview>();
  const [retry, setRetry] = useState(0);
  const opened = useRef(false); const submitting = useRef(false);
  const manifest = record(detail.data?.manifest);
  const defaults = useMemo(() => runtimeDefaults(manifest), [manifest]);
  const hasUpstream = records(manifest.workloads).some((workload) => Boolean(workload.upstream));
  const rankCount = detail.data?.compatibility?.nodeCount || 1;
  const request = useMemo<DeploymentPlanRequest>(() => ({ recipe_digest: digest,
    placements: Array.from({ length: rankCount }, (_, rank) => ({ node_id: choices.nodes[rank] || "", rank })),
    acquisition_policy: choices.policy,
    ...(choices.profile ? { launch_profile_id: choices.profile } : { variants: choices.variants, parameters: choices.parameters, ...(choices.workload_index == null ? {} : { workload_index: choices.workload_index }) }),
  }), [digest, rankCount, choices]);
  const requestKey = useMemo(() => JSON.stringify(request), [request]);
  const currentKey = useRef(requestKey); currentKey.current = requestKey;
  const targetsSelected = request.placements!.every((placement) => Boolean(placement.node_id));
  const currentPreview = preview?.key === requestKey ? preview : undefined;
  const plan = currentPreview?.plan;
  const checking = configuredDigest === digest && targetsSelected && (!currentPreview || currentPreview.pending);
  const sourceProfile = profiles.data?.find((profile) => profile.id === sourceProfileId);
  const customConfiguration = Boolean(choices.profile || choices.workload_index !== defaults.selection.workload_index || choices.policy !== "download-missing"
    || Object.keys({ ...defaults.selection.variants, ...choices.variants }).some((name) => defaults.selection.variants[name] !== choices.variants[name])
    || Object.keys({ ...defaults.parameters, ...choices.parameters }).some((name) => JSON.stringify(defaults.parameters[name]) !== JSON.stringify(choices.parameters[name])));

  const changeRecipe = useCallback((next: string) => {
    setDigest(next); setConfiguredDigest(""); setChoices(emptyChoices()); setPreview(undefined);
    setSourceProfileId(""); setProfileName(""); setSettingsOpen(false); resetCreate();
  }, [resetCreate]);
  useEffect(() => {
    if (open && !opened.current) changeRecipe(initialRecipeDigest || "");
    opened.current = open;
    if (!open) setPreview(undefined);
  }, [open, initialRecipeDigest, changeRecipe]);
  useEffect(() => () => { opened.current = false; }, []);
  useEffect(() => {
    if (!open || !detail.data || configuredDigest === digest || detail.data.digest !== digest) return;
    setChoices({ ...emptyChoices(), ...defaults.selection, parameters: defaults.parameters,
      ...(initialRecipeDigest === digest && initialSelection ? initialSelection : {}),
      ...(initialRecipeDigest === digest && initialParameters ? { parameters: { ...initialParameters } } : {}),
      nodes: initialRecipeDigest === digest ? initialNodeIds || [] : [],
    });
    setConfiguredDigest(digest);
  }, [open, detail.data, digest, configuredDigest, defaults, initialRecipeDigest, initialNodeIds, initialSelection, initialParameters]);
  useEffect(() => {
    if (!open || !digest || configuredDigest !== digest || !targetsSelected) return;
    let active = true;
    setPreview({ key: requestKey, request, pending: true });
    const timer = window.setTimeout(() => {
      void planDeployment(request).then(
        (next) => { if (active) setPreview({ key: requestKey, request, pending: false, plan: next }); },
        (error: unknown) => { if (active) setPreview({ key: requestKey, request, pending: false, error }); },
      );
    }, 200);
    return () => { active = false; window.clearTimeout(timer); };
  }, [open, digest, configuredDigest, targetsSelected, requestKey, request, retry]);

  const applyProfile = (id: string) => {
    const profile = profiles.data?.find((candidate) => candidate.id === id);
    setSourceProfileId(id);
    setChoices((current) => ({ ...current, profile: id, workload_index: undefined,
      variants: profile ? profile.variants || {} : defaults.selection.variants,
      parameters: profile ? profile.parameters || {} : defaults.parameters,
    }));
  };
  const create = async () => {
    if (!open || submitting.current || createMutation.isError || !currentPreview || !plan?.ready || currentPreview.pending || currentPreview.key !== currentKey.current) return;
    submitting.current = true;
    try {
      const deployment = await createMutation.mutateAsync({ ...currentPreview.request, plan_digest: plan.plan_digest, placements: plan.placements.map(({ rank, node_id }) => ({ rank, node_id })) });
      toast.success("Model run requested", { description: deployment.recipe_name || detail.data?.name });
      if (opened.current) { onOpenChange(false); navigate(`/serving/deployments/${deployment.id}`); }
    } catch { /* A possibly committed run is never retried automatically. */ }
    finally { submitting.current = false; }
  };
  const saveProfile = async () => {
    try { const profile = await createProfile.mutateAsync({ recipeDigest: digest, body: { name: profileName.trim(), variants: choices.variants, parameters: choices.parameters } }); setProfileName(""); setSourceProfileId(profile.id); }
    catch { /* The error and entered name remain visible. */ }
  };
  const updateSavedProfile = async () => {
    if (!sourceProfile) return;
    try { await updateProfile.mutateAsync({ id: sourceProfile.id, body: { name: sourceProfile.name, variants: choices.variants, parameters: choices.parameters } }); setRetry((value) => value + 1); }
    catch { /* Keep local selections. */ }
  };
  const removeProfile = async () => {
    if (!sourceProfile) return;
    try { await deleteProfile.mutateAsync({ id: sourceProfile.id, recipeDigest: digest }); setSourceProfileId(""); setChoices((current) => ({ ...current, profile: "" })); }
    catch { /* Keep local selections. */ }
  };

  return <Dialog open={open} onOpenChange={(next) => { if (!submitting.current) onOpenChange(next); }}>
    <DialogContent className="flex max-h-[90dvh] flex-col gap-0 overflow-hidden p-0 sm:max-w-2xl" showCloseButton={!createMutation.isPending}>
      <DialogHeader className="shrink-0 px-5 pb-4 pt-5 pr-12">
        <DialogTitle className="break-words leading-snug">Run on device{detail.data ? ` · ${detail.data.name}` : ""}</DialogTitle>
        <DialogDescription>Choose {rankCount === 1 ? "a device" : "the devices"}. Recipe defaults are applied automatically; open Settings only to customize.</DialogDescription>
      </DialogHeader>
      <div className="min-h-0 space-y-4 overflow-y-auto px-5 pb-5">
        {[recipes, nodes, detail].map((query, index) => query.isError ? <RecipeError key={index} error={query.error} retry={() => void query.refetch()} /> : null)}
        {!initialRecipeDigest ? <label className="grid gap-2 text-sm">Recipe<select aria-label="Recipe" className="control bg-panel p-2" value={digest} disabled={createMutation.isPending} onChange={(event) => changeRecipe(event.target.value)}><option value="">Choose a recipe</option>{(recipes.data || []).map((recipe) => <option key={recipe.digest} value={recipe.digest}>{recipe.name} · {recipe.version}</option>)}</select></label> : null}
        {detail.isPending && digest ? <p role="status" className="text-sm text-muted">Loading recipe defaults…</p> : null}
        {detail.data ? <>
          <DeviceSelections nodes={nodes.data || []} selected={choices.nodes} rankCount={rankCount} onChange={(selected) => setChoices((current) => ({ ...current, nodes: selected }))} disabled={createMutation.isPending} />
          <p className="text-sm">Configuration: <strong>{customConfiguration ? "Custom" : "Default"}</strong></p>
          <details open={settingsOpen} onToggle={(event) => setSettingsOpen(event.currentTarget.open)} className="rounded-md border border-rule p-3">
            <summary className="cursor-pointer text-sm font-medium">Settings <span className="font-normal text-muted">· search or customize</span></summary>
            {settingsOpen ? <div className="mt-4 space-y-4">
              <Button variant="outline" size="sm" disabled={createMutation.isPending} onClick={() => { setChoices((current) => ({ ...emptyChoices(), ...defaults.selection, parameters: defaults.parameters, nodes: current.nodes })); setSourceProfileId(""); }}>Use default configuration</Button>
              <RuntimeSelections manifest={manifest} value={choices} parameters={choices.parameters} resolvedParameters={plan?.parameters ?? undefined} onChange={(next) => setChoices((current) => ({ ...current, ...next, profile: "" }))} onParametersChange={(parameters) => setChoices((current) => ({ ...current, parameters, profile: "" }))} disabled={createMutation.isPending} compact />
              <fieldset className="space-y-2 border-t border-rule pt-3" disabled={createMutation.isPending}>
                <legend className="text-sm font-medium">File downloads</legend>
                <label className="flex gap-2 text-sm"><input type="radio" name="acquisition-policy" checked={choices.policy === "download-missing"} onChange={() => setChoices((current) => ({ ...current, policy: "download-missing" }))} />Reuse existing files and download missing files when I run</label>
                <label className="flex gap-2 text-sm"><input type="radio" name="acquisition-policy" checked={choices.policy === "require-existing"} onChange={() => setChoices((current) => ({ ...current, policy: "require-existing" }))} />Use existing files only</label>
                {hasUpstream ? <p className="text-xs text-muted">This controls app-managed files. The recipe's own scripts may also download files or build dependencies.</p> : null}
              </fieldset>
              <details className="border-t border-rule pt-3"><summary className="cursor-pointer text-sm">Saved profiles</summary><div className="mt-3 space-y-3">
                {profiles.isError ? <RecipeError error={profiles.error} retry={() => void profiles.refetch()} /> : null}
                {profiles.data?.length ? <label className="grid gap-1 text-sm">Launch profile<select aria-label="Profile" className="control bg-panel p-2" value={choices.profile} disabled={createMutation.isPending} onChange={(event) => applyProfile(event.target.value)}><option value="">Recipe defaults</option>{profiles.data.map((profile) => <option key={profile.id} value={profile.id}>{profile.name}</option>)}</select></label> : null}
                <div className="flex flex-wrap gap-2"><Input aria-label="New profile name" value={profileName} disabled={createMutation.isPending} onChange={(event) => setProfileName(event.target.value)} /><Button size="sm" variant="outline" disabled={!profileName.trim() || createProfile.isPending || createMutation.isPending} onClick={() => void saveProfile()}>Save profile</Button>{sourceProfile ? <><Button size="sm" variant="outline" disabled={updateProfile.isPending || createMutation.isPending} onClick={() => void updateSavedProfile()}>Update profile</Button><Button size="sm" variant="outline" disabled={deleteProfile.isPending || createMutation.isPending} onClick={() => void removeProfile()}>Delete profile</Button></> : null}</div>
                {[createProfile, updateProfile, deleteProfile].map((mutation, index) => mutation.isError ? <RecipeError key={index} error={mutation.error} /> : null)}
              </div></details>
            </div> : null}
          </details>
          {!targetsSelected ? <p className="text-sm text-muted">Choose {rankCount === 1 ? "a device" : "each device"} to check whether this recipe is ready to run.</p> : checking ? <p role="status" className="text-sm text-muted">Checking device readiness…</p> : null}
          {currentPreview?.error ? <RecipeError error={currentPreview.error} retry={() => setRetry((value) => value + 1)} /> : null}
          {plan ? <>
            <p role="status" className={`text-sm ${plan.ready ? "text-success" : "text-warning"}`}>{plan.ready ? "Ready to run." : "Cannot run on the selected devices yet."}</p>
            {plan.diagnostics?.filter((diagnostic) => diagnostic.code !== "upstream.host_execution").map((diagnostic, index) => <p key={index} role={diagnostic.severity === "error" ? "alert" : undefined} className={diagnostic.severity === "error" ? "text-sm text-fault" : "text-sm text-warning"}>{diagnostic.message}</p>)}
            {!plan.ready ? <Button variant="outline" size="sm" disabled={checking || createMutation.isPending} onClick={() => setRetry((value) => value + 1)}>Retry readiness check</Button> : null}
            <details><summary className="cursor-pointer text-sm text-muted">Run details</summary><div className="mt-3"><LaunchPlanDetails plan={plan} /></div></details>
          </> : null}
          <p className="text-xs text-muted">{choices.policy === "download-missing" ? "Existing app-managed files are reused. Missing files download only after you click Run." : "Only verified existing app-managed files will be used."}</p>
          {hasUpstream ? <p className="text-xs text-warning">Clicking Run authorizes this recipe's source code to execute on the selected devices, including Docker, host changes, downloads and builds outside the app's sandbox.</p> : null}
          {currentPreview?.error || plan && !plan.ready ? <Link className="block text-sm underline" to={`/library/recipes/packages/${encodeURIComponent(digest)}`} onClick={() => onOpenChange(false)}>Open recipe configuration</Link> : null}
        </> : null}
        {createMutation.isError ? <><RecipeError error={createMutation.error} /><p className="text-sm text-warning">The run may have been accepted. Check <Link className="underline" to="/runs">recent runs</Link> before trying again. Nothing is retried automatically.</p></> : null}
      </div>
      <DialogFooter className="mx-0 mb-0 shrink-0 px-5">
        <Button variant="outline" disabled={createMutation.isPending} onClick={() => onOpenChange(false)}>Cancel</Button>
        <Button disabled={!targetsSelected || !plan?.ready || checking || createMutation.isPending || createMutation.isError} onClick={() => void create()}>{createMutation.isPending ? "Starting…" : "Run"}</Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>;
}
