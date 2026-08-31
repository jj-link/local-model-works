/// <reference types="@testing-library/jest-dom" />
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { MemoryRouter } from "react-router";
import { describe, expect, it } from "vitest";

import RecipesRoute from "~/routes/library/recipes/index";
import { server } from "../msw/server";

const alphaDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
const betaDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";
const gammaDigest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc";
const glmDigest = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd";
const alphaRepositoryId = "repo-alpha";

const recipes = [
  {
    digest: alphaDigest,
    name: "MiaAI-Lab/alpha-recipe",
    model: "Alpha 27B",
    engine: "vllm",
    version: "2.0.0",
    version_count: 2,
    description: "A verified two-node recipe.",
    license: "MIT",
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
    name: "MiaAI-Lab/beta-recipe",
    version: "1.0.0",
    description: "A local single-node recipe.",
    license: "Apache-2.0",
    source: { type: "git", remote: "https://git.example/beta" },
    compatibility: { nodeCount: 1 },
    installed_at: "2026-01-01T00:00:00Z",
  },
  {
    digest: gammaDigest,
    name: "MiaAI-Lab/gamma-recipe",
    version: "1.5.0",
    description: "A local recipe.",
    license: "BSD-3-Clause",
    source: { type: "local", remote: "/srv/gamma" },
    compatibility: { nodeCount: 2 },
    installed_at: "2026-01-02T00:00:00Z",
  },
] as const;

const installedDevices = [
  { node_id: "node-1", node_name: "spark1", node_status: "online", installed_digests: [alphaDigest] },
  { node_id: "node-2", node_name: "spark2", node_status: "online", installed_digests: [alphaDigest] },
];

const repositories = recipes.map((recipe, index) => ({
  id: index === 0 ? alphaRepositoryId : `repo-${recipe.name}`,
  source_url: `https://github.com/${recipe.name}`,
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
  installed_devices: index === 0 ? installedDevices : [],
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-04T00:00:00Z",
}));

const deployments = [
  {
    id: "11111111-1111-4111-8111-111111111111",
    recipe_digest: alphaDigest,
    parameters: {},
    placements: [
      { node_id: "node-1", node_name: "spark1", rank: 0 },
      { node_id: "node-2", node_name: "spark2", rank: 1 },
    ],
    desired_state: "running",
    observed_state: "healthy",
  },
];


const updateDevices = installedDevices.map((device) => ({
  ...device,
  status: "pending",
  phase: "fetching",
  current_step: 0,
  total_steps: 2,
}));

