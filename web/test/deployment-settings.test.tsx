/// <reference types="@testing-library/jest-dom" />
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { MemoryRouter, useLocation } from "react-router";
import { beforeEach, describe, expect, it } from "vitest";
import { DeploymentSettings } from "~/components/recipes/deployment-settings";
import { server } from "../msw/server";

const A = `sha256:${"a".repeat(64)}`;
const B = `sha256:${"b".repeat(64)}`;
const context = { name: "context_length", label: "Context length", type: "int", default: 4096 };
const recipes = [
  { digest: A, name: "fixture", version: "1", manifest: { parameters: [context], artifacts: [], workloads: [{ name: "native" }] } },
  { digest: B, name: "fixture", version: "2", manifest: { parameters: [{ name: "batch_size", label: "Batch size", type: "int", default: 32 }], artifacts: [], workloads: [{ name: "native" }, { name: "upstream" }] } },
];
const deployment = { id: "dep", recipe_digest: A, recipe_name: "fixture", recipe_version: "1", parameters: { context_length: 4096 }, variants: {}, workload_index: 0, placements: [{ node_id: "node", rank: 0 }], desired_state: "running", observed_state: "healthy" };
const effectivePlan = { recipe_digest: A, recipe_name: "fixture", recipe_version: "1", workload_index: 0, parameters: { context_length: 8192 }, variants: {}, placements: deployment.placements, transfers: [], conflicts: [], diagnostics: [], risks: [], ready: true, plan_digest: "effective" };
const review = { repository_id: "", target_digest: A, deployment_ids: ["dep"], unchanged_deployment_ids: [], current_permissions: [], candidate_permissions: [], added_permissions: [], removed_permissions: [], installed_devices: [], running_deployments: [], diagnostics: [], ready: true, plan_digest: "reviewed", deployments: [{ source_deployment_id: "dep", source_digest: A, current_permissions: [], added_permissions: [], removed_permissions: [], deployment_plan: effectivePlan }] };

function Location() {
  const location = useLocation();
  return <output aria-label="Current route">{location.pathname}</output>;
}

function renderSettings() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return { ...render(<QueryClientProvider client={client}><MemoryRouter><DeploymentSettings deploymentID="dep" /><Location /></MemoryRouter></QueryClientProvider>), client };
}

beforeEach(() => {
  sessionStorage.clear();
  server.use(
    http.get("*/api/v1/deployments/dep", () => HttpResponse.json(deployment)),
    http.get("*/api/v1/recipe-repositories", () => HttpResponse.json([])),
    http.get("*/api/v1/recipes/:digest", ({ params }) => HttpResponse.json(recipes.find((recipe) => recipe.digest === decodeURIComponent(String(params.digest))))),
  );
});

