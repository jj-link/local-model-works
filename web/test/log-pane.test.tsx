/// <reference types="@testing-library/jest-dom" />
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { SseEvent, StreamState } from "~/lib/api/sse";

const streamSpies = vi.hoisted(() => ({
  close: vi.fn(),
  open: vi.fn(),
}));

vi.mock("~/lib/api/sse", () => ({
  streamEvents: streamSpies.open,
}));

import { LogPane } from "~/components/log-pane";

beforeEach(() => {
  streamSpies.close.mockClear();
  streamSpies.open.mockReset();
  streamSpies.open.mockImplementation((_url, options) => {
    options.onEvent({ id: "12", event: "message", data: JSON.stringify("retained worker failure") });
    options.onEvent({ id: "13", event: "end", data: "done" });
    return { close: streamSpies.close };
  });
});

afterEach(() => vi.restoreAllMocks());

describe("LogPane retained output", () => {
  it("loads terminal workload logs when the source is no longer active", async () => {
    render(<LogPane url="/api/v1/deployments/dep/logs?rank=1" active={false} />);

    await waitFor(() => expect(streamSpies.open).toHaveBeenCalledOnce());
    expect(screen.getByText("retained worker failure")).toBeInTheDocument();
    expect(screen.getByText("stream ended")).toBeInTheDocument();
  });

  it("shows receipt of hidden diagnostics without adding them to operator output", () => {
    let emit!: (event: SseEvent) => void;
    streamSpies.open.mockImplementation((_url, options) => {
      emit = options.onEvent;
      options.onState({ connected: true, reconnecting: false, attempt: 0 });
      return { close: streamSpies.close };
    });
    const clock = vi.spyOn(Date, "now").mockReturnValue(1000);
    const { container } = render(<LogPane url="/logs" active summarize />);
    act(() => emit({ id: "1", event: "snapshot", data: JSON.stringify("worker ready\n") }));
    // Retained history is not evidence that the source produced fresh output.
    expect(container.querySelector("time")).toBeNull();
    clock.mockReturnValue(2000);
    const poll = 'INFO: "GET /health HTTP/1.1" 200 OK';
    act(() => emit({ id: "2", event: "message", data: JSON.stringify(poll + "\n") }));
    expect(container.querySelector("time")).toHaveAttribute("datetime", new Date(2000).toISOString());
    expect(screen.getByRole("log")).toHaveTextContent("worker ready");
    expect(screen.getByRole("log")).not.toHaveTextContent(poll);
  });

  it("resumes following immediately even when an idle source sends no more output", () => {
    streamSpies.open.mockImplementation((_url, options) => {
      options.onEvent({ id: "1", event: "snapshot", data: JSON.stringify("latest output\n") });
      return { close: streamSpies.close };
    });
    render(<LogPane url="/logs" active summarize />);
    const log = screen.getByRole("log");
    Object.defineProperty(log, "scrollHeight", { configurable: true, value: 800 });
    fireEvent.click(screen.getByRole("button", { name: "Pause follow" }));
    log.scrollTop = 0;
    fireEvent.click(screen.getByRole("button", { name: "Resume follow" }));
    expect(log.scrollTop).toBe(800);
  });

  it("retains both snapshot streams, reconnect fragments and raw diagnostics without reopening", () => {
    let emit!: (event: SseEvent) => void;
    let connection!: (state: StreamState) => void;
    streamSpies.open.mockImplementation((_url, options) => {
      emit = options.onEvent;
      connection = options.onState;
      return { close: streamSpies.close };
    });
    render(<LogPane url="/deployment/logs" active summarize />);
    const poll = 'INFO: "GET /health HTTP/1.1" 200 OK';
    act(() => emit({
      id: "20:40", event: "snapshot",
      data: JSON.stringify({ stdout: "stdout ready\nstdout par", stderr: `llama ready\n${poll}\nstderr par` }),
    }));
    expect(screen.getByText("stdout ready")).toBeInTheDocument();
    expect(screen.getByText("llama ready")).toBeInTheDocument();
    expect(screen.getByText("stdout par")).toBeInTheDocument();
    expect(screen.getByText("stderr par")).toBeInTheDocument();
    expect(screen.queryByText(poll)).not.toBeInTheDocument();
    act(() => connection({ connected: false, reconnecting: true, attempt: 1 }));
    expect(screen.getByText("reconnecting")).toBeInTheDocument();
    act(() => {
      connection({ connected: true, reconnecting: false, attempt: 0 });
      emit({ id: "25:40", event: "message", data: JSON.stringify({ stream: "stdout", data: "tial\nout tail" }) });
      emit({ id: "25:45", event: "message", data: JSON.stringify({ stream: "stderr", data: "tial\nerr tail" }) });
    });
    expect(screen.getAllByText("stdout ready")).toHaveLength(1);
    expect(screen.getAllByText("llama ready")).toHaveLength(1);
    expect(screen.getByText("stdout partial")).toBeInTheDocument();
    expect(screen.getByText("stderr partial")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Raw diagnostics" }));
    expect(screen.getByText(poll)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Raw diagnostics" }));
    expect(screen.queryByText(poll)).not.toBeInTheDocument();
    expect(streamSpies.open).toHaveBeenCalledOnce();
    expect(streamSpies.close).not.toHaveBeenCalled();
    act(() => emit({ id: "25:45", event: "end", data: JSON.stringify({ stdout: 25, stderr: 45 }) }));
    expect(screen.getByText("out tail")).toBeInTheDocument();
    expect(screen.getByText("err tail")).toBeInTheDocument();
    expect(screen.getByText("stream ended")).toBeInTheDocument();
    expect(streamSpies.close).toHaveBeenCalledOnce();
  });

  it("replaces both streams together on a fresh snapshot rather than erasing either half", () => {
    let emit!: (event: SseEvent) => void;
    streamSpies.open.mockImplementation((_url, options) => {
      emit = options.onEvent;
      return { close: streamSpies.close };
    });
    render(<LogPane url="/deployment/logs" active />);
    act(() => emit({ id: "10:10", event: "snapshot", data: JSON.stringify({ stdout: "old out\n", stderr: "old err\n" }) }));
    act(() => emit({ id: "20:20", event: "snapshot", data: JSON.stringify({ stdout: "new out\n", stderr: "new err\n" }) }));
    expect(screen.queryByText("old out")).not.toBeInTheDocument();
    expect(screen.queryByText("old err")).not.toBeInTheDocument();
    expect(screen.getByText("new out")).toBeInTheDocument();
    expect(screen.getByText("new err")).toBeInTheDocument();
  });
  it("keeps the same cursor and partial buffers when workload activity changes", () => {
    let emit!: (event: SseEvent) => void;
    streamSpies.open.mockImplementation((_url, options) => {
      emit = options.onEvent;
      options.onState({ connected: true, reconnecting: false, attempt: 0 });
      return { close: streamSpies.close };
    });
    const view = render(<LogPane url="/deployment/logs" active />);
    act(() => emit({ id: "4:4", event: "snapshot", data: JSON.stringify({ stdout: "out ", stderr: "err " }) }));
    view.rerender(<LogPane url="/deployment/logs" active={false} />);
    expect(screen.getByText(/source inactive/)).toBeInTheDocument();
    expect(streamSpies.open).toHaveBeenCalledOnce();
    expect(streamSpies.close).not.toHaveBeenCalled();
    act(() => {
      emit({ id: "9:4", event: "message", data: JSON.stringify({ stream: "stdout", data: "tail\n" }) });
      emit({ id: "9:9", event: "message", data: JSON.stringify({ stream: "stderr", data: "tail\n" }) });
      emit({ id: "9:9", event: "end", data: "{}" });
    });
    expect(screen.getAllByText("out tail")).toHaveLength(1);
    expect(screen.getAllByText("err tail")).toHaveLength(1);
    expect(screen.getByText("stream ended")).toBeInTheDocument();
  });

  it("clears connected state after session expiry without erasing retained output", () => {
    let connection!: (state: StreamState) => void;
    streamSpies.open.mockImplementation((_url, options) => {
      connection = options.onState;
      options.onEvent({ id: "10", event: "message", data: JSON.stringify("retained\n") });
      options.onState({ connected: true, reconnecting: false, attempt: 0 });
      return { close: streamSpies.close };
    });
    render(<LogPane url="/run/logs" active />);
    expect(screen.getByText(/^connected/)).toBeInTheDocument();
    act(() => connection({ connected: false, reconnecting: false, attempt: 1, status: 401, error: "Stream unavailable (HTTP 401)" }));
    expect(screen.queryByText(/^connected/)).not.toBeInTheDocument();
    expect(screen.getByText("session expired — sign in to reconnect")).toBeInTheDocument();
    expect(screen.getByText("retained")).toBeInTheDocument();
    expect(streamSpies.close).toHaveBeenCalledOnce();
  });

  it("ignores late callbacks from the previous URL", () => {
    const emitters: ((event: SseEvent) => void)[] = [];
    streamSpies.open.mockImplementation((_url, options) => {
      emitters.push(options.onEvent);
      return { close: streamSpies.close };
    });
    const view = render(<LogPane url="/rank/0" active />);
    view.rerender(<LogPane url="/rank/1" active />);
    act(() => {
      emitters[1]({ id: "1", event: "message", data: JSON.stringify("current rank\n") });
      emitters[0]({ id: "2", event: "snapshot", data: JSON.stringify("obsolete rank\n") });
    });
    expect(screen.getByText("current rank")).toBeInTheDocument();
    expect(screen.queryByText("obsolete rank")).not.toBeInTheDocument();
  });
});
