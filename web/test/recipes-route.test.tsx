/// <reference types="@testing-library/jest-dom" />
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { MemoryRouter, Route, Routes } from "react-router";
import { describe, expect, it } from "vitest";

import RecipesRoute from "~/routes/library/recipes/index";
import RecipeDetailRoute from "~/routes/library/recipes/$id";
import { server } from "../msw/server";

const alphaDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
const betaDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";
const gammaDigest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc";
const alphaRepositoryId = "repo-alpha";

const recipes = [
  {
    digest: alphaDigest,
    name: "alpha-recipe",
    display_name: "Alpha Model",
    model: "Alpha 27B",
    engine: "vllm",
    version: "2.0.0",
    version_count: 2,
    description: "A verified two-node recipe.",
    license: "MIT",
    trust_state: "verified",
    source: { type: "catalog", remote: "https://catalog.example/alpha" },
    compatibility: { nodeCount: 2, fabric: { transport: "roce" } },
    installed_at: "2026-01-03T00:00:00Z",
    update: {
      state: "available",
      repository_id: alphaRepositoryId,
      remote: "https://github.com/MiaAI-Lab/alpha",
      tracking_ref: "main",
      path: ".",
      installed_revision: "1111111111111111111111111111111111111111",
      candidate_revision: "2222222222222222222222222222222222222222",
      checked_at: "2026-01-04T00:00:00Z",
    },
  },
  {
    digest: betaDigest,
    name: "beta-recipe",
    display_name: "Beta Model",
    version: "1.0.0",
    description: "A local single-node recipe.",
    license: "Apache-2.0",
    trust_state: "local",
    source: { type: "git", remote: "https://git.example/beta" },
    compatibility: { nodeCount: 1 },
    installed_at: "2026-01-01T00:00:00Z",
  },
  {
    digest: gammaDigest,
    name: "gamma-recipe",
    display_name: "Gamma Model",
    version: "1.5.0",
    description: "An untrusted local recipe.",
    license: "BSD-3-Clause",
    trust_state: "untrusted",
    source: { type: "local", remote: "/srv/gamma" },
    compatibility: { nodeCount: 2 },
    installed_at: "2026-01-02T00:00:00Z",
  },
] as const;

const repositories = recipes.map((recipe, index) => ({
  id: index === 0 ? alphaRepositoryId : `repo-${recipe.name}`,
  source_url: `https://github.com/MiaAI-Lab/${recipe.name}`,
  source_path: ".",
  tracking_ref: "main",
  current_recipe: recipe,
  installed_commit: index === 0 ? "1111111111111111111111111111111111111111" : `${index + 3}`.repeat(40),
  observed_head_commit: index === 0 ? "2222222222222222222222222222222222222222" : `${index + 3}`.repeat(40),
  head_checked_at: "2026-01-04T00:00:00Z",
  update_available: index === 0,
  update_supported: index < 2,
  versions: [{
    recipe,
    commit_sha: index === 0 ? "1111111111111111111111111111111111111111" : `${index + 3}`.repeat(40),
    canonical: true,
    installed_at: recipe.installed_at,
  }],
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-04T00:00:00Z",
}));

const deployments = [
  {
    id: "11111111-1111-4111-8111-111111111111",
    recipe_digest: alphaDigest,
    profile: "",
    placements: [
      { node_id: "node-1", node_name: "spark1", rank: 0 },
      { node_id: "node-2", node_name: "spark2", rank: 1 },
    ],
    desired_state: "running",
    observed_state: "healthy",
  },
];

const affectedHardware = [
  { node_id: "node-1", node_name: "spark1", node_status: "online", deployment_ids: [deployments[0].id], state: "healthy" },
  { node_id: "node-2", node_name: "spark2", node_status: "online", deployment_ids: [deployments[0].id], state: "healthy" },
];

const updateTargets = affectedHardware.map((hardware, rank) => ({
  source_deployment_id: hardware.deployment_ids[0],
  node_id: hardware.node_id,
  node_name: hardware.node_name,
  node_status: hardware.node_status,
  rank,
  status: "pending",
  phase: "fetching",
  current_step: 0,
  total_steps: 5,
}));

