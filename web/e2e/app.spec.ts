import { expect, test, type Page, type Route } from "@playwright/test";

const moduleRows = [
  ["fleet", "Fleet", "/fleet", 10], ["library", "Library", "/library", 20],
  ["serving", "Serving", "/serving", 30], ["benchmarks", "Benchmarks", "/benchmarks", 40],
  ["workshop", "Workshop", "/workshop", 45], ["runs", "Runs", "/runs", 50],
  ["chat", "Chat", "/chat", 55], ["settings", "Settings", "/settings", 60],
] as const;

const nowMs = Date.now();
const nowSec = Math.floor(nowMs / 1000);
const nodes = [
  {
    id: "11111111-1111-7111-8111-111111111111", display_name: "RTX Workshop", status: "online",
    last_heartbeat: new Date(nowMs).toISOString(), agent_version: "2.4.0",
    inventory: { arch: "amd64", memory_bytes: 137438953472,
      accelerators: [{ index: 0, uuid: "gpu-0", vendor: "NVIDIA", name: "RTX PRO 6000 Blackwell", memory_bytes: 103079215104, architecture: "sm120", features: [] }],
      network_interfaces: [], rdma_devices: [] },
  },
  {
    id: "77777777-7777-7777-8777-777777777777", display_name: "gpu-fleet", status: "online",
    last_heartbeat: new Date(nowMs).toISOString(), agent_version: "2.4.0",
    inventory: { arch: "amd64", memory_bytes: 137438953472,
      accelerators: [
        { index: 0, uuid: "gpu-f0", vendor: "NVIDIA", name: "RTX PRO 6000 Blackwell", memory_bytes: 103079215104, architecture: "sm120", features: [] },
        { index: 1, uuid: "gpu-f1", vendor: "NVIDIA", name: "RTX PRO 6000 Blackwell", memory_bytes: 103079215104, architecture: "sm120", features: [] },
      ],
      network_interfaces: [], rdma_devices: [] },
  },
  { id: "55555555-5555-7555-8555-555555555555", display_name: "pending-node", status: "pending", inventory: {} },
  { id: "66666666-6666-7666-8666-666666666666", display_name: "offline-node", status: "offline", last_heartbeat: "2026-08-01T12:00:00Z", inventory: {} },
];
const fleetTelemetry = [{
  node_id: "77777777-7777-7777-8777-777777777777", ts: nowSec,
  payload: { cpu: { usage_percent: 34, cores: 16 }, memory: { used_bytes: 34359738368, total_bytes: 137438953472 },
    uptime_seconds: 3600, network: { rx_bytes_per_second: 1200, tx_bytes_per_second: 800 },
    accelerators: [
      { index: 0, utilization_percent: 48, memory_used_bytes: 3000000000, memory_total_bytes: 103079215104, temperature_c: 58, power_mw: 120000, power_limit_mw: 300000 },
      { index: 1, utilization_percent: 72, memory_used_bytes: 4000000000, memory_total_bytes: 103079215104, temperature_c: 70, power_mw: 150000, power_limit_mw: 300000 },
    ] },
}];
const nodeHistory = Array.from({ length: 12 }, (_, i) => ({
  node_id: "77777777-7777-7777-8777-777777777777", ts: nowSec - (11 - i) * 5,
  payload: { cpu: { usage_percent: 34 }, memory: { used_bytes: 34359738368, total_bytes: 137438953472 },
    accelerators: [{ index: 0, utilization_percent: 48, memory_used_bytes: 3000000000, memory_total_bytes: 103079215104 }] },
}));
const fabrics = [{ id: "22222222-2222-7222-8222-222222222222", name: "workshop-fabric", transport: "tcp", members: [nodes[0].id], state: "ok", version: "1" }];
const deployments = [{ id: "33333333-3333-7333-8333-333333333333", recipe_digest: "sha256:qwen", recipe_name: "qwen3.8", engine: "sglang", profile: "rtx6000", placements: [{ node_id: nodes[0].id, node_name: "RTX Workshop", rank: 0 }], desired_state: "running", observed_state: "healthy", created_at: "2026-08-20T11:00:00Z", updated_at: "2026-08-20T12:00:00Z", endpoint: { host: "127.0.0.1", port: 8000, model: "Qwen3.8-27B" } }];
const deployments = [
  { id: "33333333-3333-7333-8333-333333333333", recipe_name: "qwen3.8", profile: "rtx6000", desired_state: "running", observed_state: "healthy", updated_at: "2026-08-20T12:00:00Z", placements: [{ node_id: "77777777-7777-7777-8777-777777777777", rank: 0 }], endpoint: { host: "127.0.0.1", port: 8000, model: "Qwen3.8-27B" } },
  { id: "44444444-4444-7444-8444-444444444444", recipe_name: "qwen3.8", profile: "worker", desired_state: "running", observed_state: "healthy", updated_at: "2026-08-20T12:00:00Z", placements: [{ node_id: "77777777-7777-7777-8777-777777777777", rank: 1 }], endpoint: { host: "127.0.0.1", port: 8000, model: "Qwen3.8-27B" } },
];
const servingTelemetry = [{
  deployment_id: "33333333-3333-7333-8333-333333333333", ts: nowSec,
  payload: { available: true, backend: "vllm", model_id: "Qwen3.8-27B", generation_tps: 10, prefill_tps: 4, requests_running: 2, slots_active: 2, slots_total: 32 },
}];
const servingHistory = [{
  deployment_id: "33333333-3333-7333-8333-333333333333", ts: nowSec,
  payload: { available: true, backend: "vllm", model_id: "Qwen3.8-27B", generation_tps: 10, prefill_tps: 4, requests_running: 2 },
}];

