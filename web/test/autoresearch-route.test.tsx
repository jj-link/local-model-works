/// <reference types="@testing-library/jest-dom" />
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { MemoryRouter } from "react-router";
import { describe, expect, it } from "vitest";
import AutoResearchRoute from "~/routes/autoresearch";
import { server } from "../msw/server";

const projectId = "00000000-0000-4000-8000-000000000099";
const runId = "10000000-0000-4000-8000-000000000099";
const now = "2026-08-23T12:10:00Z";
const project = {
  id: projectId,
  name: "Sparse world models",
  status: "running",
  idea_prompt: "Initial prompt",
  config: { candidate_count: 1, paper_max_rounds: 5, roles: {}, advisors: {}, fallbacks: {}, auditor_prompts: {}, human_gates: {} },
  version: 2,
  created_at: now,
  updated_at: now,
};
const idea = {
  id: "20000000-0000-4000-8000-000000000099",
  project_id: projectId,
  ordinal: 1,
  source: "human",
  title: "Sparse latent world models for long-horizon planning",
  body: "Evaluate sparse latent state transitions.",
  selected: true,
  version: 1,
  created_at: now,
  updated_at: now,
};

function renderRoute() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/autoresearch"]}><AutoResearchRoute /></MemoryRouter>
    </QueryClientProvider>,
  );
}

function installProject(state: "running" | "paused" | "succeeded" = "running") {
  const run = {
    id: runId,
    module: "autoresearch",
    kind: "autoresearch-factory",
    state,
    resources: { nodes: [], accelerators: [], fabrics: [] },
    input: {},
    created_at: "2026-08-23T12:00:00Z",
    started_at: "2026-08-23T12:00:00Z",
    finished_at: state === "succeeded" ? now : undefined,
  };
  server.use(
    http.get("*/api/v1/nodes", () => HttpResponse.json([])),
    http.get("*/api/v1/autoresearch/projects", () => HttpResponse.json([project])),
    http.get("*/api/v1/autoresearch/projects/:projectId", () => HttpResponse.json(project)),
    http.get("*/api/v1/autoresearch/projects/:projectId/ideas", () => HttpResponse.json([idea])),
    http.get("*/api/v1/autoresearch/projects/:projectId/sources", () => HttpResponse.json([])),
    http.get("*/api/v1/autoresearch/projects/:projectId/runs", () => HttpResponse.json([run])),
    http.get("*/api/v1/autoresearch/projects/:projectId/paper/files", () => HttpResponse.json([])),
  );
  sessionStorage.setItem(`autoresearch.events.${runId}`, JSON.stringify({
    lastEventId: "usage-2",
    events: [
      { version: 1, event_id: "start-1", run_id: runId, invocation_id: "inv-1", node_id: "experiment.coder", timestamp: now, type: "agent.started", payload: { role: "experiment-coder", backend: "codex", model: "real-coder-model" } },
      { version: 1, event_id: "usage-1", run_id: runId, invocation_id: "inv-1", node_id: "experiment.coder", timestamp: now, type: "agent.usage", payload: { input_tokens: 1000, output_tokens: 250, cost_usd: 0.42, output_rate: 25, context_percent: 40 } },
      { version: 1, event_id: "usage-2", run_id: runId, invocation_id: "inv-1", node_id: "experiment.coder", timestamp: now, type: "agent.usage", payload: { input_tokens: 1000, output_tokens: 250, cost_usd: 0.42, output_rate: 25, context_percent: 40 } },
    ],
  }));
  return run;
}

