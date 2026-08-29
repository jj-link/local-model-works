/// <reference types="@testing-library/jest-dom" />
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { MemoryRouter } from "react-router";
import { describe, expect, it } from "vitest";

import { PlanDeploymentDialog } from "~/components/dialogs/plan-deployment-dialog";
import { server } from "../msw/server";

const D = "sha256:3a2e1e83a23a63803d39224b3a0641dd50f223000a1f756f538803ba32e888e2";

// Mirrors the live GET /recipes/:digest response: compatibility uses
// camelCase `nodeCount` (not snake_case), on both the top level and the
// embedded manifest.
const recipe = {
  name: "deepseek-v4-flash-0731-dspark-tp2",
  version: "1.0.0",
  digest: D,
  trust_state: "local",
  installed_at: "2026-08-21T06:17:58Z",
  compatibility: { nodeCount: 2, accelerator: { vendor: "nvidia", architectures: ["sm_121"], count: 1 } },
};

const recipeDetail = {
  ...recipe,
  manifest: { apiVersion: "lmw/v1", kind: "Recipe", metadata: { name: recipe.name }, compatibility: recipe.compatibility, artifacts: [], workloads: [], assets: [] },
};

const nodes = [
  { id: "n-spark2", display_name: "spark2", status: "online" },
  { id: "n-spark3", display_name: "spark3", status: "online" },
  { id: "n-spark1", display_name: "spark1", status: "offline" },
];

const plan = {
  recipe_digest: D,
  recipe_name: recipe.name,
  recipe_version: recipe.version,
  profile: "",
  placements: [
    { node_id: "n-spark2", node_name: "spark2", rank: 0 },
    { node_id: "n-spark3", node_name: "spark3", rank: 1 },
  ],
  transfers: [],
  ports: [{ node_id: "n-spark2", node_name: "spark2", host_port: 8888, container_port: 8888, protocol: "tcp" }],
  endpoint: { host: "spark2", port: 8888 },
  risks: [],
  ready: true,
  plan_digest: "sha256:af58cc518066a4b40c86fbca6ea123d4d27c16bb59b2cf442ec419b07d36de99",
};

const created = { id: "dep-1", recipe_name: recipe.name, profile: "", recipe_digest: D };

function renderDialog(open = true, initialRecipeDigest?: string) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const view = (currentOpen: boolean, digest?: string) => (
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={["/serving/deployments"]}>
        <PlanDeploymentDialog
          open={currentOpen}
          onOpenChange={() => {}}
          initialRecipeDigest={digest}
        />
      </MemoryRouter>
    </QueryClientProvider>
  );
  const result = render(view(open, initialRecipeDigest));
  return {
    ...result,
    rerenderDialog: (nextOpen: boolean, digest = initialRecipeDigest) =>
      result.rerender(view(nextOpen, digest)),
  };
}

