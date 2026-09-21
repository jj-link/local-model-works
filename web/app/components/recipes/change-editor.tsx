import { useEffect, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useLocation } from "react-router";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import { Label } from "~/components/ui/label";
import * as api from "~/lib/api";
import { qk, useDeployment, useRecipeDraft, useSecrets } from "~/lib/queries";
import { ConfigurationComparison, RecipeError, RecipeOperation, record, records, terminalRunStates } from "./workflow";
import { draftPath } from "./links";

type Manifest = Record<string, unknown>;
type Candidate = api.RecipeDraft["candidates"][number];
type FileSelection = api.RecipeDraftContextFile;
type FileBuffer = { selection: FileSelection; origin: string; content: string; dirty: boolean };
type Excerpt = api.RecipeGenerationPreviewRequest["run_excerpts"][number];
type ReferenceInputs = Parameters<typeof api.resolveRecipeDraftReferences>[2];
type Buffer = {
  schemaVersion: 2; baseVersion: number; manifest: Manifest; selected: api.RecipeDraftAssetSelection[];
  context: api.RecipeDraftContextFile[]; answers: Record<string, string>; jsonText: string; jsonError: string;
  manifestDirty: boolean; contextDirty: boolean; answersDirty: boolean; rawDirty: boolean;
  files: Record<string, FileBuffer>; instruction: string; providerId: string;
  diagnosticIds: string[]; runExcerpts: Excerpt[]; references: ReferenceInputs;
};
const importInstruction = "Investigate the whole pinned repository and relevant linked instructions. Preserve upstream Dockerfiles, scripts and launch commands. Produce a separate recipe for each documented launch procedure, with evidence, hardware requirements, fixed upstream models or documented deployment parameters, and explicit adaptations with reasons. Ask specific questions only for information that cannot be established from the sources. Do not execute upstream code.";
function freshBuffer(draft: api.RecipeDraft): Buffer {
  return { schemaVersion: 2, baseVersion: draft.version, manifest: record(draft.manifest), selected: draft.selected_assets,
    context: draft.context_selection, answers: Object.fromEntries(draft.questions.map((question) => [question.id, question.answer || ""])),
    jsonText: JSON.stringify(draft.manifest, null, 2), jsonError: "", manifestDirty: false, contextDirty: false,
    answersDirty: false, rawDirty: false, files: {}, instruction: draft.change_context?.base_recipe_digest ? "" : importInstruction, providerId: "", diagnosticIds: [], runExcerpts: [], references: {} };
}
function readRecovery(id: string): { value?: Partial<Buffer>; unavailable?: boolean } {
  try {
    const raw = sessionStorage.getItem(`lmw.recipe-draft.${id}`);
    if (!raw) return {};
    const value = JSON.parse(raw) as Partial<Buffer>;
    if (!value.manifest || !Array.isArray(value.selected) || !Array.isArray(value.context)) return { unavailable: true };
    // The original recovery shape did not distinguish independent dirty buffers.
    return { value: { ...value, jsonText: value.jsonText ?? JSON.stringify(value.manifest, null, 2),
      manifestDirty: value.manifestDirty ?? true, contextDirty: value.contextDirty ?? true } };
  } catch { return { unavailable: true }; }
}
function localChanges(buffer: Buffer): boolean {
  return buffer.manifestDirty || buffer.contextDirty || buffer.answersDirty || buffer.rawDirty || Object.values(buffer.files).some((file) => file.dirty);
}
function fileKey(selection: FileSelection): string {
  return JSON.stringify([selection.source_commit || "", selection.origin || "", selection.path, selection.sha256]);
}
async function waitForRun(runId: string, signal: AbortSignal): Promise<void> {
  while (true) {
    signal.throwIfAborted();
    const run = await api.getRun(runId, { signal });
    if (terminalRunStates[run.state]) {
      if (run.state !== "succeeded") throw new Error(`Operation ${run.state}. Review run ${runId}; the next save step was not started.`);
      return;
    }
    await new Promise<void>((resolve, reject) => {
      const abort = () => { clearTimeout(timer); reject(signal.reason); };
      const timer = setTimeout(() => { signal.removeEventListener("abort", abort); resolve(); }, 750);
      signal.addEventListener("abort", abort, { once: true });
    });
  }
}

function ReviewValues({ value }: { value: Record<string, unknown> }) {
  return <dl className="grid gap-2 text-sm">{Object.entries(value).map(([key, item]) => <div key={key}><dt className="font-medium">{key}</dt><dd className="break-all whitespace-pre-wrap font-mono text-xs">{typeof item === "string" ? item : JSON.stringify(item, null, 2)}</dd></div>)}</dl>;
}

function ProcedureReview({ manifest, adaptations = [] }: { manifest: Manifest; adaptations?: { path: string; description: string; reason: string }[] }) {
  const workloads = records(manifest.workloads);
  const parameters = records(manifest.parameters);
  return <div className="space-y-4">
    <section className="space-y-2"><h4 className="font-semibold">Executable procedure</h4>
      {!workloads.length ? <p className="text-sm text-warning">No executable runtime has been established yet. Investigate the repository or answer the missing-information questions below.</p> : workloads.map((workload, index) => <article className="space-y-2 border border-rule p-3" key={index}><h5 className="font-medium">{String(workload.name || `Runtime ${index + 1}`)}</h5><ReviewValues value={workload} /></article>)}
      {["prepare", "verify"].map((key) => manifest[key] ? <details key={key}><summary className="cursor-pointer text-sm">{key === "prepare" ? "Upstream preparation" : "Declared verification"} (not executed during import)</summary><pre className="mt-2 overflow-auto text-xs">{JSON.stringify(manifest[key], null, 2)}</pre></details> : null)}
    </section>
    <section className="space-y-2"><h4 className="font-semibold">Hardware requirements</h4>{Object.keys(record(manifest.compatibility)).length ? <ReviewValues value={record(manifest.compatibility)} /> : <p className="text-sm text-warning">No hardware requirements recorded. This is not evidence of hardware compatibility.</p>}<p className="text-xs text-muted">Requirements are source-derived declarations, not a successful hardware test.</p></section>
    <section className="space-y-2"><h4 className="font-semibold">Upstream models and data</h4>{records(manifest.artifacts).map((artifact, index) => <article key={index} className="border border-rule p-3"><ReviewValues value={artifact} /></article>)}<p className="text-xs text-muted">Recorded upstream model references are recipe facts, not an import-time model selection. Any documented configurable model is a deployment parameter below.</p></section>
    <section className="space-y-2"><h4 className="font-semibold">Deployment parameters</h4>{parameters.length ? parameters.map((parameter, index) => <article key={index} className="border border-rule p-3"><ReviewValues value={parameter} /></article>) : <p className="text-sm text-muted">No deployment parameters declared.</p>}<p className="text-xs text-muted">Choose parameter values when running the saved recipe, not while importing it.</p></section>
    <section className="space-y-2"><h4 className="font-semibold">Adaptations to upstream</h4>{adaptations.length ? adaptations.map((adaptation, index) => <article key={index} className="border border-warning/40 p-3 text-sm"><p className="break-all font-mono text-xs">{adaptation.path}</p><p>{adaptation.description}</p><p className="mt-1 text-muted">Reason: {adaptation.reason}</p></article>) : <p className="text-sm text-muted">No adaptations reported.</p>}</section>
  </div>;
}