describe("AutoResearchRoute", () => {
  it("keeps the approved hierarchy in the empty state and opens project creation", async () => {
    server.use(
      http.get("*/api/v1/autoresearch/projects", () => HttpResponse.json([])),
      http.get("*/api/v1/nodes", () => HttpResponse.json([])),
    );
    const user = userEvent.setup();
    const { container } = renderRoute();
    expect(await screen.findByText("No research project selected")).toBeVisible();
    const canvas = container.querySelector<HTMLElement>(".arf-canvas");
    const hero = container.querySelector<HTMLElement>(".arf-hero");
    const composer = container.querySelector<HTMLElement>(".arf-composer");
    const workspace = container.querySelector<HTMLElement>(".arf-workspace");
    if (!canvas || !hero || !composer || !workspace) throw new Error("Factory hierarchy did not render");
    expect(canvas).toContainElement(hero);
    expect(hero.compareDocumentPosition(composer) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(composer.compareDocumentPosition(workspace) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(within(composer).getByRole("button", { name: "Start run" })).toBeDisabled();
    expect(within(workspace).getByLabelText("Live Agon workflow")).toBeVisible();
    expect(within(workspace).getByLabelText("Generation stream")).toBeVisible();
    expect(within(workspace).getByRole("heading", { name: "Research topology" })).toBeVisible();
    expect(within(workspace).getByRole("heading", { name: "Generation stream" })).toBeVisible();
    expect(within(workspace).getByRole("button", { name: /Idea creator.*waiting/i })).toBeVisible();
    expect(workspace).not.toHaveTextContent(/QWEN-3\.5|\$1\.84|#018/);
    for (const mode of ["Ideas & sources", "Paper studio", "Role controls"]) {
      await user.click(screen.getByRole("button", { name: mode }));
      expect(screen.getByRole("heading", { name: mode })).toBeVisible();
      expect(screen.getByText(`Select or create a research project to use ${mode}.`)).toBeVisible();
      expect(screen.getByRole("button", { name: mode })).toHaveAttribute("aria-current", "page");
    }
    await user.click(screen.getByRole("button", { name: "Factory" }));
    expect(screen.getByLabelText("Live Agon workflow")).toBeVisible();
    expect(screen.getByLabelText("Generation stream")).toBeVisible();
    await user.click(screen.getAllByRole("button", { name: "New project" })[0]);
    expect(screen.getByRole("dialog", { name: "New AutoResearch project" })).toBeVisible();
    expect(screen.getByLabelText("Project name")).toBeEnabled();
  });

  it("shows real active-run summary, topic, model, usage, and pause control", async () => {
    installProject("running");
    renderRoute();
    expect(await screen.findByDisplayValue(idea.title)).toBeVisible();
    expect(screen.getByText("10000000…")).toBeVisible();
    expect(screen.getByText("$0.42")).toBeVisible();
    expect(screen.getByText("1,250")).toBeVisible();
    expect(screen.getByRole("button", { name: "Pause run" })).toBeEnabled();
    expect(screen.getByRole("button", { name: /Experiment coder — real-coder-model — generating/i })).toBeVisible();
  });

  it("shows resume for paused runs", async () => {
    installProject("paused");
    renderRoute();
    expect(await screen.findByRole("button", { name: "Resume run" })).toBeEnabled();
  });

  it("starts the selected real factory", async () => {
    installProject("succeeded");
    let requestBody: unknown;
    server.use(http.post("*/api/v1/autoresearch/projects/:projectId/runs", async ({ request }) => {
      requestBody = await request.json();
      return HttpResponse.json({ id: "new-run", state: "queued" });
    }));
    const user = userEvent.setup();
    renderRoute();
    await user.selectOptions(await screen.findByLabelText("Start at"), "experiment");
    await user.click(screen.getByRole("button", { name: "Start run" }));
    expect(requestBody).toEqual({ factory: "experiment" });
  });

  it("keeps candidate editing reachable from Ideas and sources", async () => {
    server.use(http.get("*/api/v1/nodes", () => HttpResponse.json([])));
    const user = userEvent.setup();
    renderRoute();
    await user.click(await screen.findByRole("button", { name: "Ideas & sources" }));
    expect(await screen.findByRole("tab", { name: /01/ })).toBeVisible();
    expect(screen.getByLabelText("candidate title")).toHaveValue("Candidate one");
    expect(screen.getByText("license acceptance required")).toBeVisible();
    expect(screen.getByRole("button", { name: /continue selected/i })).toBeDisabled();
  });

  it("keeps findings and release reachable in Paper studio", async () => {
    server.use(http.get("*/api/v1/nodes", () => HttpResponse.json([])));
    const user = userEvent.setup();
    renderRoute();
    const projectSelect = await screen.findByLabelText("Active project");
    await user.selectOptions(projectSelect, "00000000-0000-4000-8000-000000000005");
    await user.click(await screen.findByRole("button", { name: "Paper studio" }));
    expect(await screen.findByText("REV-001")).toBeVisible();
    expect(screen.getByText("Tighten the limitation.")).toBeVisible();
    expect(screen.getByRole("button", { name: /release/i })).toBeEnabled();
  });
});
