import { useEffect, useMemo, useState } from "react";
import { ChevronDown, Plus, Settings2 } from "lucide-react";
import {
  useAutoResearchIdeas,
  useAutoResearchPaperFiles,
  useAutoResearchProject,
  useAutoResearchProjects,
  useAutoResearchRuns,
  useAutoResearchSources,
  useControlAutoResearchRun,
  useCreateAutoResearchRun,
  useNodes,
  useUpdateAutoResearchIdea,
  useUpdateAutoResearchProject,
} from "~/lib/queries";
import { duration, shortId } from "~/lib/format";
import { cn } from "~/lib/utils";
import { GenerationStream } from "./generation-stream";
import { IdeaWorkspace } from "./idea-workspace";
import { NewAutoResearchProjectDialog } from "./new-project-dialog";
import { PaperStudio } from "./paper-studio";
import { RoleControls } from "./role-controls";
import { summarizeAutoResearchUsage, useAutoResearchEvents } from "./events";
import { WorkflowGraph } from "./workflow-graph";
import "./factory.css";

const TERMINAL = new Set(["succeeded", "failed", "cancelled", "interrupted"]);
const TRANSITIONAL_LABELS: Record<string, string> = {
  queued: "Queued",
  planning: "Planning",
  waiting: "Waiting",
  verifying: "Verifying",
  cancelling: "Cancelling",
};

type WorkspaceTab = "factory" | "ideas" | "paper" | "settings";
type Factory = "idea" | "proposal" | "deep_lit" | "experiment";

const TAB_LABELS: Record<WorkspaceTab, string> = {
  factory: "Factory",
  ideas: "Ideas & sources",
  paper: "Paper studio",
  settings: "Role controls",
};

function statusTone(status: string | undefined): "healthy" | "running" | "waiting" | "failed" | "info" {
  if (status === "completed" || status === "succeeded") return "healthy";
  if (status === "running") return "running";
  if (status === "paused" || status === "awaiting_idea_selection" || status === "waiting") return "waiting";
  if (status === "failed" || status === "interrupted" || status === "cancelled") return "failed";
  return "info";
}

function errorMessage(error: unknown): string | null {
  return error instanceof Error ? error.message : error ? "Action failed" : null;
}

