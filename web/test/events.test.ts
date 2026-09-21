import { QueryClient, QueryObserver } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { SseEvent } from "~/lib/api/sse";
import { eventStream } from "~/lib/events";

const transport = vi.hoisted(() => ({ receive: undefined as ((event: SseEvent) => void) | undefined }));
vi.mock("~/lib/api/sse", () => ({
  streamEvents: (_url: string, options: { onEvent: (event: SseEvent) => void }) => {
    transport.receive = options.onEvent;
    return { close: () => { transport.receive = undefined; } };
  },
}));

let client: QueryClient;
let unsubscribe: (() => void)[];
function observe<T>(key: string[], initial: T, queryFn: () => Promise<T>) {
  client.setQueryData(key, initial);
  const observer = new QueryObserver(client, { queryKey: key, queryFn });
  unsubscribe.push(observer.subscribe(() => {}));
  return observer;
}
function emit(type: string) {
  transport.receive!({ id: null, event: type, data: "{}" });
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(new Date("2026-09-09T05:43:47.984Z"));
  client = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity, gcTime: Infinity, retry: false } } });
  unsubscribe = [];
  eventStream.bindQueryClient(client);
  eventStream.start();
});
afterEach(() => {
  eventStream.stop();
  unsubscribe.forEach((stop) => stop());
  client.clear();
  vi.useRealTimers();
});

describe("event-driven query refresh", () => {
  it("refreshes the recipe and deployment after adjacent updates without postponing or duplicating the batch", async () => {
    let serverPort = 8000;
    const readRecipe = vi.fn(async () => ({ port: serverPort }));
    const readDeployment = vi.fn(async () => "healthy");
    const recipe = observe(["recipes", "repositories", "qwen"], { port: 8888 }, readRecipe);
    const deployment = observe(["deployments", "qwen"], "starting", readDeployment);

    emit("deployment.state");
    await vi.advanceTimersByTimeAsync(500);
    emit("deployment.state");
    await vi.advanceTimersByTimeAsync(390);
    emit("recipe.installed");
    emit("recipe.installed");
    await vi.advanceTimersByTimeAsync(110);

    expect(recipe.getCurrentResult().data).toEqual({ port: 8000 });
    expect(deployment.getCurrentResult().data).toBe("healthy");
    expect(readRecipe).toHaveBeenCalledTimes(1);
    expect(readDeployment).toHaveBeenCalledTimes(1);

    serverPort = 8001;
    emit("recipe.installed");
    await vi.advanceTimersByTimeAsync(1000);
    expect(recipe.getCurrentResult().data).toEqual({ port: 8001 });
    expect(readRecipe).toHaveBeenCalledTimes(2);
    expect(readDeployment).toHaveBeenCalledTimes(1);
  });

  it("discards pending refreshes on stop and handles fresh events after restart", async () => {
    const recipe = observe(["recipes", "repositories", "qwen"], { port: 8888 }, async () => ({ port: 8000 }));
    emit("recipe.installed");
    await vi.advanceTimersByTimeAsync(400);
    eventStream.stop();
    await vi.advanceTimersByTimeAsync(1000);
    expect(recipe.getCurrentResult().data).toEqual({ port: 8888 });

    eventStream.start();
    emit("deployment.state");
    await vi.advanceTimersByTimeAsync(1000);
    expect(recipe.getCurrentResult().data).toEqual({ port: 8888 });
    emit("recipe.installed");
    await vi.advanceTimersByTimeAsync(1000);
    expect(recipe.getCurrentResult().data).toEqual({ port: 8000 });
  });
});
