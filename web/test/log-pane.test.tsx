/// <reference types="@testing-library/jest-dom" />
import { render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

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

describe("LogPane retained output", () => {
  it("loads terminal workload logs when the source is no longer active", async () => {
    render(<LogPane url="/api/v1/deployments/dep/logs?rank=1" active={false} />);

    await waitFor(() => expect(streamSpies.open).toHaveBeenCalledOnce());
    expect(screen.getByText("retained worker failure")).toBeInTheDocument();
    expect(screen.getByText("stream ended")).toBeInTheDocument();
  });
});
