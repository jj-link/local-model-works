/// <reference types="@testing-library/jest-dom" />
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { MemoryRouter, useLocation } from "react-router";
import { describe, expect, it, vi } from "vitest";

import DeploymentDetailRoute from "~/routes/serving/deployments/$id";
import { server } from "../msw/server";

vi.mock("~/components/log-pane", () => ({
  LogPane: () => <div data-testid="log-pane" />,
}));

const baseDeployment = {
  id: "dep-1",
  recipe_digest: "sha256:recipe",
  recipe_name: "Qwen",
  recipe_version: "1.0.0",
  profile: "default",
  run_id: "run-1",
  desired_state: "running",
  observed_state: "healthy",
  placements: [],
  diagnostics: [],
};

const baseRun = {
  id: "run-1",
  module: "serving",
  kind: "serve",
  state: "running",
  resources: { nodes: [], accelerators: [], fabrics: [] },
  input: {},
  progress: {},
  created_at: "2026-08-24T00:00:00Z",
};

function LocationProbe() {
  return <output data-testid="location">{useLocation().pathname}</output>;
}

function renderDetail(
  deploymentOverrides: Record<string, unknown> = {},
  runOverrides: Record<string, unknown> = {},
) {
  const deployment = { ...baseDeployment, ...deploymentOverrides };
  const run = { ...baseRun, ...runOverrides };
  server.use(
    http.get("*/api/v1/deployments/dep-1", () => HttpResponse.json(deployment)),
    http.get("*/api/v1/deployments/deployments", () => HttpResponse.json(null, { status: 404 })),
    http.get("*/api/v1/nodes", () => HttpResponse.json([])),
    http.get("*/api/v1/runs/run-1", () => HttpResponse.json(run)),
    http.delete("*/api/v1/deployments/dep-1", () => new HttpResponse(null, { status: 204 })),
  );
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/serving/deployments/dep-1"]}>
        <DeploymentDetailRoute />
        <LocationProbe />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("Deployment lifecycle detail", () => {
  it("shows Verify only for healthy deployments and Stop while desired running", async () => {
    renderDetail();

    expect(await screen.findByRole("button", { name: "Verify" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Stop" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Retry stop" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Start" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Restart" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Delete" })).not.toBeInTheDocument();
    expect(screen.getByText("desired")).toBeInTheDocument();
    expect(screen.getByText("observed")).toBeInTheDocument();
  });

  it("exposes Retry stop while desired stopped remains unresolved", async () => {
    renderDetail({ desired_state: "stopped", observed_state: "stopping" }, { state: "cancelling" });

    expect(await screen.findByRole("button", { name: "Retry stop" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Verify" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Stop" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Start" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Restart" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Delete" })).not.toBeInTheDocument();
  });

  it("shows failed-run source, Restart, crash logs, and navigates after Delete", async () => {
    const user = userEvent.setup();
    renderDetail(
      { desired_state: "stopped", observed_state: "stopped" },
      {
        state: "failed",
        error_code: "workload.oom_killed",
        error_message: "rank 0; container c1; state=exited; exit_code=137; oom_killed=true",
      },
    );

    expect(await screen.findByRole("button", { name: "Restart" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Delete" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Start" })).not.toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Last failure" })).toBeInTheDocument();
    expect(screen.getByText("workload.oom_killed")).toBeInTheDocument();
    expect(screen.getByText(/exit_code=137/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "View crash logs" })).toHaveAttribute("href", "/runs/run-1");

    await user.click(screen.getByRole("button", { name: "Delete" }));
    const dialog = await screen.findByRole("dialog");
    await user.click(within(dialog).getByRole("button", { name: "delete" }));
    await waitFor(() => {
      expect(screen.getByTestId("location")).toHaveTextContent("/serving/deployments");
    });
  });

  it("uses Start for a fully stopped nonfailed run", async () => {
    renderDetail({ desired_state: "stopped", observed_state: "stopped" }, { state: "cancelled" });

    expect(await screen.findByRole("button", { name: "Start" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Delete" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Restart" })).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Last failure" })).not.toBeInTheDocument();
  });
});
