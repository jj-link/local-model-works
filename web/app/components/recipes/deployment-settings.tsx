import { useEffect, useRef, useState } from "react";
import { Link, useNavigate } from "react-router";
import { Button } from "~/components/ui/button";
import type { Deployment, RecipeDetail, RecipeRepositoryReplacementPlanRequest, RecipeUpdatePlan } from "~/lib/api";
import { useApplyDeploymentConfiguration, useDeployment, usePlanDeploymentConfiguration, usePlanRecipeRepositoryReplacement, useRecipe, useRecipeRepositories, useStartRecipeRepositoryReplacement } from "~/lib/queries";
import { LaunchPlanDetails, RuntimeSelections, UpstreamExecutionAcknowledgment, type RuntimeSelection } from "./launch-options";
import { parameterLabel } from "./presentation";
import { RecipeError, record, records } from "./workflow";

type Settings = { parameters: Record<string, unknown>; selection: RuntimeSelection };

export function DeploymentSettings({ deploymentID, repositoryId }: { deploymentID: string; repositoryId?: string }) {
  const deployment = useDeployment(deploymentID);
  const [choice, setChoice] = useState<{ deploymentID: string; digest: string; settings: Record<string, Settings> }>();
  const selected = choice?.deploymentID === deploymentID ? choice : undefined;
  const recipe = useRecipe(selected?.digest ?? deployment.data?.recipe_digest);
  const selectedDigest = recipe.data?.digest;
  const repositories = useRecipeRepositories();
  const matches = (repositories.data || []).filter((repository) => (!repositoryId || repository.id === repositoryId) && (repository.current_recipe?.digest === deployment.data?.recipe_digest || repository.versions.some((version) => version.recipe.digest === deployment.data?.recipe_digest)));
  const versions = matches.length === 1 ? [...new Map([
    ...(deployment.data ? [{ digest: deployment.data.recipe_digest, version: deployment.data.recipe_version || deployment.data.recipe_digest }] : []),
    ...matches[0].versions.map((version) => version.recipe),
    ...(matches[0].current_recipe ? [matches[0].current_recipe] : []),
  ].map((version) => [version.digest, version])).values()] : [];
  return <section className="control space-y-4 p-4" aria-label="Configure deployment">
    <div className="flex flex-wrap items-center justify-between gap-2"><h2 className="font-display text-xl font-semibold">Configure deployment</h2><Link className="text-sm underline" to={`/serving/deployments/${encodeURIComponent(deploymentID)}`}>Deployment, stop controls & logs</Link></div>
    {[deployment, recipe, repositories].map((query, index) => query.isError ? <RecipeError key={index} error={query.error} retry={() => void query.refetch()} /> : null)}
    {deployment.isPending || recipe.isPending ? <p role="status" className="text-sm text-muted">Loading this deployment’s saved settings and selected recipe…</p> : deployment.data && recipe.data && selectedDigest ? <DeploymentSettingsEditor key={`${deploymentID}:${selectedDigest}`} deployment={deployment.data} recipe={recipe.data} repositoryId={matches.length === 1 ? matches[0].id : undefined} versions={versions} initialSettings={selected?.settings[selectedDigest]} onRecipeChange={(digest, settings) => setChoice((current) => ({ deploymentID, digest, settings: { ...(current?.deploymentID === deploymentID ? current.settings : {}), [selectedDigest]: settings } }))} /> : null}
  </section>;
}

