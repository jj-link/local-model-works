/// <reference types="@testing-library/jest-dom" />
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { MemoryRouter } from "react-router";
import { describe, expect, it } from "vitest";
import CodingTracesRoute from "../app/routes/coding-traces/index";
import { server } from "../msw/server";

describe("Coding trace settings", () => {
  it("writes typed defaults with the current ETag", async () => {
    let received: unknown;
    let ifMatch = "";
    server.use(
      http.get("*/api/v1/modules/coding-traces/settings", () => HttpResponse.json({ module: "coding-traces", settings: { capture_reasoning: true, retention_days: 0, export_max_context_tokens: 32768, export_success_cap_per_task: 2 }, version: "version-1234567890" })),
      http.put("*/api/v1/modules/coding-traces/settings", async ({ request }) => {
        received = await request.json();
        ifMatch = request.headers.get("if-match") ?? "";
        return HttpResponse.json({ ...(received as object), version: "version-next" });
      }),
    );
    const user = userEvent.setup();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    render(<QueryClientProvider client={client}><MemoryRouter initialEntries={["/coding-traces?tab=settings"]}><CodingTracesRoute /></MemoryRouter></QueryClientProvider>);
    const fields = await screen.findAllByRole("spinbutton");
    await user.clear(fields[0]);
    await user.type(fields[0], "30");
    await user.click(screen.getByRole("button", { name: /save settings/i }));
    await waitFor(() => expect(received).toBeTruthy());
    expect(ifMatch).toBe("version-1234567890");
    expect(received).toMatchObject({ module: "coding-traces", settings: { retention_days: 30, capture_reasoning: true, export_max_context_tokens: 32768, export_success_cap_per_task: 2 } });
  });
});
