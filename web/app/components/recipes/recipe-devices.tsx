import { useEffect, useRef, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import { PlanDeploymentDialog } from "~/components/dialogs/plan-deployment-dialog";
import { RecipeReplacementDialog } from "~/components/dialogs/recipe-replacement-dialog";
import * as api from "~/lib/api";
import { qk, useLaunchProfiles, useNodes, useRecipeAvailability, useRecipeDownloads, useRuns, useSecrets } from "~/lib/queries";
import { bytes } from "~/lib/format";
import { AcquisitionPlanDetails, DeviceSelections, RuntimeSelections, type RuntimeSelection } from "./launch-options";
import { RecipeError, RecipeOperation, record, records, terminalRunStates } from "./workflow";

type Choices = RuntimeSelection & { targets: api.RecipeDownloadPlanRequest["targets"]; credentials: NonNullable<api.RecipeDownloadPlanRequest["credentials"]>; profile: string };
function defaults(recipe: api.RecipeDetail): Choices {
  return { targets: [], credentials: [], profile: "", variants: Object.fromEntries(records(record(recipe.manifest).artifacts).filter((artifact) => artifact.defaultVariant && Array.isArray(artifact.variants)).map((artifact) => [String(artifact.name), String(artifact.defaultVariant)])) };
}
function fileSummary(resources: api.RecipeAvailability["devices"][number]["resources"], online: boolean): string {
  if (!resources.length) return "None declared";
  if (!online) return "Last observation only; device offline";
  if (resources.every((resource) => resource.verification.state === "available" && !resource.verification.stale && resource.verification.verified_at)) return "Files downloaded";
  if (resources.some((resource) => resource.verification.state === "invalid")) return "Invalid files; recheck required";
  if (resources.some((resource) => resource.verification.state === "partial")) return "Partially downloaded";
  if (resources.some((resource) => resource.verification.state === "missing")) return "Missing files";
  return "Check existing files";
}
export function RecipeDevices({ recipe, repositoryId }: { recipe: api.RecipeDetail; repositoryId?: string }) {
  const digest = recipe.digest; const manifest = record(recipe.manifest);
  const nodes = useNodes(); const secrets = useSecrets(); const profiles = useLaunchProfiles(digest);
  const history = useRuns({ module: "library" });
  const replacements = (history.data?.items || []).filter((run) => run.kind === "recipe-update" && (record(run.input).target_digest === digest || Boolean(repositoryId && record(run.input).repository_id === repositoryId)));
  const [choices, setChoices] = useState(() => defaults(recipe));
  const recoveryDefaults = useRef(choices); const mounted = useRef(false);
  const [recovery, setRecovery] = useState<Choices>(); const [recoveryChecked, setRecoveryChecked] = useState(false);
  const [storageUnavailable, setStorageUnavailable] = useState(false);
  const [resumeRunId, setResumeRunId] = useState<string>();
  const [reviewed, setReviewed] = useState<{ key: string; request: api.RecipeDownloadPlanRequest; plan: api.RecipeDownloadPlan }>();
  const [consent, setConsent] = useState(false); const [lastRun, setLastRun] = useState<string>();
  const [runOpen, setRunOpen] = useState(false); const [replaceOpen, setReplaceOpen] = useState(false);
  const [replacementIDs, setReplacementIDs] = useState<string[]>([]);
  const serial = useRef(0); const client = useQueryClient();
  const runtime = choices.profile ? { launch_profile_id: choices.profile } : { variants: choices.variants, ...(choices.workload_index == null ? {} : { workload_index: choices.workload_index }) };
  const availabilitySelection: api.RecipeAvailabilityRequest = { ...runtime, refresh: false };
  const availability = useRecipeAvailability(digest, availabilitySelection);
  const attempts = useRecipeDownloads(digest);
  const request: api.RecipeDownloadPlanRequest = { ...runtime, targets: choices.targets, credentials: choices.credentials, ...(resumeRunId ? { resume_run_id: resumeRunId } : {}) };
  const key = JSON.stringify([digest, request]); const currentKey = useRef(key); currentKey.current = key;
  const plan = reviewed?.key === key ? reviewed.plan : undefined;
  const planMutation = useMutation({ mutationFn: (body: api.RecipeDownloadPlanRequest) => api.planRecipeDownload(digest, body), retry: false });
  const refresh = useMutation({ mutationFn: () => api.getRecipeAvailability(digest, { ...availabilitySelection, refresh: true }), retry: false,
    onSuccess: (value) => client.setQueryData(qk.recipeAvailability(digest, availabilitySelection), value) });
  const submit = useMutation({ mutationFn: async () => {
    if (!reviewed || reviewed.key !== currentKey.current || !reviewed.plan.ready || !consent) throw new Error("Review the current selection before confirming.");
    const result = reviewed.request.resume_run_id
      ? await api.resumeRecipeDownload(digest, reviewed.request.resume_run_id, { plan_digest: reviewed.plan.plan_digest, credentials: reviewed.request.credentials })
      : await api.startRecipeDownload(digest, { ...reviewed.request, plan_digest: reviewed.plan.plan_digest });
    setLastRun(result.run_id); setReviewed(undefined); setConsent(false); setResumeRunId(undefined);
    await client.invalidateQueries({ queryKey: qk.recipeDownloads(digest) });
  }, retry: false });
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);
  useEffect(() => {
    try { const saved = sessionStorage.getItem(`lmw.recipe-devices.${digest}`); if (saved) setRecovery({ ...recoveryDefaults.current, ...JSON.parse(saved) }); }
    catch { setStorageUnavailable(true); }
    setRecoveryChecked(true);
  }, [digest]);
  useEffect(() => {
    if (!recoveryChecked || recovery) return;
    try { sessionStorage.setItem(`lmw.recipe-devices.${digest}`, JSON.stringify({ ...choices, schemaVersion: 2 })); setStorageUnavailable(false); }
    catch { setStorageUnavailable(true); }
  }, [digest, choices, recovery, recoveryChecked]);
  useEffect(() => { serial.current++; setReviewed(undefined); setConsent(false); }, [key]);
  const check = async () => {
    const ticket = ++serial.current; setReviewed(undefined); setConsent(false); submit.reset();
    try { const value = await planMutation.mutateAsync(request); if (mounted.current && ticket === serial.current && currentKey.current === key) setReviewed({ key, request, plan: value }); }
    catch { /* Keep exact choices and show the typed failure. */ }
  };
  const selectResume = (attempt: api.RecipeDownloadRun) => {
    const frozen = attempt.input.plan;
    setChoices({ targets: frozen.targets, variants: frozen.variants, workload_index: frozen.workload_index, credentials: attempt.input.credentials || [], profile: "" });
    setResumeRunId(attempt.run_id); setRecovery(undefined); setReviewed(undefined); setConsent(false);
  };
  const running = [...new Map((availability.data?.devices || []).flatMap((device) => device.running_deployments).map((deployment) => [deployment.deployment_id, deployment])).values()];
  return <div className="space-y-5">
    <section className="space-y-3"><div className="flex flex-wrap items-center justify-between gap-3"><div><h2 className="font-display text-xl font-semibold">Device files and running versions</h2><p className="text-sm text-muted">Recipe packages, model files and running containers are independent. Files downloaded does not mean tested or ready to run.</p></div><Button variant="outline" disabled={refresh.isPending || submit.isPending} onClick={() => refresh.mutate()}>{refresh.isPending ? "Checking existing bytes…" : "Check existing files"}</Button></div>
      {[nodes, profiles, availability, attempts].map((query, index) => query.isError ? <RecipeError key={index} error={query.error} retry={() => void query.refetch()} /> : null)}
      {refresh.isError ? <RecipeError error={refresh.error} /> : null}
      {availability.isPending ? <p className="text-sm text-muted">Loading observed file availability…</p> : null}
      <div className="space-y-3">{(availability.data?.devices || []).map((device) => <article className="control space-y-3 p-4" key={device.node_id}><header className="flex flex-wrap justify-between gap-2"><h3 className="font-semibold">{device.node_name}</h3><p className="text-xs text-muted">{device.online ? "Online" : "Offline"} · Last checked {device.last_checked_at || "not yet"}</p></header><div className="grid gap-3 md:grid-cols-3"><div><h4 className="lmw-label">Model and data files</h4><p className="mt-1 text-sm">{fileSummary(device.resources.filter((resource) => resource.kind === "artifact"), device.online)}</p></div><div><h4 className="lmw-label">Images and helper package</h4><p className="mt-1 text-sm">{fileSummary(device.resources.filter((resource) => resource.kind !== "artifact"), device.online)}</p></div><div><h4 className="lmw-label">Running versions</h4>{device.running_deployments.length ? device.running_deployments.map((deployment) => <Link key={`${deployment.deployment_id}:${deployment.rank}`} className="mt-1 block text-sm underline" to={`/serving/deployments/${deployment.deployment_id}`}>{deployment.recipe_digest === digest ? "Selected saved version" : "Running older saved version"} · rank {deployment.rank} · {deployment.state}</Link>) : <p className="mt-1 text-sm">Not running</p>}</div></div>
        <details><summary className="cursor-pointer text-sm">Exact resources and observations</summary><div className="mt-2 space-y-2">{device.resources.map((resource) => <div className="border border-rule p-2 text-xs" key={resource.key}><p className="break-all font-mono">{resource.identity}</p><p className="break-all">{resource.destination}</p><p>{resource.verification.state}{resource.verification.stale ? " · stale observation" : ""} · {resource.verification.verified_at || "not verified"}</p>{(resource.verification.diagnostics || []).map((diagnostic, index) => <p className="text-warning" key={index}>{diagnostic.code}: {diagnostic.message}</p>)}</div>)}</div></details>
        {device.other_versions.length ? <details><summary className="cursor-pointer text-sm">Other versions on this device</summary><div className="mt-2 space-y-2">{device.other_versions.map((alternative, index) => <article className="space-y-2 border border-rule p-2 text-xs" key={index}><p className="break-all font-mono">{alternative.identity}</p><p>{alternative.verification.state}{alternative.verification.stale ? " · stale" : ""}</p>{alternative.supported_variants && Object.keys(alternative.supported_variants).length ? <Button size="sm" variant="outline" disabled={Boolean(resumeRunId)} onClick={() => setChoices((current) => ({ ...current, variants: { ...current.variants, ...alternative.supported_variants }, profile: "" }))}>Use this supported version in a new plan</Button> : <p>This is not a supported selected variant; it will not be substituted automatically.</p>}</article>)}</div></details> : null}
        <details><summary className="cursor-pointer text-sm">Runtime suitability — separate from downloading</summary>{device.runtime_diagnostics.length ? device.runtime_diagnostics.map((diagnostic, index) => <p key={index} className="mt-2 text-xs">{diagnostic.code}: {diagnostic.message}</p>) : <p className="mt-2 text-xs text-muted">Use the separate run plan to check hardware, ports, fabric and current occupancy.</p>}</details>
        {device.diagnostics.map((diagnostic, index) => <p className="text-sm text-warning" key={index}>{diagnostic.code}: {diagnostic.message}</p>)}{device.active_run_ids.map((runId) => <Link className="block text-sm underline" key={runId} to={`/runs/${runId}`}>Active download {runId}</Link>)}
      </article>)}</div>{availability.data?.diagnostics.map((diagnostic, index) => <p className="text-sm text-warning" key={index}>{diagnostic.code}: {diagnostic.message}</p>)}
    </section>

    <section className="control space-y-4 p-4"><h2 className="font-display text-xl font-semibold">Download to device</h2><p className="text-sm text-muted">Acquire every declared resource for the selected runtime. This does not create containers, run helpers, apply host changes, reserve GPUs, or start a model.</p>
      {storageUnavailable ? <p role="alert" className="text-sm text-warning">Browser device-choice recovery is unavailable. Current selections remain in memory.</p> : null}
      {recovery ? <div className="border border-warning/40 p-3"><p className="text-sm">Restore saved browser device choices? No operation will be repeated.</p><div className="mt-2 flex gap-2"><Button variant="outline" onClick={() => { setChoices(recovery); setRecovery(undefined); }}>Restore device choices</Button><Button variant="ghost" onClick={() => setRecovery(undefined)}>Use current choices</Button></div></div> : null}
      {resumeRunId ? <div className="border border-warning/40 p-3"><p className="text-sm">Resume attempt {resumeRunId}. Content, devices, destinations and runtime are frozen. Review a fresh plan; reconnect never resumes automatically.</p><Button className="mt-2" variant="outline" size="sm" onClick={() => setResumeRunId(undefined)}>Plan a new download instead</Button></div> : null}
      <DeviceSelections nodes={nodes.data || []} selected={choices.targets.map((target) => target.node_id)} disabled={Boolean(resumeRunId) || submit.isPending} onChange={(selected) => setChoices((current) => ({ ...current, targets: selected.map((node_id) => current.targets.find((target) => target.node_id === node_id) || { node_id }) }))} />
      {choices.targets.map((target, index) => { const node = nodes.data?.find((item) => item.id === target.node_id); const roots = records(record(node?.inventory).cache_roots); return <label key={target.node_id} className="grid gap-1 text-sm">{node?.display_name || target.node_id} cache root<select className="control bg-panel p-2" disabled={Boolean(resumeRunId) || submit.isPending} value={target.cache_root || ""} onChange={(event) => setChoices((current) => ({ ...current, targets: current.targets.map((item, i) => i === index ? { node_id: target.node_id, ...(event.target.value ? { cache_root: event.target.value } : {}) } : item) }))}><option value="">First reported writable root (shown in preview)</option>{roots.map((root) => <option key={String(root.path)} value={String(root.path)} disabled={root.writable !== true}>{String(root.path)}{root.writable ? "" : " · read-only"}</option>)}</select></label>; })}
      {profiles.data?.length ? <label className="grid gap-1 text-sm">Saved launch profile<select className="control bg-panel p-2" disabled={Boolean(resumeRunId) || submit.isPending} value={choices.profile} onChange={(event) => { const profile = profiles.data.find((item) => item.id === event.target.value); setChoices((current) => ({ ...current, profile: event.target.value, ...(profile ? { workload_index: undefined, variants: profile.variants || {} } : {}) })); }}><option value="">Explicit runtime and variants</option>{profiles.data.map((profile) => <option key={profile.id} value={profile.id}>{profile.name}</option>)}</select></label> : null}
      <RuntimeSelections manifest={manifest} value={choices} disabled={Boolean(resumeRunId) || submit.isPending} onChange={(selection) => setChoices((current) => ({ ...current, ...selection, profile: "" }))} />
      <details><summary className="cursor-pointer text-sm">Resource-specific download credentials</summary><p className="mt-2 text-xs text-muted">Only saved Hugging Face or registry credentials can be selected for their exact host and resource. AI/Codex credentials are never borrowed.</p>{secrets.isError ? <RecipeError error={secrets.error} retry={() => void secrets.refetch()} /> : null}<div className="mt-3 space-y-2">{choices.credentials.map((credential, index) => <div key={index} className="grid gap-2 border border-rule p-2 md:grid-cols-3">{(["resource", "host"] as const).map((name) => <Input key={name} aria-label={`Download credential ${name}`} placeholder={name} value={credential[name]} onChange={(event) => setChoices((current) => ({ ...current, credentials: current.credentials.map((item, i) => i === index ? { ...item, [name]: event.target.value } : item) }))} />)}<select aria-label="Download saved credential" className="control bg-panel p-2" value={credential.secret_id} onChange={(event) => setChoices((current) => ({ ...current, credentials: current.credentials.map((item, i) => i === index ? { ...item, secret_id: event.target.value } : item) }))}><option value="">Select saved credential</option>{(secrets.data || []).filter((secret) => ["huggingface", "registry"].includes(secret.purpose)).map((secret) => <option key={secret.id} value={secret.id}>{secret.name}</option>)}</select><Button variant="outline" size="sm" onClick={() => setChoices((current) => ({ ...current, credentials: current.credentials.filter((_, i) => i !== index) }))}>Remove credential</Button></div>)}</div><Button className="mt-2" variant="outline" size="sm" onClick={() => setChoices((current) => ({ ...current, credentials: [...current.credentials, { resource: "", host: "", secret_id: "" }] }))}>Add credential selection</Button></details>
      <Button variant="outline" disabled={!choices.targets.length || planMutation.isPending || submit.isPending || Boolean(recovery)} onClick={() => void check()}>{planMutation.isPending ? "Inspecting exact resources…" : plan ? "Recheck identical download choices" : "Review download plan"}</Button>
      {planMutation.isError ? <RecipeError error={planMutation.error} /> : null}{submit.isError ? <RecipeError error={submit.error} /> : null}
      {plan ? <section className="space-y-3"><AcquisitionPlanDetails plan={plan} /><label className="flex gap-2 border border-warning/40 p-3 text-sm"><input type="checkbox" checked={consent} onChange={(event) => setConsent(event.target.checked)} /><span>I approve the listed download/copy sources, exact versions, credentials and device storage destinations. No model will be started.</span></label><Button disabled={!plan.ready || !consent || submit.isPending || submit.isError} onClick={() => submit.mutate()}>{submit.isPending ? "Requesting download…" : resumeRunId ? "Resume reviewed download" : "Download to selected devices"}</Button></section> : null}
    </section>

    <section className="control space-y-3 p-4"><h2 className="font-display text-xl font-semibold">Run on device</h2><p className="text-sm text-muted">The shared run planner checks ranked hardware, settings and all required resources. It defaults to verified existing files only; Download and run requires an explicit choice.</p><Button variant="outline" onClick={() => setRunOpen(true)}>Run on device</Button>
      {repositoryId && running.length ? <fieldset className="space-y-2"><legend className="py-2 font-medium">Replace selected running versions</legend>{running.map((deployment) => <label key={deployment.deployment_id} className="flex gap-2 text-sm"><input type="checkbox" checked={replacementIDs.includes(deployment.deployment_id)} disabled={deployment.recipe_digest === digest} onChange={() => setReplacementIDs((current) => current.includes(deployment.deployment_id) ? current.filter((id) => id !== deployment.deployment_id) : [...current, deployment.deployment_id])} /><span>{deployment.deployment_id} · {deployment.recipe_digest === digest ? "Already using this saved version" : "Older saved version"} · {deployment.state}</span></label>)}<Button variant="outline" disabled={!replacementIDs.length} onClick={() => setReplaceOpen(true)}>Replace running version</Button></fieldset> : null}
    </section>

    <section className="space-y-3"><h2 className="font-display text-xl font-semibold">Download history and recovery</h2>{lastRun && !attempts.data?.some((attempt) => attempt.run_id === lastRun) ? <RecipeOperation runId={lastRun} digest={digest} /> : null}{(attempts.data || []).map((attempt) => <article className="control space-y-3 p-3" key={attempt.run_id}><RecipeOperation runId={attempt.run_id} digest={digest} /><p className="text-xs text-muted">{attempt.state === "succeeded" ? "All declared files downloaded and verified at completion; recheck current availability before running." : "Partial successes remain separately usable. Completion requires every required resource."}</p>{attempt.items.map((item) => { const progress = record(record(item.checkpoint).progress); return <div key={item.id} className="border border-rule p-2 text-xs"><p className="break-all">{nodes.data?.find((node) => node.id === item.node_id)?.display_name || item.node_id} · {item.resource.kind} · {item.state}</p><p className="break-all font-mono">{item.resource.identity}</p><p>{String(progress.phase || item.state)}{progress.current_file ? ` · ${String(progress.current_file)}` : ""} · {typeof progress.bytes_done === "number" ? bytes(progress.bytes_done) : "Unknown bytes"} / {typeof progress.bytes_total === "number" ? bytes(progress.bytes_total) : "Unknown total"}</p>{item.error ? <p className="text-fault">{item.error.code}: {item.error.message}</p> : null}</div>; })}{terminalRunStates[attempt.state] && attempt.state !== "succeeded" ? <Button variant="outline" disabled={submit.isPending} onClick={() => selectResume(attempt)}>Resume</Button> : null}<details><summary className="cursor-pointer text-xs">Frozen runtime, variants and destinations</summary><pre className="max-h-64 overflow-auto text-xs">{JSON.stringify(attempt.input.plan, null, 2)}</pre></details></article>)}</section>
    <section className="space-y-3"><h2 className="font-display text-xl font-semibold">Recent running-version replacements</h2><p className="text-sm text-muted">Persisted operations remain visible after refresh or login. <Link className="underline" to="/runs">Open full operation history</Link> for older attempts.</p>{history.isError ? <RecipeError error={history.error} retry={() => void history.refetch()} /> : null}{replacements.map((run) => <RecipeOperation key={run.id} runId={run.id} digest={digest} repositoryId={repositoryId} />)}</section>
    <PlanDeploymentDialog open={runOpen} onOpenChange={setRunOpen} initialRecipeDigest={digest} initialNodeIds={choices.targets.map((target) => target.node_id)} initialSelection={{ variants: choices.variants, workload_index: choices.workload_index }} />
    {repositoryId ? <RecipeReplacementDialog open={replaceOpen} onOpenChange={setReplaceOpen} repositoryId={repositoryId} targetDigest={digest} deploymentIDs={replacementIDs} /> : null}
  </div>;
}