function renderCatalog() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/library/recipes"]}>
        <RecipesRoute />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function renderRecipeDetail() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[`/library/recipes/${alphaDigest}`]}>
        <Routes>
          <Route path="/library/recipes/:id" element={<RecipeDetailRoute />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function installCatalogHandlers(rows: readonly unknown[] = repositories) {
  server.use(
    http.get("*/api/v1/recipe-repositories", () => HttpResponse.json(rows)),
    http.get(`*/api/v1/recipe-repositories/${alphaRepositoryId}`, () =>
      HttpResponse.json({ ...repositories[0], affected_hardware: affectedHardware }),
    ),
    http.get("*/api/v1/recipes", () => HttpResponse.json(recipes)),
    http.get(`*/api/v1/recipes/${alphaDigest}`, () =>
      HttpResponse.json({ ...recipes[0], manifest: { artifacts: [], workloads: [] } }),
    ),
    http.get("*/api/v1/deployments", () => HttpResponse.json(deployments)),
    http.get("*/api/v1/nodes", () => HttpResponse.json([])),
    http.get("*/api/v1/fabrics", () => HttpResponse.json([])),
    http.post("*/api/v1/recipes/check-updates", () =>
      HttpResponse.json([recipes[0].update]),
    ),
    http.post(`*/api/v1/recipe-repositories/${alphaRepositoryId}/update/plan`, () =>
      HttpResponse.json({ plan_digest: "sha256:update-plan", ready: true, targets: updateTargets, diagnostics: [] }),
    ),
  );
}

describe("RecipesRoute repository catalog", () => {
  it("renders one repository card and preserves the full-card hardware action", async () => {
    installCatalogHandlers();
    const user = userEvent.setup();
    renderCatalog();

    expect(await screen.findByRole("heading", { name: "Recipe catalog" })).toBeInTheDocument();
    expect(await screen.findByRole("heading", { name: "All recipes" })).toBeInTheDocument();

    const alphaCard = screen.getByRole("button", {
      name: "Choose installation hardware for Alpha Model",
    });
    expect(within(alphaCard).getByText("Alpha Model")).toBeInTheDocument();
    expect(within(alphaCard).getByText("A verified two-node recipe.")).toBeInTheDocument();
    expect(within(alphaCard).getByText("2 nodes · RDMA fabric")).toBeInTheDocument();
    expect(within(alphaCard).getByText("Installed on 2 devices")).toBeInTheDocument();
    expect(within(alphaCard).getByText("Choose installation hardware →")).toBeInTheDocument();
    expect(within(alphaCard).getByText(/2\.0\.0 · sha256:aaaaa… · MIT/)).toBeInTheDocument();
    expect(within(alphaCard).getByLabelText("git source")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Update hardware using this recipe" })).toBeInTheDocument();

    expect(screen.getAllByRole("button", { name: /Choose installation hardware for/ })).toHaveLength(3);
    await user.type(screen.getByRole("searchbox", { name: "Search recipes" }), gammaDigest);
    expect(screen.getByRole("button", { name: "Choose installation hardware for Gamma Model" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Choose installation hardware for Alpha Model" })).not.toBeInTheDocument();
  });

  it("opens the hardware chooser from the unchanged card button", async () => {
    installCatalogHandlers();
    const user = userEvent.setup();
    renderCatalog();
    await user.click(await screen.findByRole("button", { name: "Choose installation hardware for Alpha Model" }));
    expect(await screen.findByRole("heading", { name: "Choose hardware" })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByLabelText("Recipe")).toHaveValue(alphaDigest));
  });

  it("shows exact hardware and advances persisted update progress to completion", async () => {
    installCatalogHandlers();
    let runReads = 0;
    server.use(
      http.post(`*/api/v1/recipe-repositories/${alphaRepositoryId}/update`, () =>
        HttpResponse.json({ run_id: "run-update" }, { status: 202 }),
      ),
      http.get("*/api/v1/runs/run-update", () => {
        runReads += 1;
        if (runReads === 1) {
          return HttpResponse.json({
            id: "run-update", module: "library", kind: "recipe-update", state: "running",
            progress: {
              phase: "pulling", total_hardware: 2, completed_hardware: 0,
              hardware: updateTargets.map((target) => ({ ...target, status: "running", phase: "pulling", current_step: 3 })),
            },
          });
        }
        return HttpResponse.json({
          id: "run-update", module: "library", kind: "recipe-update", state: "succeeded",
          progress: {
            phase: "ready", total_hardware: 2, completed_hardware: 2,
            hardware: updateTargets.map((target) => ({ ...target, status: "succeeded", phase: "ready", current_step: 5 })),
          },
        });
      }),
    );
    const user = userEvent.setup();
    renderCatalog();
    await user.click(await screen.findByRole("button", { name: "Update hardware using this recipe" }));
    expect(await screen.findByText("spark1")).toBeInTheDocument();
    expect(screen.getByText("spark2")).toBeInTheDocument();
    await user.click(await screen.findByRole("button", { name: "Update this hardware" }));
    expect((await screen.findAllByText("pulling")).length).toBeGreaterThan(0);
    expect(await screen.findByRole("heading", { name: "Update complete" }, { timeout: 3_000 })).toBeInTheDocument();
    expect(screen.getByText("2 of 2 hardware targets complete")).toBeInTheDocument();
  });

  it("shows hardware-specific failure and rollback instead of success", async () => {
    installCatalogHandlers();
    server.use(
      http.post(`*/api/v1/recipe-repositories/${alphaRepositoryId}/update`, () =>
        HttpResponse.json({ run_id: "run-failed" }, { status: 202 }),
      ),
      http.get("*/api/v1/runs/run-failed", () =>
        HttpResponse.json({
          id: "run-failed", module: "library", kind: "recipe-update", state: "failed",
          error_message: "replacement failed; source restored",
          progress: {
            phase: "restored", total_hardware: 2, completed_hardware: 0,
            hardware: [
              { ...updateTargets[0], status: "failed", phase: "restored", error_code: "workload.start_failed", error_message: "container exited" },
              { ...updateTargets[1], status: "failed", phase: "restored" },
            ],
          },
        }),
      ),
    );
    const user = userEvent.setup();
    renderCatalog();
    await user.click(await screen.findByRole("button", { name: "Update hardware using this recipe" }));
    await user.click(await screen.findByRole("button", { name: "Update this hardware" }));
    expect(await screen.findByRole("heading", { name: "Update failed" })).toBeInTheDocument();
    expect(screen.getByText("workload.start_failed: container exited")).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Update complete" })).not.toBeInTheDocument();
  });

  it("refreshes cached update status and removes the Recipe Builder update action", async () => {
    installCatalogHandlers();
    let checks = 0;
    server.use(
      http.post("*/api/v1/recipes/check-updates", () => {
        checks += 1;
        return HttpResponse.json([recipes[0].update]);
      }),
    );
    const user = userEvent.setup();
    renderCatalog();
    await user.click(await screen.findByRole("button", { name: "Check updates" }));
    await waitFor(() => expect(checks).toBe(1));

    renderRecipeDetail();
    expect(await screen.findByText("Update available")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Inspect update in builder" })).not.toBeInTheDocument();
  });

  it("distinguishes loading, API-empty, and retryable error states", async () => {
    let release: (() => void) | undefined;
    server.use(
      http.get("*/api/v1/recipe-repositories", async () => {
        await new Promise<void>((resolve) => { release = resolve; });
        return HttpResponse.json([]);
      }),
      http.get("*/api/v1/deployments", () => HttpResponse.json([])),
      http.get("*/api/v1/recipes", () => HttpResponse.json([])),
      http.get("*/api/v1/nodes", () => HttpResponse.json([])),
      http.get("*/api/v1/fabrics", () => HttpResponse.json([])),
    );
    const loadingView = renderCatalog();
    expect(await screen.findByText("Loading recipes")).toBeInTheDocument();
    release?.();
    expect(await screen.findByText("No recipes installed")).toBeInTheDocument();
    loadingView.unmount();

    let attempts = 0;
    server.use(
      http.get("*/api/v1/recipe-repositories", () => {
        attempts += 1;
        return attempts === 1
          ? HttpResponse.json({ code: "test.failure", message: "Catalog unavailable" }, { status: 503 })
          : HttpResponse.json([]);
      }),
      http.get("*/api/v1/deployments", () => HttpResponse.json([])),
      http.get("*/api/v1/recipes", () => HttpResponse.json([])),
      http.get("*/api/v1/nodes", () => HttpResponse.json([])),
      http.get("*/api/v1/fabrics", () => HttpResponse.json([])),
    );
    const user = userEvent.setup();
    renderCatalog();
    expect(await screen.findByText("Cannot load recipes")).toBeInTheDocument();
    expect(screen.getByText("Catalog unavailable")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("No recipes installed")).toBeInTheDocument();
  });
});
