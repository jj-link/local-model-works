/// <reference types="@testing-library/jest-dom" />
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { MemoryRouter } from "react-router";
import { beforeEach, describe, expect, it } from "vitest";

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

beforeEach(() => sessionStorage.clear());

describe("PlanDeploymentDialog", () => {
  it("requires explicit ranked targets and a reviewed plan before starting without implicit downloads", async () => {
    const planned: Record<string, unknown>[] = [];
    let createBody: Record<string, unknown> | undefined;
    installHandlers((body) => { planned.push(body); return plan; }, (body) => { createBody = body; });
    const user = userEvent.setup();
    renderDialog();
    const recipeSelect = await screen.findByLabelText("Recipe");
    await screen.findByRole("option", { name: new RegExp(recipe.name) });
    await user.selectOptions(recipeSelect, D);
    await user.selectOptions(await screen.findByLabelText("Rank 0 node"), "n-spark2");
    expect(screen.getByRole("button", { name: "Check run plan" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Run on device" })).toBeDisabled();
    await user.selectOptions(screen.getByLabelText("Rank 1 node"), "n-spark3");
    expect(planned).toEqual([]);
    expect(screen.getByRole("button", { name: "Run on device" })).toBeDisabled();
    await user.click(screen.getByRole("button", { name: "Check run plan" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Run on device" })).toBeEnabled());
    await user.click(screen.getByRole("button", { name: "Run on device" }));
    await waitFor(() => expect(createBody).toMatchObject({
      recipe_digest: D, acquisition_policy: "require-existing", plan_digest: plan.plan_digest,
      placements: [{ rank: 0, node_id: "n-spark2" }, { rank: 1, node_id: "n-spark3" }],
    }));
  });

  it("invalidates reviewed or in-flight plans when targets or acquisition consent change", async () => {
    installHandlers();
    let release: (() => void) | undefined;
    const planned: Record<string, unknown>[] = [];
    server.use(http.post("*/api/v1/deployments/plan", async ({ request }) => {
      planned.push(await request.json() as Record<string, unknown>);
      if (planned.length === 1) await new Promise<void>((resolve) => { release = resolve; });
      return HttpResponse.json(plan);
    }));
    const user = userEvent.setup();
    renderDialog(true, D);
    await user.selectOptions(await screen.findByLabelText("Rank 0 node"), "n-spark2");
    await user.selectOptions(screen.getByLabelText("Rank 1 node"), "n-spark3");
    await user.click(screen.getByRole("button", { name: "Check run plan" }));
    await waitFor(() => expect(release).toBeTypeOf("function"));
    await user.selectOptions(screen.getByLabelText("Rank 0 node"), "n-spark3");
    await user.selectOptions(screen.getByLabelText("Rank 1 node"), "n-spark2");
    release!();
    await waitFor(() => expect(screen.getByRole("button", { name: "Check run plan" })).toBeEnabled());
    expect(screen.getByRole("button", { name: "Run on device" })).toBeDisabled();
    await user.click(screen.getByRole("button", { name: "Check run plan" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Run on device" })).toBeEnabled());
    await user.click(screen.getByRole("radio", { name: /Download missing files, then run/ }));
    expect(screen.getByRole("button", { name: "Download and run" })).toBeDisabled();
    await user.click(screen.getByRole("button", { name: "Check run plan" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Download and run" })).toBeEnabled());
    expect(planned.at(-1)?.acquisition_policy).toBe("download-missing");
    expect(planned).toHaveLength(3);
  });

  it("shows every blocking diagnostic and offers configuration help without enabling a run", async () => {
    installHandlers(() => ({ ...plan, ready: false, diagnostics: [
      { code: "placement.no_capacity", severity: "error", message: "no eligible node for rank 0" },
      { code: "fabric.unavailable", severity: "error", message: "required fabric is unavailable" },
    ] }));
    const user = userEvent.setup();
    renderDialog(true, D);
    await user.selectOptions(await screen.findByLabelText("Rank 0 node"), "n-spark2");
    await user.selectOptions(screen.getByLabelText("Rank 1 node"), "n-spark3");
    await user.click(screen.getByRole("button", { name: "Check run plan" }));
    expect(await screen.findByText(/placement.no_capacity: no eligible node for rank 0/)).toBeInTheDocument();
    expect(screen.getByText(/fabric.unavailable: required fabric is unavailable/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Run on device" })).toBeDisabled();
    expect(screen.getByRole("link", { name: "Open recipe configuration" })).toHaveAttribute("href", `/library/recipes/packages/${encodeURIComponent(D)}`);
  });

  it("retains choices after a transient planning failure and retries only on explicit request", async () => {
    const bodies: Record<string, unknown>[] = [];
    installHandlers();
    server.use(http.post("*/api/v1/deployments/plan", async ({ request }) => {
      bodies.push(await request.json() as Record<string, unknown>);
      return bodies.length === 1
        ? HttpResponse.json({ code: "planner.unavailable", message: "planner unavailable" }, { status: 503 })
        : HttpResponse.json(plan);
    }));
    const user = userEvent.setup();
    renderDialog(true, D);
    await user.selectOptions(await screen.findByLabelText("Rank 0 node"), "n-spark2");
    await user.selectOptions(screen.getByLabelText("Rank 1 node"), "n-spark3");
    await user.click(screen.getByRole("button", { name: "Check run plan" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("planner unavailable");
    expect(bodies).toHaveLength(1);
    expect(screen.getByLabelText("Rank 0 node")).toHaveValue("n-spark2");
    expect(screen.getByRole("button", { name: "Run on device" })).toBeDisabled();
    await user.click(screen.getByRole("button", { name: "Check run plan" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Run on device" })).toBeEnabled());
    expect(bodies).toHaveLength(2);
    expect(bodies[1]).toEqual(bodies[0]);
  });

  it("restores choices explicitly after reopening without storing sensitive parameters or reusing consent", async () => {
    const bodies: Record<string, unknown>[] = [];
    installHandlers((body) => { bodies.push(body); return plan; });
    server.use(http.get(`*/api/v1/recipes/${D}`, () => HttpResponse.json({
      ...recipeDetail, manifest: { ...recipeDetail.manifest, parameters: [
        { name: "token_limit", type: "integer", enum: [32, 64], default: 32 },
        { name: "access_token", type: "string", sensitive: true },
      ] },
    })));
    const user = userEvent.setup();
    const view = renderDialog(true, D);
    await user.selectOptions(await screen.findByLabelText("Rank 0 node"), "n-spark3");
    await user.selectOptions(screen.getByLabelText("Rank 1 node"), "n-spark2");
    await user.selectOptions(screen.getByLabelText("token_limit"), "64");
    await user.type(screen.getByLabelText("access_token"), "never-persist-this-token");
    await user.click(screen.getByRole("button", { name: "Check run plan" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Run on device" })).toBeEnabled());
    expect((bodies[0].parameters as Record<string, unknown>).token_limit).toBe(64);
    expect(sessionStorage.getItem(`lmw.recipe-launch.${D}`)).not.toContain("never-persist-this-token");
    view.rerenderDialog(false, D);
    view.rerenderDialog(true, D);
    await user.click(await screen.findByRole("button", { name: "Restore device choices" }));
    expect(screen.getByLabelText("Rank 0 node")).toHaveValue("n-spark3");
    expect(screen.getByLabelText("token_limit")).toHaveValue("64");
    expect(screen.getByLabelText("access_token")).toHaveValue("");
    expect(screen.getByRole("button", { name: "Run on device" })).toBeDisabled();
    expect(bodies).toHaveLength(1);
  });
});
