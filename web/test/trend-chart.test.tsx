/// <reference types="@testing-library/jest-dom" />
import { render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const plotSpies = vi.hoisted(() => ({
  setData: vi.fn(),
  destroy: vi.fn(),
}));

vi.mock("uplot", () => {
  class FakeUPlot {
    private readonly canvas: HTMLCanvasElement;

    constructor(_options: unknown, _data: unknown, host: HTMLElement) {
      this.canvas = document.createElement("canvas");
      host.appendChild(this.canvas);
    }

    setData(data: unknown) {
      plotSpies.setData(data);
    }

    setSize() {}

    destroy() {
      plotSpies.destroy();
      this.canvas.remove();
    }
  }

  return { default: FakeUPlot };
});

import { TrendChart, type TrendSeries } from "~/components/trend-chart";

class ResizeObserverStub implements ResizeObserver {
  readonly observe = vi.fn();
  readonly unobserve = vi.fn();
  readonly disconnect = vi.fn();
}

beforeEach(() => {
  plotSpies.setData.mockClear();
  plotSpies.destroy.mockClear();
  vi.stubGlobal("ResizeObserver", ResizeObserverStub);
});

const cpuSeries = (points: [number, number][]): TrendSeries[] => [
  { label: "cpu", color: "#5b6368", points },
];

describe("TrendChart lifecycle", () => {
  it("shows an empty placeholder, then mounts a uPlot canvas when data arrives", () => {
    const { rerender, container } = render(
      <TrendChart series={[]} ariaLabel="cpu utilization" />,
    );
    expect(screen.getByText("no data")).toBeInTheDocument();
    expect(container.querySelector("canvas")).toBeNull();

    rerender(<TrendChart series={cpuSeries([[1, 10], [2, 20]])} ariaLabel="cpu utilization" />);
    expect(screen.getByRole("img", { name: "cpu utilization" })).toBeInTheDocument();
    expect(container.querySelector("canvas")).not.toBeNull();
  });

  it("returns to the empty placeholder and cleans up the plot", () => {
    const { rerender, container } = render(
      <TrendChart series={cpuSeries([[1, 10]])} ariaLabel="cpu utilization" />,
    );
    expect(container.querySelector("canvas")).not.toBeNull();

    rerender(<TrendChart series={[]} ariaLabel="cpu utilization" />);
    expect(screen.getByText("no data")).toBeInTheDocument();
    expect(container.querySelector("canvas")).toBeNull();
    expect(plotSpies.destroy).toHaveBeenCalledTimes(1);
  });

  it("keeps one plot instance across data updates with a fixed y-range", () => {
    const { rerender, container } = render(
      <TrendChart series={cpuSeries([[1, 1]])} yFixed={[0, 100]} ariaLabel="cpu utilization" />,
    );
    const canvas = container.querySelector("canvas");
    expect(canvas).not.toBeNull();

    rerender(
      <TrendChart series={cpuSeries([[1, 2], [2, 3], [3, 5]])} yFixed={[0, 100]} ariaLabel="cpu utilization" />,
    );
    expect(container.querySelectorAll("canvas")).toHaveLength(1);
    expect(container.querySelector("canvas")).toBe(canvas);
    expect(plotSpies.destroy).not.toHaveBeenCalled();
    expect(plotSpies.setData).toHaveBeenCalled();
  });
});
