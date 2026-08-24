import { useEffect, useMemo, useState } from "react";
import { Check, FileUp, Link2, Plus, RefreshCw, Rocket } from "lucide-react";
import { Button } from "~/components/ui/button";
import type { AutoResearchIdea, AutoResearchSource } from "~/lib/api";
import {
  useCreateAutoResearchRun,
  useCreateAutoResearchSource,
  useGenerateAutoResearchIdeas,
  useSelectAutoResearchIdea,
  useUpdateAutoResearchIdea,
  useUploadAutoResearchSource,
} from "~/lib/queries";
import { cn } from "~/lib/utils";

export function IdeaWorkspace({
  projectId,
  projectPrompt,
  ideas,
  sources,
}: {
  projectId: string;
  projectPrompt: string;
  ideas: AutoResearchIdea[];
  sources: AutoResearchSource[];
}) {
  const [activeId, setActiveId] = useState<string>(ideas[0]?.id ?? "");
  const [title, setTitle] = useState("");
  const [body, setBody] = useState("");
  const [candidateCount, setCandidateCount] = useState(1);
  const [prompt, setPrompt] = useState(projectPrompt);
  const [sourceKind, setSourceKind] = useState<"arxiv" | "doi" | "url">("arxiv");
  const [locator, setLocator] = useState("");
  const activeIdea = useMemo(() => ideas.find((idea) => idea.id === activeId) ?? ideas[0], [activeId, ideas]);
  const updateIdea = useUpdateAutoResearchIdea(projectId);
  const selectIdea = useSelectAutoResearchIdea(projectId);
  const generate = useGenerateAutoResearchIdeas(projectId);
  const createSource = useCreateAutoResearchSource(projectId);
  const uploadSource = useUploadAutoResearchSource(projectId);
  const createRun = useCreateAutoResearchRun(projectId);

  useEffect(() => {
    if (!activeIdea) {
      setTitle("");
      setBody("");
      return;
    }
    setActiveId(activeIdea.id);
    setTitle(activeIdea.title);
    setBody(activeIdea.body);
  }, [activeIdea]);

  const pending = updateIdea.isPending || selectIdea.isPending || generate.isPending || createSource.isPending || uploadSource.isPending || createRun.isPending;
  const selectedCount = ideas.filter((idea) => idea.selected).length;
  const sourceBlocked = sources.some((source) => source.status !== "ready");

  return (
    <section className="lmw-panel overflow-hidden">
      <header className="lmw-panel-head">
        <h2 className="lmw-label">idea workspace</h2>
        <span className="ml-auto font-mono text-[10px] text-faint">{selectedCount} selected · {sources.length} sources</span>
      </header>
      <div className="grid gap-px bg-hairline xl:grid-cols-[320px_minmax(0,1fr)]">
        <aside className="bg-panel p-3">
          <label className="lmw-label mb-1 block" htmlFor="candidate-prompt">topic radar</label>
          <textarea id="candidate-prompt" value={prompt} onChange={(event) => setPrompt(event.target.value)} className="min-h-24 w-full resize-y rounded border border-hairline bg-background px-3 py-2 font-mono text-xs" placeholder="Prompt candidate generation with a bounded research topic…" />
          <div className="mt-2 flex items-end gap-2">
            <label className="min-w-0 flex-1 font-mono text-[10px] text-faint">candidates
              <input className="mt-1 h-8 w-full rounded border border-hairline bg-background px-2 font-mono text-xs" type="number" min={1} max={10} value={candidateCount} onChange={(event) => setCandidateCount(Math.min(10, Math.max(1, Number(event.target.value))))} />
            </label>
            <Button size="sm" disabled={pending || sourceBlocked || prompt.trim() === ""} onClick={() => generate.mutate({ candidate_count: candidateCount, prompt })}>
              <RefreshCw aria-hidden /> {ideas.length ? "regenerate" : "generate"}
            </Button>
          </div>

          <div className="my-3 border-t border-hairline" />
          <p className="lmw-label mb-2">supporting sources</p>
          <div className="flex gap-1">
            <select aria-label="Source kind" className="h-8 rounded border border-hairline bg-background px-2 font-mono text-xs" value={sourceKind} onChange={(event) => setSourceKind(event.target.value as typeof sourceKind)}>
              <option value="arxiv">arXiv</option><option value="doi">DOI</option><option value="url">HTTPS URL</option>
            </select>
            <input aria-label="Source locator" className="h-8 min-w-0 flex-1 rounded border border-hairline bg-background px-2 font-mono text-xs" value={locator} onChange={(event) => setLocator(event.target.value)} placeholder="locator" />
            <Button size="icon" variant="outline" aria-label="Attach source" disabled={pending || locator.trim() === ""} onClick={() => createSource.mutate({ kind: sourceKind, locator }, { onSuccess: () => setLocator("") })}><Plus aria-hidden /></Button>
          </div>
          <label className="control mt-2 flex cursor-pointer items-center justify-center gap-2 rounded border border-dashed border-hairline px-3 py-2 font-mono text-[10px] text-muted hover:border-primary/50 hover:text-foreground">
            <FileUp className="h-3.5 w-3.5" aria-hidden /> upload PDF
            <input className="sr-only" type="file" accept="application/pdf,.pdf" onChange={(event) => { const file = event.target.files?.[0]; if (file) uploadSource.mutate(file); event.currentTarget.value = ""; }} />
          </label>
          <ul className="mt-2 grid gap-1" aria-label="Attached sources">
            {sources.length === 0 ? <li className="font-mono text-[10px] text-faint">No sources attached. Topic-only intake is allowed.</li> : sources.map((source) => (
              <li key={source.id} className="rounded border border-hairline bg-raised/40 px-2 py-1.5">
                <div className="flex items-center gap-1.5"><Link2 className="h-3 w-3 text-faint" aria-hidden /><span className="min-w-0 flex-1 truncate font-mono text-[10px]">{source.title ?? source.locator}</span><span className={cn("font-mono text-[9px]", source.status === "ready" ? "text-ok" : source.status === "blocked" ? "text-warn" : source.status === "failed" ? "text-danger" : "text-faint")}>{source.status}</span></div>
                {source.error ? <p className="mt-1 font-mono text-[9px] text-warn">{source.error}</p> : null}
              </li>
            ))}
          </ul>
        </aside>

        <div className="min-w-0 bg-panel">
          <div className="flex gap-1 overflow-x-auto border-b border-hairline px-3 pt-2" role="tablist" aria-label="Idea candidates">
            {ideas.map((idea, index) => (
              <button key={idea.id} role="tab" aria-selected={activeIdea?.id === idea.id} className={cn("control shrink-0 rounded-t border border-b-0 px-3 py-1.5 font-mono text-[10px]", activeIdea?.id === idea.id ? "border-primary/50 bg-raised text-foreground" : "border-hairline text-faint hover:text-foreground")} onClick={() => setActiveId(idea.id)}>
                {String(index + 1).padStart(2, "0")} {idea.selected ? "✓" : ""}
              </button>
            ))}
          </div>
          {activeIdea ? (
            <div className="p-3">
              <label className="lmw-label mb-1 block" htmlFor="idea-title">candidate title</label>
              <input id="idea-title" className="h-9 w-full rounded border border-hairline bg-background px-3 font-display text-sm" value={title} onChange={(event) => setTitle(event.target.value)} />
              <label className="lmw-label mb-1 mt-3 block" htmlFor="idea-body">candidate document</label>
              <textarea id="idea-body" className="min-h-[340px] w-full resize-y rounded border border-hairline bg-background px-3 py-2 font-mono text-xs leading-5" value={body} onChange={(event) => setBody(event.target.value)} />
              <div className="mt-3 flex flex-wrap gap-2">
                <Button variant="outline" size="sm" disabled={pending || title.trim() === "" || body.trim() === "" || (title === activeIdea.title && body === activeIdea.body)} onClick={() => updateIdea.mutate({ ideaId: activeIdea.id, version: activeIdea.version, body: { title, body } })}>save candidate</Button>
                <Button variant="outline" size="sm" disabled={pending || activeIdea.selected} onClick={() => selectIdea.mutate(activeIdea.id)}><Check aria-hidden /> {activeIdea.selected ? "selected" : "select"}</Button>
                <Button size="sm" className="ml-auto" disabled={pending || selectedCount === 0} onClick={() => createRun.mutate({ factory: "idea" })}><Rocket aria-hidden /> continue selected</Button>
              </div>
            </div>
          ) : <div className="grid min-h-[460px] place-items-center p-6 text-center"><div><p className="font-display text-lg">No candidate document</p><p className="mt-1 max-w-sm font-mono text-xs text-faint">Write a direct idea when creating the project or generate one to open the selection gate.</p></div></div>}
        </div>
      </div>
      {(generate.error || updateIdea.error || selectIdea.error || createSource.error || uploadSource.error || createRun.error) ? <p className="border-t border-hairline px-3 py-2 font-mono text-[10px] text-danger">{String((generate.error ?? updateIdea.error ?? selectIdea.error ?? createSource.error ?? uploadSource.error ?? createRun.error) instanceof Error ? (generate.error ?? updateIdea.error ?? selectIdea.error ?? createSource.error ?? uploadSource.error ?? createRun.error) : "Action failed")}</p> : null}
    </section>
  );
}
