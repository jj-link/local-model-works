import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useNavigate } from "react-router";
import { toast } from "sonner";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "~/components/ui/dialog";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import { useCreateDeployment, useCreateLaunchProfile, useDeleteLaunchProfile, useLaunchProfiles, useNodes, usePlanDeployment, useRecipe, useRecipes, useUpdateLaunchProfile } from "~/lib/queries";
import type { DeploymentPlan, DeploymentPlanRequest } from "~/lib/api";
import { DeviceSelections, LaunchPlanDetails, RuntimeSelections, type RuntimeSelection } from "~/components/recipes/launch-options";
import { RecipeError, record, records } from "~/components/recipes/workflow";

type Choices = RuntimeSelection & { nodes: string[]; parameters: Record<string, unknown>; profile: string; policy: "require-existing" | "download-missing" };
const emptyChoices = (): Choices => ({ nodes: [], variants: {}, parameters: {}, profile: "", policy: "require-existing" });
export function PlanDeploymentDialog({ open, onOpenChange, initialRecipeDigest, initialNodeIds, initialSelection, initialParameters }: {
  open: boolean; onOpenChange: (open: boolean) => void; initialRecipeDigest?: string;
  initialNodeIds?: string[]; initialSelection?: RuntimeSelection; initialParameters?: Record<string, unknown>;
}) {
  const navigate = useNavigate();
  const recipes = useRecipes(); const nodes = useNodes();
  const [digest, setDigest] = useState(initialRecipeDigest || "");
  const detail = useRecipe(digest || undefined);
  const profiles = useLaunchProfiles(digest || undefined);
  const planMutation = usePlanDeployment(); const createMutation = useCreateDeployment();
  const createProfile = useCreateLaunchProfile(); const updateProfile = useUpdateLaunchProfile(); const deleteProfile = useDeleteLaunchProfile();
  const resetPlan = planMutation.reset; const resetCreate = createMutation.reset;
  const [choices, setChoices] = useState<Choices>(emptyChoices);
  const [profileName, setProfileName] = useState("");
  const [sourceProfileId, setSourceProfileId] = useState("");
  const [reviewed, setReviewed] = useState<{ key: string; request: DeploymentPlanRequest; plan: DeploymentPlan }>();
  const [recovery, setRecovery] = useState<Choices>();
  const [recoveryIdentity, setRecoveryIdentity] = useState("");
  const [storageUnavailable, setStorageUnavailable] = useState(false);
  const opened = useRef(false); const initialized = useRef(""); const serial = useRef(0);
  const manifest = record(detail.data?.manifest);
  const rankCount = detail.data?.compatibility?.nodeCount || 1;
  const placements = Array.from({ length: rankCount }, (_, rank) => ({ node_id: choices.nodes[rank] || "", rank }));
  const request: DeploymentPlanRequest = { recipe_digest: digest, placements, acquisition_policy: choices.policy,
    ...(choices.profile ? { launch_profile_id: choices.profile } : { variants: choices.variants, parameters: choices.parameters, ...(choices.workload_index == null ? {} : { workload_index: choices.workload_index }) }) };
  const requestKey = JSON.stringify(request);
  const currentKey = useRef(requestKey); currentKey.current = requestKey;
  const plan = reviewed?.key === requestKey ? reviewed.plan : undefined;
  const sourceProfile = profiles.data?.find((profile) => profile.id === sourceProfileId);
  const correctionURL = `/library/recipes/packages/${encodeURIComponent(digest)}`;

  const changeRecipe = useCallback((next: string) => {
    serial.current++; initialized.current = ""; setDigest(next); setChoices(emptyChoices()); setReviewed(undefined);
    setSourceProfileId(""); setProfileName(""); setRecovery(undefined); resetPlan(); resetCreate();
    setRecoveryIdentity("");
  }, [resetPlan, resetCreate]);
  useEffect(() => {
    if (open && !opened.current) { changeRecipe(initialRecipeDigest || ""); }
    if (!open && opened.current) { serial.current++; setReviewed(undefined); }
    opened.current = open;
  }, [open, initialRecipeDigest, changeRecipe]);
  useEffect(() => {
    if (!open || !detail.data || initialized.current === digest || detail.data.digest !== digest) return;
    initialized.current = digest;
    const next: Choices = { ...emptyChoices(), variants: Object.fromEntries(records(manifest.artifacts).filter((artifact) => Array.isArray(artifact.variants) && artifact.defaultVariant).map((artifact) => [String(artifact.name), String(artifact.defaultVariant)])),
      parameters: Object.fromEntries(records(manifest.parameters).filter((parameter) => parameter.default !== undefined).map((parameter) => [String(parameter.name), parameter.default])),
      ...(initialRecipeDigest === digest && initialSelection ? initialSelection : {}), nodes: initialRecipeDigest === digest ? initialNodeIds || [] : [] };
    if (initialRecipeDigest === digest && initialParameters) next.parameters = { ...next.parameters, ...initialParameters };
    setChoices(next);
    try { const saved = sessionStorage.getItem(`lmw.recipe-launch.${digest}`); if (saved) setRecovery({ ...emptyChoices(), ...JSON.parse(saved) }); }
    catch { setStorageUnavailable(true); }
    setRecoveryIdentity(digest);
  }, [open, detail.data, digest, initialRecipeDigest, initialNodeIds, initialSelection, initialParameters, manifest.artifacts, manifest.parameters]);
  useEffect(() => {
    if (!open || !digest || recoveryIdentity !== digest || recovery) return;
    try {
      const sensitive = records(manifest.parameters).filter((parameter) => parameter.sensitive === true).map((parameter) => String(parameter.name));
      sessionStorage.setItem(`lmw.recipe-launch.${digest}`, JSON.stringify({ ...choices, schemaVersion: 2, parameters: Object.fromEntries(Object.entries(choices.parameters).filter(([name]) => !sensitive.includes(name))) }));
      setStorageUnavailable(false);
    } catch { setStorageUnavailable(true); }
  }, [open, digest, choices, recovery, recoveryIdentity, manifest.parameters]);
  useEffect(() => { serial.current++; setReviewed(undefined); }, [requestKey]);

  const applyProfile = (id: string) => {
    const profile = profiles.data?.find((candidate) => candidate.id === id);
    setSourceProfileId(id);
    setChoices((current) => ({ ...current, profile: id, ...(profile ? { workload_index: undefined, variants: profile.variants || {}, parameters: profile.parameters || {} } : {}) }));
  };
  const check = async () => {
    const ticket = ++serial.current; const key = requestKey; const frozen = request;
    setReviewed(undefined); createMutation.reset();
    try { const next = await planMutation.mutateAsync(frozen); if (ticket === serial.current && currentKey.current === key && opened.current) setReviewed({ key, request: frozen, plan: next }); }
    catch { /* The typed mutation error remains visible; selections are retained. */ }
  };
  const create = async () => {
    if (!reviewed || !plan?.ready || reviewed.key !== currentKey.current) return;
    try {
      const deployment = await createMutation.mutateAsync({ ...reviewed.request, plan_digest: plan.plan_digest, placements: plan.placements.map(({ rank, node_id }) => ({ rank, node_id })) });
      toast.success("Model run requested", { description: deployment.recipe_name || detail.data?.name });
      onOpenChange(false); navigate(`/serving/deployments/${deployment.id}`);
    } catch { /* Never retry a possibly committed creation automatically. */ }
  };
  const saveProfile = async () => {
    try { const profile = await createProfile.mutateAsync({ recipeDigest: digest, body: { name: profileName.trim(), variants: choices.variants, parameters: choices.parameters } }); setProfileName(""); setSourceProfileId(profile.id); }
    catch { /* The error and entered name remain visible. */ }
  };
  const updateSavedProfile = async () => {
    if (!sourceProfile) return;
    try { await updateProfile.mutateAsync({ id: sourceProfile.id, body: { name: sourceProfile.name, variants: choices.variants, parameters: choices.parameters } }); serial.current++; setReviewed(undefined); }
    catch { /* Keep local selections. */ }
  };
  const removeProfile = async () => {
    if (!sourceProfile) return;
    try { await deleteProfile.mutateAsync({ id: sourceProfile.id, recipeDigest: digest }); setSourceProfileId(""); setChoices((current) => ({ ...current, profile: "" })); }
    catch { /* Keep local selections. */ }
  };
  return <Dialog open={open} onOpenChange={onOpenChange}><DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-3xl"><DialogHeader><DialogTitle>Run on device{detail.data ? ` · ${detail.data.name}` : ""}</DialogTitle><DialogDescription>Select exact devices and runtime settings, review all consequences, then confirm. A missing-file check never starts an implicit download.</DialogDescription></DialogHeader>
    {[recipes, nodes, detail, profiles].map((query, index) => query.isError ? <RecipeError key={index} error={query.error} retry={() => void query.refetch()} /> : null)}
    {!initialRecipeDigest ? <label className="grid gap-2 text-sm">Saved recipe<select aria-label="Recipe" className="control bg-panel p-2" value={digest} onChange={(event) => changeRecipe(event.target.value)}><option value="">Choose saved recipe</option>{(recipes.data || []).map((recipe) => <option key={recipe.digest} value={recipe.digest}>{recipe.name} · {recipe.version} · {recipe.digest.slice(-12)}</option>)}</select></label> : null}
    {storageUnavailable ? <p role="alert" className="text-sm text-warning">Device-choice recovery storage is unavailable. This dialog still retains your selections.</p> : null}
    {recovery ? <section className="border border-warning/40 p-3"><p className="text-sm">Saved browser device and runtime choices are available. Restoring never repeats a run or download.</p><div className="mt-2 flex gap-2"><Button variant="outline" onClick={() => { setChoices(recovery); setRecovery(undefined); }}>Restore device choices</Button><Button variant="ghost" onClick={() => setRecovery(undefined)}>Use current choices</Button></div></section> : null}
    {detail.data ? <>
      <DeviceSelections nodes={nodes.data || []} selected={choices.nodes} rankCount={rankCount} onChange={(selected) => setChoices((current) => ({ ...current, nodes: selected }))} disabled={createMutation.isPending} />
      <section className="control space-y-3 p-3"><h3 className="font-medium">Runtime settings</h3>{profiles.data?.length ? <label className="grid gap-1 text-sm">Saved launch profile<select aria-label="Profile" className="control bg-panel p-2" value={choices.profile} onChange={(event) => applyProfile(event.target.value)}><option value="">Custom</option>{profiles.data.map((profile) => <option key={profile.id} value={profile.id}>{profile.name}</option>)}</select></label> : null}
        <RuntimeSelections manifest={manifest} value={choices} parameters={choices.parameters} onChange={(next) => setChoices((current) => ({ ...current, ...next, profile: "" }))} onParametersChange={(parameters) => setChoices((current) => ({ ...current, parameters, profile: "" }))} disabled={createMutation.isPending} />
        <details><summary className="cursor-pointer text-sm">Save these settings as a profile</summary><div className="mt-2 flex flex-wrap gap-2"><Input aria-label="New profile name" value={profileName} onChange={(event) => setProfileName(event.target.value)} /><Button size="sm" variant="outline" disabled={!profileName.trim() || createProfile.isPending} onClick={() => void saveProfile()}>Save profile</Button>{sourceProfile ? <><Button size="sm" variant="outline" disabled={updateProfile.isPending} onClick={() => void updateSavedProfile()}>Update profile</Button><Button size="sm" variant="outline" disabled={deleteProfile.isPending} onClick={() => void removeProfile()}>Delete profile</Button></> : null}</div></details>
      </section>
      <fieldset className="space-y-2 border border-rule p-3" disabled={createMutation.isPending}><legend className="px-1 text-sm font-medium">Files required before running</legend><label className="flex gap-2 text-sm"><input type="radio" name="acquisition-policy" checked={choices.policy === "require-existing"} onChange={() => setChoices((current) => ({ ...current, policy: "require-existing" }))} /><span>Use verified existing files only. Missing or changed files block the run.</span></label><label className="flex gap-2 text-sm"><input type="radio" name="acquisition-policy" checked={choices.policy === "download-missing"} onChange={() => setChoices((current) => ({ ...current, policy: "download-missing" }))} /><span>Download missing files, then run. Review download sources and storage before confirming.</span></label></fieldset>
      <Button variant="outline" disabled={planMutation.isPending || createMutation.isPending || placements.some((placement) => !placement.node_id) || Boolean(recovery)} onClick={() => void check()}>{planMutation.isPending ? "Checking…" : plan ? "Recheck identical inputs" : "Check run plan"}</Button>
    </> : null}
    {[planMutation, createMutation, createProfile, updateProfile, deleteProfile].map((mutation, index) => mutation.isError ? <RecipeError key={index} error={mutation.error} /> : null)}
    {createMutation.isError ? <p className="text-sm text-warning">Selections were retained. If the response was interrupted, inspect <Link className="underline" to="/runs">recent runs</Link> before confirming again.</p> : null}
    {plan ? <LaunchPlanDetails plan={plan} /> : null}
    {digest ? <Link className="text-sm underline" to={correctionURL} onClick={() => onOpenChange(false)}>Open recipe configuration</Link> : null}
    <DialogFooter><Button variant="outline" onClick={() => onOpenChange(false)}>Cancel</Button><Button disabled={!plan?.ready || createMutation.isPending || planMutation.isPending || createMutation.isError || Boolean(recovery)} onClick={() => void create()}>{createMutation.isPending ? "Requesting run…" : choices.policy === "download-missing" ? "Download and run" : "Run on device"}</Button></DialogFooter>
  </DialogContent></Dialog>;
}
