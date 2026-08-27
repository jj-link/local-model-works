/// <reference types="@testing-library/jest-dom" />
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { MemoryRouter } from "react-router";
import { describe, expect, it } from "vitest";

import NodesRoute from "~/routes/fleet/nodes/index";
import DeploymentsRoute from "~/routes/serving/deployments/index";
import { server } from "../msw/server";

const idleNode = {
  id: "11111111-1111-4111-8111-111111111111",
  display_name: "Idle Device",
  status: "online",
  created_at: "2026-01-01T00:00:00Z",
  last_heartbeat: "2026-01-03T00:00:00Z",
};
const busyNode = {
  id: "22222222-2222-4222-8222-222222222222",
  display_name: "Busy Device",
  status: "online",
  created_at: "2026-01-01T00:00:00Z",
  last_heartbeat: "2026-01-03T00:00:00Z",
  inventory: {
    arch: "amd64",
    hostname: "busy",
    os: "linux",
    docker: { ok: true, version: "1" },
    accelerators: [{ index: 0, uuid: "gpu-1", vendor: "NVIDIA", name: "RTX PRO 6000", memory_bytes: 103079215104 }],
  },
};

const deployments = [
  {
    id: "33333333-3333-4333-8333-333333333333",
    recipe_digest: "sha256:recipe-a",
    recipe_name: "Recipe A",
    engine: "vllm",
    profile: "fast",
    placements: [{ node_id: busyNode.id, node_name: busyNode.display_name, rank: 0 }],
    desired_state: "running",
    observed_state: "healthy",
    endpoint: { host: "busy", port: 8888, model: "Model A" },
    created_at: "2026-01-02T00:00:00Z",
    updated_at: "2026-01-03T00:00:00Z",
  },
  {
    id: "55555555-5555-4555-8555-555555555555",
    recipe_digest: "sha256:recipe-a-old",
    recipe_name: "Recipe A",
    engine: "vllm",
    profile: "fast",
    placements: [{ node_id: busyNode.id, node_name: busyNode.display_name, rank: 0 }],
    desired_state: "stopped",
    observed_state: "stopped",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T01:00:00Z",
  },
  {
    id: "44444444-4444-4444-8444-444444444444",
    recipe_digest: "sha256:recipe-b",
    recipe_name: "Recipe B",
    profile: "",
    placements: [{ node_id: busyNode.id, node_name: busyNode.display_name, rank: 0 }],
    desired_state: "running",
    observed_state: "degraded",
    endpoint: { host: "busy", port: 8889 },
    created_at: "2026-01-02T00:00:00Z",
    updated_at: "2026-01-03T00:00:00Z",
  },
];

function renderRoute(component: React.ReactNode, path: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>{component}</MemoryRouter>
    </QueryClientProvider>,
  );
}

function installHandlers() {
  server.use(
    http.get("*/api/v1/nodes", () => HttpResponse.json([idleNode, busyNode])),
    http.get("*/api/v1/nodes/telemetry", () => HttpResponse.json([])),
    http.get("*/api/v1/deployments", () => HttpResponse.json(deployments)),
    http.get("*/api/v1/deployments/telemetry", () => HttpResponse.json([])),
    http.get("*/api/v1/recipes", () => HttpResponse.json([])),
    http.get("*/api/v1/fabrics", () => HttpResponse.json([])),
  );
}

describe("Fleet and Serving workload views", () => {
  it("renders card rows with truthful joins, engine/model fallback, recipe link, degraded missing metadata, and visible idle node", async () => {
    installHandlers();
    renderRoute(<NodesRoute />, "/fleet/nodes");

    // Idle node card is visible and explicitly reports no managed deployments.
    expect(await screen.findByText("Idle Device")).toBeInTheDocument();
    expect(await screen.findByText("No managed deployments")).toBeInTheDocument();

    // Busy node joins its two deployments truthfully.
    expect(screen.getByText("Busy Device")).toBeInTheDocument();
    expect(screen.getByText("healthy")).toBeInTheDocument();

    // Recipe A: no live serving telemetry, so the row falls back to the
    // persisted deployment engine + endpoint model.
    expect(screen.getByText("vllm · Model A")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Recipe A" })).toHaveAttribute(
      "href",
      "/serving/deployments/33333333-3333-4333-8333-333333333333",
    );

    // The recipe catalog link is separate from the deployment detail link.
    const recipeLinks = screen.getAllByRole("link", { name: /recipe →/ });
    expect(recipeLinks.some((l) => l.getAttribute("href") === "/library/recipes/sha256:recipe-a")).toBe(true);

    // Recipe B is degraded with neither engine nor endpoint model -> Not reported.
    expect(screen.getByRole("link", { name: "Recipe B" })).toHaveAttribute(
      "href",
      "/serving/deployments/44444444-4444-4444-8444-444444444444",
    );
    expect(screen.getByText("Not reported")).toBeInTheDocument();
    expect(screen.getByText("degraded")).toBeInTheDocument();
  });

  it("shows model and engine alongside the existing Serving fields", async () => {
    installHandlers();
    renderRoute(<DeploymentsRoute />, "/serving/deployments");

    const table = await screen.findByRole("table");
    expect(within(table).getByRole("columnheader", { name: "Model" })).toBeInTheDocument();
    expect(within(table).getByRole("columnheader", { name: "Engine" })).toBeInTheDocument();
    const rows = within(table).getAllByRole("row").slice(1);
    expect(rows).toHaveLength(2);
    expect(within(table).getByRole("link", { name: "Recipe A" })).toHaveAttribute(
      "href",
      "/serving/deployments/33333333-3333-4333-8333-333333333333",
    );
    expect(rows[0]).toHaveTextContent("Model A");
    expect(rows[0]).toHaveTextContent("vLLM");
    expect(rows[0]).toHaveTextContent("fast");
    expect(rows[0]).toHaveTextContent("healthy");
    expect(rows[1]).toHaveTextContent("Not reported");
    expect(rows[1]).toHaveTextContent("degraded");
  });
});
