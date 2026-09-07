import { useEffect, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useLocation } from "react-router";
import { Button } from "~/components/ui/button";
import { Input } from "~/components/ui/input";
import { Label } from "~/components/ui/label";
import * as api from "~/lib/api";
import { qk, useDeployment, useModuleSettings, useRecipeDraft, useSecrets } from "~/lib/queries";
import { ConfigurationComparison, RecipeError, RecipeOperation, record, records, terminalRunStates } from "./workflow";

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
function freshBuffer(draft: api.RecipeDraft): Buffer {
  return { schemaVersion: 2, baseVersion: draft.version, manifest: record(draft.manifest), selected: draft.selected_assets,
    context: draft.context_selection, answers: Object.fromEntries(draft.questions.map((question) => [question.id, question.answer || ""])),
    jsonText: JSON.stringify(draft.manifest, null, 2), jsonError: "", manifestDirty: false, contextDirty: false,
    answersDirty: false, rawDirty: false, files: {}, instruction: "", providerId: "", diagnosticIds: [], runExcerpts: [], references: {} };
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

export function ChangeEditor({ initialDraft, section, onSaved, existingRecipeURL }: { initialDraft: api.RecipeDraft; section: "configuration" | "updates"; onSaved: (digest: string) => void; existingRecipeURL?: string }) {
  const id = initialDraft.id;
  const query = useRecipeDraft(id);
  const draft = query.data || initialDraft;
  const client = useQueryClient();
  const location = useLocation();
  const settings = useModuleSettings("library");
  const secrets = useSecrets();
  const deployment = useDeployment(draft.change_context?.deployment_id);
  const comparison = useQuery({ queryKey: [...qk.recipeComparison(id), draft.version], queryFn: ({ signal }) => api.compareRecipeDraft(id, { signal }) });
  const [buffer, setBuffer] = useState(() => freshBuffer(initialDraft));
  const [recovery, setRecovery] = useState(() => readRecovery(id));
  const [storageUnavailable, setStorageUnavailable] = useState(Boolean(recovery.unavailable));
  const [advanced, setAdvanced] = useState(false);
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
  const providers = records(record(settings.data?.settings.assistant).providers);
  const selectedProvider = providers.find((provider) => provider.id === buffer.providerId);
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
      const hasInput = localChanges(buffer) || buffer.instruction || buffer.providerId || buffer.runExcerpts.length || buffer.diagnosticIds.length || buffer.references?.credentials?.length || buffer.references?.file_checksums?.length;
      if (hasInput) sessionStorage.setItem(`lmw.recipe-draft.${id}`, JSON.stringify(buffer));
      else sessionStorage.removeItem(`lmw.recipe-draft.${id}`);
      setStorageUnavailable(false);
    }
    catch { setStorageUnavailable(true); }
  }, [id, buffer, recovery.value]);
  useEffect(() => { previewSerial.current++; setPreview(undefined); setContentConsent(false); }, [draft.version, buffer.providerId, buffer.instruction, buffer.diagnosticIds, buffer.runExcerpts, settings.data?.version]);

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
    const request: api.RecipeGenerationPreviewRequest = { provider_id: buffer.providerId, provider_version: settings.data!.version, instruction: buffer.instruction,
      diagnostic_ids: buffer.diagnosticIds, run_excerpts: buffer.runExcerpts, ...(selectedProvider?.model ? { model: String(selectedProvider.model) } : {}) };
    const value = await api.previewRecipeGeneration(id, buffer.baseVersion, request);
    if (ticket !== previewSerial.current || lifetime.current?.signal.aborted) return;
    setPreview({ value, request, version: buffer.baseVersion }); setContentConsent(false);
  };

  const saveToLibrary = async () => {
    if (existingRecipeURL && !draft.change_context?.base_recipe_digest) throw new Error("This repository is already in the catalog. Open its recipe page to review changes; Add cannot replace its saved version.");
    const signal = lifetime.current!.signal;
    signal.throwIfAborted();
    let exact = draft;
    if (!exact.package_digest) {
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
    <header className="flex flex-wrap items-start justify-between gap-3"><div><h2 className="font-display text-xl font-semibold">{draft.change_context?.kind === "update" ? "Review upstream changes" : draft.change_context?.kind === "repair" ? "Help fix this configuration" : "Review configuration"}</h2><p className="mt-1 text-sm text-muted">Editable work is separate from the saved recipe and running models.</p></div><Link className="text-sm underline" to={settingsURL}>AI assistance settings</Link></header>
    {query.isError ? <RecipeError error={query.error} retry={() => void query.refetch()} /> : null}
    {mutation.isError ? <RecipeError error={mutation.error} /> : null}
    {storageUnavailable ? <p role="alert" className="border border-warning/40 p-3 text-sm text-warning">Browser recovery storage is unavailable. Keep this tab open; unsaved inputs remain in memory.</p> : null}
    {recovery.value ? <section className="border border-warning/40 p-3"><h3 className="font-semibold">Restore unsaved changes?</h3><p className="mt-1 text-sm text-muted">A buffer for this exact recipe document was retained. Restoring never repeats a save, AI request, download, or run.</p><div className="mt-3 flex gap-2"><Button onClick={() => { setBuffer({ ...freshBuffer(draft), ...recovery.value, schemaVersion: 2 }); setRecovery({}); }}>Restore unsaved changes</Button><Button variant="outline" onClick={() => { if (window.confirm("Discard this recipe's browser recovery buffer?")) setRecovery({}); }}>Discard recovered changes</Button></div></section> : null}
    {conflict ? <section role="alert" className="space-y-3 border border-warning/40 p-3"><p>A newer saved document is available. Your independent local buffers were retained.</p><div className="flex flex-wrap gap-2"><Button variant="outline" onClick={() => { if (window.confirm("Discard unsaved manifest, raw JSON, answers, context and helper-file changes?")) { setBuffer(freshBuffer(draft)); setReviewLatest(false); } }}>Reload saved version</Button><Button variant="outline" onClick={() => setReviewLatest(true)}>Review local and latest values</Button></div>{reviewLatest ? <><div className="grid gap-2 md:grid-cols-2"><div><h4>Latest server document</h4><pre className="max-h-80 overflow-auto text-xs">{JSON.stringify({ manifest: draft.manifest, selected: draft.selected_assets, context: draft.context_selection, questions: draft.questions }, null, 2)}</pre></div><div><h4>Local buffers</h4><pre className="max-h-80 overflow-auto text-xs">{JSON.stringify({ manifest: buffer.manifest, selected: buffer.selected, context: buffer.context, answers: buffer.answers, files: buffer.files }, null, 2)}</pre></div></div><Button disabled={active || mutation.isPending || !editable} onClick={() => mutation.mutate(() => saveWork(draft.version))}>Save local configuration against version {draft.version}</Button><p className="text-xs text-muted">Only the manifest, selected helpers and answers are saved. Unsaved context, raw JSON and helper text stay separate.</p></> : null}</section> : null}
    {queuedRun || draft.run_id ? <RecipeOperation runId={queuedRun || draft.run_id!} draftId={id} digest={draft.change_context?.base_recipe_digest} repositoryId={draft.change_context?.repository_id} /> : null}
    {!editable ? <section className="border border-ok/40 p-3"><p>Saved to the catalog. This document is immutable; downloading and running remain separate actions.</p><Button className="mt-2" variant="outline" onClick={() => onSaved(draft.package_digest!)}>Open saved recipe</Button></section> : null}

    <section className="control space-y-3 p-4"><h3 className="font-display text-lg font-semibold">Source and comparison</h3><p className="break-all text-sm">{String(record(draft.source).remote || "Saved package")}</p>
      {draft.change_context?.base_source_status === "unavailable" ? <p className="text-sm text-warning">Original repository files are unavailable; the saved configuration and helper files are still available. {draft.change_context.base_source_error}</p> : null}
      <details><summary className="cursor-pointer text-sm">Pinned source details</summary><pre className="mt-2 overflow-auto text-xs">{JSON.stringify({ source: draft.source, saved_commit: draft.change_context?.base_commit, selected_commit: draft.resolved_commit, tree: draft.resolved_tree }, null, 2)}</pre></details>
      <Button variant="outline" disabled={disabled || dirty} onClick={() => mutation.mutate(() => queue(api.inspectRecipeDraft(id, buffer.baseVersion)))}>Inspect pinned source</Button>
      {comparison.isError ? <RecipeError error={comparison.error} retry={() => void comparison.refetch()} /> : null}
      {comparison.data?.files.length ? <details open={section === "updates"}><summary className="cursor-pointer font-medium">Changed repository files</summary><div className="mt-3 space-y-2">{comparison.data.files.map((changed) => <article key={changed.path} className="flex flex-wrap items-center gap-2 border border-rule p-2 text-xs"><span className="min-w-0 flex-1 break-all font-mono">{changed.path} · {changed.change}</span>{changed.binary ? <span>Binary · {changed.before_sha256 || "absent"} → {changed.after_sha256 || "absent"}</span> : <>{changed.before_sha256 ? <Button size="sm" variant="outline" onClick={() => mutation.mutate(() => openFile({ path: changed.path, sha256: changed.before_sha256!, source_commit: comparison.data?.base_commit, origin: "source" }))}>Before</Button> : null}{changed.after_sha256 ? <Button size="sm" variant="outline" onClick={() => mutation.mutate(() => openFile({ path: changed.path, sha256: changed.after_sha256!, source_commit: comparison.data?.target_commit, origin: "source" }))}>After</Button> : null}</>}</article>)}</div></details> : null}
      {comparison.data ? <details open={section === "updates" || Boolean(draft.proposal)}><summary className="cursor-pointer font-medium">Saved versus suggested configuration</summary><div className="mt-3"><ConfigurationComparison changes={comparison.data.configuration_changes} /></div></details> : null}
    </section>

    <section className="control space-y-3 p-4"><h3 className="font-display text-lg font-semibold">Approved source context</h3><p className="text-sm text-muted">Choose bounded, hashed source text. Old and new revisions are separate evidence; selecting files does not send them to AI.</p>
      <div className="max-h-80 space-y-2 overflow-auto">{sourceFiles.map((candidate, index) => { const selection = sourceSelection(candidate); return <label key={`${fileKey(selection)}:${index}`} className="flex gap-2 border border-rule p-2 text-xs"><input type="checkbox" disabled={disabled || candidate.binary} checked={buffer.context.some((item) => contextKey(item) === contextKey(selection))} onChange={() => toggleContext(selection)} /><span className="break-all">{candidate.path} · {candidate.source_commit === draft.change_context?.base_commit ? "Saved source" : "Selected source"} · {candidate.size} bytes{candidate.binary ? " · binary, not sent" : ""}</span></label>; })}</div>
      <Button variant="outline" disabled={disabled || !buffer.contextDirty} onClick={() => mutation.mutate(async () => adopt(await api.updateRecipeDraftContext(id, buffer.baseVersion, buffer.context), "context"))}>Save context selection</Button>
    </section>

    {section === "configuration" ? <>
      <section className="control space-y-4 p-4"><div className="flex flex-wrap items-center justify-between gap-2"><h3 className="font-display text-lg font-semibold">Configuration</h3><Button variant="outline" size="sm" onClick={() => setAdvanced(!advanced)}>{advanced ? "Structured fields" : "Advanced JSON"}</Button></div>
        {advanced ? <div className="space-y-2"><Label htmlFor="recipe-json">Raw configuration JSON</Label><textarea id="recipe-json" className="control min-h-96 w-full bg-raised p-3 font-mono text-xs" disabled={!editable || active || mutation.isPending} value={buffer.jsonText} onChange={(event) => { let jsonError = ""; try { const parsed: unknown = JSON.parse(event.target.value); if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) jsonError = "Configuration must be an object"; } catch (error) { jsonError = error instanceof Error ? error.message : "Invalid JSON"; } patch({ jsonText: event.target.value, jsonError, rawDirty: true }); }} />{buffer.jsonError ? <p role="alert" className="text-sm text-fault">{buffer.jsonError}</p> : null}<Button disabled={disabled || !buffer.rawDirty || Boolean(buffer.jsonError)} onClick={() => { const manifest = record(JSON.parse(buffer.jsonText)); patch({ manifest, manifestDirty: true, rawDirty: false }); }}>Apply valid JSON</Button><p className="text-xs text-muted">Invalid or unapplied JSON stays in browser recovery. Save editable work can still save the valid structured configuration and answers.</p></div> : <fieldset disabled={disabled} className="grid gap-3 md:grid-cols-2">{["name", "version", "description", "license"].map((name) => <label key={name} className="space-y-1 text-sm"><span className="capitalize">{name}</span><Input value={String(metadata[name] || "")} onChange={(event) => setField("metadata", name, event.target.value)} /></label>)}<label className="space-y-1 text-sm">Hardware node count<Input type="number" min={1} value={String(record(buffer.manifest.compatibility).nodeCount || 1)} onChange={(event) => setField("compatibility", "nodeCount", Number(event.target.value))} /></label></fieldset>}
      </section>
      {!advanced ? <fieldset disabled={disabled} className="space-y-4"><section className="control space-y-3 p-4"><h3 className="font-display text-lg font-semibold">Models and data</h3>{records(buffer.manifest.artifacts).map((artifact, index) => { const source = record(artifact.source); return <div key={index} className="grid gap-2 border border-rule p-3 md:grid-cols-2"><Input aria-label="Artifact name" value={String(artifact.name || "")} onChange={(event) => setArrayField("artifacts", index, "name", event.target.value)} /><Input aria-label="Artifact mount" value={String(artifact.mount || "")} onChange={(event) => setArrayField("artifacts", index, "mount", event.target.value)} /><select aria-label="Artifact source type" className="control bg-panel p-2" value={String(source.type || "huggingface")} onChange={(event) => setArrayField("artifacts", index, "source", { ...source, type: event.target.value })}>{["huggingface", "file", "local", "oci"].map((type) => <option key={type}>{type}</option>)}</select>{["identity", "revision", "digest"].map((name) => <Input key={name} aria-label={`Artifact source ${name}`} placeholder={name} value={String(source[name] || "")} onChange={(event) => setArrayField("artifacts", index, "source", { ...source, [name]: event.target.value })} />)}<Button size="sm" variant="outline" onClick={() => setArray("artifacts", records(buffer.manifest.artifacts).filter((_, item) => item !== index))}>Remove artifact</Button></div>; })}<Button variant="outline" onClick={() => setArray("artifacts", [...records(buffer.manifest.artifacts), { name: "", mount: "", source: { type: "huggingface", identity: "", revision: "" } }])}>Add model or data artifact</Button></section>
        <section className="control space-y-3 p-4"><h3 className="font-display text-lg font-semibold">Runtime and hardware</h3>{records(buffer.manifest.workloads).map((workload, index) => { const image = record(workload.image); return <div key={index} className="grid gap-2 border border-rule p-3 md:grid-cols-2">{["reference", "digest"].map((name) => <Input key={name} aria-label={`Image ${name}`} placeholder={`Image ${name}`} value={String(image[name] || "")} onChange={(event) => setArrayField("workloads", index, "image", { ...image, [name]: event.target.value })} />)}<textarea aria-label="Runtime command arguments, one per line" className="control min-h-24 bg-raised p-2 font-mono text-xs md:col-span-2" value={Array.isArray(workload.command) ? workload.command.join("\n") : ""} onChange={(event) => setArrayField("workloads", index, "command", event.target.value.split("\n"))} /><Button size="sm" variant="outline" onClick={() => setArray("workloads", records(buffer.manifest.workloads).filter((_, item) => item !== index))}>Remove runtime</Button></div>; })}<Button variant="outline" onClick={() => setArray("workloads", [...records(buffer.manifest.workloads), { image: { reference: "", digest: "" }, command: [] }])}>Add runtime</Button><p className="text-xs text-muted">Advanced JSON preserves and exposes all accelerator, variant, permission, extension and runtime fields.</p></section>
      </fieldset> : null}
    </> : null}

    <section className="control space-y-3 p-4"><h3 className="font-display text-lg font-semibold">Helper files</h3><p className="text-sm text-muted">Selection binds path, hash and origin. Inspected files are read-only; generated helper text has its own unsaved buffer.</p>
      <div className="grid gap-2 md:grid-cols-2">{draft.candidates.map((candidate) => { const selected = buffer.selected.some((item) => item.path === candidate.path && item.sha256 === candidate.sha256 && (!item.origin || item.origin === candidate.origin)); return <div key={fileKey(candidate)} className="space-y-2 border border-rule p-2 text-xs"><label className="flex gap-2"><input type="checkbox" checked={selected} disabled={disabled} onChange={() => patch({ manifestDirty: true, selected: selected ? buffer.selected.filter((item) => !(item.path === candidate.path && item.sha256 === candidate.sha256 && (!item.origin || item.origin === candidate.origin))) : [...buffer.selected, { path: candidate.path, sha256: candidate.sha256, origin: candidate.origin }] })} /><span className="break-all">{candidate.path} · {candidate.origin} · {candidate.size} bytes</span></label>{!candidate.binary ? <Button size="sm" variant="outline" onClick={() => mutation.mutate(() => openFile(candidate))}>Open file</Button> : null}</div>; })}</div>
      <div className="flex gap-2"><Input aria-label="New helper file path" value={newFile} onChange={(event) => setNewFile(event.target.value)} placeholder="helpers/config.py" /><Button variant="outline" disabled={disabled || !newFile.trim()} onClick={() => { const selection = { path: newFile.trim(), sha256: "", origin: "generated" as const }; const key = fileKey(selection); patch({ files: { ...buffer.files, [key]: { selection, origin: "generated", content: "", dirty: true } } }); setOpenedFile(key); setNewFile(""); }}>Add helper file</Button></div>
      {Object.entries(buffer.files).some(([, item]) => item.dirty) ? <div className="flex flex-wrap gap-2">{Object.entries(buffer.files).filter(([, item]) => item.dirty).map(([key, item]) => <Button key={key} size="sm" variant="outline" onClick={() => setOpenedFile(key)}>Unsaved: {item.selection.path}</Button>)}</div> : null}
    </section>
    {file ? <section className="control space-y-3 p-4"><h3 className="break-all font-mono text-sm">{file.selection.path} · {file.origin}{file.selection.source_commit ? ` · ${file.selection.source_commit}` : ""}</h3><textarea aria-label="Helper or evidence file content" className="control min-h-72 w-full bg-raised p-3 font-mono text-xs" readOnly={file.origin !== "generated" || !editable || active || mutation.isPending} value={file.content} onChange={(event) => patch({ files: { ...buffer.files, [openedFile]: { ...file, content: event.target.value, dirty: true } } })} />{file.origin === "generated" ? <Button disabled={disabled || !file.dirty} onClick={() => mutation.mutate(async () => adopt(await api.updateRecipeDraftFile(id, buffer.baseVersion, { path: file.selection.path, content: file.content }), "file", file.selection.path))}>Save helper text</Button> : null}</section> : null}

    <section className="control space-y-3 p-4"><h3 className="font-display text-lg font-semibold">Questions and validation</h3><p className="text-sm text-muted">Configuration checked; not tested on hardware.</p>{draft.questions.map((question) => <label className="block space-y-2 border border-rule p-3 text-sm" key={question.id}><span>{question.question}</span><Input disabled={disabled} value={buffer.answers[question.id] ?? question.answer ?? ""} onChange={(event) => patch({ answersDirty: true, answers: { ...buffer.answers, [question.id]: event.target.value } })} /></label>)}
      {draft.diagnostics.map((diagnostic) => <article className={`space-y-1 border p-3 ${diagnostic.blocking ? "border-fault/40" : "border-warning/40"}`} key={diagnostic.id}><p className="break-all font-mono text-xs">{diagnostic.code}{diagnostic.path ? ` · ${diagnostic.path}` : ""}</p><p className="text-sm">{diagnostic.message}</p><p className="text-xs text-muted">{diagnostic.remediation}</p>{diagnostic.dismissible ? <Button size="sm" variant="outline" disabled={disabled || dirty} onClick={() => mutation.mutate(async () => adopt(await api.dismissRecipeDraftDiagnostics(id, buffer.baseVersion, [diagnostic.id]), "all"))}>Dismiss optional finding</Button> : null}</article>)}
      <Button disabled={disabled || (!buffer.manifestDirty && !buffer.answersDirty)} onClick={() => mutation.mutate(() => saveWork())}>Save editable work</Button>
      <details><summary className="cursor-pointer text-sm">Reference credentials and checksum evidence</summary><div className="mt-3 space-y-3">{secrets.isError ? <RecipeError error={secrets.error} retry={() => void secrets.refetch()} /> : null}{(buffer.references?.credentials || []).map((credential, index) => <div className="grid gap-2 md:grid-cols-3" key={index}>{["path", "host"].map((name) => <Input key={name} aria-label={`Reference credential ${name}`} placeholder={name} value={credential[name as "path" | "host"]} onChange={(event) => patch({ references: { ...buffer.references, credentials: buffer.references!.credentials!.map((item, i) => i === index ? { ...item, [name]: event.target.value } : item) } })} />)}<select aria-label="Reference secret" className="control bg-panel p-2" value={credential.secret_id} onChange={(event) => patch({ references: { ...buffer.references, credentials: buffer.references!.credentials!.map((item, i) => i === index ? { ...item, secret_id: event.target.value } : item) } })}><option value="">Select saved credential</option>{(secrets.data || []).filter((secret) => ["huggingface", "registry"].includes(secret.purpose)).map((secret) => <option key={secret.id} value={secret.id}>{secret.name}</option>)}</select><Button size="sm" variant="outline" onClick={() => patch({ references: { ...buffer.references, credentials: buffer.references!.credentials!.filter((_, i) => i !== index) } })}>Remove credential selection</Button></div>)}<Button size="sm" variant="outline" onClick={() => patch({ references: { ...buffer.references, credentials: [...(buffer.references?.credentials || []), { path: "", host: "", secret_id: "" }] } })}>Add reference credential</Button>
        {(buffer.references?.file_checksums || []).map((checksum, index) => <div className="grid gap-2 md:grid-cols-2" key={index}>{(["path", "url", "sha256", "evidence_note"] as const).map((name) => <Input key={name} aria-label={`Checksum ${name}`} placeholder={name} value={checksum[name]} onChange={(event) => patch({ references: { ...buffer.references, file_checksums: buffer.references!.file_checksums!.map((item, i) => i === index ? { ...item, [name]: event.target.value } : item) } })} />)}<Button size="sm" variant="outline" onClick={() => patch({ references: { ...buffer.references, file_checksums: buffer.references!.file_checksums!.filter((_, i) => i !== index) } })}>Remove checksum evidence</Button></div>)}<Button size="sm" variant="outline" onClick={() => patch({ references: { ...buffer.references, file_checksums: [...(buffer.references?.file_checksums || []), { path: "", url: "", sha256: "", evidence_note: "" }] } })}>Add file checksum evidence</Button></div></details>
      <Button variant="outline" disabled={disabled || dirty} onClick={() => mutation.mutate(() => queue(api.resolveRecipeDraftReferences(id, buffer.baseVersion, buffer.references)))}>Verify references</Button>
    </section>

    <section className="control space-y-3 p-4"><div className="flex flex-wrap items-center justify-between gap-3"><h3 className="font-display text-lg font-semibold">AI assistance</h3><Link className="text-sm underline" to={settingsURL}>Manage saved connections</Link></div><p className="text-sm text-muted">AI suggests configuration only. First review the exact destination and source/log content. Nothing is applied automatically.</p>
      {settings.isError ? <RecipeError error={settings.error} retry={() => void settings.refetch()} /> : null}
      <label className="block space-y-1 text-sm">Saved provider<select className="control w-full bg-panel p-2" value={buffer.providerId} onChange={(event) => patch({ providerId: event.target.value })}><option value="">Choose a provider</option>{providers.map((provider) => <option key={String(provider.id)} value={String(provider.id)}>{String(provider.label || provider.id)}</option>)}</select></label>
      <label className="block space-y-1 text-sm">Instruction<textarea className="control min-h-24 w-full bg-panel p-2" value={buffer.instruction} onChange={(event) => patch({ instruction: event.target.value })} /></label>
      <details><summary className="cursor-pointer text-sm">Choose diagnostics to send</summary><div className="mt-2 space-y-2">{draft.diagnostics.map((diagnostic) => <label key={diagnostic.id} className="flex gap-2 text-sm"><input type="checkbox" checked={buffer.diagnosticIds.includes(diagnostic.id)} onChange={() => patch({ diagnosticIds: buffer.diagnosticIds.includes(diagnostic.id) ? buffer.diagnosticIds.filter((value) => value !== diagnostic.id) : [...buffer.diagnosticIds, diagnostic.id] })} /><span>{diagnostic.code}: {diagnostic.message}</span></label>)}</div></details>
      {draft.change_context?.deployment_id ? <details><summary className="cursor-pointer text-sm">Choose bounded model failure logs</summary><p className="mt-2 text-xs text-muted">Only explicitly selected excerpts from this exact deployment are eligible. The preview redacts known secrets, but you must review it for sensitive content.</p>{deployment.isError ? <RecipeError error={deployment.error} retry={() => void deployment.refetch()} /> : null}{buffer.runExcerpts.map((excerpt, index) => <div className="mt-2 grid gap-2 border border-rule p-2 md:grid-cols-3" key={index}><Input aria-label="Failure run ID" value={excerpt.run_id} onChange={(event) => patch({ runExcerpts: buffer.runExcerpts.map((item, i) => i === index ? { ...item, run_id: event.target.value } : item) })} /><select aria-label="Log stream" className="control bg-panel p-2" value={excerpt.stream} onChange={(event) => patch({ runExcerpts: buffer.runExcerpts.map((item, i) => i === index ? { ...item, stream: event.target.value as "stdout" | "stderr" } : item) })}><option>stderr</option><option>stdout</option></select>{(["rank", "offset", "byte_count"] as const).map((name) => <Input key={name} aria-label={`Log ${name}`} type="number" min={name === "byte_count" ? 1 : 0} max={name === "byte_count" ? 16384 : undefined} value={excerpt[name]} onChange={(event) => patch({ runExcerpts: buffer.runExcerpts.map((item, i) => i === index ? { ...item, [name]: Number(event.target.value) } : item) })} />)}<Button size="sm" variant="outline" onClick={() => patch({ runExcerpts: buffer.runExcerpts.filter((_, i) => i !== index) })}>Remove excerpt</Button></div>)}<Button className="mt-2" size="sm" variant="outline" disabled={buffer.runExcerpts.length >= 4} onClick={() => patch({ runExcerpts: [...buffer.runExcerpts, { deployment_id: draft.change_context!.deployment_id!, run_id: deployment.data?.run_id || "", rank: 0, stream: "stderr", offset: 0, byte_count: 8192 }] })}>Add failure log selection</Button></details> : null}
      <Button variant="outline" disabled={disabled || dirty || !selectedProvider || !settings.data?.version} onClick={() => mutation.mutate(requestPreview)}>Review content before sending</Button>
      {dirty ? <p className="text-xs text-warning">Save context, structured edits and helper text, and apply valid raw JSON before requesting a preview.</p> : null}
      {preview ? <section className="space-y-3 border border-warning/40 p-3"><h4 className="font-semibold">Exact AI request</h4><p className="break-all text-sm">Destination: {preview.value.destination} · Model: {preview.value.model}</p>{preview.value.warnings.map((warning, index) => <p className="text-sm text-warning" key={index}>{warning}</p>)}<pre className="max-h-96 overflow-auto bg-raised p-3 text-xs">{JSON.stringify(preview.value.request, null, 2)}</pre><label className="flex gap-2 text-sm"><input type="checkbox" checked={contentConsent} onChange={(event) => setContentConsent(event.target.checked)} />I approve sending exactly this content to this provider and understand it may use my quota.</label><Button disabled={disabled || dirty || !contentConsent || preview.version !== draft.version} onClick={() => mutation.mutate(async () => { await queue(api.generateRecipeDraft(id, preview.version, { ...preview.request, preview_sha256: preview.value.preview_sha256, content_consent: true })); setPreview(undefined); setContentConsent(false); })}>Request suggested changes</Button></section> : null}
    </section>

    {draft.proposal ? <section className="space-y-3 border border-accent/40 p-4"><h3 className="font-display text-lg font-semibold">Review suggested changes</h3><p>{draft.proposal.summary || "Suggested configuration"}</p><p className="text-sm text-muted">Use these changes only updates editable work. The saved recipe and running models remain unchanged.</p>{draft.proposal.base_version !== draft.version ? <p className="text-sm text-warning">This suggestion is stale. It remains available for comparison, but cannot be accepted.</p> : null}<details><summary className="cursor-pointer text-sm">Exact suggested manifest</summary><pre className="max-h-96 overflow-auto text-xs">{JSON.stringify(draft.proposal.manifest, null, 2)}</pre></details>{draft.proposal.files.map((suggested) => <details key={suggested.path}><summary className="cursor-pointer break-all text-sm">Suggested helper: {suggested.path}</summary><pre className="max-h-80 overflow-auto text-xs">{suggested.content}</pre></details>)}{draft.proposal.evidence.map((evidence, index) => <div className="flex flex-wrap items-center gap-2 text-xs" key={index}><span className="break-all">{evidence.path} · {evidence.source_commit || draft.resolved_commit}{evidence.start_line ? ` · lines ${evidence.start_line}–${evidence.end_line || evidence.start_line}` : " · whole file"}</span><Button size="sm" variant="outline" onClick={() => mutation.mutate(() => openFile(evidence))}>Read cited source</Button></div>)}{draft.proposal.questions.map((question) => <p key={question.id} className="text-sm text-warning">Unanswered: {question.question}</p>)}{draft.proposal.diagnostics.map((diagnostic) => <p className="text-sm text-warning" key={diagnostic.id}>{diagnostic.code}: {diagnostic.message}</p>)}<div className="flex flex-wrap gap-2"><Button disabled={disabled || dirty || draft.proposal.base_version !== draft.version} onClick={() => mutation.mutate(async () => adopt(await api.acceptRecipeDraftProposal(id, draft.version, draft.proposal!.id), "all"))}>Use these changes</Button><Button variant="outline" disabled={disabled} onClick={() => mutation.mutate(async () => adopt(await api.discardRecipeDraftProposal(id, draft.version, draft.proposal!.id), "metadata"))}>Discard suggestion</Button></div></section> : null}

    {existingRecipeURL && !draft.change_context?.base_recipe_digest ? <p role="alert" className="border border-warning/40 p-3 text-sm">This repository is already in the catalog. <Link className="underline" to={existingRecipeURL}>Open its recipe page to review changes</Link>. This addition cannot replace its saved version.</p> : null}
    {editable ? <section className="control space-y-3 p-4"><h3 className="font-display text-lg font-semibold">{draft.change_context?.base_recipe_digest ? "Save updated recipe" : "Add to library"}</h3><p className="text-sm text-muted">This saves only the reviewed configuration and helper package. It does not download files to devices, replace running versions, or start models.</p><details><summary className="cursor-pointer text-sm">Permissions and runtime consequences</summary><pre className="max-h-64 overflow-auto text-xs">{JSON.stringify({ permissions: buffer.manifest.permissions, prepare: buffer.manifest.prepare, verify: buffer.manifest.verify, workloads: buffer.manifest.workloads }, null, 2)}</pre></details>{warnings.map((warning) => <label className="flex gap-2 border border-warning/40 p-2 text-sm" key={warning.id}><input type="checkbox" disabled={!warning.acknowledgement} checked={Boolean(warning.acknowledgement && warningsAccepted.includes(warning.acknowledgement))} onChange={() => setWarningsAccepted((current) => current.includes(warning.acknowledgement!) ? current.filter((token) => token !== warning.acknowledgement) : [...current, warning.acknowledgement!])} /><span>I acknowledge {warning.code}: {warning.message}</span></label>)}{blockers.length ? <p className="text-sm text-fault">Resolve all {blockers.length} blocking findings before saving to the catalog. Editable work can still be saved.</p> : null}<Button disabled={disabled || dirty || Boolean(blockers.length) || !allWarningsAccepted || Boolean(existingRecipeURL && !draft.change_context?.base_recipe_digest)} onClick={() => mutation.mutate(saveToLibrary)}>{draft.change_context?.base_recipe_digest ? "Save updated recipe" : "Add to library"}</Button><p className="text-xs text-muted">A refresh, login interruption, or failed request stops the package-to-save continuation. Review the saved operation before confirming again.</p></section> : null}
  </div>;
}