async function fulfill(route: Route, body: unknown, status = 200) {
  await route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
}

async function installAPI(page: Page, options: { signedIn?: boolean; failNodes?: boolean } = {}) {
  await page.route("**/api/v1/**", async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    if (path === "/api/v1/session") return fulfill(route, options.signedIn === false ? { code: "auth.unauthorized" } : { username: "operator", csrf_token: "test-csrf", expires_at: "2099-01-01T00:00:00Z" }, options.signedIn === false ? 401 : 200);
    if (path === "/api/v1/modules") return fulfill(route, moduleRows.map(([id, title, moduleRoute, order]) => ({ id, title, route: moduleRoute, nav: { label: title, order, icon: id }, capabilities: [] })));
    if (path === "/api/v1/nodes") return fulfill(route, options.failNodes ? { code: "test.failure", error: "node inventory unavailable" } : nodes, options.failNodes ? 503 : 200);
    if (path === "/api/v1/fabrics") return fulfill(route, fabrics);
    if (path === "/api/v1/deployments") return fulfill(route, deployments);
    if (path === "/api/v1/nodes/telemetry") return fulfill(route, fleetTelemetry);
    if (path === "/api/v1/deployments/telemetry") return fulfill(route, servingTelemetry);
    if (path === "/api/v1/nodes/77777777-7777-7777-8777-777777777777/telemetry") return fulfill(route, nodeHistory);
    if (path === "/api/v1/deployments/33333333-3333-7333-8333-333333333333/telemetry") return fulfill(route, servingHistory);
    if (path === "/api/v1/nodes/77777777-7777-7777-8777-777777777777") return fulfill(route, nodes[1]);
    if (path === "/api/v1/nodes/55555555-5555-7555-8555-555555555555") return fulfill(route, nodes[1]);
    if (path === "/api/v1/nodes/66666666-6666-7666-8666-666666666666") return fulfill(route, nodes[2]);
    if (path === "/api/v1/runs") return fulfill(route, { items: [] });
    if (path === "/api/v1/recipes" || path === "/api/v1/artifacts" || path === "/api/v1/transfers" || path === "/api/v1/recipe-drafts" || path === "/api/v1/benchmarks" || path === "/api/v1/benchmark-results" || path === "/api/v1/secrets") return fulfill(route, []);
    if (path === "/api/v1/system/info") return fulfill(route, { version: "test", commit: "abc123", build: "test" });
    if (path.startsWith("/api/v1/module-settings/")) return fulfill(route, { module: path.split("/").pop(), settings: {}, version: "1" });
    return fulfill(route, request.method() === "GET" ? [] : {});
  });
}

