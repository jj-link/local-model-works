/// <reference types="@testing-library/jest-dom" />
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { MemoryRouter } from "react-router";
import { describe, expect, it } from "vitest";

import Chat from "~/routes/chat/index";
import { server } from "../msw/server";

const deploymentId = "11111111-1111-4111-8111-111111111111";
const readyDeployment = {
  id: deploymentId,
  recipe_digest: "sha256:ready",
  recipe_name: "deepseek-recipe",
  engine: "vllm",
  profile: "",
  placements: [{ node_id: "node-1", node_name: "spark1", rank: 0 }],
  desired_state: "running",
  observed_state: "healthy",
  endpoint: { host: "spark1", port: 8888, model: "DeepSeek V4 Flash" },
  created_at: "2026-08-22T00:00:00Z",
  updated_at: "2026-08-22T00:00:00Z",
};

function renderChat() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/chat"]}>
        <Chat />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function completion(content = "A real response") {
  return {
    id: "chat-1",
    model: "DeepSeek V4 Flash",
    message: { role: "assistant", content, reasoning_content: "Checked the request." },
    finish_reason: "stop",
    usage: { prompt_tokens: 4, completion_tokens: 3, total_tokens: 7 },
  };
}

describe("Chat", () => {
  it("filters deployments and displays the real completion", async () => {
    let posted: unknown;
    server.use(
      http.get("*/api/v1/deployments", () =>
        HttpResponse.json([
          readyDeployment,
          { ...readyDeployment, id: "22222222-2222-4222-8222-222222222222", observed_state: "degraded", endpoint: { ...readyDeployment.endpoint, model: "Hidden unhealthy" } },
          { ...readyDeployment, id: "33333333-3333-4333-8333-333333333333", endpoint: { host: "spark3", port: 8888 } },
        ]),
      ),
      http.post("*/api/v1/chat/completions", async ({ request }) => {
        posted = await request.json();
        return HttpResponse.json(completion());
      }),
    );
    const user = userEvent.setup();
    renderChat();

    const selector = await screen.findByRole("combobox", { name: "Deployment" });
    expect(selector).toHaveTextContent("DeepSeek V4 Flash · deepseek-recipe · vLLM · spark1");
    expect(screen.queryByText(/Hidden unhealthy/)).not.toBeInTheDocument();

    await user.type(screen.getByRole("textbox", { name: "Chat message" }), "Explain this model");
    await user.click(screen.getByRole("button", { name: "Send" }));

    expect(await screen.findByText("A real response")).toBeInTheDocument();
    expect(screen.getByText("Checked the request.")).toBeInTheDocument();
    expect(screen.getByText(/4 prompt · 3 completion · 7 total tokens/)).toBeInTheDocument();
    expect(posted).toEqual({
      deployment_id: deploymentId,
      messages: [{ role: "user", content: "Explain this model" }],
    });
  });

  it("locks duplicate sends while a completion is pending and supports cancel", async () => {
    let release: (() => void) | undefined;
    server.use(
      http.get("*/api/v1/deployments", () => HttpResponse.json([readyDeployment])),
      http.post("*/api/v1/chat/completions", async () => {
        await new Promise<void>((resolve) => {
          release = resolve;
        });
        return HttpResponse.json(completion());
      }),
    );
    const user = userEvent.setup();
    renderChat();

    const composer = await screen.findByRole("textbox", { name: "Chat message" });
    await user.type(composer, "Wait for this");
    await user.click(screen.getByRole("button", { name: "Send" }));

    expect(await screen.findByText("Waiting for the deployment…")).toBeInTheDocument();
    expect(composer).toBeDisabled();
    expect(screen.queryByRole("button", { name: "Send" })).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    release?.();
    await waitFor(() => expect(screen.queryByText("Waiting for the deployment…")).not.toBeInTheDocument());
    expect(within(screen.getByRole("region", { name: "Chat conversation" })).getByText("Wait for this")).toBeInTheDocument();
  });

  it("retries a failed completion without duplicating the user message", async () => {
    let attempts = 0;
    server.use(
      http.get("*/api/v1/deployments", () => HttpResponse.json([readyDeployment])),
      http.post("*/api/v1/chat/completions", () => {
        attempts += 1;
        if (attempts === 1) {
          return HttpResponse.json(
            { code: "chat.upstream_unavailable", message: "Engine unavailable" },
            { status: 502 },
          );
        }
        return HttpResponse.json(completion("Recovered response"));
      }),
    );
    const user = userEvent.setup();
    renderChat();

    await user.type(await screen.findByRole("textbox", { name: "Chat message" }), "Keep this question");
    await user.click(screen.getByRole("button", { name: "Send" }));
    expect(await screen.findByText("Engine unavailable")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Retry" }));

    expect(await screen.findByText("Recovered response")).toBeInTheDocument();
    expect(within(screen.getByRole("region", { name: "Chat conversation" })).getAllByText("Keep this question")).toHaveLength(1);
    expect(attempts).toBe(2);
  });

  it("offers the real deployment planner when no deployment is usable", async () => {
    server.use(
      http.get("*/api/v1/deployments", () => HttpResponse.json([])),
      http.get("*/api/v1/recipes", () => HttpResponse.json([])),
      http.get("*/api/v1/nodes", () => HttpResponse.json([])),
      http.get("*/api/v1/fabrics", () => HttpResponse.json([])),
    );
    const user = userEvent.setup();
    renderChat();

    expect(await screen.findByText("No chat-ready deployment")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Launch deployment" }));
    expect(await screen.findByRole("heading", { name: "Launch deployment" })).toBeInTheDocument();
  });
});
