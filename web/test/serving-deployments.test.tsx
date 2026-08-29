/// <reference types="@testing-library/jest-dom" />
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { MemoryRouter } from "react-router";
import { describe, expect, it } from "vitest";

import DeploymentsRoute from "~/routes/serving/deployments/index";
import { server } from "../msw/server";

const deployments = [
  {
    id: "dep-healthy",
    recipe_digest: "sha256:healthy",
    recipe_name: "Healthy Model",
    profile: "default",
    desired_state: "running",
    observed_state: "healthy",
    placements: [],
  },
  {
    id: "dep-stopping",
    recipe_digest: "sha256:stopping",
    recipe_name: "Stopping Model",
    profile: "default",
    desired_state: "stopped",
    observed_state: "stopping",
    placements: [],
  },
  {
    id: "dep-stopped",
    recipe_digest: "sha256:stopped",
    recipe_name: "Stopped Model",
    profile: "default",
    desired_state: "stopped",
    observed_state: "stopped",
    placements: [],
  },
];

function renderRoute() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/serving/deployments"]}>
        <DeploymentsRoute />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("Serving deployments", () => {
  it("keeps transitions active and stopped history discoverable", async () => {
    server.use(
      http.get("*/api/v1/deployments", () => HttpResponse.json(deployments)),
      http.get("*/api/v1/recipes", () => HttpResponse.json([])),
      http.get("*/api/v1/nodes", () => HttpResponse.json([])),
      http.get("*/api/v1/fabrics", () => HttpResponse.json([])),
    );
    renderRoute();
    await screen.findByText("Healthy Model");

    const activeHeading = screen.getByRole("heading", { name: "Active deployments" });
    const stoppedHeading = screen.getByRole("heading", { name: "Stopped deployments" });
    const activeSection = activeHeading.closest("section");
    const stoppedSection = stoppedHeading.closest("section");
    expect(activeSection).not.toBeNull();
    expect(stoppedSection).not.toBeNull();

    expect(within(activeSection as HTMLElement).getByText("Healthy Model")).toBeInTheDocument();
    expect(within(activeSection as HTMLElement).getByText("Stopping Model")).toBeInTheDocument();
    expect(within(activeSection as HTMLElement).queryByText("Stopped Model")).not.toBeInTheDocument();
    expect(within(stoppedSection as HTMLElement).getByText("Stopped Model")).toBeInTheDocument();
    expect(within(stoppedSection as HTMLElement).queryByText("Stopping Model")).not.toBeInTheDocument();

    expect(within(activeSection as HTMLElement).getAllByRole("link", { name: "Manage →" })).toHaveLength(2);
    expect(within(stoppedSection as HTMLElement).getByRole("link", { name: "Start →" })).toHaveAttribute(
      "href",
      "/serving/deployments/dep-stopped",
    );
    expect(screen.getByRole("button", { name: "Launch deployment" })).toBeInTheDocument();
  });
});