test("unauthenticated navigation lands on the operator login", async ({ page }) => {
  await installAPI(page, { signedIn: false });
  await page.goto("/workshop");
  await expect(page).toHaveURL(/\/login$/);
  await expect(page.getByRole("heading", { name: "Console" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Sign in" })).toBeDisabled();
});

test("every first-party module route mounts inside the authenticated shell", async ({ page }) => {
  await installAPI(page);
  const routes = ["/", "/fleet", "/fleet/nodes", "/fleet/fabrics", "/library", "/library/recipes", "/library/artifacts", "/library/transfers", "/library/builder", "/profiles", "/knowledge", "/serving", "/serving/deployments", "/benchmarks", "/benchmarks/leaderboard", "/research/autoresearch", "/research/experiments", "/research/workflows", "/scheduled", "/usage", "/fine-tuning", "/projects", "/workshop", "/runs", "/chat"];
  for (const path of routes) {
    await page.goto(path);
    await expect(page.getByRole("navigation")).toBeVisible();
    await expect(page.locator("body")).not.toContainText("Application Error");
    await expect(page.locator("main").first()).toBeVisible();
  }
});

test("Sample A navigation exposes real and skeleton destinations on desktop and mobile", async ({ page }) => {
  await installAPI(page);
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto("/library/recipes");

  const desktopNav = page.getByRole("navigation", { name: "Primary" });
  await expect(desktopNav).toBeVisible();
  for (const label of [
    "Nodes", "Fabrics", "Catalog", "Recipe Builder", "Profiles & Sharing",
    "Knowledge & RAG", "Serving", "Community Leaderboard",
    "Autoresearch", "Experiment Builder", "Workflow Builder",
    "Scheduled Tasks & Automations", "Usage & Costs", "Integrated Fine-tuning",
    "Projects", "Chat",
  ]) {
    await expect(desktopNav.getByRole("link", { name: label, exact: true })).toBeVisible();
  }
  await expect(desktopNav.getByRole("link", { name: "Overview", exact: true })).toHaveCount(2);
  for (const group of ["Workshop", "Fleet", "Recipes", "Benchmarks", "Research"]) {
    await expect(desktopNav.getByRole("button", { name: group, exact: true })).toBeVisible();
  }
  for (const absentLabel of ["Settings", "Modules", "Topology", "Artifacts", "Transfers", "Runs"]) {
    await expect(desktopNav.getByText(absentLabel, { exact: true })).toHaveCount(0);
  }

  await expect(page.locator("header.sticky").getByRole("heading", { name: "Catalog", exact: true })).toBeVisible();
  await expect(page.locator("aside").first()).toHaveCSS("width", "220px");
  const recipesGroup = desktopNav.getByRole("button", { name: "Recipes" });
  await expect(recipesGroup).toHaveAttribute("aria-expanded", "true");
  await recipesGroup.click();
  await expect(recipesGroup).toHaveAttribute("aria-expanded", "false");
  await expect(desktopNav.getByRole("link", { name: "Catalog", exact: true })).toHaveCount(0);
  await recipesGroup.click();
  await expect(desktopNav.getByRole("link", { name: "Catalog", exact: true })).toBeVisible();
  await page.goto("/profiles");
  await expect(page.locator("#main-content").getByRole("heading", { name: "Profiles & Sharing" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Section skeleton" })).toBeVisible();
  await expect(page.getByText(/does not claim data or actions/i)).toBeVisible();

  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/chat");
  await expect(page.locator("aside").first()).toBeHidden();
  await page.getByRole("button", { name: "Open navigation" }).click();
  const mobileNav = page.getByRole("dialog").getByRole("navigation", { name: "Primary" });
  await expect(mobileNav).toBeVisible();
  await expect(mobileNav.getByRole("link", { name: "Chat", exact: true })).toBeVisible();
  await mobileNav.getByRole("link", { name: "Overview", exact: true }).first().click();
  await expect(page).toHaveURL(/\/$/);
  await expect(page.getByRole("dialog")).toHaveCount(0);
});

test("Workshop renders inventory, topology, and serving instruments", async ({ page }) => {
  await installAPI(page);
  await page.goto("/workshop");
  await expect(page.locator("#workshop-title")).toBeVisible();
  await expect(page.getByText("RTX Workshop")).toBeVisible();
  await expect(page.getByText("workshop-fabric")).toBeVisible();
  await expect(page.getByText("qwen3.8@rtx6000")).toBeVisible();
  await expect(page.getByText("288 GiB")).toBeVisible();
});

test("API failures expose a retryable operator state", async ({ page }) => {
  await installAPI(page, { failNodes: true });
  await page.goto("/workshop");
  await expect(page.getByText("Workshop instruments unavailable")).toBeVisible();
  await expect(page.getByText("Service Unavailable")).toBeVisible();
  await expect(page.getByRole("button", { name: /retry/i })).toBeVisible();
});

test("Fleet overview renders live monitoring cards and serving rows", async ({ page }) => {
  await installAPI(page);
  await page.goto("/fleet/nodes");
  await expect(page.getByText("RTX Workshop")).toBeVisible();
  // Two-GPU rejection: max utilization 72 renders on the card.
  await expect(page.getByText("72%")).toBeVisible();
  // Pending + offline labels.
  await expect(page.getByText("pending-node")).toBeVisible();
  await expect(page.getByText("offline-node")).toBeVisible();
  // Rank-zero service shows live throughput; the worker shows its rank.
  await expect(page.getByText(/10\.0 tok\/s/)).toBeVisible();
  await expect(page.getByText(/rank 1 worker/)).toBeVisible();
});

test("Node detail switches range and keeps admin sections", async ({ page }) => {
  const requests: string[] = [];
  page.on("request", (req) => {
    if (req.url().includes("/nodes/77777777-7777-7777-8777-777777777777/telemetry")) requests.push(String(req.url()));
  });
  await installAPI(page);
  await page.goto("/fleet/nodes/77777777-7777-7777-8777-777777777777");
  await expect(page.getByRole("heading", { name: "gpu-fleet" })).toBeVisible();
  await expect(page.getByRole("img", { name: "cpu utilization" })).toBeVisible();
  await page.getByRole("button", { name: "7d" }).click();
  await expect(page.getByRole("button", { name: "7d" })).toHaveAttribute("aria-pressed", "true");
  await expect.poll(() => requests.some((u) => u.includes("resolution=1m") && u.includes("limit=10080"))).toBe(true);
  // Resource charts + serving panel and admin sections still mount.
  await expect(page.getByRole("heading", { name: "accelerators" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "transfers" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "serving" })).toBeVisible();
});
