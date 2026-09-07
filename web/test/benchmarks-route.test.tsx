/// <reference types="@testing-library/jest-dom" />
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { MemoryRouter } from "react-router";
import { describe, expect, it, beforeEach } from "vitest";
import { server } from "../msw/server";
import BenchmarksRoute from "~/routes/benchmarks/index";

const healthyDeployment = {
  id: "11111111-1111-4111-8111-111111111111",
  recipe_name: "alpha-model",
  recipe_digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  parameters: { model: "alpha-model" },
  placements: [{ node_id: "node-1", node_name: "spark1", rank: 0 }],
  desired_state: "running",
  observed_state: "healthy",
};

const catalog = {
  benchmarks: [
    {
      benchmark_id: "terminal-bench",
      title: "Terminal-Bench",
      version: "3.0.0",
      harnesses: [
        { id: "codegen-generative", title: "Generative agent", requires_generation_deployment: true, accepts_openai_compatible: true },
        { id: "oracle", title: "Oracle reference appendix", requires_generation_deployment: false, accepts_openai_compatible: false },
      ],
      supports_task_filters: true,
      supports_repeated_candidates: true,
      supports_verifier: true,
      task_count: 74,
      task_ids: ["html-js-filter", "commit-message-cleanliness"],
    },
  ],
  runner: { configured: true, online: true, max_concurrency: 4 },
};

const runResult = {
  run: {
    id: "01900000-0000-7000-8000-000000000010",
    module: "benchmarks",
    kind: "benchmark",
    state: "succeeded",
    created_at: "2026-08-31T12:00:00Z",
  },
  result: {
    run_id: "01900000-0000-7000-8000-000000000010",
    benchmark_id: "terminal-bench",
    benchmark_version: "3.0.0",
    harness: "codegen-generative",
    oracle_pass_rate: null,
    pass_at_1: 0.5,
    verifier_pass_rate: 0.75,
    candidate_count: 1,
    task_count: 2,
    passed_count: 1,
    prompt_tokens: 100,
    completion_tokens: 200,
    total_tokens: 300,
    wall_seconds: 60,
    language_results: [
      {
        run_id: "01900000-0000-7000-8000-000000000010",
        language: "python",
        created_at: "2026-08-31T12:00:00Z",
        requests: 4,
        successes: 2,
        tokens_per_second: 12.5,
        prompt_tokens: 100,
        completion_tokens: 200,
        total_tokens: 300,
        wall_seconds: 60,
      },
    ],
  },
};


function baseHandlers() {
  return [
    http.get("*/api/v1/benchmarks/catalog", () => HttpResponse.json(catalog)),
    http.get("*/api/v1/benchmarks/results", () => HttpResponse.json([runResult])),
    http.get("*/api/v1/benchmarks", () => HttpResponse.json([])),
    http.get("*/api/v1/deployments", () => HttpResponse.json([healthyDeployment])),
  ];
}

function renderBenchmarks() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/benchmarks"]}>
        <BenchmarksRoute />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  server.use(...baseHandlers());
});

describe("BenchmarksRoute catalog contract", () => {
  it("shows run-level language rows from full run results", async () => {
    renderBenchmarks();
    expect(await screen.findByText("terminal-bench@3.0.0")).toBeInTheDocument();
    expect(screen.getByText("python")).toBeInTheDocument();
    expect(screen.getByText("50%")).toBeInTheDocument(); // pass@1
    expect(screen.getByText("75%")).toBeInTheDocument(); // verifier
    expect(screen.getByText("2/4")).toBeInTheDocument(); // successes/requests
  });

});