function DeploymentSettingsEditor({ deployment, recipe, repositoryId, versions, initialSettings, onRecipeChange }: { deployment: Deployment; recipe: RecipeDetail; repositoryId?: string; versions: Pick<RecipeDetail, "digest" | "version">[]; initialSettings?: Settings; onRecipeChange: (digest: string, settings: Settings) => void }) {
  const navigate = useNavigate();
  const manifest = record(recipe.manifest);
  const fields = records(manifest.parameters);
  const saved: Settings = { parameters: deployment.parameters ?? {}, selection: { variants: deployment.variants ?? {}, workload_index: deployment.workload_index } };
  const savedKey = JSON.stringify([deployment.recipe_digest, deployment.run_id, saved, deployment.placements?.map(({ rank, node_id }) => ({ rank, node_id })), deployment.fabric, deployment.desired_state]);
  const [baseline, setBaseline] = useState(() => ({ key: savedKey, settings: saved }));
  const supportedSettings = (value: Settings): Settings => {
    const supported = new Set(fields.map((field) => field.name));
    const artifacts = new Set(records(manifest.artifacts).filter((artifact) => records(artifact.variants).length).map((artifact) => artifact.name));
    return { parameters: Object.fromEntries(Object.entries(value.parameters).filter(([name]) => supported.has(name))), selection: { ...value.selection, variants: Object.fromEntries(Object.entries(value.selection.variants).filter(([name]) => artifacts.has(name))) } };
  };
  const [settings, setSettings] = useState<Settings>(() => supportedSettings(initialSettings ?? saved));
  const [reviewed, setReviewed] = useState<{ key: string; request: RecipeRepositoryReplacementPlanRequest; plan: RecipeUpdatePlan }>();
  const [consentPlan, setConsentPlan] = useState<RecipeUpdatePlan>();
  const [upstreamPlan, setUpstreamPlan] = useState<RecipeUpdatePlan>();
  const [editing, setEditing] = useState(true);
  const repositoryPlan = usePlanRecipeRepositoryReplacement();
  const repositoryStart = useStartRecipeRepositoryReplacement();
  const configurationPlan = usePlanDeploymentConfiguration();
  const configurationApply = useApplyDeploymentConfiguration();
  const versionSwitch = recipe.digest !== deployment.recipe_digest;
  const planMutation = versionSwitch ? repositoryPlan : configurationPlan;
  const startMutation = versionSwitch ? repositoryStart : configurationApply;
  const serial = useRef(0);
  const mounted = useRef(true);
  const request: RecipeRepositoryReplacementPlanRequest = { target_digest: recipe.digest, deployment_ids: [deployment.id], deployment_settings: { [deployment.id]: { parameters: supportedSettings(settings).parameters, variants: settings.selection.variants, ...(settings.selection.workload_index === undefined ? {} : { workload_index: settings.selection.workload_index }) } } };
  const key = JSON.stringify([repositoryId, savedKey, request]);
  const currentKey = useRef(key); currentKey.current = key;
  const stale = baseline.key !== savedKey;
  const plan = reviewed?.key === key && !stale ? reviewed.plan : undefined;
  const upstream = Boolean(plan?.deployments?.some((entry) => entry.current_permissions.includes("host.upstream-exec") || entry.deployment_plan.risks?.includes("host.upstream-exec")));
  const changes: { name: string; before: unknown; after: unknown; sensitive?: boolean }[] = [];
  if (recipe.digest !== deployment.recipe_digest) changes.push({ name: "Recipe version", before: `${deployment.recipe_version || "Saved recipe"} · ${deployment.recipe_digest.slice(0, 19)}`, after: `${recipe.version} · ${recipe.digest.slice(0, 19)}` });
  for (const name of new Set([...Object.keys(baseline.settings.parameters), ...Object.keys(settings.parameters)])) {
    const field = fields.find((candidate) => candidate.name === name);
    const before = baseline.settings.parameters[name];
    const after = Object.hasOwn(settings.parameters, name) ? settings.parameters[name] : field?.optional === true ? undefined : field?.default;
    if (JSON.stringify(before) !== JSON.stringify(after)) changes.push({ name: String(field?.label || parameterLabel(name)), before, after, sensitive: !field || field.sensitive === true });
  }
  for (const name of new Set([...Object.keys(baseline.settings.selection.variants), ...Object.keys(settings.selection.variants)])) {
    const before = baseline.settings.selection.variants[name]; const after = settings.selection.variants[name];
    if (before !== after) changes.push({ name: `${parameterLabel(name)} variant`, before, after });
  }
  if (baseline.settings.selection.workload_index !== settings.selection.workload_index) changes.push({ name: "Runtime", before: baseline.settings.selection.workload_index, after: settings.selection.workload_index });
  useEffect(() => { serial.current++; setReviewed(undefined); setConsentPlan(undefined); setUpstreamPlan(undefined); setEditing(true); }, [key]);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; serial.current++; }; }, []);
  const check = async () => {
    if (stale || !changes.length || (versionSwitch && !repositoryId)) return;
    const ticket = ++serial.current; const frozen = request;
    setReviewed(undefined); setConsentPlan(undefined); setUpstreamPlan(undefined); startMutation.reset();
    try {
      const value = versionSwitch && repositoryId
        ? await repositoryPlan.mutateAsync({ id: repositoryId, ...frozen })
        : await configurationPlan.mutateAsync({ id: deployment.id, ...frozen.deployment_settings![deployment.id] });
      if (mounted.current && ticket === serial.current && currentKey.current === key) { setReviewed({ key, request: frozen, plan: value }); setEditing(false); }
    } catch { /* Preserve settings for an explicit retry. */ }
  };
  const apply = async () => {
    if (!reviewed || !plan?.ready || currentKey.current !== reviewed.key || consentPlan !== plan || (upstream && upstreamPlan !== plan) || (versionSwitch && !repositoryId)) return;
    try {
      const result = versionSwitch && repositoryId
        ? await repositoryStart.mutateAsync({ id: repositoryId, ...reviewed.request, plan_digest: plan.plan_digest })
        : await configurationApply.mutateAsync({ id: deployment.id, ...reviewed.request.deployment_settings![deployment.id], plan_digest: plan.plan_digest });
      if (mounted.current) navigate(`/runs/${encodeURIComponent(result.run_id)}`);
    } catch { /* Never automatically replay an uncertain replacement request. */ }
  };
  return <div className="space-y-4">
    <p className="text-sm text-muted">{recipe.name} · {recipe.version}. Applies only to this deployment, preserving its devices and fabric. The catalog and other deployments stay unchanged.</p>
    {repositoryId ? <label className="grid gap-1.5 text-sm"><span>Recipe version</span><select aria-label="Recipe version" className="control w-full bg-panel p-2 text-sm" value={recipe.digest} disabled={startMutation.isPending} onChange={(event) => onRecipeChange(event.target.value, settings)}>{versions.map((version) => <option key={version.digest} value={version.digest}>{version.version} · {version.digest.slice(0, 19)}{version.digest === deployment.recipe_digest ? " · saved deployment" : ""}</option>)}</select></label> : <p className="text-sm text-muted">Configuring this deployment’s exact saved recipe. Switching versions requires selecting a linked repository.</p>}
    {recipe.digest !== deployment.recipe_digest ? <p className="text-sm text-muted">A different saved recipe version is selected. Its supported settings are shown below; removed settings and the version change must be reviewed before restarting.</p> : null}
    {stale ? <div className="space-y-2 border border-warning/40 p-3" role="alert"><p className="text-sm">The saved deployment changed while you were editing. Your values are retained, but the old review cannot be applied.</p><Button variant="outline" disabled={startMutation.isPending} onClick={() => { setBaseline({ key: savedKey, settings: saved }); setSettings(supportedSettings(saved)); setReviewed(undefined); setConsentPlan(undefined); setUpstreamPlan(undefined); setEditing(true); planMutation.reset(); startMutation.reset(); }}>Reload saved deployment settings</Button></div> : null}
    {editing ? <><RuntimeSelections manifest={manifest} parameters={settings.parameters} value={settings.selection} onParametersChange={(parameters) => setSettings((current) => ({ ...current, parameters }))} onChange={(selection) => setSettings((current) => ({ ...current, selection }))} disabled={startMutation.isPending} compact /><Button variant="outline" disabled={startMutation.isPending} onClick={() => { setSettings(supportedSettings(baseline.settings)); setReviewed(undefined); setConsentPlan(undefined); setUpstreamPlan(undefined); planMutation.reset(); startMutation.reset(); }}>Reset to saved deployment settings</Button></> : <Button variant="outline" disabled={startMutation.isPending} onClick={() => setEditing(true)}>Edit settings</Button>}
    <section className="space-y-2" aria-label="Changed settings"><h3 className="text-sm font-medium">Changed settings ({changes.length})</h3>{changes.length ? <dl className="divide-y divide-rule">{changes.map((change, index) => <div className="grid gap-1 py-2 text-sm sm:grid-cols-2" key={index}><dt>{change.name}</dt><dd className="break-all">{change.before === undefined ? "Upstream / automatic default" : change.sensitive ? "Hidden value" : JSON.stringify(change.before)} → {change.after === undefined ? "Upstream / automatic default" : change.sensitive ? "Hidden value" : JSON.stringify(change.after)}</dd></div>)}</dl> : <p className="text-sm text-muted">No runtime changes. Search settings above to choose an override.</p>}</section>
    {[planMutation, startMutation].map((mutation, index) => mutation.isError ? <RecipeError key={index} error={mutation.error} /> : null)}
    {startMutation.isError ? <p className="text-sm text-warning">Your settings and review are retained. Check <Link className="underline" to="/runs">recent operations</Link> before retrying an uncertain submission. Nothing is retried automatically.</p> : null}
    <Button variant="outline" disabled={stale || !changes.length || planMutation.isPending || startMutation.isPending} onClick={() => void check()}>{planMutation.isPending ? "Checking restart plan…" : plan ? "Recheck restart plan" : "Review changes and restart"}</Button>
    {plan ? <section className="space-y-3">
      {plan.diagnostics.map((diagnostic, index) => <p key={index} className={diagnostic.severity === "error" ? "text-sm text-fault" : "text-sm text-warning"}>{diagnostic.message}</p>)}
      {(plan.deployments || []).map((entry) => <div key={entry.source_deployment_id} className="border border-rule p-3"><LaunchPlanDetails plan={entry.deployment_plan} /></div>)}
      <label className="flex gap-2 border border-warning/40 p-3 text-sm"><input type="checkbox" checked={consentPlan === plan} disabled={startMutation.isPending || planMutation.isPending} onChange={(event) => setConsentPlan(event.target.checked ? plan : undefined)} /><span>I approve the reviewed settings, downloads and permissions, and starting the replacement{deployment.desired_state === "running" ? " after interrupting this deployment" : " from this stopped deployment"}. If replacement fails, restoration of the old deployment’s saved running or stopped state is attempted but not guaranteed.</span></label>
      {upstream ? <UpstreamExecutionAcknowledgment checked={upstreamPlan === plan} onChange={(checked) => setUpstreamPlan(checked ? plan : undefined)} disabled={startMutation.isPending || planMutation.isPending} /> : null}
      <Button disabled={!plan.ready || consentPlan !== plan || (upstream && upstreamPlan !== plan) || startMutation.isPending || planMutation.isPending || editing} onClick={() => void apply()}>{startMutation.isPending ? "Requesting restart…" : startMutation.isError ? "Retry Apply and restart" : "Apply and restart"}</Button>
    </section> : null}
  </div>;
}
