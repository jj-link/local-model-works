import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { autoResearchHandlers } from "./autoresearch-fixtures";

/**
 * Minimal MSW server for component tests. Extend with per-test handlers as
 * component tests are added; the setup file enforces unhandled-request errors
 * so real network calls never sneak into the suite.
 */
export const handlers = [
  http.get("*/api/v1/healthz", () =>
    HttpResponse.json({ ok: true, version: "test", commit: "test" }),
  ),
  ...autoResearchHandlers,
];

export const server = setupServer(...handlers);
