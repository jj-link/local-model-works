import { useEffect, useMemo, useState } from "react";
import { Beaker, ChevronDown, FlaskConical, Plus, Settings2 } from "lucide-react";
import { Button } from "~/components/ui/button";
import {
  useAutoResearchIdeas,
  useAutoResearchPaperFiles,
  useAutoResearchProject,
  useAutoResearchProjects,
  useAutoResearchRuns,
  useAutoResearchSources,
  useControlAutoResearchRun,
  useCreateAutoResearchProject,
  useNodes,
} from "~/lib/queries";
import { duration, number, shortId } from "~/lib/format";
import { cn } from "~/lib/utils";
import { GenerationStream } from "./generation-stream";
import { IdeaWorkspace } from "./idea-workspace";
import { PaperStudio } from "./paper-studio";
import { RoleControls } from "./role-controls";
import { useAutoResearchEvents } from "./events";
import { WorkflowGraph } from "./workflow-graph";

const TERMINAL = new Set(["succeeded", "failed", "cancelled", "interrupted"]);

type WorkspaceTab = "factory" | "ideas" | "paper" | "settings";

export default function AutoResearchRoute() {
  const projects = useAutoResearchProjects();
  const nodes = useNodes();
  const createProject = useCreateAutoResearchProject();
  const [projectId, setProjectId] = useState("");
  const [tab, setTab] = useState<WorkspaceTab>("factory");
  const [creating, setCreating] = useState(false);
  const [name, setName] = useState("");
  const [ideaPrompt, setIdeaPrompt] = useState("");
  const [runnerNodeId, setRunnerNodeId] = useState("");

  useEffect(() => {
    if (!projectId && projects.data?.[0]) setProjectId(projects.data[0].id);
  }, [projectId, projects.data]);

  useEffect(() => {
    if (!runnerNodeId) {
      const online = nodes.data?.find((node) => node.status === "online");
      if (online) setRunnerNodeId(online.id);
    }
  }, [nodes.data, runnerNodeId]);

  const project = useAutoResearchProject(projectId || undefined);
  const ideas = useAutoResearchIdeas(projectId || undefined);
  const sources = useAutoResearchSources(projectId || undefined);
  const runs = useAutoResearchRuns(projectId || undefined);
  const paperFiles = useAutoResearchPaperFiles(projectId || undefined);
  const control = useControlAutoResearchRun(projectId);
  const latestRun = useMemo(() => runs.data?.[0], [runs.data]);
  const stream = useAutoResearchEvents(latestRun?.id, Boolean(latestRun));
  const totalTokens = useMemo(
    () => stream.events.reduce((total, event) => {
      if (event.type !== "agent.usage") return total;
      const input = Number(event.payload.input_tokens ?? 0);
      const output = Number(event.payload.output_tokens ?? 0);
      return total + (Number.isFinite(input) ? input : 0) + (Number.isFinite(output) ? output : 0);
    }, 0),
    [stream.events],
  );
  const selectedIdea = ideas.data?.find((idea) => idea.selected);
  const researchTopic = selectedIdea?.title ?? project.data?.idea_prompt ?? "Awaiting an idea or generated candidate";
  const tabLabels: Record<WorkspaceTab, string> = {
    factory: "Factory",
    ideas: "Ideas & sources",
    paper: "Paper studio",
    settings: "Role controls",
  };

  useEffect(() => {
    if (project.data?.status === "paper_editing" || project.data?.status === "completed") setTab("paper");
  }, [project.data?.status]);

  const create = () => {
    createProject.mutate({ name, idea_prompt: ideaPrompt || undefined, runner_node_id: runnerNodeId || undefined }, {
      onSuccess: (created) => {
        setProjectId(created.id);
        setCreating(false);
        setName("");
        setIdeaPrompt("");
        setTab(created.status === "awaiting_idea_selection" ? "ideas" : "factory");
      },
    });
  };

  if (projects.isPending) return <div className="lmw-panel p-8 text-center font-mono text-xs text-faint">loading AutoResearch projects…</div>;
  if (projects.isError) return <div className="lmw-panel p-8 text-center"><p className="font-display text-lg">AutoResearch Factory unavailable</p><p className="mt-1 font-mono text-xs text-fault">{projects.error.message}</p></div>;

  return (
    <div className="grid gap-4">
      <section className="grid gap-5 border-b border-hairline pb-5 lg:grid-cols-[minmax(0,1fr)_auto] lg:items-end">
        <div>
          <p className="lmw-label text-accent">Autonomous research workspace</p>
          <h1 className="mt-1 font-display text-4xl leading-none tracking-[-0.025em] text-foreground sm:text-5xl">
            AutoResearch Factory
          </h1>
          <p className="mt-3 max-w-2xl text-sm leading-6 text-muted">
            Take a research question from source-backed ideas to experiments and a release-ready paper, with every agent handoff visible.
          </p>
        </div>
        <dl className="grid min-w-[320px] grid-cols-3 divide-x divide-hairline border border-hairline bg-panel">
          <div className="px-3 py-2.5"><dt className="lmw-label">Run</dt><dd className="mt-1 font-mono text-sm font-medium">{latestRun ? shortId(latestRun.id) : "—"}</dd></div>
          <div className="px-3 py-2.5"><dt className="lmw-label">Elapsed</dt><dd className="mt-1 font-mono text-sm font-medium tnum">{latestRun ? duration(latestRun.started_at, latestRun.finished_at) : "—"}</dd></div>
          <div className="px-3 py-2.5"><dt className="lmw-label">Tokens</dt><dd className="mt-1 font-mono text-sm font-medium tnum">{totalTokens ? number(totalTokens) : "—"}</dd></div>
        </dl>
      </section>

      <section className="lmw-panel overflow-hidden">
        <div className="flex flex-wrap items-center gap-3 p-4">
          <div className="grid h-10 w-10 place-items-center rounded border border-accent/35 bg-accent/8 text-accent"><FlaskConical className="h-5 w-5" aria-hidden /></div>
          <div className="min-w-0">
            <p className="lmw-label">Research project</p>
            <p className="truncate font-display text-lg font-semibold">{project.data?.name ?? "Select or create a project"}</p>
          </div>
          <div className="ml-auto flex min-w-0 items-center gap-2">
            {projects.data?.length ? <label className="relative min-w-0"><span className="sr-only">Active project</span><select value={projectId} onChange={(event) => setProjectId(event.target.value)} className="h-9 max-w-64 appearance-none rounded border border-hairline bg-panel pl-3 pr-8 font-mono text-xs"><option value="">select project</option>{projects.data.map((item) => <option key={item.id} value={item.id}>{item.name} · {item.status}</option>)}</select><ChevronDown className="pointer-events-none absolute right-2 top-2.5 h-4 w-4 text-faint" aria-hidden /></label> : null}
            <Button size="sm" onClick={() => setCreating((value) => !value)}><Plus aria-hidden /> new project</Button>
          </div>
        </div>
        {creating ? <div className="grid gap-3 border-t border-hairline bg-raised/40 p-4 lg:grid-cols-[240px_minmax(0,1fr)_220px_auto]">
          <label className="font-mono text-[10px] text-muted">project name<input value={name} onChange={(event) => setName(event.target.value)} className="mt-1 block h-9 w-full rounded border border-hairline bg-panel px-3 font-display text-sm text-foreground" placeholder="Evidence-aware decoding" /></label>
          <label className="font-mono text-[10px] text-muted">direct idea (optional)<textarea value={ideaPrompt} onChange={(event) => setIdeaPrompt(event.target.value)} className="mt-1 min-h-20 w-full resize-y rounded border border-hairline bg-panel px-3 py-2 font-mono text-xs text-foreground" placeholder="Leave blank to generate candidates from a prompt and sources." /></label>
          <label className="font-mono text-[10px] text-muted">runner node<select value={runnerNodeId} onChange={(event) => setRunnerNodeId(event.target.value)} className="mt-1 block h-9 w-full rounded border border-hairline bg-panel px-2 font-mono text-xs text-foreground"><option value="">not configured</option>{nodes.data?.map((node) => <option key={node.id} value={node.id}>{node.display_name} · {node.status}</option>)}</select></label>
          <Button className="self-end" disabled={createProject.isPending || name.trim() === ""} onClick={create}><Beaker aria-hidden /> create</Button>
        </div> : null}
      </section>

      {!projectId || !project.data ? (
        <section className="lmw-panel grid min-h-[420px] place-items-center p-8 text-center"><div><FlaskConical className="mx-auto h-8 w-8 text-accent/70" aria-hidden /><h2 className="mt-3 font-display text-2xl">No research project selected</h2><p className="mt-2 max-w-lg text-sm leading-6 text-muted">Create a project with a direct idea, or begin with bounded candidate generation and verified sources.</p></div></section>
      ) : (
        <>
          <section className="lmw-panel grid gap-px overflow-hidden bg-hairline lg:grid-cols-[minmax(0,1fr)_180px_180px]">
            <div className="bg-panel p-4">
              <p className="lmw-label">Research topic</p>
              <p className="mt-2 font-display text-xl leading-snug">{researchTopic}</p>
            </div>
            <div className="bg-panel p-4"><p className="lmw-label">Current stage</p><p className="mt-2 font-mono text-xs">{latestRun?.kind ?? project.data.status}</p></div>
            <div className="bg-panel p-4"><p className="lmw-label">Project state</p><p className={cn("mt-2 font-mono text-xs", project.data.status === "failed" ? "text-fault" : project.data.status === "completed" ? "text-ok" : "text-accent")}>{project.data.status}</p></div>
          </section>

          <nav className="flex items-center gap-1 overflow-x-auto border-b border-hairline" aria-label="AutoResearch Factory workspace">
            {(["factory", "ideas", "paper", "settings"] as const).map((value) => <button key={value} type="button" className={cn("control border-b-2 px-3 py-2 font-mono text-[11px]", tab === value ? "border-accent text-foreground" : "border-transparent text-muted hover:text-foreground")} onClick={() => setTab(value)}>{tabLabels[value]}</button>)}
          </nav>

          {tab === "factory" ? <div className="grid gap-3 xl:grid-cols-[minmax(0,1.35fr)_420px]"><WorkflowGraph active={stream.activeInvocations} /><GenerationStream run={latestRun} events={stream.events} active={stream.activeInvocations} reconnecting={stream.reconnecting} streamError={stream.error} controlPending={control.isPending} onControl={(action) => latestRun && control.mutate({ runId: latestRun.id, action })} /></div> : null}
          {tab === "ideas" ? <IdeaWorkspace projectId={projectId} projectPrompt={project.data.idea_prompt} ideas={ideas.data ?? []} sources={sources.data ?? []} /> : null}
          {tab === "paper" ? <PaperStudio projectId={projectId} files={paperFiles.data ?? []} runs={runs.data ?? []} /> : null}
          {tab === "settings" ? <RoleControls project={project.data} /> : null}

          {latestRun && TERMINAL.has(latestRun.state) && latestRun.state !== "succeeded" ? <div className="lmw-panel flex items-center gap-2 border-fault/30 px-3 py-2 font-mono text-[10px] text-fault"><Settings2 className="h-3.5 w-3.5" aria-hidden />{latestRun.error_message ?? `run ${latestRun.state}`}</div> : null}
        </>
      )}
    </div>
  );
}
