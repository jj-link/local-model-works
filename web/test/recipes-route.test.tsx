/// <reference types="@testing-library/jest-dom" />
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { MemoryRouter, useLocation } from "react-router";
import { describe, expect, it } from "vitest";

import RecipesRoute from "~/routes/library/recipes/index";
import { server } from "../msw/server";

const alphaDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
const betaDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";
const gammaDigest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc";

const recipes = [
  {
    digest: alphaDigest,
    name: "alpha-recipe",
    display_name: "Alpha Model",
    model: "Alpha 27B",
    engine: "vllm",
    version: "2.0.0",
    version_count: 2,
    description: "A verified two-node recipe.",
    license: "MIT",
    trust_state: "verified",
    source: { type: "catalog", remote: "https://catalog.example/alpha" },
    compatibility: { nodeCount: 2, fabric: { transport: "roce" } },
    installed_at: "2026-01-03T00:00:00Z",
  },
  {
    digest: betaDigest,
    name: "beta-recipe",
    display_name: "Beta Model",
    version: "1.0.0",
    trust_state: "local",
    source: { type: "git", remote: "https://git.example/beta" },
    compatibility: { nodeCount: 1 },
    installed_at: "2026-01-01T00:00:00Z",
  },
  {
    digest: gammaDigest,
    name: "gamma-recipe",
    display_name: "Gamma Model",
    version: "1.5.0",
    trust_state: "untrusted",
    source: { type: "local", remote: "/srv/gamma" },
    compatibility: { nodeCount: 2 },
    installed_at: "2026-01-02T00:00:00Z",
  },
] as const;

function LocationProbe() {
  return <output aria-label="Current path">{useLocation().pathname}</output>;
}

function renderCatalog() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/library/recipes"]}>
        <RecipesRoute />
        <LocationProbe />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function installCatalogHandlers(rows: readonly unknown[] = recipes) {
  server.use(
    http.get("*/api/v1/recipes", () => HttpResponse.json(rows)),
    http.get(`*/api/v1/recipes/${alphaDigest}`, () =>
      HttpResponse.json({ ...recipes[0], manifest: { artifacts: [], workloads: [] } }),
    ),
    http.get("*/api/v1/nodes", () => HttpResponse.json([])),
    http.get("*/api/v1/fabrics", () => HttpResponse.json([])),
  );
}

describe("RecipesRoute", () => {
  it("renders real card fields, deterministic filters, sorting, and digest navigation", async () => {
    installCatalogHandlers();
    const user = userEvent.setup();
    renderCatalog();

    const alphaLink = await screen.findByRole("link", { name: "Alpha Model" });
    const alphaCard = alphaLink.closest("article");
    expect(alphaCard).not.toBeNull();
    expect(within(alphaCard as HTMLElement).getByText("Alpha 27B")).toBeInTheDocument();
    expect(within(alphaCard as HTMLElement).getByText("vLLM")).toBeInTheDocument();
    expect(within(alphaCard as HTMLElement).getByText(/roce/i)).toBeInTheDocument();
    expect(within(alphaCard as HTMLElement).getByText("MIT")).toBeInTheDocument();
    expect(within(alphaCard as HTMLElement).getByText("2.0.0 · 2 installed")).toBeInTheDocument();
    expect(within(alphaCard as HTMLElement).getByTitle(alphaDigest)).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Trust verified" }));
    expect(screen.getByRole("link", { name: "Alpha Model" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Beta Model" })).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Trust local" }));
    expect(screen.getByRole("link", { name: "Beta Model" })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Source git" }));
    expect(screen.queryByRole("link", { name: "Alpha Model" })).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Beta Model" })).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Trust verified" }));
    await user.click(screen.getByRole("button", { name: "Trust local" }));
    await user.click(screen.getByRole("button", { name: "Source git" }));
    await user.selectOptions(screen.getByLabelText("Node count"), "2");
    expect(screen.getByRole("link", { name: "Alpha Model" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Gamma Model" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Beta Model" })).not.toBeInTheDocument();

    await user.selectOptions(screen.getByLabelText("Node count"), "");
    await user.selectOptions(screen.getByLabelText("Sort recipes"), "newest");
    expect(screen.getAllByRole("link").map((link) => link.textContent)).toEqual([
      "Alpha Model",
      "Gamma Model",
      "Beta Model",
    ]);

    await user.clear(screen.getByLabelText("Search recipes"));
    await user.type(screen.getByLabelText("Search recipes"), gammaDigest);
    expect(screen.getByRole("link", { name: "Gamma Model" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Alpha Model" })).not.toBeInTheDocument();
    await user.clear(screen.getByLabelText("Search recipes"));
    await user.type(screen.getByLabelText("Search recipes"), "does-not-exist");
    expect(screen.getByText("No recipes match")).toBeInTheDocument();

    await user.clear(screen.getByLabelText("Search recipes"));
    await user.click(await screen.findByRole("link", { name: "Alpha Model" }));
    expect(screen.getByLabelText("Current path")).toHaveTextContent(`/library/recipes/${alphaDigest}`);
  });

  it("opens the real import and recipe-preselected hardware dialogs", async () => {
    installCatalogHandlers();
    const user = userEvent.setup();
    renderCatalog();

    await user.click(await screen.findByRole("button", { name: "Import recipe" }));
    expect(await screen.findByRole("heading", { name: "Install recipe" })).toBeInTheDocument();
    await user.keyboard("{Escape}");

    const alphaCard = (await screen.findByRole("link", { name: "Alpha Model" })).closest("article");
    await user.click(within(alphaCard as HTMLElement).getByRole("button", { name: "Choose hardware" }));
    expect(await screen.findByRole("heading", { name: "Choose hardware" })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByLabelText("Recipe")).toHaveValue(alphaDigest));
  });

  it("distinguishes loading, API-empty, and retryable error states", async () => {
    let release: (() => void) | undefined;
    server.use(
      http.get("*/api/v1/recipes", async () => {
        await new Promise<void>((resolve) => { release = resolve; });
        return HttpResponse.json([]);
      }),
      http.get("*/api/v1/nodes", () => HttpResponse.json([])),
      http.get("*/api/v1/fabrics", () => HttpResponse.json([])),
    );
    const loadingView = renderCatalog();
    expect(await screen.findByText("Loading recipes…")).toBeInTheDocument();
    release?.();
    expect(await screen.findByText("No recipes installed")).toBeInTheDocument();
    loadingView.unmount();

    let attempts = 0;
    server.use(
      http.get("*/api/v1/recipes", () => {
        attempts += 1;
        return attempts === 1
          ? HttpResponse.json({ code: "test.failure", message: "Catalog unavailable" }, { status: 503 })
          : HttpResponse.json([]);
      }),
      http.get("*/api/v1/nodes", () => HttpResponse.json([])),
      http.get("*/api/v1/fabrics", () => HttpResponse.json([])),
    );
    const user = userEvent.setup();
    renderCatalog();
    expect(await screen.findByText("Cannot load recipes")).toBeInTheDocument();
    expect(screen.getByText("Catalog unavailable")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "retry" }));
    expect(await screen.findByText("No recipes installed")).toBeInTheDocument();
  });
});