describe("PlanDeploymentDialog", () => {
  it("plans and creates a deployment for a profile-less 2-node recipe", async () => {
    const user = userEvent.setup();
    server.use(
      http.get("*/api/v1/recipes", () => HttpResponse.json([recipe])),
      http.get(`*/api/v1/recipes/${D}`, () => HttpResponse.json(recipeDetail)),
      http.get("*/api/v1/nodes", () => HttpResponse.json(nodes)),
      http.get("*/api/v1/fabrics", () => HttpResponse.json([])),
      http.post("*/api/v1/deployments/plan", async ({ request }) => {
        const body = (await request.json()) as { recipe_digest: string; profile: string; placements?: unknown[] };
        return HttpResponse.json({
          ...plan,
          placements: body.placements ?? plan.placements,
        });
      }),
      http.post("*/api/v1/deployments", async ({ request }) => {
        const body = (await request.json()) as { recipe_digest: string; profile: string; placements: { rank: number; node_id: string }[] };
        // record what create received for assertions
        (globalThis as Record<string, unknown>).__createBody = body;
        return HttpResponse.json(created, { status: 201 });
      }),
    );

    renderDialog();

    // recipe select appears with the installed recipe as an option
    const recipeSelect = await screen.findByLabelText("Recipe");
    expect(recipeSelect).toBeInTheDocument();
    await waitFor(() => {
      expect((recipeSelect as HTMLSelectElement).options).toHaveLength(2);
    });
    await user.selectOptions(recipeSelect, D);

    // profile-less recipe: profile control shows "no profiles" but does not block preview
    expect(await screen.findByText(/no profiles/i)).toBeInTheDocument();

    // selecting a 2-node recipe mounts one native rank control per rank (regression for the #185 crash)
    expect((await screen.findAllByLabelText(/Rank 0 node/)).length).toBe(1);
    expect((await screen.findAllByLabelText(/Rank 1 node/)).length).toBe(1);

    // preview produces a ready plan
    await user.click(screen.getByRole("button", { name: /preview placement/i }));
    await waitFor(() => {
      expect(screen.getByRole("button", { name: /^launch$/i })).toBeEnabled();
    });

    // create posts the planned placements and returns the deployment
    await user.click(screen.getByRole("button", { name: /^launch$/i }));
    await waitFor(() => {
      const body = (globalThis as Record<string, unknown>).__createBody as { placements: { rank: number; node_id: string }[] };
      expect(body).toBeTruthy();
      expect(body.placements).toEqual(
        expect.arrayContaining([
          { rank: 0, node_id: "n-spark2" },
          { rank: 1, node_id: "n-spark3" },
        ]),
      );
    });
  });

  it("sends explicit node overrides as rank placements", async () => {
    const user = userEvent.setup();
    server.use(
      http.get("*/api/v1/recipes", () => HttpResponse.json([recipe])),
      http.get(`*/api/v1/recipes/${D}`, () => HttpResponse.json(recipeDetail)),
      http.get("*/api/v1/nodes", () => HttpResponse.json(nodes)),
      http.get("*/api/v1/fabrics", () => HttpResponse.json([])),
      http.post("*/api/v1/deployments/plan", async ({ request }) => {
        const b = (await request.json()) as { placements?: { rank: number; node_id: string }[] };
        (globalThis as Record<string, unknown>).__planPlacements = b.placements;
        return HttpResponse.json(plan);
      }),
      http.post("*/api/v1/deployments", () => HttpResponse.json(created, { status: 201 })),
    );

    renderDialog();
    const recipeSelect = await screen.findByLabelText("Recipe");
    await waitFor(() => {
      expect((recipeSelect as HTMLSelectElement).options).toHaveLength(2);
    });
    await user.selectOptions(recipeSelect, D);

    // pin rank 0 to spark3 explicitly
    const rank0 = (await screen.findAllByLabelText(/Rank 0 node/))[0];
    await user.selectOptions(rank0, "n-spark3");

    await user.click(screen.getByRole("button", { name: /preview placement/i }));
    await waitFor(() => {
      const p = (globalThis as Record<string, unknown>).__planPlacements as { rank: number; node_id: string }[] | undefined;
      expect(p).toBeTruthy();
      expect(p).toEqual([{ rank: 0, node_id: "n-spark3" }]);
    });
  });

  it("links an occupying deployment when placement is blocked", async () => {
    const user = userEvent.setup();
    server.use(
      http.get("*/api/v1/recipes", () => HttpResponse.json([recipe])),
      http.get(`*/api/v1/recipes/${D}`, () => HttpResponse.json(recipeDetail)),
      http.get("*/api/v1/nodes", () => HttpResponse.json(nodes)),
      http.get("*/api/v1/fabrics", () => HttpResponse.json([])),
      http.post("*/api/v1/deployments/plan", () =>
        HttpResponse.json({
          ...plan,
          placements: [],
          ready: false,
          conflicts: [{
            resource: "gpu:n-spark2:GPU-a",
            occupied_by: "dep-occupant",
            deployment_id: "dep-occupant",
          }],
          diagnostics: [{
            code: "placement.no_capacity",
            severity: "error",
            message: "no eligible node for rank 0",
          }],
        }),
      ),
    );
    renderDialog();
    const recipeSelect = await screen.findByLabelText("Recipe");
    await waitFor(() => expect((recipeSelect as HTMLSelectElement).options).toHaveLength(2));
    await user.selectOptions(recipeSelect, D);
    await user.click(screen.getByRole("button", { name: /preview placement/i }));

    expect(await screen.findByText("No placement available")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "dep-occupant" })).toHaveAttribute(
      "href",
      "/serving/deployments/dep-occupant",
    );
    expect(screen.getByRole("button", { name: /^launch$/i })).toBeDisabled();
  });

  it("preselects the caller recipe and resets it on every reopen", async () => {
    const user = userEvent.setup();
    server.use(
      http.get("*/api/v1/recipes", () => HttpResponse.json([recipe])),
      http.get(`*/api/v1/recipes/${D}`, () => HttpResponse.json(recipeDetail)),
      http.get("*/api/v1/nodes", () => HttpResponse.json(nodes)),
      http.get("*/api/v1/fabrics", () => HttpResponse.json([])),
    );

    const view = renderDialog(true, D);
    const recipeSelect = await screen.findByLabelText("Recipe");
    await waitFor(() => expect(recipeSelect).toHaveValue(D));
    await user.selectOptions(recipeSelect, "");
    expect(recipeSelect).toHaveValue("");

    view.rerenderDialog(false, D);
    view.rerenderDialog(true, D);
    await waitFor(() => expect(screen.getByLabelText("Recipe")).toHaveValue(D));
  });
});
