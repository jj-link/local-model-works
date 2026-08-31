/// <reference types="@testing-library/jest-dom" />
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { MemoryRouter } from "react-router";
import { describe, expect, it } from "vitest";

import DashboardRoute from "~/routes/dashboard";
import { server } from "../msw/server";

function renderDashboard() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <DashboardRoute />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("DashboardRoute", () => {
  it("shows only active deployments and omits activity and all links", async () => {
    server.use(
      http.get("*/api/v1/nodes", () => HttpResponse.json([])),
      http.get("*/api/v1/deployments", () =>
        HttpResponse.json([
          {
            id: "active",
            recipe_digest: "sha256:qwen",
            recipe_name: "qwen",
            parameters: {},
            placements: [],
            desired_state: "running",
            observed_state: "degraded",
            updated_at: "2026-08-25T12:00:00Z",
          },
          {
            id: "stopped-duplicate",
            recipe_digest: "sha256:qwen",
            recipe_name: "qwen",
            parameters: {},
            placements: [],
            desired_state: "stopped",
            observed_state: "stopped",
            updated_at: "2026-08-25T11:00:00Z",
          },
          {
            id: "stopping",
            recipe_digest: "sha256:other",
            recipe_name: "other",
            parameters: {},
            placements: [],
            desired_state: "stopped",
            observed_state: "stopping",
            updated_at: "2026-08-25T12:00:00Z",
          },
        ]),
      ),
      http.get("*/api/v1/recipes", () => HttpResponse.json([])),
      http.get("*/api/v1/runs", () => HttpResponse.json({ items: [] })),
    );

    renderDashboard();

    const table = await screen.findByRole("table", { name: "Active deployments" });
    expect(within(table).getAllByRole("row")).toHaveLength(3);
    expect(within(table).getAllByText("qwen")).toHaveLength(1);
    expect(within(table).queryByText("stopped-duplicate")).not.toBeInTheDocument();
    expect(screen.queryByText(/live activity/i)).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "all →" })).not.toBeInTheDocument();
  });
});
