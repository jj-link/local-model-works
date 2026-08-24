/// <reference types="@testing-library/jest-dom" />
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { MemoryRouter } from "react-router";
import { describe, expect, it } from "vitest";
import AutoResearchRoute from "~/routes/autoresearch";
import { server } from "../msw/server";

function renderRoute() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/autoresearch"]}><AutoResearchRoute /></MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("AutoResearchRoute", () => {
  it("renders the intentional empty project state", async () => {
    server.use(
      http.get("*/api/v1/autoresearch/projects", () => HttpResponse.json([])),
      http.get("*/api/v1/nodes", () => HttpResponse.json([])),
    );
    renderRoute();
    expect(await screen.findByText("No research project selected")).toBeVisible();
    expect(screen.getByRole("button", { name: /project/i })).toBeEnabled();
  });

  it("shows editable candidates and a blocked source at the selection gate", async () => {
    server.use(http.get("*/api/v1/nodes", () => HttpResponse.json([])));
    const user = userEvent.setup();
    renderRoute();
    const ideasButton = await screen.findByRole("button", { name: "ideas" });
    await user.click(ideasButton);
    expect(await screen.findByRole("tab", { name: /01/ })).toBeVisible();
    expect(screen.getByLabelText("candidate title")).toHaveValue("Candidate one");
    expect(screen.getByText("license acceptance required")).toBeVisible();
    expect(screen.getByRole("button", { name: /continue selected/i })).toBeDisabled();
  });
});
