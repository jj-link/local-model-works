/// <reference types="@testing-library/jest-dom" />
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { MemoryRouter, Route, Routes } from "react-router";
import { describe, expect, it } from "vitest";
import { CodingTraceDetailRoute } from "../app/routes/coding-traces/index";
import { server } from "../msw/server";

describe("CodingTraceDetailRoute", () => {
  it("shows ordered events, diff, verification, and redaction metadata", async () => {
    server.use(
      http.get("*/api/v1/coding-traces/trace-1", () => HttpResponse.json({ id: "trace-1", run_id: "run-1", task_id: "task-1", problem: "repair", repository: "owner/repo", base_revision: "abc", model_source: "external_api", model: "model", scaffold: "openhands-codeact", sampling: { temperature: 0 }, state: "completed", final_diff: "diff --git a/a b/a", success_label: false, failure_kind: "tests_failed", token_count: 33, turn_count: 2, pinned: false, schema_version: "localmodelworks/agent-trace/v1", redaction_version: "lmw-redaction-v1", redaction_count: 3, digest: "sha256:1234567890abcdef", created_at: "2026-08-24T00:00:00Z", verification: { status: "unresolved", fail_to_pass_report: { failed: ["test_fix"] } } })),
      http.get("*/api/v1/coding-traces/trace-1/events", () => HttpResponse.json({ items: [{ trace_id: "trace-1", sequence: 0, event_id: "event-1", agent_id: "", parent_agent_id: "", occurred_at: "2026-08-24T00:00:01Z", kind: "tool.call", payload: { command: "pytest" }, input_tokens: 10, output_tokens: 5, redaction_count: 1 }], next_cursor: "1" })),
    );
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><MemoryRouter initialEntries={["/coding-traces/trace-1"]}><Routes><Route path="/coding-traces/:id" element={<CodingTraceDetailRoute />} /></Routes></MemoryRouter></QueryClientProvider>);
    expect(await screen.findByText("task-1")).toBeInTheDocument();
    expect(screen.getByText("tool.call")).toBeInTheDocument();
    expect(screen.getByText(/diff --git a\/a b\/a/)).toBeInTheDocument();
    expect(screen.getByText(/test_fix/)).toBeInTheDocument();
    expect(screen.getByText("3")).toBeInTheDocument();
  });
});