export default function AutoResearchRoute() {
  const projects = useAutoResearchProjects();
  const nodes = useNodes();
  const [projectId, setProjectId] = useState("");
  const [tab, setTab] = useState<WorkspaceTab>("factory");
  const [dialogOpen, setDialogOpen] = useState(false);
  const [factory, setFactory] = useState<Factory>("idea");

  useEffect(() => {
    if (!projectId && projects.data?.[0]) setProjectId(projects.data[0].id);
  }, [projectId, projects.data]);

  const project = useAutoResearchProject(projectId || undefined);
  const ideas = useAutoResearchIdeas(projectId || undefined);
  const sources = useAutoResearchSources(projectId || undefined);
  const runs = useAutoResearchRuns(projectId || undefined);
  const paperFiles = useAutoResearchPaperFiles(projectId || undefined);
  const createRun = useCreateAutoResearchRun(projectId);
  const control = useControlAutoResearchRun(projectId);
  const updateIdea = useUpdateAutoResearchIdea(projectId);
  const updateProject = useUpdateAutoResearchProject();
  const latestRun = useMemo(() => runs.data?.[0], [runs.data]);
  const stream = useAutoResearchEvents(latestRun?.id, Boolean(latestRun));
  const usage = useMemo(() => summarizeAutoResearchUsage(stream.events), [stream.events]);
  const selectedIdea = ideas.data?.find((idea) => idea.selected);
  const persistedTopic = selectedIdea?.title ?? project.data?.idea_prompt ?? "";
  const [researchTopic, setResearchTopic] = useState("");

  useEffect(() => setResearchTopic(persistedTopic), [persistedTopic, projectId]);

  useEffect(() => {
    if (project.data?.status === "paper_editing" || project.data?.status === "completed") setTab("paper");
  }, [project.data?.status]);

  const runIsTerminal = !latestRun || TERMINAL.has(latestRun.state);
  const topicReadOnly = Boolean(latestRun && !TERMINAL.has(latestRun.state));
  const topicError = errorMessage(updateIdea.error ?? updateProject.error);
  const runError = errorMessage(createRun.error ?? control.error);

  const saveTopic = () => {
    const next = researchTopic.trim();
    if (!project.data || next === persistedTopic || topicReadOnly) return;
    if (selectedIdea) {
      if (!next) {
        setResearchTopic(persistedTopic);
        return;
      }
      updateIdea.mutate({
        ideaId: selectedIdea.id,
        version: selectedIdea.version,
        body: { title: next, body: selectedIdea.body },
      });
      return;
    }
    updateProject.mutate({
      id: project.data.id,
      version: project.data.version,
      body: { idea_prompt: next },
    });
  };

  const primaryAction = () => {
    if (!latestRun || TERMINAL.has(latestRun.state)) {
      createRun.mutate({ factory });
    } else if (latestRun.state === "running") {
      control.mutate({ runId: latestRun.id, action: "pause" });
    } else if (latestRun.state === "paused") {
      control.mutate({ runId: latestRun.id, action: "resume" });
    }
  };

  const primaryLabel = !latestRun || runIsTerminal
    ? "Start run"
    : latestRun.state === "running"
      ? "Pause run"
      : latestRun.state === "paused"
        ? "Resume run"
        : TRANSITIONAL_LABELS[latestRun.state] ?? latestRun.state;
  const primaryDisabled = !project.data || createRun.isPending || control.isPending ||
    Boolean(latestRun && !runIsTerminal && latestRun.state !== "running" && latestRun.state !== "paused");

  const hero = (
    <section className="arf-hero">
      <div>
        <div className="arf-eyebrow">Autonomous research workspace</div>
        <h1>AutoResearch Factory</h1>
        <p>Take a research question from topic radar to experiments, with every agent handoff visible.</p>
      </div>
      <div className="arf-hero-stats" aria-label="Run summary">
        <div className="arf-stat"><div className="arf-stat-label">Run</div><div className="arf-stat-value">{latestRun ? shortId(latestRun.id) : "—"}</div></div>
        <div className="arf-stat"><div className="arf-stat-label">Elapsed</div><div className="arf-stat-value">{latestRun ? duration(latestRun.started_at, latestRun.finished_at) : "—"}</div></div>
        <div className="arf-stat"><div className="arf-stat-label">Cost</div><div className="arf-stat-value">{usage.costUsd === null ? "—" : `$${usage.costUsd.toFixed(2)}`}</div></div>
      </div>
    </section>
  );

  if (projects.isPending) {
    return <div className="arf-canvas">{hero}<section className="arf-composer" aria-label="Start a research run"><div className="arf-composer-input"><label>Research topic</label><textarea disabled value="" readOnly /></div></section><div className="arf-workspace"><section className="arf-panel arf-empty-workspace">Loading AutoResearch projects…</section></div></div>;
  }

  if (projects.isError) {
    return <div className="arf-canvas">{hero}<section className="arf-panel arf-empty-workspace"><div><h2>AutoResearch Factory unavailable</h2><p className="arf-inline-error">{projects.error.message}</p></div></section></div>;
  }

  return (
    <main className="arf-canvas">
      <div className="arf-utility-row">
        <label className="arf-project-select">
          <span className="sr-only">Active project</span>
          <select value={projectId} onChange={(event) => setProjectId(event.target.value)}>
            <option value="">select project</option>
            {projects.data?.map((item) => <option key={item.id} value={item.id}>{item.name} · {item.status}</option>)}
          </select>
          <ChevronDown aria-hidden />
        </label>
        <button type="button" className="arf-utility-button" onClick={() => setDialogOpen(true)}><Plus aria-hidden /> New project</button>
        {project.data ? <span className="arf-status arf-mono text-[10px]" data-tone={statusTone(latestRun?.state ?? project.data.status)}>{latestRun?.state ?? project.data.status}</span> : null}
        <nav className="arf-mode-nav" aria-label="AutoResearch Factory workspace">
          {(Object.keys(TAB_LABELS) as WorkspaceTab[]).map((value) => (
            <button key={value} type="button" className="arf-mode-button" aria-current={tab === value ? "page" : undefined} onClick={() => setTab(value)}>
              {TAB_LABELS[value]}
            </button>
          ))}
        </nav>
      </div>

      {hero}

      <section className="arf-composer" aria-label="Start a research run">
        <div className="arf-composer-input">
          <label htmlFor="arf-topic">Research topic</label>
          <textarea
            id="arf-topic"
            value={researchTopic}
            disabled={!project.data}
            readOnly={topicReadOnly}
            placeholder={project.data ? "Awaiting an idea or generated candidate" : "Select or create a project"}
            onChange={(event) => setResearchTopic(event.target.value)}
            onBlur={saveTopic}
          />
          {topicError ? <p className="arf-inline-error" role="alert">{topicError}</p> : null}
        </div>
        <div className="arf-composer-controls">
          <div className="arf-select-wrap">
            <label htmlFor="arf-entry">Start at</label>
            <select id="arf-entry" value={factory} disabled={!project.data || !runIsTerminal} onChange={(event) => setFactory(event.target.value as Factory)}>
              <option value="idea">Idea factory</option>
              <option value="proposal">Proposal factory</option>
              <option value="experiment">Experiment factory</option>
              <option value="deep_lit">Deep literature</option>
            </select>
          </div>
          <button type="button" className={cn("arf-run-button", latestRun?.state === "running" && "arf-running")} disabled={primaryDisabled} onClick={primaryAction}>
            {primaryLabel}
          </button>
        </div>
      </section>

      {!projectId || !project.data ? (
        <div className="arf-workspace">
          <section className="arf-panel arf-empty-workspace">
            <div>
              <h2>No research project selected</h2>
              <p>Create a project with a direct idea, or begin with bounded candidate generation and verified sources.</p>
              <button type="button" className="arf-utility-button" onClick={() => setDialogOpen(true)}><Plus aria-hidden /> New project</button>
            </div>
          </section>
        </div>
      ) : (
        <>
          {tab === "factory" ? <div className="arf-workspace">
            <WorkflowGraph active={stream.activeInvocations} paused={latestRun?.state === "paused"} />
            <GenerationStream
              run={latestRun}
              events={stream.events}
              active={stream.activeInvocations}
              usage={usage}
              reconnecting={stream.reconnecting}
              streamError={stream.error}
              controlPending={control.isPending}
              onStop={() => latestRun && control.mutate({ runId: latestRun.id, action: "stop" })}
            />
          </div> : null}
          {tab === "ideas" ? <div className="arf-mode-frame"><IdeaWorkspace projectId={projectId} projectPrompt={project.data.idea_prompt} ideas={ideas.data ?? []} sources={sources.data ?? []} /></div> : null}
          {tab === "paper" ? <div className="arf-mode-frame"><PaperStudio projectId={projectId} files={paperFiles.data ?? []} runs={runs.data ?? []} /></div> : null}
          {tab === "settings" ? <div className="arf-mode-frame"><RoleControls project={project.data} /></div> : null}
          {runError ? <p className="arf-run-error" role="alert">{runError}</p> : null}
          {latestRun && TERMINAL.has(latestRun.state) && latestRun.state !== "succeeded" ? <div className="arf-run-error"><Settings2 className="inline h-3.5 w-3.5" aria-hidden /> {latestRun.error_message ?? `run ${latestRun.state}`}</div> : null}
        </>
      )}

      <NewAutoResearchProjectDialog
        open={dialogOpen}
        onOpenChange={setDialogOpen}
        nodes={nodes.data ?? []}
        onCreated={(created) => {
          setProjectId(created.id);
          setTab(created.status === "awaiting_idea_selection" ? "ideas" : "factory");
        }}
      />
    </main>
  );
}
