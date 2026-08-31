/// <reference types="@testing-library/jest-dom" />
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { MemoryRouter } from "react-router";
import { describe, expect, it } from "vitest";

import { CreateFabricDialog } from "~/components/dialogs/create-fabric-dialog";
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
    parameters: {},
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
    parameters: {},
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
    parameters: {},
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
  it("renders card rows with truthful joins, engine/model fallback, degraded missing metadata, and visible idle node", async () => {
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

    const table = (await screen.findAllByRole("table")).find((candidate) =>
      within(candidate).queryByRole("columnheader", { name: "Deployment" }));
    expect(table).toBeDefined();
    if (!table) throw new Error("deployment table not found");
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
    expect(rows[0]).toHaveTextContent("healthy");
    expect(rows[1]).toHaveTextContent("Not reported");
    expect(rows[1]).toHaveTextContent("degraded");
  });

  it("auto-selects each node's RDMA interface and IPv4-mapped GID", async () => {
    const head = {
      ...idleNode,
      display_name: "Spark Head",
      inventory: {
        arch: "arm64",
        hostname: "spark2",
        os: "linux",
        interfaces: [
          { name: "tailscale0", addresses: ["100.92.139.82"] },
          { name: "enp1s0f1np1", addresses: ["10.0.0.1"] },
        ],
        rdma_devices: [{
          name: "rocep1s0f1",
          network_interfaces: ["enp1s0f1np1"],
          ports: [{
            name: "1",
            gids: [
              { index: 3, value: "0000:0000:0000:0000:0000:0000:0000:0000", type: "RoCE v2" },
              { index: 4, value: "0000:0000:0000:0000:0000:ffff:0a00:0001", type: "IB/RoCE v1" },
              { index: 5, value: "0000:0000:0000:0000:0000:ffff:0a00:0001", type: "RoCE v2" },
            ],
          }],
        }],
      },
    };
    const worker = {
      ...busyNode,
      display_name: "Spark Worker",
      inventory: {
        ...busyNode.inventory,
        arch: "arm64",
        hostname: "spark3",
        interfaces: [
          { name: "tailscale0", addresses: ["100.121.117.65"] },
          { name: "enp1s0f0np0", addresses: ["10.0.0.2"] },
        ],
        rdma_devices: [{
          name: "rocep1s0f0",
          network_interfaces: ["enp1s0f0np0"],
          ports: [{
            name: "1",
            gids: [
              { index: 3, value: "0000:0000:0000:0000:0000:0000:0000:0000", type: "RoCE v2" },
              { index: 5, value: "0000:0000:0000:0000:0000:ffff:0a00:0002", type: "IB/RoCE v1" },
              { index: 6, value: "0000:0000:0000:0000:0000:ffff:0a00:0002", type: "RoCE v2" },
            ],
          }],
        }],
      },
    };
    let requestBody: Record<string, unknown> | undefined;
    server.use(
      http.get("*/api/v1/nodes", () => HttpResponse.json([head, worker])),
      http.post("*/api/v1/fabrics", async ({ request }) => {
        requestBody = await request.json() as Record<string, unknown>;
        return HttpResponse.json({
          id: "66666666-6666-4666-8666-666666666666",
          name: "spark-p2p",
          transport: "roce",
          members: [head.id, worker.id],
          bindings: requestBody.bindings,
          state: "ok",
          version: "v1",
          created_at: "2026-01-03T00:00:00Z",
          updated_at: "2026-01-03T00:00:00Z",
        }, { status: 201 });
      }),
    );

    const user = userEvent.setup();
    renderRoute(
      <CreateFabricDialog open onOpenChange={() => undefined} />,
      "/fleet/fabrics",
    );
    await user.type(await screen.findByLabelText("Name"), "spark-p2p");
    await user.click(screen.getByRole("button", { name: "Add Spark Head" }));
    await user.click(screen.getByRole("button", { name: "Add Spark Worker" }));

    expect(screen.getByLabelText("Spark Head interface")).toHaveValue("enp1s0f1np1");
    expect(screen.getByLabelText("Spark Head fabric address")).toHaveValue("10.0.0.1");
    expect(screen.getByLabelText("Spark Head RDMA device")).toHaveValue("rocep1s0f1");
    expect(screen.getByLabelText("Spark Head GID index")).toHaveValue("5");
    expect(screen.getByLabelText("Spark Worker interface")).toHaveValue("enp1s0f0np0");
    expect(screen.getByLabelText("Spark Worker GID index")).toHaveValue("6");
    await user.click(screen.getByRole("button", { name: "Create & validate" }));

    await waitFor(() => expect(requestBody).toBeDefined());
    expect(requestBody?.bindings).toEqual([
      {
        node_id: head.id,
        interface_name: "enp1s0f1np1",
        address: "10.0.0.1",
        rdma_device: "rocep1s0f1",
        gid_index: 5,
      },
      {
        node_id: worker.id,
        interface_name: "enp1s0f0np0",
        address: "10.0.0.2",
        rdma_device: "rocep1s0f0",
        gid_index: 6,
      },
    ]);
  });
});
