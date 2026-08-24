/// <reference types="@testing-library/jest-dom" />
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { MemoryRouter } from "react-router";
import { describe, expect, it } from "vitest";
import CodingTracesRoute from "../app/routes/coding-traces/index";
import { server } from "../msw/server";

function renderRoute(path: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><MemoryRouter initialEntries={[path]}><CodingTracesRoute /></MemoryRouter></QueryClientProvider>);
}

describe("CodingTracesRoute", () => {
  it("uses SWE-Gym as the default subtab", async () => {
    server.use(
      http.get("*/api/v1/deployments", () => HttpResponse.json([])),
      http.get("*/api/v1/secrets", () => HttpResponse.json([])),
      http.get("*/api/v1/coding-traces/swe-gym/experiments", () => HttpResponse.json({ items: [], next_cursor: "" })),
    );
    renderRoute("/coding-traces");
    expect(screen.getByRole("tab", { name: /SWE-Gym Replication/i })).toHaveAttribute("aria-selected", "true");
    expect(await screen.findByText("replication control")).toBeInTheDocument();
    expect(screen.getByText(/pinned sources/i)).toBeInTheDocument();
  });

  it("renders sanitized trajectory eligibility and counts", async () => {
    server.use(http.get("*/api/v1/coding-traces", () => HttpResponse.json({ items: [{ id: "trace-1", run_id: "run-1", task_id: "getmoto__moto-5752", problem: "fix", repository: "getmoto/moto", base_revision: "abc", model_source: "lmw_deployment", model: "local-model", scaffold: "openhands-codeact", sampling: {}, state: "completed", success_label: true, token_count: 1450, turn_count: 12, pinned: false, schema_version: "localmodelworks/agent-trace/v1", redaction_version: "lmw-redaction-v1", redaction_count: 2, created_at: "2026-08-24T00:00:00Z" }], next_cursor: "" })));
    renderRoute("/coding-traces?tab=traces");
    expect(await screen.findByText("getmoto__moto-5752")).toBeInTheDocument();
    expect(screen.getByText("resolved")).toBeInTheDocument();
    expect(screen.getByText("1,450")).toBeInTheDocument();
  });
});
