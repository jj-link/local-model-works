import { expect, test, type Page, type Route } from "@playwright/test";

const moduleRows = [
  ["fleet", "Fleet", "/fleet", 10], ["library", "Library", "/library", 20],
  ["serving", "Serving", "/serving", 30], ["benchmarks", "Benchmarks", "/benchmarks", 40],
  ["autoresearch", "AutoResearch Factory", "/autoresearch", 45], ["workshop", "Workshop", "/workshop", 46],
  ["runs", "Runs", "/runs", 50], ["chat", "Chat", "/chat", 55], ["settings", "Settings", "/settings", 60],
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
const sampleDeployments = [{ id: "33333333-3333-7333-8333-333333333333", recipe_digest: "sha256:qwen", recipe_name: "qwen3.8", engine: "sglang", profile: "rtx6000", placements: [{ node_id: nodes[0].id, node_name: "RTX Workshop", rank: 0 }], desired_state: "running", observed_state: "healthy", created_at: "2026-08-20T11:00:00Z", updated_at: "2026-08-20T12:00:00Z", endpoint: { host: "127.0.0.1", port: 8000, model: "Qwen3.8-27B" } }];
const fleetDeployments = [
  { id: "33333333-3333-7333-8333-333333333333", recipe_digest: "sha256:qwen", recipe_name: "qwen3.8", engine: "vllm", profile: "rtx6000", desired_state: "running", observed_state: "healthy", updated_at: "2026-08-20T12:00:00Z", placements: [{ node_id: "77777777-7777-7777-8777-777777777777", rank: 0 }], endpoint: { host: "127.0.0.1", port: 8000, model: "Qwen3.8-27B" } },
  { id: "44444444-4444-7444-8444-444444444444", recipe_digest: "sha256:qwen", recipe_name: "qwen3.8", engine: "vllm", profile: "worker", desired_state: "running", observed_state: "healthy", updated_at: "2026-08-20T12:00:00Z", placements: [{ node_id: "77777777-7777-7777-8777-777777777777", rank: 1 }], endpoint: { host: "127.0.0.1", port: 8000, model: "Qwen3.8-27B" } },
];
const servingTelemetry = [{
  deployment_id: "33333333-3333-7333-8333-333333333333", ts: nowSec,
  payload: { available: true, backend: "vllm", model_id: "Qwen3.8-27B", generation_tps: 10, prefill_tps: 4, requests_running: 2, slots_active: 2, slots_total: 32 },
}];
const servingHistory = [{
  deployment_id: "33333333-3333-7333-8333-333333333333", ts: nowSec,
  payload: { available: true, backend: "vllm", model_id: "Qwen3.8-27B", generation_tps: 10, prefill_tps: 4, requests_running: 2 },
}];
const autoResearchProject = {
  id: "88888888-8888-4888-8888-888888888888",
  name: "Sparse world models",
  status: "running",
  runner_node_id: nodes[0].id,
  idea_prompt: "Can sparse latent world models improve long-horizon robotic planning?",
  config: { candidate_count: 1, paper_max_rounds: 5, human_gates: { idea_selection: true, paper_post_edit: true }, roles: {}, fallbacks: {}, advisors: {} },
  version: 1,
  created_at: "2026-08-24T20:00:00Z",
  updated_at: "2026-08-24T20:10:00Z",
};
const autoResearchRun = {
  id: "99999999-9999-4999-8999-999999999999",
  module: "autoresearch",
  kind: "autoresearch-factory",
  state: "running",
  created_at: "2026-08-24T20:10:00Z",
  started_at: "2026-08-24T20:11:00Z",
};
const autoResearchIdea = {
  id: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
  project_id: autoResearchProject.id,
  ordinal: 1,
  source: "generated",
  title: "Sparse latent world models for long-horizon planning",
  body: "## Research question\nCan sparse state improve planning?",
  selected: true,
  version: 2,
  created_at: "2026-08-24T20:02:00Z",
  updated_at: "2026-08-24T20:05:00Z",
};
const autoResearchEvents = [
  { version: 1, event_id: "arf-start", run_id: autoResearchRun.id, invocation_id: "inv-coder", node_id: "experiment.coder", timestamp: "2026-08-24T20:12:00Z", type: "agent.started", payload: { role: "experiment-coder", backend: "spark2", model: "real-coder-model" } },
  { version: 1, event_id: "arf-usage-1", run_id: autoResearchRun.id, invocation_id: "inv-coder", node_id: "experiment.coder", timestamp: "2026-08-24T20:12:01Z", type: "agent.usage", payload: { input_tokens: 1200, output_tokens: 300, cost_usd: 0.42, output_rate: 25.5, context_percent: 40 } },
  { version: 1, event_id: "arf-usage-2", run_id: autoResearchRun.id, invocation_id: "inv-coder", node_id: "experiment.coder", timestamp: "2026-08-24T20:12:02Z", type: "agent.usage", payload: { input_tokens: 1200, output_tokens: 300, cost_usd: 0.42, output_rate: 25.5, context_percent: 40 } },
  { version: 1, event_id: "arf-text", run_id: autoResearchRun.id, invocation_id: "inv-coder", node_id: "experiment.coder", timestamp: "2026-08-24T20:12:03Z", type: "agent.text.delta", payload: { delta: "Running the deterministic sparse-latent ablation." } },
] as const;

async function fulfill(route: Route, body: unknown, status = 200) {
  await route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
}

async function installAPI(page: Page, options: { signedIn?: boolean; failNodes?: boolean; fleetScenario?: boolean; autoResearchScenario?: boolean } = {}) {
  let autoResearchState = autoResearchRun.state;
  await page.route("**/api/v1/**", async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    if (path === "/api/v1/session") return fulfill(route, options.signedIn === false ? { code: "auth.unauthorized" } : { username: "operator", csrf_token: "test-csrf", expires_at: "2099-01-01T00:00:00Z" }, options.signedIn === false ? 401 : 200);
    if (path === "/api/v1/modules") return fulfill(route, moduleRows.map(([id, title, moduleRoute, order]) => ({ id, title, route: moduleRoute, nav: { label: title, order, icon: id }, capabilities: [] })));
    if (path === "/api/v1/nodes") return fulfill(route, options.failNodes ? { code: "test.failure", error: "node inventory unavailable" } : options.fleetScenario ? nodes : [nodes[0]], options.failNodes ? 503 : 200);
    if (path === "/api/v1/fabrics") return fulfill(route, fabrics);
    if (path === "/api/v1/deployments") return fulfill(route, options.fleetScenario ? fleetDeployments : sampleDeployments);
    if (path === "/api/v1/nodes/telemetry") return fulfill(route, options.fleetScenario ? fleetTelemetry : []);
    if (path === "/api/v1/deployments/telemetry") return fulfill(route, options.fleetScenario ? servingTelemetry : []);
    if (path === "/api/v1/nodes/77777777-7777-7777-8777-777777777777/telemetry") return fulfill(route, nodeHistory);
    if (path === "/api/v1/deployments/33333333-3333-7333-8333-333333333333/telemetry") return fulfill(route, servingHistory);
    if (path === "/api/v1/nodes/77777777-7777-7777-8777-777777777777") return fulfill(route, nodes[1]);
    if (path === "/api/v1/nodes/55555555-5555-7555-8555-555555555555") return fulfill(route, nodes[2]);
    if (path === "/api/v1/nodes/66666666-6666-7666-8666-666666666666") return fulfill(route, nodes[3]);
    if (path === "/api/v1/runs") return fulfill(route, { items: [] });
    if (path === "/api/v1/recipes" || path === "/api/v1/artifacts" || path === "/api/v1/transfers" || path === "/api/v1/recipe-drafts" || path === "/api/v1/benchmarks" || path === "/api/v1/benchmark-results" || path === "/api/v1/secrets") return fulfill(route, []);
    if (path === "/api/v1/system/info") return fulfill(route, { version: "test", commit: "abc123", build: "test" });
    if (options.autoResearchScenario) {
      if (path === "/api/v1/autoresearch/projects") return fulfill(route, [autoResearchProject]);
      if (path === `/api/v1/autoresearch/projects/${autoResearchProject.id}`) return fulfill(route, autoResearchProject);
      if (path === `/api/v1/autoresearch/projects/${autoResearchProject.id}/ideas`) return fulfill(route, [autoResearchIdea]);
      if (path === `/api/v1/autoresearch/projects/${autoResearchProject.id}/sources`) return fulfill(route, []);
      if (path === `/api/v1/autoresearch/projects/${autoResearchProject.id}/runs`) {
        if (request.method() === "POST") {
          autoResearchState = "queued";
          return fulfill(route, { ...autoResearchRun, id: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", state: autoResearchState });
        }
        return fulfill(route, [{ ...autoResearchRun, state: autoResearchState }]);
      }
      if (path === `/api/v1/autoresearch/projects/${autoResearchProject.id}/paper/files`) return fulfill(route, []);
      if (path === `/api/v1/autoresearch/runs/${autoResearchRun.id}/pause`) {
        autoResearchState = "paused";
        return fulfill(route, { ...autoResearchRun, state: autoResearchState });
      }
      if (path === `/api/v1/autoresearch/runs/${autoResearchRun.id}/resume`) {
        autoResearchState = "running";
        return fulfill(route, { ...autoResearchRun, state: autoResearchState });
      }
      if (path === `/api/v1/autoresearch/runs/${autoResearchRun.id}/stop`) {
        autoResearchState = "cancelled";
        return fulfill(route, { ...autoResearchRun, state: autoResearchState });
      }
      if (path === `/api/v1/runs/${autoResearchRun.id}/logs`) {
        const frames = autoResearchEvents.map((event) => `id: ${event.event_id}\ndata: ${JSON.stringify(`${JSON.stringify(event)}\n`)}\n\n`).join("");
        return route.fulfill({ status: 200, contentType: "text/event-stream", body: `${frames}event: end\ndata: {}\n\n` });
      }
    }
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
  const routes = ["/", "/fleet", "/fleet/nodes", "/fleet/fabrics", "/library", "/library/recipes", "/library/artifacts", "/library/transfers", "/library/builder", "/profiles", "/knowledge", "/serving", "/serving/deployments", "/benchmarks", "/benchmarks/leaderboard", "/autoresearch", "/research/autoresearch", "/research/experiments", "/research/workflows", "/scheduled", "/usage", "/fine-tuning", "/projects", "/workshop", "/runs", "/chat"];
  for (const path of routes) {
    await page.goto(path);
    await expect(page.getByRole("navigation")).toBeVisible();
    await expect(page.locator("body")).not.toContainText("Application Error");
    await expect(page.locator("main").first()).toBeVisible();
  }
});

test("current navigation exposes real and skeleton destinations on desktop and mobile", async ({ page }) => {
  await installAPI(page);
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto("/library/recipes");

  const desktopNav = page.getByRole("navigation", { name: "Primary" });
  await expect(desktopNav).toBeVisible();
  for (const label of [
    "Nodes", "Fabrics", "Catalog", "Recipe Builder", "Profiles & Sharing",
    "Knowledge & RAG", "Serving", "Community Leaderboard",
    "AutoResearch Factory", "Experiment Builder", "Workflow Builder",
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

test("AutoResearch Factory preserves topology and stream in the empty state", async ({ page }) => {
  await installAPI(page);
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto("/autoresearch");
  await expect(page.getByText("No research project selected")).toBeVisible();
  await expect(page.getByLabel("Live Agon workflow")).toBeVisible();
  await expect(page.getByLabel("Generation stream")).toBeVisible();
  const desktop = await page.locator(".arf-workspace").evaluate((workspace) => {
    const graph = workspace.querySelector(".arf-flow-panel") as HTMLElement;
    const stream = workspace.querySelector(".arf-stream-panel") as HTMLElement;
    const notice = workspace.querySelector(".arf-empty-notice") as HTMLElement;
    return {
      display: getComputedStyle(workspace).display,
      streamNarrower: stream.clientWidth < graph.clientWidth,
      noticeClear: notice.getBoundingClientRect().bottom <= graph.getBoundingClientRect().top &&
        notice.getBoundingClientRect().bottom <= stream.getBoundingClientRect().top,
    };
  });
  expect(desktop).toEqual({ display: "grid", streamNarrower: true, noticeClear: true });
  await expect(page.locator(".arf-canvas")).not.toContainText(/QWEN-3\.5|\$1\.84|#018/);

  await page.setViewportSize({ width: 390, height: 844 });
  const mobile = await page.locator(".arf-workspace").evaluate((workspace) => {
    const graph = workspace.querySelector(".arf-flow-panel") as HTMLElement;
    const stream = workspace.querySelector(".arf-stream-panel") as HTMLElement;
    const notice = workspace.querySelector(".arf-empty-notice") as HTMLElement;
    return {
      display: getComputedStyle(workspace).display,
      stacked: stream.getBoundingClientRect().top > graph.getBoundingClientRect().bottom,
      graphScrolls: graph.scrollWidth > graph.clientWidth,
      noticeClear: notice.getBoundingClientRect().bottom <= graph.getBoundingClientRect().top,
      overflow: document.documentElement.scrollWidth - document.documentElement.clientWidth,
    };
  });
  expect(mobile).toEqual({ display: "block", stacked: true, graphScrolls: true, noticeClear: true, overflow: 0 });
  const verticalOrder = await page.locator(".arf-hero, .arf-composer, .arf-workspace").evaluateAll((elements) => elements.map((element) => element.className));
  expect(verticalOrder).toEqual(["arf-hero", "arf-composer", "arf-workspace"]);
  const modeNav = page.getByRole("navigation", { name: "AutoResearch Factory workspace" });
  for (const mode of ["Ideas & sources", "Paper studio", "Role controls"]) {
    const button = modeNav.getByRole("button", { name: mode, exact: true });
    await button.click();
    await expect(page.getByRole("heading", { name: mode, exact: true })).toBeVisible();
    await expect(page.getByText(`Select or create a research project to use ${mode}.`)).toBeVisible();
    await expect(button).toHaveAttribute("aria-current", "page");
  }
  await modeNav.getByRole("button", { name: "Factory", exact: true }).click();
  await expect(page.getByLabel("Live Agon workflow")).toBeVisible();
  await expect(page.getByLabel("Generation stream")).toBeVisible();

  await page.getByRole("button", { name: "New project", exact: true }).first().click();
  await expect(page.getByRole("dialog", { name: "New AutoResearch project" })).toBeVisible();
  await page.getByLabel("Project name").fill("Mobile research project");
  await expect(page.getByRole("button", { name: "Create", exact: true })).toBeEnabled();
  await expect(page.getByLabel("Direct idea")).toBeVisible();
  await expect(page.getByLabel("Runner node")).toBeVisible();
});

test("AutoResearch Factory renders real desktop fidelity and run controls", async ({ page }) => {
  await installAPI(page, { autoResearchScenario: true });
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto("/autoresearch");

  const primaryNav = page.getByRole("navigation", { name: "Primary" });
  await expect(primaryNav.getByRole("link", { name: "AutoResearch Factory", exact: true })).toBeVisible();
  await expect(page.locator(".arf-hero")).toBeVisible();
  await expect(page.locator(".arf-composer")).toBeVisible();
  await expect(page.getByLabel("Live Agon workflow")).toBeVisible();
  await expect(page.getByRole("heading", { name: "Generation stream", exact: true })).toBeVisible();
  await expect(page.getByLabel("Research topic")).toHaveValue("Sparse latent world models for long-horizon planning");
  await expect(page.getByText("99999999…")).toBeVisible();
  await expect(page.getByText("$0.42")).toBeVisible();
  await expect(page.getByText("1,500")).toBeVisible();
  await expect(page.getByRole("button", { name: "Experiment coder — real-coder-model — generating" })).toBeVisible();

  const geometry = await page.locator(".arf-workspace").evaluate((workspace) => {
    const graph = workspace.querySelector(".arf-flow-panel")!.getBoundingClientRect();
    const stream = workspace.querySelector(".arf-stream-panel")!.getBoundingClientRect();
    return {
      display: getComputedStyle(workspace).display,
      columns: getComputedStyle(workspace).gridTemplateColumns,
      graphWidth: graph.width,
      streamWidth: stream.width,
      streamAfterGraph: stream.left > graph.left,
    };
  });
  expect(geometry.display).toBe("grid");
  expect(geometry.columns).not.toBe("none");
  expect(geometry.streamAfterGraph).toBe(true);
  expect(geometry.streamWidth).toBeLessThan(geometry.graphWidth);
  expect(await page.locator("html").evaluate((element) => element.scrollWidth)).toBe(1440);
  await expect(page.locator(".arf-canvas")).not.toContainText("QWEN-3.5");
  await expect(page.locator(".arf-canvas")).not.toContainText("$1.84");
  await expect(page.locator(".arf-canvas")).not.toContainText("#018");

  await page.getByRole("button", { name: "Pause run" }).click();
  await expect(page.getByRole("button", { name: "Resume run" })).toBeVisible();
  await page.getByRole("button", { name: "Resume run" }).click();
  await expect(page.getByRole("button", { name: "Pause run" })).toBeVisible();
  await page.getByRole("button", { name: "Stop run" }).click();
  await expect(page.getByRole("button", { name: "Start run" })).toBeVisible();
  await page.getByLabel("Start at").selectOption("experiment");
  const startRequest = page.waitForRequest((request) => request.method() === "POST" && request.url().endsWith(`/autoresearch/projects/${autoResearchProject.id}/runs`));
  await page.getByRole("button", { name: "Start run" }).click();
  expect((await startRequest).postDataJSON()).toEqual({ factory: "experiment" });

  await page.getByRole("button", { name: "Ideas & sources", exact: true }).click();
  await expect(page.getByRole("heading", { name: "idea workspace", exact: true })).toBeVisible();
  await expect(page.getByLabel("candidate title")).toHaveValue("Sparse latent world models for long-horizon planning");
});

test("AutoResearch Factory stacks and contains topology overflow on mobile", async ({ page }) => {
  await installAPI(page, { autoResearchScenario: true });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/autoresearch");
  await expect(page.getByRole("button", { name: "Pause run" })).toBeVisible();
  await expect(page.locator(".arf-workspace")).toBeVisible();
  const mobileGeometry = await page.locator(".arf-workspace").evaluate((workspace) => {
    const graph = workspace.querySelector(".arf-flow-panel") as HTMLElement;
    const stream = workspace.querySelector(".arf-stream-panel") as HTMLElement;
    return {
      display: getComputedStyle(workspace).display,
      stacked: stream.getBoundingClientRect().top > graph.getBoundingClientRect().bottom,
      graphScrolls: graph.scrollWidth > graph.clientWidth,
    };
  });
  expect(mobileGeometry.display).toBe("block");
  expect(mobileGeometry.stacked).toBe(true);
  expect(mobileGeometry.graphScrolls).toBe(true);
  expect(await page.locator("html").evaluate((element) => element.scrollWidth)).toBe(390);

  const modeNav = page.getByRole("navigation", { name: "AutoResearch Factory workspace" });
  for (const name of ["Factory", "Ideas & sources", "Paper studio", "Role controls"]) {
    const button = modeNav.getByRole("button", { name, exact: true });
    await expect(button).toBeEnabled();
    await button.focus();
    await expect(button).toBeFocused();
  }
});

test("Workshop renders inventory, topology, and serving instruments", async ({ page }) => {
  await installAPI(page);
  await page.goto("/workshop");
  await expect(page.locator("#workshop-title")).toBeVisible();
  await expect(page.getByText("RTX Workshop")).toBeVisible();
  await expect(page.getByText("workshop-fabric")).toBeVisible();
  await expect(page.getByText("qwen3.8@rtx6000")).toBeVisible();
  await expect(page.getByText("96.0 GiB")).toBeVisible();
});

test("API failures expose a retryable operator state", async ({ page }) => {
  await installAPI(page, { failNodes: true });
  await page.goto("/workshop");
  await expect(page.getByText("Workshop instruments unavailable")).toBeVisible();
  await expect(page.getByText("Service Unavailable")).toBeVisible();
  await expect(page.getByRole("button", { name: /retry/i })).toBeVisible();
});

test("Fleet overview renders live monitoring cards and serving rows", async ({ page }) => {
  await installAPI(page, { fleetScenario: true });
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
  await installAPI(page, { fleetScenario: true });
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