describe("deployment configuration", () => {
  it.each(["running", "stopped"])("reviews and applies a %s standalone deployment without a repository", async (state) => {
    const user = userEvent.setup();
    const submissions: unknown[] = [];
    server.use(
      http.get("*/api/v1/deployments/dep", () => HttpResponse.json({ ...deployment, desired_state: state, observed_state: state === "running" ? "healthy" : "stopped" })),
      http.post("*/api/v1/deployments/dep/configuration/plan", async ({ request }) => {
        submissions.push(await request.json());
        return HttpResponse.json(review);
      }),
      http.post("*/api/v1/deployments/dep/configuration", async ({ request }) => {
        submissions.push(await request.json());
        return HttpResponse.json({ run_id: "configuration-run" }, { status: 202 });
      }),
    );
    renderSettings();
    const input = await screen.findByRole("spinbutton", { name: "Context length" });
    await user.clear(input);
    await user.type(input, "8192");
    expect(screen.queryByRole("combobox", { name: "Recipe version" })).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Review changes and restart" }));
    const apply = await screen.findByRole("button", { name: "Apply and restart" });
    expect(apply).toBeDisabled();
    expect(within(screen.getByRole("region", { name: "Effective launch selections" })).getByText(/8192/)).toBeVisible();
    await user.click(screen.getByRole("checkbox", { name: /I approve the reviewed settings/ }));
    await user.click(apply);
    await waitFor(() => expect(screen.getByLabelText("Current route")).toHaveTextContent("/runs/configuration-run"));
    expect(submissions).toEqual([
      { parameters: { context_length: 8192 }, variants: {}, workload_index: 0 },
      { parameters: { context_length: 8192 }, variants: {}, workload_index: 0, plan_digest: "reviewed" },
    ]);
  });

  it("invalidates consent when another run replaces the saved deployment", async () => {
    const user = userEvent.setup();
    server.use(http.post("*/api/v1/deployments/dep/configuration/plan", () => HttpResponse.json(review)));
    const { client } = renderSettings();
    const input = await screen.findByRole("spinbutton", { name: "Context length" });
    await user.clear(input);
    await user.type(input, "8192");
    await user.click(screen.getByRole("button", { name: "Review changes and restart" }));
    const apply = await screen.findByRole("button", { name: "Apply and restart" });
    await user.click(screen.getByRole("checkbox", { name: /I approve the reviewed settings/ }));
    expect(apply).toBeEnabled();

    server.use(http.get("*/api/v1/deployments/dep", () => HttpResponse.json({ ...deployment, run_id: "another-run" })));
    await act(async () => { await client.invalidateQueries(); });
    expect(await screen.findByRole("alert")).toBeVisible();
    expect(screen.getByRole("spinbutton", { name: "Context length" })).toHaveValue(8192);
    expect(screen.queryByRole("button", { name: "Apply and restart" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Review changes and restart" })).toBeDisabled();
    await user.click(screen.getByRole("button", { name: "Reload saved deployment settings" }));
    expect(screen.getByRole("spinbutton", { name: "Context length" })).toHaveValue(4096);
  });

  it("retains edits by digest while filtering the selected version request", async () => {
    const user = userEvent.setup();
    const submissions: unknown[] = [];
    server.use(
      http.get("*/api/v1/recipe-repositories", () => HttpResponse.json([{ id: "repo", current_recipe: recipes[1], versions: recipes.map((recipe) => ({ recipe })) }])),
      http.post("*/api/v1/recipe-repositories/repo/replacements/plan", async ({ request }) => { submissions.push(await request.json()); return HttpResponse.json({ ...review, target_digest: B }); }),
    );
    renderSettings();
    const input = await screen.findByRole("spinbutton", { name: "Context length" });
    await user.clear(input);
    await user.type(input, "12288");
    await user.selectOptions(await screen.findByRole("combobox", { name: "Recipe version" }), B);
    expect(await screen.findByRole("spinbutton", { name: "Batch size" })).toHaveValue(32);
    await user.click(screen.getByRole("button", { name: "Review changes and restart" }));
    await waitFor(() => expect(submissions).toEqual([{ target_digest: B, deployment_ids: ["dep"], deployment_settings: { dep: { parameters: {}, variants: {}, workload_index: 0 } } }]));
    await user.selectOptions(screen.getByRole("combobox", { name: "Recipe version" }), A);
    expect(await screen.findByRole("spinbutton", { name: "Context length" })).toHaveValue(12288);
    await user.click(screen.getByRole("button", { name: "Reset to saved deployment settings" }));
    await user.selectOptions(screen.getByRole("combobox", { name: "Recipe version" }), B);
    await screen.findByRole("spinbutton", { name: "Batch size" });
    await user.selectOptions(screen.getByRole("combobox", { name: "Recipe version" }), A);
    expect(await screen.findByRole("spinbutton", { name: "Context length" })).toHaveValue(4096);
  });

  it("shows the selected revision, resolved source edits and automatic workload before renewed consent", async () => {
    const user = userEvent.setup();
    const revision = "0123456789abcdef0123456789abcdef01234567";
    const upstream = { node_id: "node", rank: 0, source: { url: "https://fixtures.local/upstream", revision, path: "launch" }, execution: { start: ["bash", "start.sh"], stop: ["bash", "stop.sh"], configuration: [{ path: "start.sh", sha256: "original-source", edits: [{ start: 10, end: 12, parameter: "batch_size", format: "shell" }, { start: 20, end: 25, variable: "CACHE_ROOT", format: "shell-word" }] }] }, environment: { BATCH_SIZE: "32" }, configuration: [{ path: "start.sh", sha256: "original-source", edits: [{ start: 10, end: 12, replacement: "'32'" }, { start: 20, end: 25, replacement: "$(printf '%q' \"${CACHE_ROOT}\")" }] }] };
    const plan = { ...effectivePlan, recipe_digest: B, recipe_version: "2", workload_index: 1, parameters: { batch_size: 32 }, risks: ["host.upstream-exec"], upstream: [upstream] };
    server.use(
      http.get("*/api/v1/recipe-repositories", () => HttpResponse.json([{ id: "repo", current_recipe: recipes[1], versions: recipes.map((recipe) => ({ recipe })) }])),
      http.post("*/api/v1/recipe-repositories/repo/replacements/plan", () => HttpResponse.json({ ...review, target_digest: B, deployments: [{ ...review.deployments[0], deployment_plan: plan }] })),
    );
    renderSettings();
    await user.selectOptions(await screen.findByRole("combobox", { name: "Recipe version" }), B);
    await user.selectOptions(await screen.findByRole("combobox", { name: "Runtime" }), "");
    await user.click(screen.getByRole("button", { name: "Review changes and restart" }));
    expect(await screen.findByText(revision, { exact: true })).toBeVisible();
    expect(screen.getByText('"bash" "start.sh"')).toBeVisible();
    expect(screen.getByText(/10–12 · Parameter batch_size: '32'/)).toBeVisible();
    expect(screen.getByText(/Preserve upstream variable CACHE_ROOT/)).toBeVisible();
    const selections = screen.getByRole("region", { name: "Effective launch selections" });
    expect(within(selections).getByText(/Runtime index: 1/)).toBeVisible();
    expect(within(selections).getByText(/"batch_size": 32/)).toBeVisible();
    const apply = screen.getByRole("button", { name: "Apply and restart" });
    await user.click(screen.getByRole("checkbox", { name: /I approve the reviewed settings/ }));
    expect(apply).toBeDisabled();
    await user.click(screen.getByRole("checkbox", { name: /I authorize original upstream code/ }));
    expect(apply).toBeEnabled();
    await user.click(screen.getByRole("button", { name: "Edit settings" }));
    await user.clear(screen.getByRole("spinbutton", { name: "Batch size" }));
    await user.type(screen.getByRole("spinbutton", { name: "Batch size" }), "64");
    expect(screen.queryByRole("button", { name: "Apply and restart" })).not.toBeInTheDocument();
  });
});
