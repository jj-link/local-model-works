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
  it("starts a single-device recipe with defaults without opening settings or restoring browser choices", async () => {
    const planned: Record<string, unknown>[] = [];
    let createBody: Record<string, unknown> | undefined;
    const singlePlan = { ...plan, placements: [plan.placements[0]] };
    installHandlers((body) => { planned.push(body); return singlePlan; }, (body) => { createBody = body; });
    server.use(http.get(`*/api/v1/recipes/${D}`, () => HttpResponse.json({
      ...recipeDetail, compatibility: { ...recipe.compatibility, nodeCount: 1 },
      manifest: { ...recipeDetail.manifest, parameters: [{ name: "context_length", type: "integer", default: 4096 }] },
    })));
    sessionStorage.setItem(`lmw.recipe-launch.${D}`, JSON.stringify({ nodes: ["n-spark3"], parameters: { context_length: 8192 } }));
    const user = userEvent.setup();
    renderDialog(true, D);
    expect(await screen.findByLabelText("Device")).toHaveValue("");
    expect(screen.queryByRole("searchbox")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Restore device choices" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Check run plan" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Run" })).toBeDisabled();
    expect(planned).toEqual([]);
    await user.selectOptions(screen.getByLabelText("Device"), "n-spark2");
    await waitFor(() => expect(screen.getByRole("button", { name: "Run" })).toBeEnabled());
    expect(createBody).toBeUndefined();
    await user.click(screen.getByRole("button", { name: "Run" }));
    await waitFor(() => expect(createBody).toMatchObject({
      acquisition_policy: "download-missing", parameters: { context_length: 4096 },
      placements: [{ rank: 0, node_id: "n-spark2" }], plan_digest: singlePlan.plan_digest,
    }));
  });

  it("requires all cluster devices and explicit Run before executing a source-owned recipe", async () => {
    const planned: Record<string, unknown>[] = [];
    let createdCount = 0;
    installHandlers((body) => { planned.push(body); return { ...plan, risks: ["host.upstream-exec"] }; }, () => { createdCount++; });
    server.use(http.get(`*/api/v1/recipes/${D}`, () => HttpResponse.json({
      ...recipeDetail, manifest: { ...recipeDetail.manifest, workloads: [{ upstream: { start: ["bash", "start.sh"] } }] },
    })));
    const user = userEvent.setup();
    renderDialog();
    await screen.findByRole("option", { name: new RegExp(recipe.name) });
    await user.selectOptions(await screen.findByLabelText("Recipe"), D);
    await user.selectOptions(await screen.findByLabelText("Head device"), "n-spark2");
    expect(planned).toEqual([]);
    expect(screen.getByRole("button", { name: "Run" })).toBeDisabled();
    await user.selectOptions(screen.getByLabelText("Worker 1"), "n-spark3");
    await waitFor(() => expect(screen.getByRole("button", { name: "Run" })).toBeEnabled());
    expect(screen.getByText(/Clicking Run authorizes/)).toBeVisible();
    expect(createdCount).toBe(0);
    await user.click(screen.getByRole("button", { name: "Run" }));
    await waitFor(() => expect(createdCount).toBe(1));
  });

  it("never enables a changed request from an older in-flight preview", async () => {
    installHandlers();
    let releaseOld: (() => void) | undefined;
    let releaseCurrent: (() => void) | undefined;
    let createBody: Record<string, unknown> | undefined;
    const currentPlan = { ...plan, plan_digest: "sha256:current", placements: [
      { node_id: "n-spark3", rank: 0 }, { node_id: "n-spark2", rank: 1 },
    ] };
    server.use(
      http.post("*/api/v1/deployments/plan", async ({ request }) => {
        const body = await request.json() as { placements: { node_id: string }[] };
        if (body.placements[0].node_id === "n-spark2") {
          await new Promise<void>((resolve) => { releaseOld = resolve; });
          return HttpResponse.json(plan);
        }
        await new Promise<void>((resolve) => { releaseCurrent = resolve; });
        return HttpResponse.json(currentPlan);
      }),
      http.post("*/api/v1/deployments", async ({ request }) => {
        createBody = await request.json() as Record<string, unknown>;
        return HttpResponse.json(created, { status: 201 });
      }),
    );
    const user = userEvent.setup();
    renderDialog(true, D);
    await user.selectOptions(await screen.findByLabelText("Head device"), "n-spark2");
    await user.selectOptions(screen.getByLabelText("Worker 1"), "n-spark3");
    await waitFor(() => expect(releaseOld).toBeTypeOf("function"));
    await user.selectOptions(screen.getByLabelText("Head device"), "n-spark3");
    await user.selectOptions(screen.getByLabelText("Worker 1"), "n-spark2");
    await waitFor(() => expect(releaseCurrent).toBeTypeOf("function"));
    releaseOld!();
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Checking device readiness"));
    expect(screen.getByRole("button", { name: "Run" })).toBeDisabled();
    releaseCurrent!();
    await waitFor(() => expect(screen.getByRole("button", { name: "Run" })).toBeEnabled());
    await user.click(screen.getByRole("button", { name: "Run" }));
    await waitFor(() => expect(createBody).toMatchObject({ placements: currentPlan.placements, plan_digest: currentPlan.plan_digest }));
  });

  it("keeps inferred settings device-dependent when another setting is edited or filtered", async () => {
    let createBody: Record<string, unknown> | undefined;
    installHandlers((body) => {
      const placements = body.placements as { node_id: string; rank: number }[];
      return { ...plan, placements, parameters: {
        worker_home: placements[1].node_id === "n-spark3" ? "/home/worker-three" : "/home/worker-two",
        ...body.parameters as Record<string, unknown>,
      } };
    }, (body) => { createBody = body; });
    server.use(http.get(`*/api/v1/recipes/${D}`, () => HttpResponse.json({
      ...recipeDetail, manifest: { ...recipeDetail.manifest, parameters: [
        { name: "context_length", label: "Context length", type: "integer", default: 4096, group: "Memory" },
        { name: "worker_home", label: "Worker home", type: "string", group: "Device" },
      ] },
    })));
    const user = userEvent.setup();
    renderDialog(true, D);
    await user.selectOptions(await screen.findByLabelText("Head device"), "n-spark2");
    await user.selectOptions(screen.getByLabelText("Worker 1"), "n-spark3");
    await waitFor(() => expect(screen.getByRole("button", { name: "Run" })).toBeEnabled());
    await user.click(screen.getByText("Settings", { exact: false, selector: "summary" }));
    expect(await screen.findByLabelText("Worker home")).toHaveValue("/home/worker-three");
    await user.type(screen.getByRole("searchbox"), "context");
    expect(screen.queryByLabelText("Worker home")).not.toBeInTheDocument();
    await user.clear(screen.getByLabelText("Context length"));
    await user.type(screen.getByLabelText("Context length"), "8192");
    await user.clear(screen.getByRole("searchbox"));
    await user.selectOptions(screen.getByLabelText("Head device"), "n-spark3");
    await user.selectOptions(screen.getByLabelText("Worker 1"), "n-spark2");
    await waitFor(() => expect(screen.getByLabelText("Worker home")).toHaveValue("/home/worker-two"));
    expect(screen.getByLabelText("Context length")).toHaveValue(8192);
    await user.click(screen.getByText("Settings", { exact: false, selector: "summary" }));
    await user.click(screen.getByRole("button", { name: "Run" }));
    await waitFor(() => expect(createBody).toBeDefined());
    expect(createBody?.parameters).toEqual({ context_length: 8192 });
  });

  it("keeps Run blocked until corrected prerequisites pass a requested check", async () => {
    let prerequisitesReady = false;
    let created = false;
    const requests: Record<string, unknown>[] = [];
    installHandlers((body) => {
      requests.push(body);
      return prerequisitesReady ? plan : { ...plan, ready: false, diagnostics: [
        { code: "upstream.ssh_failed", severity: "error", message: "The head cannot authenticate to the selected worker." },
        { code: "fabric.unavailable", severity: "error", message: "The required device connection is unavailable." },
      ] };
    }, () => { created = true; });
    const user = userEvent.setup();
    renderDialog(true, D);
    await user.selectOptions(await screen.findByLabelText("Head device"), "n-spark2");
    await user.selectOptions(screen.getByLabelText("Worker 1"), "n-spark3");
    const alerts = await screen.findAllByRole("alert");
    expect(alerts).toHaveLength(2);
    for (const alert of alerts) expect(alert).toBeVisible();
    expect(screen.getByRole("button", { name: "Run" })).toBeDisabled();
    expect(screen.getByRole("link", { name: "Open recipe configuration" })).toHaveAttribute("href", `/library/recipes/packages/${encodeURIComponent(D)}`);
    prerequisitesReady = true;
    await user.click(screen.getByRole("button", { name: "Retry readiness check" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Run" })).toBeEnabled());
    expect(requests).toHaveLength(2);
    expect(requests[1]).toEqual(requests[0]);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(created).toBe(false);
  });

  it("retains selections after a preview failure and retries only on request", async () => {
    const bodies: Record<string, unknown>[] = [];
    installHandlers();
    server.use(http.post("*/api/v1/deployments/plan", async ({ request }) => {
      bodies.push(await request.json() as Record<string, unknown>);
      return bodies.length === 1 ? HttpResponse.json({ code: "planner.unavailable", message: "planner unavailable" }, { status: 503 }) : HttpResponse.json(plan);
    }));
    const user = userEvent.setup();
    renderDialog(true, D);
    await user.selectOptions(await screen.findByLabelText("Head device"), "n-spark2");
    await user.selectOptions(screen.getByLabelText("Worker 1"), "n-spark3");
    expect(await screen.findByRole("alert")).toHaveTextContent("planner unavailable");
    expect(screen.getByLabelText("Head device")).toHaveValue("n-spark2");
    expect(screen.getByRole("button", { name: "Run" })).toBeDisabled();
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Run" })).toBeEnabled());
    expect(bodies).toHaveLength(2);
  });

  it("invalidates readiness when the file policy changes and never replays an uncertain Run", async () => {
    const planned: Record<string, unknown>[] = [];
    let attempts = 0;
    installHandlers((body) => { planned.push(body); return plan; });
    server.use(http.post("*/api/v1/deployments", () => { attempts++; return HttpResponse.json({ message: "response interrupted" }, { status: 503 }); }));
    const user = userEvent.setup();
    renderDialog(true, D);
    await user.selectOptions(await screen.findByLabelText("Head device"), "n-spark2");
    await user.selectOptions(screen.getByLabelText("Worker 1"), "n-spark3");
    await waitFor(() => expect(screen.getByRole("button", { name: "Run" })).toBeEnabled());
    await user.click(screen.getByText("Settings", { exact: false, selector: "summary" }));
    await user.click(await screen.findByRole("radio", { name: "Use existing files only" }));
    expect(screen.getByRole("button", { name: "Run" })).toBeDisabled();
    await waitFor(() => expect(screen.getByRole("button", { name: "Run" })).toBeEnabled());
    expect(planned.at(-1)?.acquisition_policy).toBe("require-existing");
    await user.click(screen.getByRole("button", { name: "Run" }));
    expect(await screen.findByText(/The run may have been accepted/)).toBeVisible();
    expect(screen.getByRole("button", { name: "Run" })).toBeDisabled();
    expect(attempts).toBe(1);
  });
});