export function ChangeEditor({ initialDraft, section, onSaved, initialAssistance }: { initialDraft: api.RecipeDraft; section: "configuration" | "updates"; onSaved: (digest: string) => void; initialAssistance?: { providerId: string; instruction: string } }) {
  const id = initialDraft.id;
  const query = useRecipeDraft(id);
  const draft = query.data || initialDraft;
  const adding = !draft.change_context?.base_recipe_digest;
  const needsProcedureReview = adding && !draft.review;
  const client = useQueryClient();
  const location = useLocation();
  const providerCatalog = useQuery({ queryKey: ["recipe-assistant", "providers"], queryFn: ({ signal }) => api.listRecipeAssistantProviders({ signal }) });
  const secrets = useSecrets();
  const deployment = useDeployment(draft.change_context?.deployment_id);
  const comparison = useQuery({ queryKey: [...qk.recipeComparison(id), draft.version], queryFn: ({ signal }) => api.compareRecipeDraft(id, { signal }) });
  const [recovery, setRecovery] = useState(() => readRecovery(id));
  const [buffer, setBuffer] = useState<Buffer>(() => ({ ...freshBuffer(initialDraft), ...(!recovery.value ? initialAssistance : undefined) }));
  const [storageUnavailable, setStorageUnavailable] = useState(Boolean(recovery.unavailable));
  const [advanced, setAdvanced] = useState(false);
  const [authoring, setAuthoring] = useState(false);
  const [openedFile, setOpenedFile] = useState("");
  const [newFile, setNewFile] = useState("");
  const [queuedRun, setQueuedRun] = useState<string>();
  const [warningsAccepted, setWarningsAccepted] = useState<string[]>([]);
  const [preview, setPreview] = useState<{ value: api.RecipeGenerationPreview; request: api.RecipeGenerationPreviewRequest; version: number }>();
  const [contentConsent, setContentConsent] = useState(false);
  const [reviewLatest, setReviewLatest] = useState(false);
  const lifetime = useRef<AbortController | null>(null);
  const previewSerial = useRef(0);
  const fileRequest = useRef(0);
  const dirty = localChanges(buffer);
  const conflict = dirty && buffer.baseVersion !== draft.version;
  const active = Boolean(draft.operation);
  const editable = draft.state !== "installed";
  const canInspect = draft.state === "failed" || (draft.state === "needs_input" && draft.candidates.length === 0)
    || Boolean(draft.change_context?.base_recipe_digest && !draft.run_id)
    || (draft.change_context?.kind === "repair" && draft.change_context.base_source_status === "unavailable" && !["installed", "packaged"].includes(draft.state));
  const providers = providerCatalog.data?.providers ?? [];
  const providerId = buffer.providerId;
  const selectedProvider = providers.find((provider) => provider.id === providerId);
  const settingsURL = `/settings/ai-assistance?return=${encodeURIComponent(location.pathname + location.search)}`;
  const warnings = draft.diagnostics.filter((diagnostic) => diagnostic.severity === "warning" && !diagnostic.blocking);
  const blockers = draft.diagnostics.filter((diagnostic) => diagnostic.blocking);
  const acceptedTokens = warnings.flatMap((warning) => warning.acknowledgement && warningsAccepted.includes(warning.acknowledgement) ? [warning.acknowledgement] : []);
  const allWarningsAccepted = warnings.every((warning) => warning.acknowledgement && acceptedTokens.includes(warning.acknowledgement));
  const mutation = useMutation({ mutationFn: async (action: () => Promise<void>) => action(), retry: false,
    onSettled: () => { void query.refetch(); } });
  const disabled = !editable || active || mutation.isPending || conflict || Boolean(recovery.value);

  useEffect(() => {
    const controller = new AbortController(); lifetime.current = controller;
    return () => controller.abort(new Error("Navigation stopped the save continuation. Review the persisted operation before continuing."));
  }, []);
  useEffect(() => {
    if (draft.version !== buffer.baseVersion && !dirty && !recovery.value) {
      setBuffer((current) => ({ ...current, ...freshBuffer(draft), instruction: current.instruction, providerId: current.providerId,
        diagnosticIds: current.diagnosticIds, runExcerpts: current.runExcerpts, references: current.references, files: current.files }));
    }
  }, [draft, buffer.baseVersion, dirty, recovery.value]);
  useEffect(() => {
    if (recovery.value) return;
    try {
      const hasInput = localChanges(buffer) || (buffer.instruction && buffer.instruction !== importInstruction) || buffer.providerId || buffer.runExcerpts.length || buffer.diagnosticIds.length || buffer.references?.credentials?.length || buffer.references?.file_checksums?.length;
      if (hasInput) sessionStorage.setItem(`lmw.recipe-draft.${id}`, JSON.stringify(buffer));
      else sessionStorage.removeItem(`lmw.recipe-draft.${id}`);
      setStorageUnavailable(false);
    }
    catch { setStorageUnavailable(true); }
  }, [id, buffer, recovery.value]);
  useEffect(() => { previewSerial.current++; setPreview(undefined); setContentConsent(false); }, [draft.version, buffer.providerId, buffer.instruction, buffer.diagnosticIds, buffer.runExcerpts, providerCatalog.data?.version, selectedProvider]);

  const patch = (value: Partial<Buffer>) => setBuffer((current) => ({ ...current, ...value }));
  const adopt = (saved: api.RecipeDraft, part: "manifest" | "context" | "all" | "file" | "metadata", savedPath?: string) => {
    client.setQueryData(qk.recipeDraft(id), saved);
    setBuffer((current) => {
      const next = { ...current, baseVersion: saved.version };
      if (part === "all" || part === "manifest") {
        next.manifest = record(saved.manifest); next.selected = saved.selected_assets; next.manifestDirty = false;
        next.answers = Object.fromEntries(saved.questions.map((question) => [question.id, question.answer || ""])); next.answersDirty = false;
        if (!current.rawDirty) { next.jsonText = JSON.stringify(saved.manifest, null, 2); next.jsonError = ""; }
      }
      if (part === "all" || part === "context") { next.context = saved.context_selection; next.contextDirty = false; }
      if (part === "file") {
        const files = { ...current.files };
        for (const [key, file] of Object.entries(files)) if (file.origin === "generated" && file.selection.path === savedPath) {
          const candidate = saved.candidates.find((item) => item.origin === "generated" && item.path === savedPath);
          files[key] = { ...file, dirty: false, selection: candidate ? { path: candidate.path, sha256: candidate.sha256, origin: candidate.origin } : file.selection };
        }
        next.files = files;
        if (!current.manifestDirty) next.selected = saved.selected_assets;
        else {
          const candidate = saved.candidates.find((item) => item.origin === "generated" && item.path === savedPath);
          if (candidate) next.selected = current.selected.map((item) => item.origin === "generated" && item.path === savedPath ? { ...item, sha256: candidate.sha256 } : item);
        }
      }
      return next;
    });
    void client.invalidateQueries({ queryKey: qk.recipeComparison(id) });
  };
  const saveWork = async (version = buffer.baseVersion) => {
    const saved = await api.updateRecipeDraft(id, version, { manifest: buffer.manifest, selected_assets: buffer.selected,
      answers: Object.entries(buffer.answers).map(([question_id, answer]) => ({ question_id, answer })), acknowledged_warnings: acceptedTokens });
    adopt(saved, "manifest"); setReviewLatest(false);
  };
  const queue = async (action: Promise<api.RecipeDraftOperationAccepted>) => { const result = await action; setQueuedRun(result.run_id); await query.refetch(); };
  const replaceManifest = (manifest: Manifest) => patch({ manifest, manifestDirty: true, ...(!buffer.rawDirty ? { jsonText: JSON.stringify(manifest, null, 2) } : {}) });
  const setField = (sectionName: string, key: string, value: unknown) => replaceManifest({ ...buffer.manifest, [sectionName]: { ...record(buffer.manifest[sectionName]), [key]: value } });
  const setArray = (name: string, values: Manifest[]) => replaceManifest({ ...buffer.manifest, [name]: values });
  const setArrayField = (name: string, index: number, key: string, value: unknown) => {
    const values = records(buffer.manifest[name]); values[index] = { ...values[index], [key]: value }; setArray(name, values);
  };
  const contextKey = (selection: FileSelection) => fileKey({ ...selection, origin: selection.origin || "source", source_commit: selection.source_commit || draft.resolved_commit });
  const toggleContext = (selection: FileSelection) => patch({ contextDirty: true,
    context: buffer.context.some((item) => contextKey(item) === contextKey(selection)) ? buffer.context.filter((item) => contextKey(item) !== contextKey(selection)) : [...buffer.context, selection] });
  const openFile = async (selection: FileSelection) => {
    const key = fileKey(selection); const requestId = ++fileRequest.current;
    if (buffer.files[key]) { setOpenedFile(key); return; }
    const file = await api.getRecipeDraftFile(id, selection);
    setBuffer((current) => ({ ...current, files: { ...current.files, [key]: { selection, origin: file.origin, content: file.content, dirty: false } } }));
    if (requestId === fileRequest.current) setOpenedFile(key);
  };
  const sourceFiles = [...new Map([...draft.candidates.filter((candidate) => candidate.origin === "source").map((candidate) => ({ ...candidate, source_commit: draft.resolved_commit })),
    ...(draft.change_context?.base_candidates || []).map((candidate) => ({ ...candidate, source_commit: draft.change_context?.base_commit }))].map((candidate) => [fileKey(candidate), candidate])).values()];
  const sourceSelection = (candidate: Candidate & { source_commit?: string }): FileSelection => ({ path: candidate.path, sha256: candidate.sha256, origin: candidate.origin,
    ...(candidate.source_commit ? { source_commit: candidate.source_commit } : {}) });
  const file = buffer.files[openedFile];
  const metadata = record(buffer.manifest.metadata);

  const requestPreview = async () => {
    const ticket = ++previewSerial.current;
    const request: api.RecipeGenerationPreviewRequest = { provider_id: providerId, provider_version: providerCatalog.data!.version, instruction: buffer.instruction,
      diagnostic_ids: buffer.diagnosticIds, run_excerpts: buffer.runExcerpts, ...(selectedProvider?.model ? { model: String(selectedProvider.model) } : {}) };
    const value = await api.previewRecipeGeneration(id, buffer.baseVersion, request);
    if (ticket !== previewSerial.current || lifetime.current?.signal.aborted) return;
    setPreview({ value, request, version: buffer.baseVersion }); setContentConsent(false);
  };

  const saveToLibrary = async () => {
    if (draft.proposal) throw new Error("Accept or discard the reviewed proposal before saving this recipe.");
    if (needsProcedureReview) throw new Error("Investigate the repository, review its documented procedures and accept them before saving. The initial starter configuration is not a reviewed recipe.");
    const signal = lifetime.current!.signal;
    signal.throwIfAborted();
    let exact = draft;
    if (!exact.package_digest) {
      const resolved = await api.resolveRecipeDraftReferences(id, exact.version, buffer.references); setQueuedRun(resolved.run_id);
      await waitForRun(resolved.run_id, signal);
      exact = await api.getRecipeDraft(id, { signal }); adopt(exact, "all");
      const unresolved = exact.diagnostics.filter((item) => item.blocking);
      if (unresolved.length) throw new Error(`References need attention: ${unresolved.map((item) => `${item.message}${item.remediation ? ` ${item.remediation}` : ""}`).join("; ")}`);
      exact = await api.updateRecipeDraft(id, exact.version, { manifest: record(exact.manifest), selected_assets: exact.selected_assets, acknowledged_warnings: acceptedTokens });
      adopt(exact, "all"); signal.throwIfAborted();
      const packaged = await api.packageRecipeDraft(id, exact.version); setQueuedRun(packaged.run_id);
      await waitForRun(packaged.run_id, signal);
      exact = await api.getRecipeDraft(id, { signal }); adopt(exact, "all");
    }
    if (!exact.package_digest || exact.operation || exact.diagnostics.some((item) => item.blocking) || exact.diagnostics.some((item) => item.severity === "warning" && (!item.acknowledgement || !acceptedTokens.includes(item.acknowledgement)))) {
      throw new Error("The packaged content needs another review. No library save was started.");
    }
    signal.throwIfAborted();
    const installed = await api.installRecipeDraft(id, exact.version, { package_digest: exact.package_digest, acknowledged_warnings: acceptedTokens });
    setQueuedRun(installed.run_id); await waitForRun(installed.run_id, signal);
    const saved = await api.getRecipeDraft(id, { signal }); adopt(saved, "all");
    if (saved.state !== "installed" || !saved.package_digest) throw new Error("The save outcome is uncertain. Inspect the operation before confirming again.");
    try { sessionStorage.removeItem(`lmw.recipe-draft.${id}`); } catch { /* The saved server document is authoritative. */ }
    onSaved(saved.package_digest);
  };

  return <div className="space-y-5">
    <header className="flex flex-wrap items-start justify-between gap-3"><div><h2 className="font-display text-xl font-semibold">{draft.change_context?.kind === "update" ? "Review upstream launch changes" : draft.change_context?.kind === "repair" ? "Change launch instructions" : "Investigate and review repository"}</h2><p className="mt-1 text-sm text-muted">{adding ? "Investigate → review each documented procedure → save separate recipes. No upstream code runs during import." : "Ask an AI assistant or edit settings yourself, then review saved versus proposed instructions before saving. Running models remain unchanged."}</p></div><Link className="text-sm underline" to={settingsURL}>AI assistance settings</Link></header>
    {query.isError ? <RecipeError error={query.error} retry={() => void query.refetch()} /> : null}
    {mutation.isError ? <RecipeError error={mutation.error} /> : null}
    {storageUnavailable ? <p role="alert" className="border border-warning/40 p-3 text-sm text-warning">Browser recovery storage is unavailable. Keep this tab open; unsaved inputs remain in memory.</p> : null}
    {recovery.value ? <section className="border border-warning/40 p-3"><h3 className="font-semibold">Restore unsaved changes?</h3><p className="mt-1 text-sm text-muted">A buffer for this exact recipe document was retained. Restoring never repeats a save, AI request, download, or run.{adding ? " Repository investigation now chooses source context automatically; any old manual context selection is no longer required." : ""}</p><div className="mt-3 flex gap-2"><Button onClick={() => { setBuffer({ ...freshBuffer(draft), ...recovery.value, schemaVersion: 2, ...(adding ? { context: draft.context_selection, contextDirty: false, instruction: recovery.value?.instruction || importInstruction } : {}) }); setRecovery({}); }}>Restore unsaved changes</Button><Button variant="outline" onClick={() => { if (window.confirm("Discard this recipe's browser recovery buffer?")) setRecovery({}); }}>Discard recovered changes</Button></div></section> : null}
    {conflict ? <section role="alert" className="space-y-3 border border-warning/40 p-3"><p>A newer saved document is available. Your independent local buffers were retained.</p><div className="flex flex-wrap gap-2"><Button variant="outline" onClick={() => { if (window.confirm("Discard unsaved manifest, raw JSON, answers, context and helper-file changes?")) { setBuffer(freshBuffer(draft)); setReviewLatest(false); } }}>Reload saved version</Button><Button variant="outline" onClick={() => setReviewLatest(true)}>Review local and latest values</Button></div>{reviewLatest ? <><div className="grid gap-2 md:grid-cols-2"><div><h4>Latest server document</h4><pre className="max-h-80 overflow-auto text-xs">{JSON.stringify({ manifest: draft.manifest, selected: draft.selected_assets, context: draft.context_selection, questions: draft.questions }, null, 2)}</pre></div><div><h4>Local buffers</h4><pre className="max-h-80 overflow-auto text-xs">{JSON.stringify({ manifest: buffer.manifest, selected: buffer.selected, context: buffer.context, answers: buffer.answers, files: buffer.files }, null, 2)}</pre></div></div><Button disabled={active || mutation.isPending || !editable} onClick={() => mutation.mutate(() => saveWork(draft.version))}>Save local configuration against version {draft.version}</Button><p className="text-xs text-muted">Only the manifest, selected helpers and answers are saved. Unsaved context, raw JSON and helper text stay separate.</p></> : null}</section> : null}

    <section className="control space-y-3 p-4"><h3 className="font-display text-lg font-semibold">{adding ? "Repository investigation" : "Ask an AI assistant"}</h3><p className="text-sm text-muted">{adding ? "The pinned repository and relevant explicitly linked public instructions are investigated automatically; no file checklist is needed. Review the repository content scope and AI destination before approving the investigation. Documented launch procedures become separate reviewable recipes." : "Describe the launch-instruction change you want. Review the exact request and selected source/log content before sending. You can also edit settings below without AI."}</p>
      {providerCatalog.isError ? <RecipeError error={providerCatalog.error} retry={() => void providerCatalog.refetch()} /> : null}
      <label className="block space-y-1 text-sm">AI assistant / model<select className="control w-full bg-panel p-2" disabled={providerCatalog.isPending || providerCatalog.isError || Boolean(recovery.value)} value={selectedProvider ? providerId : ""} onChange={(event) => patch({ providerId: event.target.value })}><option value="">{providerCatalog.isPending ? "Loading assistants…" : "Choose an assistant / model"}</option>{providers.map((provider) => <option key={provider.id} value={provider.id}>{provider.label}</option>)}</select></label>
      <p className="text-xs text-muted">Running models and models from your connected Codex account appear here automatically. No extra registration is needed.</p>
      {providerCatalog.isSuccess && providers.length === 0 ? <p role="status" className="text-sm text-warning">No AI providers are available. Start a model in <Link className="underline" to="/serving">Serving</Link> or configure an AI provider in <Link className="underline" to={settingsURL}>Settings</Link>.</p> : null}
      {providerId && !selectedProvider && providerCatalog.isSuccess ? <p role="status" className="text-sm text-warning">The previously selected provider is no longer available. Choose an available provider.</p> : null}
      <label className="block space-y-1 text-sm">{adding ? "Investigation instruction" : "What should change?"}<textarea className="control min-h-24 w-full bg-panel p-2" disabled={Boolean(recovery.value)} value={buffer.instruction} onChange={(event) => patch({ instruction: event.target.value })} /></label>
      <details><summary className="cursor-pointer text-sm">Choose diagnostics to send</summary><div className="mt-2 space-y-2">{draft.diagnostics.map((diagnostic) => <label key={diagnostic.id} className="flex gap-2 text-sm"><input type="checkbox" checked={buffer.diagnosticIds.includes(diagnostic.id)} onChange={() => patch({ diagnosticIds: buffer.diagnosticIds.includes(diagnostic.id) ? buffer.diagnosticIds.filter((value) => value !== diagnostic.id) : [...buffer.diagnosticIds, diagnostic.id] })} /><span>{diagnostic.code}: {diagnostic.message}</span></label>)}</div></details>
      {draft.change_context?.deployment_id ? <details><summary className="cursor-pointer text-sm">Choose bounded model failure logs</summary><p className="mt-2 text-xs text-muted">Only explicitly selected excerpts from this exact deployment are eligible. The preview redacts known secrets, but you must review it for sensitive content.</p>{deployment.isError ? <RecipeError error={deployment.error} retry={() => void deployment.refetch()} /> : null}{buffer.runExcerpts.map((excerpt, index) => <div className="mt-2 grid gap-2 border border-rule p-2 md:grid-cols-3" key={index}><Input aria-label="Failure run ID" value={excerpt.run_id} onChange={(event) => patch({ runExcerpts: buffer.runExcerpts.map((item, i) => i === index ? { ...item, run_id: event.target.value } : item) })} /><select aria-label="Log stream" className="control bg-panel p-2" value={excerpt.stream} onChange={(event) => patch({ runExcerpts: buffer.runExcerpts.map((item, i) => i === index ? { ...item, stream: event.target.value as "stdout" | "stderr" } : item) })}><option>stderr</option><option>stdout</option></select>{(["rank", "offset", "byte_count"] as const).map((name) => <Input key={name} aria-label={`Log ${name}`} type="number" min={name === "byte_count" ? 1 : 0} max={name === "byte_count" ? 16384 : undefined} value={excerpt[name]} onChange={(event) => patch({ runExcerpts: buffer.runExcerpts.map((item, i) => i === index ? { ...item, [name]: Number(event.target.value) } : item) })} />)}<Button size="sm" variant="outline" onClick={() => patch({ runExcerpts: buffer.runExcerpts.filter((_, i) => i !== index) })}>Remove excerpt</Button></div>)}<Button className="mt-2" size="sm" variant="outline" disabled={buffer.runExcerpts.length >= 4} onClick={() => patch({ runExcerpts: [...buffer.runExcerpts, { deployment_id: draft.change_context!.deployment_id!, run_id: deployment.data?.run_id || "", rank: 0, stream: "stderr", offset: 0, byte_count: 8192 }] })}>Add failure log selection</Button></details> : null}
      <Button variant="outline" disabled={disabled || dirty || !selectedProvider || !providerCatalog.data?.version || providerCatalog.isError} onClick={() => mutation.mutate(requestPreview)}>{adding ? "Review investigation scope and destination" : "Review content before sending"}</Button>
      {dirty ? <p className="text-xs text-warning">Save context, structured edits and helper text, and apply valid raw JSON before requesting a preview.</p> : null}
      {preview ? <section className="space-y-3 border border-warning/40 p-3"><h4 className="font-semibold">{adding ? "Repository investigation consent" : "Exact AI request"}</h4><p className="break-all text-sm">Destination: {preview.value.destination} · Assistant model: {preview.value.model}</p>{adding ? <p className="text-sm">This preview records the pinned repository inventory and content hashes, not every subsequent prompt. Investigation sends the readable pinned repository files in bounded requests to this destination and may retrieve and send relevant explicitly linked public documentation. Approval covers that content and scope across the investigation; review the repository for sensitive content before approving.</p> : null}{preview.value.warnings.map((warning, index) => <p className="text-sm text-warning" key={index}>{warning}</p>)}<pre className="max-h-96 overflow-auto bg-raised p-3 text-xs">{JSON.stringify(preview.value.request, null, 2)}</pre><label className="flex gap-2 text-sm"><input type="checkbox" checked={contentConsent} onChange={(event) => setContentConsent(event.target.checked)} />{adding ? "I approve sending this pinned repository's readable content and relevant explicitly linked public documentation to this AI destination for investigation, and understand multiple requests may use my quota." : "I approve sending exactly this content to this provider and understand it may use my quota."}</label><Button disabled={disabled || dirty || !contentConsent || preview.version !== draft.version} onClick={() => mutation.mutate(async () => { await queue(api.generateRecipeDraft(id, preview.version, { ...preview.request, preview_sha256: preview.value.preview_sha256, content_consent: true })); setPreview(undefined); setContentConsent(false); })}>{adding ? "Investigate repository" : "Request suggested changes"}</Button></section> : null}
    </section>
    {queuedRun || draft.run_id ? <RecipeOperation runId={queuedRun || draft.run_id!} draftId={id} digest={draft.change_context?.base_recipe_digest} repositoryId={draft.change_context?.repository_id} /> : null}
    {!editable ? <section className="border border-ok/40 p-3"><p>Saved to the catalog. This document is immutable; downloading and running remain separate actions.</p><Button className="mt-2" variant="outline" onClick={() => onSaved(draft.package_digest!)}>Open saved recipe</Button></section> : null}
    <details open={adding}><summary className="cursor-pointer text-sm">Advanced source evidence and AI content selection</summary><div className="mt-3 space-y-4">
    <section className="control space-y-3 p-4"><h3 className="font-display text-lg font-semibold">Source evidence</h3><p className="break-all text-sm">{String(record(draft.source).remote || "Saved package")}</p>
      {!adding && draft.change_context?.base_source_status === "unavailable" ? <p className="text-sm text-warning">Original repository files are unavailable; the saved configuration and helper files are still available. {draft.change_context.base_source_error}</p> : null}
      <details><summary className="cursor-pointer text-sm">Pinned source details</summary><pre className="mt-2 overflow-auto text-xs">{JSON.stringify({ source: draft.source, saved_commit: draft.change_context?.base_commit, selected_commit: draft.resolved_commit, tree: draft.resolved_tree }, null, 2)}</pre></details>
      <Button variant="outline" disabled={disabled || dirty || !canInspect} onClick={() => mutation.mutate(() => queue(api.inspectRecipeDraft(id, buffer.baseVersion)))}>{canInspect ? "Inspect pinned source" : "Source inspected"}</Button>
      {adding && comparison.isError ? <RecipeError error={comparison.error} retry={() => void comparison.refetch()} /> : null}
      {comparison.data?.files.length ? <details open={section === "updates"}><summary className="cursor-pointer font-medium">Changed repository files</summary><div className="mt-3 space-y-2">{comparison.data.files.map((changed) => <article key={changed.path} className="flex flex-wrap items-center gap-2 border border-rule p-2 text-xs"><span className="min-w-0 flex-1 break-all font-mono">{changed.path} · {changed.change}</span>{changed.binary ? <span>Binary · {changed.before_sha256 || "absent"} → {changed.after_sha256 || "absent"}</span> : <>{changed.before_sha256 ? <Button size="sm" variant="outline" onClick={() => mutation.mutate(() => openFile({ path: changed.path, sha256: changed.before_sha256!, source_commit: comparison.data?.base_commit, origin: "source" }))}>Before</Button> : null}{changed.after_sha256 ? <Button size="sm" variant="outline" onClick={() => mutation.mutate(() => openFile({ path: changed.path, sha256: changed.after_sha256!, source_commit: comparison.data?.target_commit, origin: "source" }))}>After</Button> : null}</>}</article>)}</div></details> : null}
    </section>

    {!adding ? <section className="control space-y-3 p-4"><h3 className="font-display text-lg font-semibold">Source content for AI review</h3><p className="text-sm text-muted">Choose bounded, hashed source text. Old and new revisions are separate evidence; selecting files does not send them to AI.</p>
      <div className="max-h-80 space-y-2 overflow-auto">{sourceFiles.map((candidate, index) => { const selection = sourceSelection(candidate); return <label key={`${fileKey(selection)}:${index}`} className="flex gap-2 border border-rule p-2 text-xs"><input type="checkbox" disabled={disabled || candidate.binary} checked={buffer.context.some((item) => contextKey(item) === contextKey(selection))} onChange={() => toggleContext(selection)} /><span className="break-all">{candidate.path} · {candidate.source_commit === draft.change_context?.base_commit ? "Saved source" : "Selected source"} · {candidate.size} bytes{candidate.binary ? " · binary, not sent" : ""}</span></label>; })}</div>
      <Button variant="outline" disabled={disabled || !buffer.contextDirty} onClick={() => mutation.mutate(async () => adopt(await api.updateRecipeDraftContext(id, buffer.baseVersion, buffer.context), "context"))}>Save context selection</Button>
    </section> : null}
    </div></details>


    {draft.proposal ? <section className="space-y-4 border border-accent/40 p-4">
      <h3 className="font-display text-lg font-semibold">{adding ? "Review documented launch procedures" : "Review suggested changes"}</h3>
      <p>{draft.proposal.summary || "Suggested configuration"}</p>
      <p className="text-sm text-muted">{adding ? "Accepting creates separately reviewable recipe drafts for these procedures. Each is saved independently; nothing is deployed." : "Use these changes only updates editable work. The saved recipe and running models remain unchanged."}</p>
      {draft.proposal.base_version !== draft.version ? <p className="text-sm text-warning">This suggestion is stale. It remains available for comparison, but cannot be accepted. Request another investigation using the latest saved answers and changes.</p> : null}
      {(draft.proposal.procedures?.length ? draft.proposal.procedures : [{ ...draft.proposal, name: "Suggested recipe", description: "" }]).map((procedure, index) => <article className="space-y-4 border border-rule p-4" key={procedure.id || index}>
        <h4 className="font-display text-lg font-semibold">{procedure.name}</h4><p className="text-sm">{procedure.description}</p><p className="text-sm text-muted">{procedure.summary}</p>
        <ProcedureReview manifest={record(procedure.manifest)} adaptations={procedure.adaptations} />
        <section className="space-y-2"><h5 className="font-semibold">Preserved upstream assets</h5>{(procedure.selected_source_assets || []).map((asset) => <p key={asset.path} className="break-all font-mono text-xs">{asset.path} · {asset.sha256}</p>)}</section>
        {(procedure.files || []).map((suggested) => <details key={suggested.path}><summary className="cursor-pointer break-all text-sm">Package helper: {suggested.path}</summary><pre className="max-h-80 overflow-auto text-xs">{suggested.content}</pre></details>)}
        {(procedure.evidence || []).map((evidence, evidenceIndex) => <div className="flex flex-wrap items-center gap-2 text-xs" key={evidenceIndex}><span className="break-all">{evidence.path} · {evidence.source_commit || draft.resolved_commit}{evidence.start_line ? ` · lines ${evidence.start_line}–${evidence.end_line || evidence.start_line}` : " · whole file"}</span><Button size="sm" variant="outline" onClick={() => { setAuthoring(true); mutation.mutate(() => openFile(evidence)); }}>Read cited source</Button></div>)}
        {(procedure.questions || []).map((question) => <p key={question.id} className="text-sm text-warning">{question.answer ? `Answered: ${question.question} — ${question.answer}` : `Missing information: ${question.question}`}</p>)}
        {(procedure.diagnostics || []).map((diagnostic) => <p className="text-sm text-warning" key={diagnostic.id}>{diagnostic.path ? `${diagnostic.path}: ` : ""}{diagnostic.message} {diagnostic.remediation}</p>)}
        <details><summary className="cursor-pointer text-sm">Exact recipe manifest</summary><pre className="max-h-96 overflow-auto text-xs">{JSON.stringify(procedure.manifest, null, 2)}</pre></details>
      </article>)}
      <div className="flex flex-wrap gap-2"><Button disabled={disabled || dirty || draft.proposal.base_version !== draft.version} onClick={() => mutation.mutate(async () => { adopt(await api.acceptRecipeDraftProposal(id, draft.version, draft.proposal!.id), "all"); void client.invalidateQueries({ queryKey: qk.recipeDrafts }); })}>{adding ? "Accept procedures for saving" : "Use these changes"}</Button><Button variant="outline" disabled={disabled} onClick={() => mutation.mutate(async () => adopt(await api.discardRecipeDraftProposal(id, draft.version, draft.proposal!.id), "metadata"))}>Discard suggestion</Button></div>
    </section> : null}
    {adding && draft.review && !draft.proposal && records(buffer.manifest.workloads).length > 0 ? <section className="control space-y-4 p-4"><h3 className="font-display text-lg font-semibold">{String(metadata.name || draft.review.name || "Reviewed procedure")}</h3><p className="text-sm text-muted">{draft.review.summary || String(metadata.description || "")}</p><ProcedureReview manifest={buffer.manifest} adaptations={draft.review.adaptations} /><details><summary className="cursor-pointer text-sm">Preserved assets and source evidence</summary><div className="mt-2 space-y-2">{(draft.review.selected_source_assets || draft.selected_assets).map((asset) => <p key={asset.path} className="break-all font-mono text-xs">{asset.path} · {asset.sha256}</p>)}{(draft.review.evidence || []).map((evidence, index) => <div key={index} className="flex flex-wrap items-center gap-2 text-xs"><span className="break-all">{evidence.path}{evidence.start_line ? ` · lines ${evidence.start_line}–${evidence.end_line || evidence.start_line}` : ""}</span><Button size="sm" variant="outline" onClick={() => { setAuthoring(true); mutation.mutate(() => openFile(evidence)); }}>Read cited source</Button></div>)}</div></details></section> : null}
    {draft.related_draft_ids?.length ? <section className="control space-y-3 p-4"><h3 className="font-semibold">Other launch procedures from this investigation</h3><p className="text-sm text-muted">Each procedure is a separate recipe. Review and save each independently.</p>{draft.related_draft_ids.filter((siblingId) => siblingId !== id).map((siblingId) => <Link key={siblingId} className="block text-sm underline" to={draftPath({ ...draft, id: siblingId, change_context: undefined })}>Review procedure draft {siblingId}</Link>)}</section> : null}
    <details open={!adding || authoring} onToggle={(event) => setAuthoring(event.currentTarget.open)}><summary className="cursor-pointer text-sm">{adding ? "Advanced recipe authoring — configuration and helper files" : "Edit settings manually — no AI required"}</summary><div className="mt-3 space-y-4">
    {section === "configuration" ? <>
      <section className="control space-y-4 p-4"><div className="flex flex-wrap items-center justify-between gap-2"><h3 className="font-display text-lg font-semibold">{adding ? "Configuration" : "Proposed launch settings"}</h3><Button variant="outline" size="sm" onClick={() => setAdvanced(!advanced)}>{advanced ? "Structured fields" : "Advanced JSON"}</Button></div>
        {advanced ? <div className="space-y-2"><Label htmlFor="recipe-json">Raw configuration JSON</Label><textarea id="recipe-json" className="control min-h-96 w-full bg-raised p-3 font-mono text-xs" disabled={!editable || active || mutation.isPending} value={buffer.jsonText} onChange={(event) => { let jsonError = ""; try { const parsed: unknown = JSON.parse(event.target.value); if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) jsonError = "Configuration must be an object"; } catch (error) { jsonError = error instanceof Error ? error.message : "Invalid JSON"; } patch({ jsonText: event.target.value, jsonError, rawDirty: true }); }} />{buffer.jsonError ? <p role="alert" className="text-sm text-fault">{buffer.jsonError}</p> : null}<Button disabled={disabled || !buffer.rawDirty || Boolean(buffer.jsonError)} onClick={() => { const manifest = record(JSON.parse(buffer.jsonText)); patch({ manifest, manifestDirty: true, rawDirty: false }); }}>Apply valid JSON</Button><p className="text-xs text-muted">Invalid or unapplied JSON stays in browser recovery. Save editable work can still save the valid structured configuration and answers.</p></div> : <fieldset disabled={disabled} className="grid gap-3 md:grid-cols-2">{["name", "version", "description", "license"].map((name) => <label key={name} className="space-y-1 text-sm"><span className="capitalize">{name}</span><Input value={String(metadata[name] || "")} onChange={(event) => setField("metadata", name, event.target.value)} /></label>)}<label className="space-y-1 text-sm">Hardware node count<Input type="number" min={1} value={String(record(buffer.manifest.compatibility).nodeCount || 1)} onChange={(event) => setField("compatibility", "nodeCount", Number(event.target.value))} /></label></fieldset>}
      </section>
      {!advanced ? <fieldset disabled={disabled} className="space-y-4"><section className="control space-y-3 p-4"><h3 className="font-display text-lg font-semibold">Models and data</h3>{records(buffer.manifest.artifacts).map((artifact, index) => { const source = record(artifact.source); return <div key={index} className="grid gap-2 border border-rule p-3 md:grid-cols-2"><Input aria-label="Artifact name" value={String(artifact.name || "")} onChange={(event) => setArrayField("artifacts", index, "name", event.target.value)} /><Input aria-label="Artifact mount" value={String(artifact.mount || "")} onChange={(event) => setArrayField("artifacts", index, "mount", event.target.value)} /><select aria-label="Artifact source type" className="control bg-panel p-2" value={String(source.type || "huggingface")} onChange={(event) => setArrayField("artifacts", index, "source", { ...source, type: event.target.value })}>{["huggingface", "file", "local", "oci"].map((type) => <option key={type}>{type}</option>)}</select>{["identity", "revision", "digest"].map((name) => <Input key={name} aria-label={`Artifact source ${name}`} placeholder={name} value={String(source[name] || "")} onChange={(event) => setArrayField("artifacts", index, "source", { ...source, [name]: event.target.value })} />)}<Button size="sm" variant="outline" onClick={() => setArray("artifacts", records(buffer.manifest.artifacts).filter((_, item) => item !== index))}>Remove artifact</Button></div>; })}<Button variant="outline" onClick={() => setArray("artifacts", [...records(buffer.manifest.artifacts), { name: "", mount: "", source: { type: "huggingface", identity: "", revision: "" } }])}>Add model or data artifact</Button></section>
        <section className="control space-y-4 p-4"><h3 className="font-display text-lg font-semibold">Runtime and hardware</h3>{records(buffer.manifest.workloads).map((workload, index) => {
          const image = record(workload.image);
          const environment = record(workload.env);
          return <section key={index} className="space-y-4 border border-rule p-3">
            <h4 className="font-semibold">{String(workload.name || `Runtime ${index + 1}`)}</h4>
            <div className="grid gap-2 md:grid-cols-2">{["reference", "digest"].map((name) => <label key={name} className="space-y-1 text-sm">Image {name}<Input value={String(image[name] || "")} onChange={(event) => setArrayField("workloads", index, "image", { ...image, [name]: event.target.value })} /></label>)}</div>
            <section className="space-y-3"><h5 className="font-medium">Environment values</h5><p className="text-xs text-muted">Edit the recipe's named settings directly. Values and template expressions are kept as entered; related command flags are not changed automatically.</p>
              <div className="grid gap-3 md:grid-cols-2">{Object.entries(environment).map(([name, value]) => <label key={name} className="space-y-1 text-sm"><span className="break-all font-mono text-xs">{name}</span><textarea rows={1} className="control w-full bg-panel p-2 font-mono text-xs" value={String(value ?? "")} onChange={(event) => setArrayField("workloads", index, "env", { ...environment, [name]: event.target.value })} /></label>)}</div>
              {!Object.keys(environment).length ? <p className="text-xs text-muted">No environment values are defined. Use Advanced JSON to add or remove named environment settings.</p> : null}
            </section>
            {(["command", "args"] as const).map((field) => {
              const values: unknown[] = Array.isArray(workload[field]) ? workload[field] : [];
              return <section key={field} className="space-y-2"><h5 className="font-medium">{field === "command" ? "Launch command" : "Runtime arguments"}</h5><p className="text-xs text-muted">Each entry is one exact token, in order. Spaces within an entry are preserved.</p>
                {values.map((value, position) => <div key={position} className="flex items-start gap-2"><label className="min-w-0 flex-1 space-y-1 text-sm">{field === "command" ? "Command" : "Argument"} {position + 1}<textarea rows={1} className="control w-full bg-panel p-2 font-mono text-xs" value={String(value ?? "")} onChange={(event) => setArrayField("workloads", index, field, values.map((item, itemIndex) => itemIndex === position ? event.target.value : item))} /></label><Button className="mt-6" size="sm" variant="outline" aria-label={`Remove ${field === "command" ? "command token" : "argument"} ${position + 1}`} onClick={() => setArrayField("workloads", index, field, values.filter((_, itemIndex) => itemIndex !== position))}>Remove</Button></div>)}
                <Button size="sm" variant="outline" onClick={() => setArrayField("workloads", index, field, [...values, ""])}>{field === "command" ? "Add command token" : "Add argument"}</Button>
              </section>;
            })}
            <Button size="sm" variant="outline" onClick={() => setArray("workloads", records(buffer.manifest.workloads).filter((_, item) => item !== index))}>Remove runtime</Button>
          </section>;
        })}<Button variant="outline" onClick={() => setArray("workloads", [...records(buffer.manifest.workloads), { image: { reference: "", digest: "" }, command: [] }])}>Add runtime</Button><p className="text-xs text-muted">Advanced JSON exposes additional environment keys, accelerator, variant, permission, extension and runtime fields.</p></section>
      </fieldset> : null}
    </> : null}

    <details open={Boolean(openedFile)}><summary className="cursor-pointer text-sm">Advanced helper files and source reader</summary><div className="mt-3 space-y-4">
    <section className="control space-y-3 p-4"><h3 className="font-display text-lg font-semibold">Helper files</h3><p className="text-sm text-muted">{adding ? "Investigation binds preserved upstream assets automatically. Inspected files remain read-only; advanced helper edits are optional." : "Selection binds path, hash and origin. Inspected files are read-only; generated helper text has its own unsaved buffer."}</p>
      <div className="grid gap-2 md:grid-cols-2">{draft.candidates.map((candidate) => { const selected = buffer.selected.some((item) => item.path === candidate.path && item.sha256 === candidate.sha256 && (!item.origin || item.origin === candidate.origin)); return <div key={fileKey(candidate)} className="space-y-2 border border-rule p-2 text-xs"><label className="flex gap-2">{!adding ? <input type="checkbox" checked={selected} disabled={disabled} onChange={() => patch({ manifestDirty: true, selected: selected ? buffer.selected.filter((item) => !(item.path === candidate.path && item.sha256 === candidate.sha256 && (!item.origin || item.origin === candidate.origin))) : [...buffer.selected, { path: candidate.path, sha256: candidate.sha256, origin: candidate.origin }] })} /> : null}<span className="break-all">{candidate.path} · {candidate.origin} · {candidate.size} bytes{selected ? " · included in recipe" : ""}</span></label>{!candidate.binary ? <Button size="sm" variant="outline" onClick={() => mutation.mutate(() => openFile(candidate))}>Open file</Button> : null}</div>; })}</div>
      <div className="flex gap-2"><Input aria-label="New helper file path" value={newFile} onChange={(event) => setNewFile(event.target.value)} placeholder="helpers/config.py" /><Button variant="outline" disabled={disabled || !newFile.trim()} onClick={() => { const selection = { path: newFile.trim(), sha256: "", origin: "generated" as const }; const key = fileKey(selection); patch({ files: { ...buffer.files, [key]: { selection, origin: "generated", content: "", dirty: true } } }); setOpenedFile(key); setNewFile(""); }}>Add helper file</Button></div>
      {Object.entries(buffer.files).some(([, item]) => item.dirty) ? <div className="flex flex-wrap gap-2">{Object.entries(buffer.files).filter(([, item]) => item.dirty).map(([key, item]) => <Button key={key} size="sm" variant="outline" onClick={() => setOpenedFile(key)}>Unsaved: {item.selection.path}</Button>)}</div> : null}
    </section>
    {file ? <section className="control space-y-3 p-4"><h3 className="break-all font-mono text-sm">{file.selection.path} · {file.origin}{file.selection.source_commit ? ` · ${file.selection.source_commit}` : ""}</h3><textarea aria-label="Helper or evidence file content" className="control min-h-72 w-full bg-raised p-3 font-mono text-xs" readOnly={file.origin !== "generated" || !editable || active || mutation.isPending} value={file.content} onChange={(event) => patch({ files: { ...buffer.files, [openedFile]: { ...file, content: event.target.value, dirty: true } } })} />{file.origin === "generated" ? <Button disabled={disabled || !file.dirty} onClick={() => mutation.mutate(async () => adopt(await api.updateRecipeDraftFile(id, buffer.baseVersion, { path: file.selection.path, content: file.content }), "file", file.selection.path))}>Save helper text</Button> : null}</section> : null}
    </div></details>
    </div></details>

    <section className={draft.questions.length || draft.diagnostics.length ? "control space-y-3 p-4" : "space-y-3"}>{draft.questions.length || draft.diagnostics.length ? <><h3 className="font-display text-lg font-semibold">Missing information and unresolved findings</h3><p className="text-sm text-muted">Answer specific questions, save the answers, then investigate again to incorporate them. Reference verification resolves immutable image and artifact identities without running upstream code.</p></> : null}{draft.questions.map((question) => <label className="block space-y-2 border border-rule p-3 text-sm" key={question.id}><span>{question.question}</span><Input disabled={disabled} value={buffer.answers[question.id] ?? question.answer ?? ""} onChange={(event) => patch({ answersDirty: true, answers: { ...buffer.answers, [question.id]: event.target.value } })} /></label>)}
      {draft.diagnostics.map((diagnostic) => <article className={`space-y-1 border p-3 ${diagnostic.blocking ? "border-fault/40" : "border-warning/40"}`} key={diagnostic.id}><p className="break-all font-mono text-xs">{diagnostic.code}{diagnostic.path ? ` · ${diagnostic.path}` : ""}</p><p className="text-sm">{diagnostic.message}</p><p className="text-xs text-muted">{diagnostic.remediation}</p>{diagnostic.dismissible ? <Button size="sm" variant="outline" disabled={disabled || dirty} onClick={() => mutation.mutate(async () => adopt(await api.dismissRecipeDraftDiagnostics(id, buffer.baseVersion, [diagnostic.id]), "all"))}>Dismiss optional finding</Button> : null}</article>)}
      {adding && (buffer.manifestDirty || buffer.answersDirty) ? <Button disabled={disabled} onClick={() => mutation.mutate(() => saveWork())}>Save answers and changes</Button> : null}
      <details><summary className="cursor-pointer text-sm">Reference credentials and checksum evidence</summary><div className="mt-3 space-y-3">{secrets.isError ? <RecipeError error={secrets.error} retry={() => void secrets.refetch()} /> : null}{(buffer.references?.credentials || []).map((credential, index) => <div className="grid gap-2 md:grid-cols-3" key={index}>{["path", "host"].map((name) => <Input key={name} aria-label={`Reference credential ${name}`} placeholder={name} value={credential[name as "path" | "host"]} onChange={(event) => patch({ references: { ...buffer.references, credentials: buffer.references!.credentials!.map((item, i) => i === index ? { ...item, [name]: event.target.value } : item) } })} />)}<select aria-label="Reference secret" className="control bg-panel p-2" value={credential.secret_id} onChange={(event) => patch({ references: { ...buffer.references, credentials: buffer.references!.credentials!.map((item, i) => i === index ? { ...item, secret_id: event.target.value } : item) } })}><option value="">Select saved credential</option>{(secrets.data || []).filter((secret) => ["huggingface", "registry"].includes(secret.purpose)).map((secret) => <option key={secret.id} value={secret.id}>{secret.name}</option>)}</select><Button size="sm" variant="outline" onClick={() => patch({ references: { ...buffer.references, credentials: buffer.references!.credentials!.filter((_, i) => i !== index) } })}>Remove credential selection</Button></div>)}<Button size="sm" variant="outline" onClick={() => patch({ references: { ...buffer.references, credentials: [...(buffer.references?.credentials || []), { path: "", host: "", secret_id: "" }] } })}>Add reference credential</Button>
        {(buffer.references?.file_checksums || []).map((checksum, index) => <div className="grid gap-2 md:grid-cols-2" key={index}>{(["path", "url", "sha256", "evidence_note"] as const).map((name) => <Input key={name} aria-label={`Checksum ${name}`} placeholder={name} value={checksum[name]} onChange={(event) => patch({ references: { ...buffer.references, file_checksums: buffer.references!.file_checksums!.map((item, i) => i === index ? { ...item, [name]: event.target.value } : item) } })} />)}<Button size="sm" variant="outline" onClick={() => patch({ references: { ...buffer.references, file_checksums: buffer.references!.file_checksums!.filter((_, i) => i !== index) } })}>Remove checksum evidence</Button></div>)}<Button size="sm" variant="outline" onClick={() => patch({ references: { ...buffer.references, file_checksums: [...(buffer.references?.file_checksums || []), { path: "", url: "", sha256: "", evidence_note: "" }] } })}>Add file checksum evidence</Button></div></details>
      <Button variant="outline" disabled={disabled || dirty} onClick={() => mutation.mutate(() => queue(api.resolveRecipeDraftReferences(id, buffer.baseVersion, buffer.references)))}>Verify references</Button>
    </section>
    {!adding ? <section className="space-y-4 border border-accent/40 p-4">
      <h3 className="font-display text-lg font-semibold">Before / after: saved versus proposed launch instructions</h3>
      <p className="text-sm text-muted">{draft.proposal ? "Compare the saved recipe with the AI suggestion. Accept or discard that suggestion above before saving." : "Compare the saved recipe with your editable work. Saving creates the updated recipe; it does not update running models."}</p>
      {recovery.value ? <p role="status" className="text-sm text-warning">Restore or discard the retained inputs before reviewing proposed changes.</p> : dirty ? <p role="status" className="text-sm text-warning">The comparison does not yet include your unsaved inputs. Apply valid JSON, save structured changes and helper text, and save any context selection to refresh this review.</p> : null}
      {comparison.isError ? <RecipeError error={comparison.error} retry={() => void comparison.refetch()} /> : comparison.isPending || comparison.isFetching ? <p role="status" className="text-sm text-muted">Loading the saved-versus-proposed comparison…</p> : !dirty && !recovery.value && comparison.data ? <ConfigurationComparison changes={comparison.data.configuration_changes} /> : null}
      {(buffer.manifestDirty || buffer.answersDirty) ? <Button disabled={disabled} onClick={() => mutation.mutate(() => saveWork())}>Save editable work for review</Button> : null}
      <p className="text-xs text-muted">Source-file changes and helper contents are available above. This review does not save to the library.</p>
    </section> : null}


    {editable ? <section className="control space-y-3 p-4"><h3 className="font-display text-lg font-semibold">{adding ? "Save reviewed recipe" : "Save updated recipe"}</h3>
      <p className="text-sm text-muted">Verifies references, packages the reviewed procedure and saves it to the catalog. No model downloads, containers or running versions are changed.</p>
      <details><summary className="cursor-pointer text-sm">Permissions and runtime consequences</summary><pre className="max-h-64 overflow-auto text-xs">{JSON.stringify({ permissions: buffer.manifest.permissions, prepare: buffer.manifest.prepare, verify: buffer.manifest.verify, workloads: buffer.manifest.workloads }, null, 2)}</pre></details>
      {warnings.map((warning) => <label className="flex gap-2 border border-warning/40 p-2 text-sm" key={warning.id}><input type="checkbox" disabled={!warning.acknowledgement} checked={Boolean(warning.acknowledgement && warningsAccepted.includes(warning.acknowledgement))} onChange={() => setWarningsAccepted((current) => current.includes(warning.acknowledgement!) ? current.filter((token) => token !== warning.acknowledgement) : [...current, warning.acknowledgement!])} /><span>I acknowledge {warning.code}: {warning.message}</span></label>)}
      {blockers.length ? <div className="space-y-1 text-sm text-fault"><p>Resolve these findings before saving. Verify references for unresolved images and artifacts, or save answers and investigate again for missing procedure information.</p>{blockers.map((item) => <p key={item.id}>{item.path ? `${item.path}: ` : ""}{item.message} {item.remediation}</p>)}</div> : null}
      {draft.proposal ? <p className="text-sm text-warning">Review and accept or discard the proposed procedures above before saving.</p> : null}
      {dirty ? <p className="text-sm text-warning">Save answers and structured changes. Apply valid JSON and save modified helper files in the settings editor before continuing.</p> : null}
      {needsProcedureReview || !records(buffer.manifest.workloads).length ? <p className="text-sm text-warning">Investigate the repository and accept a documented executable procedure before saving. The initial starter configuration is investigation input only.</p> : null}
      <Button disabled={disabled || dirty || (!adding && Boolean(blockers.length)) || draft.questions.some((question) => !question.answer?.trim()) || !allWarningsAccepted || Boolean(draft.proposal) || needsProcedureReview || !records(buffer.manifest.workloads).length} onClick={() => mutation.mutate(saveToLibrary)}>{adding ? "Save recipe to library" : "Save updated recipe"}</Button>
      <p className="text-xs text-muted">A refresh, login interruption, or failed request stops the verification-to-save continuation. Review the saved operation and any new findings before confirming again.</p>
    </section> : null}
  </div>;
}
