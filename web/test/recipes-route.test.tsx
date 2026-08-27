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
      remote: "https://github.com/MiaAI-Lab/alpha.git",
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
    update: {
      state: "current",
      remote: "https://github.com/MiaAI-Lab/beta.git",
      tracking_ref: "main",
      installed_revision: "3333333333333333333333333333333333333333",
      checked_at: "2026-01-04T00:00:00Z",
    },
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
          <Route path="/library/builder" element={<p>Builder route</p>} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function installCatalogHandlers(rows: readonly unknown[] = recipes) {
  server.use(
    http.get("*/api/v1/recipes", () => HttpResponse.json(rows)),
    http.get(`*/api/v1/recipes/${alphaDigest}`, () =>
      HttpResponse.json({ ...recipes[0], manifest: { artifacts: [], workloads: [] } }),
    ),
    http.get("*/api/v1/deployments", () => HttpResponse.json(deployments)),
    http.get("*/api/v1/nodes", () => HttpResponse.json([])),
    http.get("*/api/v1/fabrics", () => HttpResponse.json([])),
    http.post("*/api/v1/recipes/check-updates", () =>
      HttpResponse.json(recipes.flatMap((recipe) => "update" in recipe ? [recipe.update] : [])),
    ),
  );
}

describe("RecipesRoute catalog", () => {
  it("renders the recipe catalog with real recipe, deployment, and update data", async () => {
    installCatalogHandlers();
    const user = userEvent.setup();
    renderCatalog();

    expect(await screen.findByRole("heading", { name: "Recipe catalog" })).toBeInTheDocument();
    expect(screen.getByText("Installed serving recipes, hardware compatibility, and upstream update status.")).toBeInTheDocument();
    expect(await screen.findByRole("heading", { name: "All recipes" })).toBeInTheDocument();

    const alphaCard = screen.getByRole("article", { name: "Recipe Alpha Model" });
    expect(within(alphaCard).getByText("Alpha Model")).toBeInTheDocument();
    expect(within(alphaCard).getByText("A verified two-node recipe.")).toBeInTheDocument();
    expect(within(alphaCard).getByText("2 nodes · RDMA fabric")).toBeInTheDocument();
    expect(within(alphaCard).getByText("Installed on 2 devices")).toBeInTheDocument();
    expect(within(alphaCard).getByRole("button", { name: "Choose hardware →" })).toBeInTheDocument();
    expect(within(alphaCard).getByText(/2\.0\.0 · sha256:aaaaa… · MIT/)).toBeInTheDocument();
    expect(within(alphaCard).getByLabelText("catalog source")).toBeInTheDocument();
    expect(within(alphaCard).getByText("Update available")).toBeInTheDocument();
    expect(screen.getByText("Up to date")).toBeInTheDocument();

    expect(screen.getAllByRole("article").map((card) => card.getAttribute("aria-label"))).toEqual([
      "Recipe Alpha Model",
      "Recipe Beta Model",
      "Recipe Gamma Model",
    ]);

    await user.type(screen.getByRole("searchbox", { name: "Search recipes" }), gammaDigest);
    expect(screen.getByRole("article", { name: "Recipe Gamma Model" })).toBeInTheDocument();
    expect(screen.queryByRole("article", { name: "Recipe Alpha Model" })).not.toBeInTheDocument();
    await user.clear(screen.getByRole("searchbox", { name: "Search recipes" }));
    await user.type(screen.getByRole("searchbox", { name: "Search recipes" }), "does-not-exist");
    expect(screen.getByText("No recipes match")).toBeInTheDocument();
  });

  it("opens the real import and recipe-preselected hardware dialogs", async () => {
    installCatalogHandlers();
    const user = userEvent.setup();
    renderCatalog();

    await user.click(await screen.findByRole("button", { name: "Import recipe" }));
    expect(await screen.findByRole("heading", { name: "Install recipe" })).toBeInTheDocument();
    await user.keyboard("{Escape}");

    const alphaCard = screen.getByRole("article", { name: "Recipe Alpha Model" });
    await user.click(within(alphaCard).getByRole("button", { name: "Choose hardware →" }));
    expect(await screen.findByRole("heading", { name: "Choose hardware" })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByLabelText("Recipe")).toHaveValue(alphaDigest));
  });

  it("refreshes cached update status on demand", async () => {
    installCatalogHandlers();
    let checks = 0;
    server.use(
      http.post("*/api/v1/recipes/check-updates", () => {
        checks += 1;
        return HttpResponse.json([recipes[0].update, recipes[1].update]);
      }),
    );
    const user = userEvent.setup();
    renderCatalog();

    await user.click(await screen.findByRole("button", { name: "Check updates" }));
    await waitFor(() => expect(checks).toBe(1));
    expect(screen.getByRole("button", { name: "Check updates" })).toBeEnabled();
  });

  it("starts an available update directly from its catalog card", async () => {
    installCatalogHandlers();
    let submitted: unknown;
    server.use(
      http.post("*/api/v1/recipe-drafts", async ({ request }) => {
        submitted = await request.json();
        return HttpResponse.json({ run_id: "run-update-card" }, { status: 202 });
      }),
    );
    const user = userEvent.setup();
    renderCatalog();

    const alphaCard = await screen.findByRole("article", { name: "Recipe Alpha Model" });
    await user.click(within(alphaCard).getByRole("button", { name: "Update recipe →" }));
    await waitFor(() =>
      expect(submitted).toEqual({
        remote: recipes[0].update.remote,
        revision: recipes[0].update.candidate_revision,
        path: ".",
        base_recipe_digest: alphaDigest,
      }),
    );
  });

  it("queues an available commit for inspection in Recipe Builder", async () => {
    installCatalogHandlers();
    let submitted: unknown;
    server.use(
      http.post("*/api/v1/recipe-drafts", async ({ request }) => {
        submitted = await request.json();
        return HttpResponse.json({ run_id: "run-update" }, { status: 202 });
      }),
    );
    const user = userEvent.setup();
    renderRecipeDetail();

    await user.click(await screen.findByRole("button", { name: "Inspect update in builder" }));
    await waitFor(() =>
      expect(submitted).toEqual({
        remote: recipes[0].update.remote,
        revision: recipes[0].update.candidate_revision,
        path: ".",
        base_recipe_digest: alphaDigest,
      }),
    );
    expect(await screen.findByText("Builder route")).toBeInTheDocument();
  });

  it("distinguishes loading, API-empty, and retryable error states", async () => {
    let release: (() => void) | undefined;
    server.use(
      http.get("*/api/v1/recipes", async () => {
        await new Promise<void>((resolve) => { release = resolve; });
        return HttpResponse.json([]);
      }),
      http.get("*/api/v1/deployments", () => HttpResponse.json([])),
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
      http.get("*/api/v1/recipes", () => {
        attempts += 1;
        return attempts === 1
          ? HttpResponse.json({ code: "test.failure", message: "Catalog unavailable" }, { status: 503 })
          : HttpResponse.json([]);
      }),
      http.get("*/api/v1/deployments", () => HttpResponse.json([])),
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
