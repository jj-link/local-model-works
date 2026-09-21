/// <reference types="@testing-library/jest-dom" />
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { MemoryRouter } from "react-router";
import { beforeEach, describe, expect, it } from "vitest";
import { RecipeUpdateButton } from "~/components/recipes/recipe-update-button";
import { server } from "../msw/server";

const oldRecipe = { digest: `sha256:${"a".repeat(64)}`, name: "GLM", version: "1.0.0" };
const newRecipe = { ...oldRecipe, digest: `sha256:${"b".repeat(64)}`, version: "2.0.0" };
const devices = [
  { node_id: "spark2", node_name: "Spark 2", node_status: "online", installed_digests: [oldRecipe.digest] },
  { node_id: "spark3", node_name: "Spark 3", node_status: "online", installed_digests: [oldRecipe.digest] },
];
const repository = { id: "repo", current_recipe: newRecipe, versions: [oldRecipe, newRecipe].map((recipe) => ({ recipe })), installed_devices: devices };
const plan = {
  repository_id: "repo", target_digest: newRecipe.digest, target_version: newRecipe.version, up_to_date: false,
  installed_devices: devices, deployment_ids: [], unchanged_deployment_ids: [], deployments: [], running_deployments: [],
  current_permissions: [], candidate_permissions: [], added_permissions: [], removed_permissions: [],
  diagnostics: [], ready: true, plan_digest: "reviewed-devices",
};

function renderUpdate() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(<QueryClientProvider client={client}><MemoryRouter><RecipeUpdateButton recipeDigest={oldRecipe.digest} /></MemoryRouter></QueryClientProvider>);
}

beforeEach(() => {
  server.use(
    http.get("*/api/v1/recipe-repositories", () => HttpResponse.json([repository])),
    http.post("*/api/v1/recipe-repositories/repo/updates/plan", () => HttpResponse.json(plan)),
  );
});

describe("device recipe updates", () => {
  it("shows the affected devices before installing and leaves cancellation side-effect free", async () => {
    let installations = 0;
    server.use(
      http.post("*/api/v1/recipe-repositories/repo/updates", async ({ request }) => {
        const approved = await request.json() as { target_digest?: string; plan_digest?: string };
        if (approved.target_digest !== newRecipe.digest || approved.plan_digest !== plan.plan_digest) {
          return HttpResponse.json({ code: "update.changed", message: "Devices changed after review" }, { status: 412 });
        }
        installations++;
        return HttpResponse.json({ run_id: "update" }, { status: 202 });
      }),
      http.get("*/api/v1/runs/update", () => HttpResponse.json({ id: "update", kind: "recipe-update", state: "succeeded", progress: { installed_devices: devices.map((device) => ({ ...device, status: "succeeded" })) } })),
    );
    const user = userEvent.setup();
    renderUpdate();
    await waitFor(() => expect(screen.getByRole("button", { name: "Update" })).toBeEnabled());
    await user.click(screen.getByRole("button", { name: "Update" }));
    const targets = await screen.findByRole("list", { name: "Update devices" });
    expect(within(targets).getByText("Spark 2")).toBeVisible();
    expect(within(targets).getByText("Spark 3")).toBeVisible();
    expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
    expect(installations).toBe(0);
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(installations).toBe(0);

    await user.click(screen.getByRole("button", { name: "Update" }));
    await user.click(await screen.findByRole("button", { name: "Install update" }));
    const progress = await screen.findByRole("list", { name: "Device update progress" });
    await waitFor(() => expect(within(progress).getAllByText("Updated")).toHaveLength(2));
    expect(installations).toBe(1);
  });

  it("does not replay an installation with an uncertain result", async () => {
    let installations = 0;
    server.use(http.post("*/api/v1/recipe-repositories/repo/updates", () => {
      installations++;
      return HttpResponse.json({ code: "update.unavailable", message: "Update result unavailable" }, { status: 503 });
    }));
    const user = userEvent.setup();
    renderUpdate();
    await waitFor(() => expect(screen.getByRole("button", { name: "Update" })).toBeEnabled());
    await user.click(screen.getByRole("button", { name: "Update" }));
    await user.click(await screen.findByRole("button", { name: "Install update" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Update result unavailable");
    const confirm = screen.getByRole("button", { name: "Install update" });
    expect(confirm).toBeDisabled();
    await user.click(confirm);
    expect(installations).toBe(1);
  });

  it("cannot replace a fresh device confirmation with a cancelled late preview", async () => {
    let previews = 0;
    let release: (() => void) | undefined;
    server.use(http.post("*/api/v1/recipe-repositories/repo/updates/plan", async () => {
      if (++previews === 1) {
        await new Promise<void>((resolve) => { release = resolve; });
        return HttpResponse.json({ ...plan, installed_devices: [devices[0]] });
      }
      return HttpResponse.json({ ...plan, installed_devices: [devices[1]], plan_digest: "second-device" });
    }));
    const user = userEvent.setup();
    renderUpdate();
    await waitFor(() => expect(screen.getByRole("button", { name: "Update" })).toBeEnabled());
    await user.click(screen.getByRole("button", { name: "Update" }));
    await waitFor(() => expect(previews).toBe(1));
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    await user.click(screen.getByRole("button", { name: "Update" }));
    const targets = await screen.findByRole("list", { name: "Update devices" });
    await act(async () => { release?.(); });
    expect(within(targets).getByText("Spark 3")).toBeVisible();
    expect(within(targets).queryByText("Spark 2")).not.toBeInTheDocument();
  });
});
