/// <reference types="@testing-library/jest-dom" />
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { MemoryRouter } from "react-router";
import { describe, expect, it } from "vitest";

import RunDetailRoute from "~/routes/runs/$id";
import { server } from "../msw/server";

const runId = "01900000-0000-7000-8000-000000000010";

const detailRun = {
  id: runId,
  module: "benchmarks",
  kind: "benchmark",
  state: "succeeded",
  created_at: "2026-08-31T12:00:00Z",
  started_at: "2026-08-31T12:00:01Z",
  finished_at: "2026-08-31T12:01:00Z",
};

const result = {
  run_id: runId,
  benchmark_id: "terminal-bench",
  benchmark_version: "3.0.0",
  harness: "codex",
  candidate_count: 2,
  task_count: 2,
  passed_count: 1,
  pass_at_1: 0.5,
  oracle_pass_rate: null,
  verifier_pass_rate: 0.75,
  wall_seconds: 59,
  prompt_tokens: 100,
  completion_tokens: 200,
  total_tokens: 300,
  language_results: [
    {
      run_id: runId,
      language: "python",
      created_at: "2026-08-31T12:00:00Z",
      requests: 4,
      successes: 3,
      tokens_per_second: 12.5,
      latency_ms: { p50: 210, p90: 480 },
      prompt_tokens: 100,
      completion_tokens: 200,
      total_tokens: 300,
      wall_seconds: 30,
    },
  ],
};

const trials = [
  {
    run_id: runId,
    task_id: "html-js-filter",
    candidate_index: 0,
    official_pass: true,
    verifier_selected: true,
    verifier_score: 0.9,
    metrics: {},
    created_at: "2026-08-31T12:00:00Z",
    trajectory_path: "jobs/x/trajectory.json",
  },
  {
    run_id: runId,
    task_id: "html-js-filter",
    candidate_index: 1,
    official_pass: false,
    verifier_selected: false,
    verifier_score: 0.2,
    metrics: {},
    created_at: "2026-08-31T12:00:30Z",
    trajectory_path: "jobs/y/trajectory.json",
  },
];

function installHandlers() {
  server.use(
    http.get(`*/api/v1/runs/${runId}`, () => HttpResponse.json(detailRun)),
    http.get(`*/api/v1/benchmarks/${runId}`, () =>
      HttpResponse.json({ run: detailRun, result }),
    ),
    http.get(`*/api/v1/benchmarks/${runId}/trials`, () => HttpResponse.json(trials)),
    http.get(`*/api/v1/runs/${runId}/logs`, () => HttpResponse.text("log line")),
  );
}

function renderDetail() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[`/runs/${runId}`]}>
        <RunDetailRoute />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("RunDetailRoute benchmark summary panel", () => {
  it("renders summary fields, language rows, and normalized trials", async () => {
    installHandlers();
    renderDetail();

    await screen.findByText("python"); // summary has landed
    expect(
      screen.getByText((_, el) => el?.textContent === `benchmark terminal-bench@3.0.0 (codex)`),
    ).toBeInTheDocument();
    expect(screen.getByText("pass@1")).toBeInTheDocument();
    expect(screen.getByText("50%")).toBeInTheDocument();

    // Language rows
    expect(screen.getByText("python")).toBeInTheDocument();
    expect(screen.getByText("3/4")).toBeInTheDocument();
    expect(screen.getByText("210ms")).toBeInTheDocument();

    // Trials inside <details>
    expect(screen.getByText(/trials \(2\)/)).toBeInTheDocument();
    expect(screen.getByText("candidate")).toBeInTheDocument();
    expect(screen.getAllByText("html-js-filter")).toHaveLength(2);
    expect(screen.getByText("0.90")).toBeInTheDocument();

    // Bundle download affordance
    expect(
      screen.getByRole("link", { name: "download bundle" }),
    ).toHaveAttribute("href", `/api/v1/benchmarks/${runId}/bundle`);
  });

  it("shows an explicit error state with retry when the summary fails", async () => {
    server.use(
      http.get(`*/api/v1/runs/${runId}`, () => HttpResponse.json(detailRun)),
      http.get(`*/api/v1/benchmarks/${runId}`, () =>
        HttpResponse.json({ code: "resource.not_found", message: "missing" }, { status: 404 }),
      ),
      http.get(`*/api/v1/benchmarks/${runId}/trials`, () => HttpResponse.json([])),
      http.get(`*/api/v1/runs/${runId}/logs`, () => HttpResponse.text("log line")),
    );
    const user = userEvent.setup();
    renderDetail();

    expect(await screen.findByText("Cannot load benchmark summary")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /retry/i }));
    expect(await screen.findByText("Cannot load benchmark summary")).toBeInTheDocument();
  });
  it("offers cancel only while the run is live and posts to the module route", async () => {
    const cancelCalls: string[] = [];
    server.use(
      http.get(`*/api/v1/runs/${runId}`, () =>
        HttpResponse.json({ ...detailRun, state: "running" })),
      http.get(`*/api/v1/benchmarks/${runId}`, () =>
        HttpResponse.json({ run: { ...detailRun, state: "running" }, result })),
      http.get(`*/api/v1/benchmarks/${runId}/trials`, () => HttpResponse.json(trials)),
      http.get(`*/api/v1/runs/${runId}/logs`, () => HttpResponse.text("log line")),
      http.post(`*/api/v1/benchmarks/${runId}/cancel`, () => {
        cancelCalls.push(runId);
        return HttpResponse.json({ ...detailRun, state: "cancelled" }, { status: 202 });
      }),
    );
    const user = userEvent.setup();
    renderDetail();
    await screen.findByText("python");
    await user.click(screen.getByRole("button", { name: "cancel benchmark" }));
    await waitFor(() => expect(cancelCalls).toEqual([runId]));
  });

  it("hides cancel once the summary is terminal", async () => {
    installHandlers();
    renderDetail();
    await screen.findByText("python");
    expect(screen.queryByRole("button", { name: "cancel benchmark" })).not.toBeInTheDocument();
  });
});

