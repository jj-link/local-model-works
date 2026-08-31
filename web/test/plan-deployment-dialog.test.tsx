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

const recipe = {
  name: "deepseek-v4-flash-0731-dspark-tp2",
  version: "1.0.0",
  digest: D,
  installed_at: "2026-08-21T06:17:58Z",
  compatibility: {
    nodeCount: 2,
    accelerator: { vendor: "nvidia", architectures: ["sm_121"], count: 1 },
  },
};

const recipeDetail = {
  ...recipe,
  manifest: {
    apiVersion: "lmw/v1",
    kind: "Recipe",
    metadata: { name: recipe.name },
    compatibility: recipe.compatibility,
    artifacts: [],
    workloads: [],
    assets: [],
  },
};

const nodes = [
  {
    id: "n-spark2",
    display_name: "spark2",
    status: "online",
    inventory: {
      accelerators: [{
        vendor: "nvidia",
        architecture: "sm_121",
        memory_bytes: 128_000_000_000,
      }],
    },
  },
  {
    id: "n-spark3",
    display_name: "spark3",
    status: "online",
    inventory: {
      accelerators: [{
        vendor: "nvidia",
        architecture: "sm_121",
        memory_bytes: 128_000_000_000,
      }],
    },
  },
  {
    id: "n-spark1",
    display_name: "spark1",
    status: "offline",
    inventory: {
      accelerators: [{
        vendor: "nvidia",
        architecture: "sm_121",
        memory_bytes: 128_000_000_000,
      }],
    },
  },
];

const plan = {
  recipe_digest: D,
  recipe_name: recipe.name,
  recipe_version: recipe.version,
  placements: [
    { node_id: "n-spark2", node_name: "spark2", rank: 0, accelerator_index: 0 },
    { node_id: "n-spark3", node_name: "spark3", rank: 1, accelerator_index: 0 },
  ],
  transfers: [],
  endpoint: { host: "spark2", port: 8888 },
  ready: true,
  plan_digest: "sha256:af58cc518066a4b40c86fbca6ea123d4d27c16bb59b2cf442ec419b07d36de99",
};

const created = {
  id: "dep-1",
  recipe_name: recipe.name,
  recipe_digest: D,
  parameters: {},
  desired_state: "running",
  observed_state: "preparing",
};

function renderDialog(open = true, initialRecipeDigest?: string) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  const view = (currentOpen: boolean, digest?: string) => (
    <QueryClientProvider client={client}>
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

function installHandlers(
  onPlan: (body: Record<string, unknown>) => Record<string, unknown> = () => plan,
  onCreate?: (body: Record<string, unknown>) => void,
) {
  server.use(
    http.get("*/api/v1/recipes", () => HttpResponse.json([recipe])),
    http.get(`*/api/v1/recipes/${D}`, () => HttpResponse.json(recipeDetail)),
    http.get("*/api/v1/recipes/:digest/launch-profiles", () => HttpResponse.json([])),
    http.get("*/api/v1/nodes", () => HttpResponse.json(nodes)),
    http.post("*/api/v1/deployments/plan", async ({ request }) => {
      const body = await request.json() as Record<string, unknown>;
      return HttpResponse.json(onPlan(body));
    }),
    http.post("*/api/v1/deployments", async ({ request }) => {
      onCreate?.(await request.json() as Record<string, unknown>);
      return HttpResponse.json(created, { status: 201 });
    }),
  );
}

describe("PlanDeploymentDialog", () => {
  it("auto-plans and launches a profile-less multi-node recipe", async () => {
    let createBody: Record<string, unknown> | undefined;
    installHandlers(undefined, (body) => { createBody = body; });
    const user = userEvent.setup();
    renderDialog();

    const recipeSelect = await screen.findByLabelText("Recipe");
    await waitFor(() => expect((recipeSelect as HTMLSelectElement).options).toHaveLength(2));
    await user.selectOptions(recipeSelect, D);

    expect(await screen.findByLabelText("Rank 0 node")).toBeInTheDocument();
    expect(screen.getByLabelText("Rank 1 node")).toBeInTheDocument();
    expect(screen.queryByText(/^Settings/)).not.toBeInTheDocument();
    expect(await screen.findByText("Installed")).toBeInTheDocument();
    const launch = await screen.findByRole("button", { name: "Launch" });
    await waitFor(() => expect(launch).toBeEnabled());
    await user.click(launch);

    await waitFor(() => expect(createBody).toMatchObject({
      recipe_digest: D,
      parameters: {},
      variants: {},
      placements: [
        { rank: 0, node_id: "n-spark2" },
        { rank: 1, node_id: "n-spark3" },
      ],
    }));
  });

  it("auto-plans explicit node overrides", async () => {
    const planBodies: Record<string, unknown>[] = [];
    installHandlers((body) => {
      planBodies.push(body);
      return plan;
    });
    const user = userEvent.setup();
    renderDialog();

    const recipeSelect = await screen.findByLabelText("Recipe");
    await waitFor(() => expect((recipeSelect as HTMLSelectElement).options).toHaveLength(2));
    await user.selectOptions(recipeSelect, D);
    await user.selectOptions(await screen.findByLabelText("Rank 0 node"), "n-spark3");

    await waitFor(() => expect(planBodies.at(-1)?.placements).toEqual([
      { rank: 0, node_id: "n-spark3" },
    ]));
  });

  it("shows the blocking diagnostic without exposing plan internals", async () => {
    installHandlers(() => ({
      ...plan,
      placements: [],
      ready: false,
      diagnostics: [{
        code: "placement.no_capacity",
        severity: "error",
        message: "no eligible node for rank 0",
      }],
    }));
    const user = userEvent.setup();
    renderDialog();

    const recipeSelect = await screen.findByLabelText("Recipe");
    await waitFor(() => expect((recipeSelect as HTMLSelectElement).options).toHaveLength(2));
    await user.selectOptions(recipeSelect, D);

    expect(await screen.findByText("no eligible node for rank 0")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Launch" })).toBeDisabled();
    expect(screen.queryByText(/images|storage|risk|transfer/i)).not.toBeInTheDocument();
  });

  it("uses the caller recipe without a chooser and resets overrides on reopen", async () => {
    installHandlers();
    const user = userEvent.setup();
    const view = renderDialog(true, D);

    expect(await screen.findByRole("heading", {
      name: `Launch ${recipe.name}`,
    })).toBeInTheDocument();
    expect(screen.queryByLabelText("Recipe")).not.toBeInTheDocument();
    const rankZero = await screen.findByLabelText("Rank 0 node");
    await user.selectOptions(rankZero, "n-spark3");
    expect(rankZero).toHaveValue("n-spark3");

    view.rerenderDialog(false, D);
    view.rerenderDialog(true, D);
    await waitFor(() => expect(screen.getByLabelText("Rank 0 node")).toHaveValue(""));
  });
});
