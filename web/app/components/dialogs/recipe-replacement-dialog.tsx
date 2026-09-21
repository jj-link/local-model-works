import { useEffect, useRef, useState } from "react";
import { Link } from "react-router";
import { useQueryClient } from "@tanstack/react-query";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "~/components/ui/dialog";
import { Button } from "~/components/ui/button";
import { qk, usePlanRecipeRepositoryReplacement, useRecipe, useRun, useStartRecipeRepositoryReplacement } from "~/lib/queries";
import type { RecipeRepositoryReplacementPlanRequest, RecipeUpdatePlan } from "~/lib/api";
import { LaunchPlanDetails, RuntimeSelections, UpstreamExecutionAcknowledgment } from "~/components/recipes/launch-options";
import { RecipeError, RecipeOperation, record, terminalRunStates } from "~/components/recipes/workflow";

export function RecipeReplacementDialog({ open, onOpenChange, repositoryId, targetDigest, deploymentIDs }: {
  open: boolean; onOpenChange: (open: boolean) => void; repositoryId: string; targetDigest: string; deploymentIDs: string[];
}) {
  const target = useRecipe(targetDigest);
  const planMutation = usePlanRecipeRepositoryReplacement();
  const startMutation = useStartRecipeRepositoryReplacement();
  const resetPlan = planMutation.reset; const resetStart = startMutation.reset;
  const [reviewed, setReviewed] = useState<{ key: string; plan: RecipeUpdatePlan }>();
  const [runId, setRunId] = useState<string>();
  const [consent, setConsent] = useState(false);
  const [acknowledgedPlan, setAcknowledgedPlan] = useState<RecipeUpdatePlan>();
  const [deploymentSettings, setDeploymentSettings] = useState<NonNullable<RecipeRepositoryReplacementPlanRequest["deployment_settings"]>>({});
  const run = useRun(runId);
  const client = useQueryClient();
  const serial = useRef(0); const settled = useRef("");
  const selectionKey = JSON.stringify([repositoryId, targetDigest, [...deploymentIDs].sort()]);
  const key = JSON.stringify([selectionKey, deploymentSettings]);
  const currentKey = useRef(key); currentKey.current = key;
  const currentOpen = useRef(open); currentOpen.current = open;
  const plan = reviewed?.key === key ? reviewed.plan : undefined;
  const requiresUpstreamConsent = Boolean(plan?.deployments?.some((deployment) => deployment.current_permissions?.includes("host.upstream-exec") || deployment.deployment_plan.risks?.includes("host.upstream-exec")));
  const upstreamConsent = Boolean(plan && acknowledgedPlan === plan);
  useEffect(() => { setDeploymentSettings({}); }, [open, selectionKey]);
  useEffect(() => { serial.current++; setReviewed(undefined); setConsent(false); setAcknowledgedPlan(undefined); setRunId(undefined); resetPlan(); resetStart(); }, [open, key, resetPlan, resetStart]);
  useEffect(() => {
    if (!runId || !run.data || !terminalRunStates[run.data.state] || settled.current === runId) return;
    settled.current = runId;
    void client.invalidateQueries({ queryKey: qk.deployments });
    void client.invalidateQueries({ queryKey: qk.recipe(targetDigest) });
  }, [client, runId, run.data, targetDigest]);
  const check = async () => {
    const ticket = ++serial.current; setReviewed(undefined); setConsent(false); setAcknowledgedPlan(undefined); resetStart();
    try { const value = await planMutation.mutateAsync({ id: repositoryId, target_digest: targetDigest, deployment_ids: deploymentIDs, deployment_settings: deploymentSettings }); if (serial.current === ticket && currentKey.current === key && currentOpen.current) setReviewed({ key, plan: value }); }
    catch { /* Retain the exact selected saved version and deployments. */ }
  };
  const replace = async () => {
    if (!plan?.ready || !consent || (requiresUpstreamConsent && !upstreamConsent) || currentKey.current !== reviewed?.key) return;
    try { const result = await startMutation.mutateAsync({ id: repositoryId, target_digest: targetDigest, deployment_ids: deploymentIDs, deployment_settings: deploymentSettings, plan_digest: plan.plan_digest }); setRunId(result.run_id); }
    catch { /* An uncertain mutation outcome must never be automatically replayed. */ }
  };
  return <Dialog open={open} onOpenChange={onOpenChange}><DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-3xl"><DialogHeader><DialogTitle>Replace running version</DialogTitle><DialogDescription>Use a version already saved in this repository. Only the explicitly selected running deployments are replaced; the catalog version is never changed by this operation.</DialogDescription></DialogHeader>
    <p className="text-sm">Saved target: {target.data?.name || "Loading…"} · {target.data?.version}</p><details><summary className="cursor-pointer text-xs">Exact saved identity</summary><p className="break-all font-mono text-xs">{targetDigest}</p></details>
    <section className="space-y-2"><h3 className="font-medium">Selected running deployments</h3>{deploymentIDs.map((id) => <Link key={id} className="block break-all text-sm underline" to={`/serving/deployments/${id}`}>{id}</Link>)}{!deploymentIDs.length ? <p role="alert" className="text-sm text-fault">Select at least one running deployment.</p> : null}</section>
    {!runId && target.data ? <section className="space-y-3"><h3 className="font-medium">Target-supported launch inputs</h3><p className="text-sm text-muted">Current inputs are preserved unless you explicitly replace them below. Replacing inputs uses only this target's declared defaults and the values you enter; old parameters and artifact choices are not silently translated.</p>{deploymentIDs.map((id) => {
      const settings = deploymentSettings[id];
      return <article key={id} className="space-y-3 border border-rule p-3"><label className="flex gap-2 text-sm"><input type="checkbox" checked={Boolean(settings)} disabled={startMutation.isPending} onChange={(event) => setDeploymentSettings((current) => { const next = { ...current }; if (event.target.checked) next[id] = { parameters: {}, variants: {} }; else delete next[id]; return next; })} /><span className="break-all">Replace launch inputs for {id}</span></label>{settings ? <RuntimeSelections manifest={record(target.data?.manifest)} value={{ workload_index: settings.workload_index, variants: settings.variants || {} }} parameters={settings.parameters || {}} onChange={(selection) => setDeploymentSettings((current) => ({ ...current, [id]: { ...current[id], ...selection } }))} onParametersChange={(parameters) => setDeploymentSettings((current) => ({ ...current, [id]: { ...current[id], parameters } }))} disabled={startMutation.isPending} /> : null}</article>;
    })}</section> : null}
    {target.isError ? <RecipeError error={target.error} retry={() => void target.refetch()} /> : null}
    {[planMutation, startMutation].map((mutation, index) => mutation.isError ? <RecipeError key={index} error={mutation.error} /> : null)}
    {!runId ? <Button variant="outline" disabled={!deploymentIDs.length || planMutation.isPending || startMutation.isPending} onClick={() => void check()}>{planMutation.isPending ? "Checking selected devices…" : plan ? "Recheck identical selection" : "Review replacement plan"}</Button> : null}
    {plan ? <section className="space-y-4">{(plan.deployments || []).map((deployment) => <article key={deployment.source_deployment_id} className="space-y-3 border border-rule p-3"><h3 className="break-all font-medium">Replace {deployment.source_deployment_id}</h3><p className="break-all text-xs">Running saved digest: {deployment.source_digest} → {targetDigest}</p><p className="text-sm">Existing ranks, devices and fabric remain selected. Any explicit target input changes are included in this review.</p><details><summary className="cursor-pointer text-sm">Current and proposed launch inputs</summary><pre className="mt-2 overflow-auto text-xs">{JSON.stringify({ current: { parameters: deployment.parameters, variants: deployment.variants, workload_index: deployment.workload_index }, proposed: { parameters: deployment.deployment_plan.parameters, variants: deployment.deployment_plan.variants, workload_index: deployment.deployment_plan.workload_index } }, null, 2)}</pre></details><p className="text-sm">Added permissions: {deployment.added_permissions.join(", ") || "None"}</p><p className="text-sm">Removed permissions: {deployment.removed_permissions.join(", ") || "None"}</p><LaunchPlanDetails plan={deployment.deployment_plan} /></article>)}{plan.unchanged_deployment_ids?.length ? <p className="text-sm">Already using this saved version; no change: {plan.unchanged_deployment_ids.join(", ")}</p> : null}{plan.diagnostics.map((diagnostic, index) => <p key={index} className="text-sm text-warning">{diagnostic.code}: {diagnostic.message}</p>)}
      {!runId ? <label className="flex gap-2 border border-warning/40 p-3 text-sm"><input type="checkbox" checked={consent} onChange={(event) => setConsent(event.target.checked)} /><span>I approve the listed downloads, permissions and restart interruption. If replacement fails, restoration of the selected old running versions will be attempted; restoration is not guaranteed. The saved catalog version is not rolled back.</span></label> : null}
      {requiresUpstreamConsent ? <p className="text-sm text-warning">Saved source revisions and version records remain available. They are not a snapshot of the node: restoring an old running version cannot guarantee reversal of upstream downloads, builds, installed dependencies or other host changes.</p> : null}
      {!runId && requiresUpstreamConsent ? <UpstreamExecutionAcknowledgment checked={upstreamConsent} onChange={(checked) => setAcknowledgedPlan(checked ? plan : undefined)} disabled={startMutation.isPending || planMutation.isPending} /> : null}
    </section> : null}
    {runId ? <RecipeOperation runId={runId} digest={targetDigest} repositoryId={repositoryId} /> : null}
    {startMutation.isError ? <p className="text-sm text-warning">Review <Link className="underline" to="/runs">recent operations</Link> before retrying an uncertain request. No automatic replacement retry occurs.</p> : null}
    <DialogFooter><Button variant="outline" onClick={() => onOpenChange(false)}>Close</Button>{!runId ? <Button disabled={!plan?.ready || !consent || (requiresUpstreamConsent && !upstreamConsent) || startMutation.isPending || startMutation.isError || planMutation.isPending} onClick={() => void replace()}>Replace selected running versions</Button> : null}</DialogFooter>
  </DialogContent></Dialog>;
}