const runningDeployments = deployments[0].placements.map((placement) => ({
  source_deployment_id: deployments[0].id,
  node_id: placement.node_id,
  node_name: placement.node_name,
  node_status: "online",
  rank: placement.rank,
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


function installCatalogHandlers(rows: readonly unknown[] = repositories) {
  server.use(
    http.get("*/api/v1/recipe-repositories", () => HttpResponse.json(rows)),
    http.get(`*/api/v1/recipe-repositories/${alphaRepositoryId}`, () =>
      HttpResponse.json(repositories[0]),
    ),
    http.get("*/api/v1/recipes", () => HttpResponse.json(recipes)),
    http.get(`*/api/v1/recipes/${alphaDigest}`, () =>
      HttpResponse.json({ ...recipes[0], manifest: { artifacts: [], workloads: [] } }),
    ),
    http.get("*/api/v1/recipes/:digest/launch-profiles", () => HttpResponse.json([])),
    http.get("*/api/v1/deployments", () => HttpResponse.json(deployments)),
    http.get("*/api/v1/nodes", () => HttpResponse.json([])),
    http.get("*/api/v1/fabrics", () => HttpResponse.json([])),
    http.post("*/api/v1/deployments/plan", () => HttpResponse.json({
      recipe_digest: alphaDigest,
      recipe_name: "alpha-recipe",
      recipe_version: "2.0.0",
      placements: [],
      ready: false,
      plan_digest: "sha256:blocked",
      diagnostics: [],
    })),
    http.post("*/api/v1/recipes/check-updates", () =>
      HttpResponse.json([recipes[0].update]),
    ),
    http.post(`*/api/v1/recipe-repositories/${alphaRepositoryId}/update/plan`, () =>
      HttpResponse.json({
        plan_digest: "sha256:update-plan", ready: true,
        current_permissions: ["network.host"],
        candidate_permissions: ["network.host", "rootfs.write"],
        added_permissions: ["rootfs.write"], removed_permissions: [],
        installed_devices: installedDevices, running_deployments: runningDeployments, diagnostics: [],
      }),
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
      name: "Launch MiaAI-Lab/alpha-recipe",
    });
    expect(within(alphaCard).getByText("MiaAI-Lab/alpha-recipe")).toBeInTheDocument();
    expect(within(alphaCard).getByText("A verified two-node recipe.")).toBeInTheDocument();
    expect(within(alphaCard).getByText("2 nodes · RDMA fabric")).toBeInTheDocument();
    expect(within(alphaCard).getByText("Recipe ready on 2 nodes")).toBeInTheDocument();
    expect(within(alphaCard).getByText("Launch →")).toBeInTheDocument();
    expect(within(alphaCard).getByText(/2\.0\.0 · sha256:aaaaa… · MIT/)).toBeInTheDocument();
    expect(within(alphaCard).getByLabelText("git source")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Update recipe" })).toBeInTheDocument();

    expect(screen.getAllByRole("button", { name: /^Launch MiaAI-Lab/ })).toHaveLength(3);
    const gammaCard = screen.getByRole("button", { name: "Launch MiaAI-Lab/gamma-recipe" });
    expect(within(gammaCard).getByText("Recipe package not cached")).toBeInTheDocument();
    await user.type(screen.getByRole("searchbox", { name: "Search recipes" }), gammaDigest);
    expect(screen.getByRole("button", { name: "Launch MiaAI-Lab/gamma-recipe" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Launch MiaAI-Lab/alpha-recipe" })).not.toBeInTheDocument();
  });

  it("offers updates only for recipes with valid device placements", async () => {
    installCatalogHandlers(repositories.map((repository, index) =>
      index === 0 ? { ...repository, installed_devices: [] } : repository,
    ));
    renderCatalog();

    const alphaCard = await screen.findByRole("button", {
      name: "Launch MiaAI-Lab/alpha-recipe",
    });
    expect(within(alphaCard).getByText("Recipe package not cached")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Update recipe" })).not.toBeInTheDocument();
  });

  it("opens a compact installed launcher and plans after explicit target selection", async () => {
    installCatalogHandlers();
    const planBodies: Record<string, unknown>[] = [];
    server.use(
      http.get(`*/api/v1/recipes/${betaDigest}`, () => HttpResponse.json({
        ...recipes[1],
        manifest: {
          artifacts: [
            {
              name: "model",
              defaultVariant: "managed",
              variants: [{ name: "managed", label: "Managed model" }],
            },
          ],
          parameters: [
            {
              name: "kv_cache_dtype",
              type: "enum",
              default: "fp8_e4m3",
              enum: ["fp8_e4m3"],
            },
          ],
          workloads: [],
        },
      })),
      http.get("*/api/v1/nodes", () => HttpResponse.json([{
        id: "node-1",
        display_name: "spark1",
        status: "online",
        inventory: { accelerators: [] },
      }])),
      http.post("*/api/v1/deployments/plan", async ({ request }) => {
        planBodies.push(await request.json() as Record<string, unknown>);
        return HttpResponse.json({
          recipe_digest: betaDigest,
          recipe_name: recipes[1].name,
          recipe_version: recipes[1].version,
          placements: [{ node_id: "node-1", node_name: "spark1", rank: 0, accelerator_index: 0 }],
          endpoint: { host: "spark1", port: 8000, path: "/v1" },
          transfers: [{
            artifact: "model",
            node_id: "node-1",
            action: "reconcile-local",
          }],
          ready: true,
          plan_digest: "sha256:ready",
          diagnostics: [],
        });
      }),
    );
    const user = userEvent.setup();
    renderCatalog();
    await user.click(await screen.findByRole("button", { name: "Launch MiaAI-Lab/beta-recipe" }));

    const dialog = await screen.findByRole("dialog", { name: "Launch MiaAI-Lab/beta-recipe" });
    expect(within(dialog).getByText("Target")).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Launch" })).toBeDisabled();
    expect(within(dialog).queryByText(/fabric|images|storage|risk|transfer/i)).not.toBeInTheDocument();

    await user.selectOptions(within(dialog).getByLabelText("Target"), "node-1");

    expect(await within(dialog).findByText("spark1:8000/v1")).toBeInTheDocument();
    expect(within(dialog).getByText("Installed")).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Launch" })).toBeEnabled();
    expect(within(dialog).queryByText(/^Settings/)).not.toBeInTheDocument();
    expect(within(dialog).queryByText(/fabric|images|storage|risk|transfer/i)).not.toBeInTheDocument();
    await waitFor(() => expect(planBodies.at(-1)).toMatchObject({
      recipe_digest: betaDigest,
      variants: { model: "managed" },
      parameters: { kv_cache_dtype: "fp8_e4m3" },
      placements: [{ rank: 0, node_id: "node-1" }],
    }));
  });

  it("resolves a GitHub URL and exposes its immutable contract for launch", async () => {
    installCatalogHandlers();
    const commit = "07aee44f76a23245b5822f4efbc01ea7ddc7bdb1";
    let importBody: Record<string, unknown> | undefined;
    const imported = {
      digest: glmDigest,
      name: "MiaAI-Lab/GLM",
      model: "GLM-5.3-Flash-EXL3",
      engine: "vllm",
      version: "1.0.0",
      description: "Two Spark GLM launch contract.",
      license: "MIT",
      source: { type: "git", remote: "https://github.com/MiaAI-Lab/GLM", revision: commit },
      compatibility: { nodeCount: 2, fabric: { transport: "roce" } },
      permissions: ["devices.rdma", "network.host", "rootfs.write"],
      high_risk: ["network.host", "rootfs.write"],
      installed_at: "2026-08-29T00:00:00Z",
    };
    server.use(
      http.post("*/api/v1/recipes/import", async ({ request }) => {
        importBody = await request.json() as Record<string, unknown>;
        return HttpResponse.json(imported, { status: 201 });
      }),
      http.get(`*/api/v1/recipes/${glmDigest}`, () => HttpResponse.json({
        ...imported,
        manifest: {
          compatibility: imported.compatibility,
          artifacts: [
            {
              name: "model", sizeBytes: 175715854754,
              source: { identity: "hf://Mia-AiLab/GLM-5.3-Flash-EXL3-TR3-4bpw", revision: commit },
            },
            {
              name: "drafter", sizeBytes: 2342175855,
              source: { identity: "hf://incoai/GLM-5.3-Flash-DFlash2", revision: commit },
            },
          ],
          workloads: [{
            image: { reference: "ghcr.io/miaai-lab/glm@sha256:9999999999999999999999999999999999999999999999999999999999999999" },
          }],
        },
      })),
    );

    const user = userEvent.setup();
    renderCatalog();
    await user.click(await screen.findByRole("button", { name: "Import recipe" }));
    const dialog = await screen.findByRole("dialog", { name: "Add a model recipe" });
    await user.type(within(dialog).getByLabelText("Repository URL"), "https://github.com/MiaAI-Lab/GLM");
    await user.click(within(dialog).getByRole("button", { name: "Resolve & review" }));

    expect(await within(dialog).findByText("MiaAI-Lab/GLM")).toBeInTheDocument();
    expect(within(dialog).getByText("07aee44f76a2…")).toBeInTheDocument();
    expect(await within(dialog).findByTitle("hf://Mia-AiLab/GLM-5.3-Flash-EXL3-TR3-4bpw")).toBeInTheDocument();
    expect(within(dialog).getByText("164 GiB")).toBeInTheDocument();
    expect((importBody?.source as Record<string, unknown>).revision).toBeUndefined();

    expect(await within(dialog).findByText("compiled · launchable")).toBeInTheDocument();
    expect(within(dialog).getByText(/Permissions:/)).toBeInTheDocument();
    const continueButton = within(dialog).getByRole("button", { name: "Continue to launch" });
    expect(continueButton).toBeEnabled();
    await user.click(continueButton);
    const launcher = await screen.findByRole("dialog", { name: "Launch MiaAI-Lab/GLM" });
    await user.click(within(launcher).getByRole("button", { name: "Back" }));
    const resumedImport = await screen.findByRole("dialog", { name: "Add a model recipe" });
    expect(within(resumedImport).getByText("compiled · launchable")).toBeInTheDocument();
  });
  it("shows installed devices and confirms an update without running deployments", async () => {
    installCatalogHandlers();
    server.use(
      http.get(`*/api/v1/recipe-repositories/${alphaRepositoryId}`, () =>
        HttpResponse.json(repositories[0]),
      ),
      http.post(`*/api/v1/recipe-repositories/${alphaRepositoryId}/update/plan`, () =>
        HttpResponse.json({
          plan_digest: "sha256:empty-update", ready: true,
          current_permissions: ["network.host"],
          candidate_permissions: ["network.host", "rootfs.write"],
          added_permissions: ["rootfs.write"], removed_permissions: [],
          installed_devices: installedDevices, running_deployments: null, diagnostics: null,
        }),
      ),
      http.post(`*/api/v1/recipe-repositories/${alphaRepositoryId}/update`, async ({ request }) => {
        expect(await request.json()).toEqual({
          expected_head_commit: "2222222222222222222222222222222222222222",
          plan_digest: "sha256:empty-update",
        });
        return HttpResponse.json({ run_id: "run-empty-update" }, { status: 202 });
      }),
      http.get("*/api/v1/runs/run-empty-update", () =>
        HttpResponse.json({
          id: "run-empty-update", module: "library", kind: "recipe-update", state: "succeeded",
          progress: {
            phase: "ready", total_devices: 2, completed_devices: 2,
            installed_devices: updateDevices.map((device) => ({
              ...device, status: "succeeded", phase: "ready", current_step: 2,
            })),
            total_running_targets: 0, completed_running_targets: 0, running_deployments: [],
          },
        }),
      ),
    );
    const user = userEvent.setup();
    renderCatalog();
    await user.click(await screen.findByRole("button", { name: "Update recipe" }));

    const dialog = await screen.findByRole("dialog", { name: "Update recipe" });
    expect(within(dialog).getByRole("heading", { name: "Permission changes" })).toBeInTheDocument();
    expect(within(dialog).getByText("rootfs.write")).toBeInTheDocument();
    expect(within(dialog).getByRole("heading", { name: "Installed devices" })).toBeInTheDocument();
    expect(within(dialog).getByText("spark1")).toBeInTheDocument();
    expect(within(dialog).getByText("spark2")).toBeInTheDocument();
    expect(within(dialog).queryByText(/sha256:/)).not.toBeInTheDocument();
    expect(within(dialog).queryByText("fetching")).not.toBeInTheDocument();
    expect(within(dialog).queryByRole("heading", { name: "Running deployments to replace" })).not.toBeInTheDocument();
    const updateButton = within(dialog).getByRole("button", { name: "Update recipe" });
    expect(updateButton).toBeEnabled();
    await user.click(updateButton);
    expect(await screen.findByRole("heading", { name: "Update complete" })).toBeInTheDocument();
    expect(screen.getByText("2 of 2 devices updated")).toBeInTheDocument();
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
              phase: "installing_recipe", total_devices: 2, completed_devices: 0,
              installed_devices: updateDevices.map((device) => ({ ...device, status: "running", phase: "fetching", current_step: 0 })),
              total_running_targets: 2, completed_running_targets: 0,
              running_deployments: runningDeployments.map((target) => ({ ...target, status: "running", phase: "pulling", current_step: 3 })),
            },
          });
        }
        return HttpResponse.json({
          id: "run-update", module: "library", kind: "recipe-update", state: "succeeded",
          progress: {
            phase: "ready", total_devices: 2, completed_devices: 2,
            installed_devices: updateDevices.map((device) => ({ ...device, status: "succeeded", phase: "ready", current_step: 2 })),
            total_running_targets: 2, completed_running_targets: 2,
            running_deployments: runningDeployments.map((target) => ({ ...target, status: "succeeded", phase: "ready", current_step: 5 })),
          },
        });
      }),
    );
    const user = userEvent.setup();
    renderCatalog();
    await user.click(await screen.findByRole("button", { name: "Update recipe" }));
    const dialog = await screen.findByRole("dialog", { name: "Update recipe" });
    const installedSection = within(dialog).getByRole("region", { name: "Installed devices" });
    expect(within(installedSection).getByText("spark1")).toBeInTheDocument();
    expect(within(installedSection).getByText("spark2")).toBeInTheDocument();
    expect(within(dialog).getByRole("heading", { name: "Running deployments to replace" })).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: "Update recipe" }));
    expect((await within(installedSection).findAllByText("fetching")).length).toBeGreaterThan(0);
    expect((await screen.findAllByText("pulling")).length).toBeGreaterThan(0);
    expect(await screen.findByRole("heading", { name: "Update complete" }, { timeout: 3_000 })).toBeInTheDocument();
    expect(screen.getByText("2 of 2 devices updated")).toBeInTheDocument();
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
            phase: "restored", total_devices: 2, completed_devices: 2,
            installed_devices: updateDevices.map((device) => ({ ...device, status: "succeeded", phase: "ready", current_step: 2 })),
            total_running_targets: 2, completed_running_targets: 0,
            running_deployments: [
              { ...runningDeployments[0], status: "failed", phase: "restored", error_code: "workload.start_failed", error_message: "container exited" },
              { ...runningDeployments[1], status: "failed", phase: "restored" },
            ],
          },
        }),
      ),
    );
    const user = userEvent.setup();
    renderCatalog();
    await user.click(await screen.findByRole("button", { name: "Update recipe" }));
    const dialog = await screen.findByRole("dialog", { name: "Update recipe" });
    await user.click(within(dialog).getByRole("button", { name: "Update recipe" }));
    expect(await screen.findByRole("heading", { name: "Update failed" })).toBeInTheDocument();
    expect(screen.getByText("workload.start_failed: container exited")).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Update complete" })).not.toBeInTheDocument();
  });


  it("distinguishes loading, API-empty, and retryable error states", async () => {
    let release: (() => void) | undefined;
    server.use(
      http.get("*/api/v1/recipe-repositories", async () => {
        await new Promise<void>((resolve) => { release = resolve; });
        return HttpResponse.json([]);
      }),
      http.get("*/api/v1/deployments", () => HttpResponse.json([])),
      http.get("*/api/v1/artifacts", () => HttpResponse.json([])),
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
      http.get("*/api/v1/artifacts", () => HttpResponse.json([])),
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
