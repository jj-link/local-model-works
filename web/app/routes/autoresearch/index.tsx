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
  if (projects.isError) return <div className="lmw-panel p-8 text-center"><p className="font-display text-lg">AutoResearch unavailable</p><p className="mt-1 font-mono text-xs text-danger">{projects.error.message}</p></div>;

  return (
    <div className="grid gap-4">
      <section className="lmw-panel overflow-hidden">
        <div className="flex flex-wrap items-center gap-3 p-4">
          <div className="grid h-10 w-10 place-items-center rounded border border-primary/50 bg-primary/10 text-primary"><FlaskConical className="h-5 w-5" aria-hidden /></div>
          <div className="min-w-0"><p className="lmw-label">first-party agon workflow</p><h1 className="truncate font-display text-xl font-semibold tracking-wide">AutoResearch Factory</h1></div>
          <div className="ml-auto flex min-w-0 items-center gap-2">
            {projects.data?.length ? <label className="relative min-w-0"><span className="sr-only">Active project</span><select value={projectId} onChange={(event) => setProjectId(event.target.value)} className="h-9 max-w-64 appearance-none rounded border border-hairline bg-background pl-3 pr-8 font-mono text-xs"><option value="">select project</option>{projects.data.map((item) => <option key={item.id} value={item.id}>{item.name} · {item.status}</option>)}</select><ChevronDown className="pointer-events-none absolute right-2 top-2.5 h-4 w-4 text-faint" aria-hidden /></label> : null}
            <Button size="sm" onClick={() => setCreating((value) => !value)}><Plus aria-hidden /> project</Button>
          </div>
        </div>
        {creating ? <div className="grid gap-3 border-t border-hairline bg-raised/25 p-4 lg:grid-cols-[240px_minmax(0,1fr)_220px_auto]">
          <label className="font-mono text-[10px] text-faint">project name<input value={name} onChange={(event) => setName(event.target.value)} className="mt-1 block h-9 w-full rounded border border-hairline bg-background px-3 font-display text-sm text-foreground" placeholder="Evidence-aware decoding" /></label>
          <label className="font-mono text-[10px] text-faint">direct idea (optional)<textarea value={ideaPrompt} onChange={(event) => setIdeaPrompt(event.target.value)} className="mt-1 min-h-20 w-full resize-y rounded border border-hairline bg-background px-3 py-2 font-mono text-xs text-foreground" placeholder="Leave blank to generate candidates from a prompt and sources." /></label>
          <label className="font-mono text-[10px] text-faint">runner node<select value={runnerNodeId} onChange={(event) => setRunnerNodeId(event.target.value)} className="mt-1 block h-9 w-full rounded border border-hairline bg-background px-2 font-mono text-xs text-foreground"><option value="">not configured</option>{nodes.data?.map((node) => <option key={node.id} value={node.id}>{node.display_name} · {node.status}</option>)}</select></label>
          <Button className="self-end" disabled={createProject.isPending || name.trim() === ""} onClick={create}><Beaker aria-hidden /> create</Button>
        </div> : null}
      </section>

      {!projectId || !project.data ? (
        <section className="lmw-panel grid min-h-[420px] place-items-center p-8 text-center"><div><FlaskConical className="mx-auto h-8 w-8 text-primary/70" aria-hidden /><h2 className="mt-3 font-display text-xl">No research project selected</h2><p className="mt-1 max-w-lg font-mono text-xs leading-5 text-faint">Create a project with a direct idea, or begin with bounded candidate generation and verified sources.</p></div></section>
      ) : (
        <>
          <nav className="flex items-center gap-1 overflow-x-auto rounded border border-hairline bg-panel p-1" aria-label="AutoResearch workspace">
            {(["factory", "ideas", "paper", "settings"] as const).map((value) => <button key={value} type="button" className={cn("control rounded px-3 py-1.5 font-mono text-[11px]", tab === value ? "bg-raised text-primary" : "text-muted hover:text-foreground")} onClick={() => setTab(value)}>{value}</button>)}
            <span className={cn("ml-auto shrink-0 rounded border px-2 py-1 font-mono text-[10px]", project.data.status === "failed" ? "border-danger/40 text-danger" : project.data.status === "completed" ? "border-ok/40 text-ok" : "border-hairline text-faint")}>{project.data.status}</span>
          </nav>

          {tab === "factory" ? <div className="grid gap-3 xl:grid-cols-[minmax(0,1.35fr)_420px]"><WorkflowGraph active={stream.activeInvocations} /><GenerationStream run={latestRun} events={stream.events} active={stream.activeInvocations} reconnecting={stream.reconnecting} streamError={stream.error} controlPending={control.isPending} onControl={(action) => latestRun && control.mutate({ runId: latestRun.id, action })} /></div> : null}
          {tab === "ideas" ? <IdeaWorkspace projectId={projectId} projectPrompt={project.data.idea_prompt} ideas={ideas.data ?? []} sources={sources.data ?? []} /> : null}
          {tab === "paper" ? <PaperStudio projectId={projectId} files={paperFiles.data ?? []} runs={runs.data ?? []} /> : null}
          {tab === "settings" ? <RoleControls project={project.data} /> : null}

          {latestRun && TERMINAL.has(latestRun.state) && latestRun.state !== "succeeded" ? <div className="lmw-panel flex items-center gap-2 border-danger/30 px-3 py-2 font-mono text-[10px] text-danger"><Settings2 className="h-3.5 w-3.5" aria-hidden />{latestRun.error_message ?? `run ${latestRun.state}`}</div> : null}
        </>
      )}
    </div>
  );
}
