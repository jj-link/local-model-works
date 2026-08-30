/// <reference types="@testing-library/jest-dom" />
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { MemoryRouter } from "react-router";
import { beforeEach, describe, expect, it } from "vitest";
import AutoResearchRoute from "~/routes/autoresearch";
import { qk } from "~/lib/queries";
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
  const rendered = render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/autoresearch"]}><AutoResearchRoute /></MemoryRouter>
    </QueryClientProvider>,
  );
  return { ...rendered, client };
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
    expect(within(composer).getByRole("button", { name: "Start research" })).toBeDisabled();
    expect(within(workspace).getByLabelText("Live Agon workflow")).toBeVisible();
    expect(within(workspace).getByLabelText("Generation stream")).toBeVisible();
    expect(within(workspace).getByRole("heading", { name: "Research topology" })).toBeVisible();
    expect(within(workspace).getByRole("heading", { name: "Generation stream" })).toBeVisible();
    expect(within(workspace).getByRole("button", { name: /Idea creator.*waiting/i })).toBeVisible();
    expect(workspace).not.toHaveTextContent(/QWEN-3\.5|\$1\.84|#018/);
    for (const mode of ["Paper studio", "Role controls"]) {
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
    expect(screen.getByLabelText("Research question")).toBeEnabled();
    expect(screen.getByLabelText("Project name")).toBeEnabled();
    expect(screen.getByRole("button", { name: "Create" })).toBeDisabled();
    expect(screen.queryByLabelText("Runner node")).not.toBeInTheDocument();
  });

  it("creates an unnamed project from a required research question", async () => {
    const created = {
      ...project,
      id: "40000000-0000-4000-8000-000000000099",
      name: "",
      status: "idea_intake",
      idea_prompt: "Can sparse latent world models improve long-horizon planning?",
      version: 1,
    };
    let projectsList: typeof project[] = [];
    let requestBody: unknown;
    server.use(
      http.get("*/api/v1/autoresearch/projects", () => HttpResponse.json(projectsList)),
      http.get("*/api/v1/nodes", () => HttpResponse.json([])),
      http.post("*/api/v1/autoresearch/projects", async ({ request }) => {
        requestBody = await request.json();
        projectsList = [created];
        return HttpResponse.json(created, { status: 201 });
      }),
    );
    const user = userEvent.setup();
    renderRoute();
    await user.click((await screen.findAllByRole("button", { name: "New project" }))[0]);
    await user.type(screen.getByLabelText("Research question"), created.idea_prompt);
    await user.click(screen.getByRole("button", { name: "Create" }));

    await waitFor(() => expect(requestBody).toEqual({ idea_prompt: created.idea_prompt }));
    expect(await screen.findByRole("option", { name: /Untitled · 40000000… · idea_intake/ })).toBeVisible();
    expect(screen.queryByLabelText("Start at")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Start research" })).toBeEnabled();
  });

  it("shows real active-run summary, topic, model, usage, and pause control", async () => {
    const user = userEvent.setup();
    installProject("running");
    renderRoute();
    expect(await screen.findByDisplayValue(idea.title)).toBeVisible();
    expect(screen.getByText("10000000…")).toBeVisible();
    expect(screen.getByText("$0.42")).toBeVisible();
    expect(screen.getByText("1,250")).toBeVisible();
    expect(screen.getByRole("button", { name: "Pause run" })).toBeEnabled();
    expect(screen.getByRole("button", { name: /Experiment coder — real-coder-model — generating/i })).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Paper studio" }));
    expect(screen.getByRole("button", { name: /compile/i })).toBeDisabled();
  });

  it("shows resume for paused runs", async () => {
    installProject("paused");
    renderRoute();
    expect(await screen.findByRole("button", { name: "Resume run" })).toBeEnabled();
  });

  it("continues research without exposing a factory selector", async () => {
    installProject("succeeded");
    let requestBody: unknown;
    server.use(http.post("*/api/v1/autoresearch/projects/:projectId/runs", async ({ request }) => {
      requestBody = await request.json();
      return HttpResponse.json({ id: "new-run", state: "queued" });
    }));
    const user = userEvent.setup();
    renderRoute();
    expect(await screen.findByRole("button", { name: "Continue research" })).toBeEnabled();
    expect(screen.queryByLabelText("Start at")).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Continue research" }));
    expect(requestBody).toEqual({});
  });

  it("keeps alternative idea generation available on demand", async () => {
    server.use(http.get("*/api/v1/nodes", () => HttpResponse.json([])));
    const user = userEvent.setup();
    renderRoute();
    expect(screen.queryByRole("button", { name: "Ideas & sources" })).not.toBeInTheDocument();
    await user.click(await screen.findByRole("button", { name: "Generate alternatives" }));
    expect(await screen.findByRole("heading", { name: "alternative ideas & sources" })).toBeVisible();
    expect(screen.getByLabelText("candidate title")).toHaveValue("Candidate one");
    expect(screen.getByText("license acceptance required")).toBeVisible();
    expect(screen.getByRole("button", { name: /continue with selected idea/i })).toBeDisabled();
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

  it("queues writer chat immediately and reports the queued state", async () => {
    let requestBody: unknown;
    server.use(http.post("*/api/v1/autoresearch/projects/:projectId/paper/chat", async ({ request }) => {
      requestBody = await request.json();
      return HttpResponse.json({
        id: "writer-run",
        module: "autoresearch",
        kind: "autoresearch-paper-edit",
        state: "queued",
        resources: { nodes: [], accelerators: [], fabrics: [] },
        input: {},
        created_at: now,
      }, { status: 202 });
    }));
    const user = userEvent.setup();
    renderRoute();
    await user.selectOptions(await screen.findByLabelText("Active project"), "00000000-0000-4000-8000-000000000005");
    await user.click(await screen.findByRole("button", { name: "Paper studio" }));
    const message = "Tighten the limitation paragraph.";
    await user.type(await screen.findByPlaceholderText(/Ask the paper writer/i), message);
    await user.click(screen.getByRole("button", { name: /apply writer edit/i }));
    await waitFor(() => expect(requestBody).toMatchObject({ message }));
    expect(await screen.findByText("Writer edit queued")).toBeVisible();
    expect(screen.getByPlaceholderText(/Ask the paper writer/i)).toHaveValue("");
  });

  it("refreshes project, ideas, and paper queries when the latest run becomes terminal", async () => {
    const activeRun = installProject("running");
    const calls = { projects: 0, project: 0, ideas: 0, paper: 0 };
    server.use(
      http.get("*/api/v1/autoresearch/projects", () => {
        calls.projects += 1;
        return HttpResponse.json([project]);
      }),
      http.get("*/api/v1/autoresearch/projects/:projectId", () => {
        calls.project += 1;
        return HttpResponse.json(project);
      }),
      http.get("*/api/v1/autoresearch/projects/:projectId/ideas", () => {
        calls.ideas += 1;
        return HttpResponse.json([idea]);
      }),
      http.get("*/api/v1/autoresearch/projects/:projectId/paper/files", () => {
        calls.paper += 1;
        return HttpResponse.json([]);
      }),
    );
    const { client } = renderRoute();
    await screen.findByRole("button", { name: "Pause run" });
    await waitFor(() => expect(Object.values(calls).every((count) => count > 0)).toBe(true));
    const before = { ...calls };
    act(() => {
      client.setQueryData(qk.autoResearchRuns(projectId), [{ ...activeRun, state: "succeeded", finished_at: now }]);
    });
    await waitFor(() => {
      expect(calls.projects).toBeGreaterThan(before.projects);
      expect(calls.project).toBeGreaterThan(before.project);
      expect(calls.ideas).toBeGreaterThan(before.ideas);
      expect(calls.paper).toBeGreaterThan(before.paper);
    });
  });
});
