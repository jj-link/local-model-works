/// <reference types="@testing-library/jest-dom" />
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { MemoryRouter } from "react-router";
import { beforeEach, describe, expect, it } from "vitest";
import AutoResearchRoute from "~/routes/autoresearch";
import { server } from "../msw/server";

const projectId = "00000000-0000-4000-8000-000000000099";
const runId = "10000000-0000-4000-8000-000000000099";
const now = "2026-08-23T12:10:00Z";
const deploymentId = "30000000-0000-4000-8000-000000000099";
const deployment = {
  id: deploymentId,
  desired_state: "running",
  observed_state: "healthy",
  profile: "spark",
  recipe_digest: "sha256:routing",
  recipe_name: "Spark local",
  endpoint: { host: "127.0.0.1", port: 8888, model: "local-routing-model" },
};
const moduleSettings = {
  module: "autoresearch",
  version: "settings-v1",
  settings: {
    default_role_assignments: {
      default: { source: "lmw", deployment_id: deploymentId, model: "local-routing-model" },
    },
    external_providers: [
      { name: "Codex cloud", backend: "codex", model: "gpt-routing", secret_name: "provider-key" },
    ],
  },
};
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


  beforeEach(() => {
    server.use(
      http.get("*/api/v1/deployments", () => HttpResponse.json([deployment])),
      http.get("*/api/v1/modules/autoresearch/settings", () => HttpResponse.json(moduleSettings)),
      http.get("*/api/v1/secrets", () => HttpResponse.json([{ id: "secret-1", name: "provider-key", purpose: "model_provider" }])),
    );
  });
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

  it("assigns a graph role from the anchored model popover", async () => {
    installProject("running");
    let requestBody: Record<string, unknown> | undefined;
    server.use(http.put("*/api/v1/autoresearch/projects/:projectId", async ({ request }) => {
      requestBody = await request.json() as Record<string, unknown>;
      return HttpResponse.json({ ...project, version: 3, ...requestBody });
    }));
    const user = userEvent.setup();
    renderRoute();

    await user.click(await screen.findByRole("button", { name: /Idea creator.*configure model/i }));
    const popover = screen.getByRole("dialog", { name: "Model assignment for Idea creator" });
    expect(within(popover).getByText(/Effective: local-routing-model · module default/i)).toBeVisible();
    await user.selectOptions(within(popover).getByLabelText("idea-creator primary provider"), screen.getByRole("option", { name: "local-routing-model · Spark local" }));
    await user.click(within(popover).getByRole("checkbox", { name: "Inherit project fallback chain" }));
    await user.click(within(popover).getByRole("button", { name: "Add fallback" }));
    await user.selectOptions(within(popover).getByLabelText("idea-creator fallback 1"), `lmw|${deploymentId}|local-routing-model`);
    expect(within(popover).getByRole("alert")).toHaveTextContent("Primary provider cannot also be a fallback.");
    expect(within(popover).getByRole("button", { name: "Save" })).toBeDisabled();
    await user.selectOptions(within(popover).getByLabelText("idea-creator fallback 1"), "external|codex|gpt-routing||provider-key");
    await user.click(within(popover).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(requestBody).toBeDefined());
    expect(requestBody).toMatchObject({
      config: {
        roles: {
          "idea-creator": { source: "lmw", deployment_id: deploymentId, model: "local-routing-model" },
        },
        fallbacks: {
          "idea-creator": [{ source: "external", backend: "codex", model: "gpt-routing", secret_name: "provider-key" }],
        },
      },
    });

    await user.click(within(popover).getByRole("button", { name: "Close model assignment" }));
    await user.click(screen.getByRole("button", { name: /Idea deep literature.*configure model/i }));
    expect(screen.getByText("Shared assignment: this role appears at 5 topology points.")).toBeVisible();
  });

  it("bulk assigns canonical roles and preserves advisor and paper controls", async () => {
    installProject("succeeded");
    const requests: Array<Record<string, unknown>> = [];
    server.use(http.put("*/api/v1/autoresearch/projects/:projectId", async ({ request }) => {
      const body = await request.json() as Record<string, unknown>;
      requests.push(body);
      return HttpResponse.json({ ...project, version: project.version + requests.length, ...body });
    }));
    const user = userEvent.setup();
    renderRoute();
    await user.click(await screen.findByRole("button", { name: "Role controls" }));

    await user.click(screen.getByRole("checkbox", { name: "Select idea-creator" }));
    await user.click(screen.getByRole("checkbox", { name: "Select proposal-refiner" }));
    await user.selectOptions(screen.getByLabelText("Bulk provider assignment"), "external|codex|gpt-routing||provider-key");
    await user.click(screen.getByRole("button", { name: "Apply to selected" }));
    await user.click(screen.getByRole("button", { name: "save model routing" }));
    await waitFor(() => expect(requests).toHaveLength(1));
    expect(requests[0]).toMatchObject({
      config: {
        roles: {
          "idea-creator": { source: "external", backend: "codex", model: "gpt-routing", secret_name: "provider-key" },
          "proposal-refiner": { source: "external", backend: "codex", model: "gpt-routing", secret_name: "provider-key" },
        },
      },
    });

    await user.click(screen.getByRole("button", { name: "Advisors" }));
    await user.click(screen.getByRole("checkbox", { name: "idea-creator" }));
    await user.selectOptions(screen.getByLabelText("idea-creator advisor backlog"), "3");
    await user.click(screen.getByRole("button", { name: "save advisor settings" }));
    await waitFor(() => expect(requests).toHaveLength(2));
    expect(requests[1]).toMatchObject({ config: { advisors: { "idea-creator": { enabled: true, backlog: 3 } } } });

    await user.click(screen.getByRole("button", { name: "Paper policy" }));
    await user.click(screen.getByLabelText("Paper round cap"));
    await user.keyboard("{Control>}a{/Control}7");
    await user.click(screen.getByRole("button", { name: "save paper policy" }));
    await waitFor(() => expect(requests).toHaveLength(3));
    expect(requests[2]).toMatchObject({ config: { paper_max_rounds: 7 } });
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
