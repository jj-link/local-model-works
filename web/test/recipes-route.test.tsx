/// <reference types="@testing-library/jest-dom" />
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { MemoryRouter, useLocation } from "react-router";
import { beforeEach, describe, expect, it } from "vitest";

import RecipesRoute from "~/routes/library/recipes/index";
import { server } from "../msw/server";

const currentDigest = `sha256:${"a".repeat(64)}`;
const retainedDigest = `sha256:${"b".repeat(64)}`;
const packageDigest = `sha256:${"c".repeat(64)}`;
const recipe = { digest: currentDigest, name: "Alpha", version: "2.0.0", source: { type: "git", remote: "https://github.com/example/alpha" }, installed_at: "2026-01-03T00:00:00Z" };
const retained = { ...recipe, digest: retainedDigest, version: "1.0.0" };
const localPackage = { ...recipe, digest: packageDigest, name: "Local package", source: { type: "local", remote: "/srv/recipe" } };
const repository = {
  id: "repo-alpha", source_url: "https://github.com/example/alpha", source_path: ".", tracking_ref: "main",
  current_recipe: recipe, installed_commit: "1".repeat(40), observed_head_commit: "2".repeat(40),
  versions: [{ recipe, commit_sha: "1".repeat(40), canonical: true }, { recipe: retained, commit_sha: "0".repeat(40), canonical: false }],
  installed_devices: [], update_available: true, update_supported: true,
};

function Location() {
  const location = useLocation();
  return <output aria-label="Current route">{location.pathname + location.search}</output>;
}
function renderCatalog() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><MemoryRouter initialEntries={["/library/recipes"]}><RecipesRoute /><Location /></MemoryRouter></QueryClientProvider>);
}
function installHandlers() {
  server.use(
    http.get("*/api/v1/recipe-repositories", () => HttpResponse.json([repository])),
    http.get("*/api/v1/recipes", () => HttpResponse.json([recipe, retained, localPackage])),
    http.get("*/api/v1/recipe-drafts", () => HttpResponse.json([])),
    http.get("*/api/v1/deployments", () => HttpResponse.json([])),
  );
}

beforeEach(() => sessionStorage.clear());
describe("Recipe catalog", () => {
  it("opens repository or package details without launching and deduplicates retained versions", async () => {
    installHandlers();
    const mutations: string[] = [];
    server.use(http.post("*/api/v1/*", ({ request }) => { mutations.push(request.url); return HttpResponse.json({}); }));
    const user = userEvent.setup();
    renderCatalog();
    const source = await screen.findByRole("link", { name: "Open Alpha" });
    expect(screen.getAllByRole("link", { name: "Open Alpha" })).toHaveLength(1);
    await user.click(source);
    expect(screen.getByLabelText("Current route")).toHaveTextContent("/library/recipes/repositories/repo-alpha");
    await user.click(screen.getByRole("link", { name: "Open Local package" }));
    expect(screen.getByLabelText("Current route")).toHaveTextContent(`/library/recipes/packages/${encodeURIComponent(packageDigest)}`);
    expect(mutations).toEqual([]);
    await user.type(screen.getByRole("searchbox", { name: "Search recipes" }), retainedDigest);
    expect(screen.getByRole("link", { name: "Open Alpha" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Open Local package" })).not.toBeInTheDocument();
  });

  it("keeps failed inspections and orphaned additions recoverable without duplicating catalog work", async () => {
    installHandlers();
    server.use(http.get("*/api/v1/recipe-drafts", () => HttpResponse.json([
      { id: "failed-addition", state: "failed", source: { remote: "https://github.com/example/failed" }, updated_at: "2026-01-04T00:00:00Z" },
      { id: "orphaned-addition", state: "installed", package_digest: `sha256:${"d".repeat(64)}`, source: { remote: "https://github.com/example/orphaned" }, updated_at: "2026-01-04T00:00:00Z" },
      { id: "already-added", state: "installed", package_digest: currentDigest, source: { remote: "https://github.com/example/added" } },
      { id: "saved-repair", state: "ready", source: { remote: "https://github.com/example/repair" }, change_context: { repository_id: repository.id, base_recipe_digest: currentDigest } },
    ])));
    renderCatalog();
    const failed = new URL((await screen.findByRole("link", { name: /example\/failed/ })).getAttribute("href")!, window.location.origin);
    const orphaned = new URL(screen.getByRole("link", { name: /example\/orphaned/ }).getAttribute("href")!, window.location.origin);
    expect(failed.pathname).toBe("/library/recipes/new");
    expect(failed.searchParams.get("draft")).toBe("failed-addition");
    expect(orphaned.pathname).toBe("/library/recipes/new");
    expect(orphaned.searchParams.get("draft")).toBe("orphaned-addition");
    expect(screen.queryByRole("link", { name: /example\/added/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /example\/repair/ })).not.toBeInTheDocument();
  });

  it("retains independently loaded packages while a catalog error is explicitly retried", async () => {
    installHandlers();
    let attempts = 0;
    server.use(http.get("*/api/v1/recipe-repositories", () => ++attempts === 1
      ? HttpResponse.json({ code: "catalog.unavailable", message: "Catalog unavailable" }, { status: 503 })
      : HttpResponse.json([repository])));
    const user = userEvent.setup();
    renderCatalog();
    expect(await screen.findByRole("alert")).toHaveTextContent("Catalog unavailable");
    expect(screen.getByRole("link", { name: "Open Local package" })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());
    expect(screen.getByRole("link", { name: "Open Alpha" })).toHaveAttribute("href", "/library/recipes/repositories/repo-alpha");
    expect(attempts).toBe(2);
  });
});
