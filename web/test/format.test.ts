import { describe, expect, it } from "vitest";
import {
  bytes,
  duration,
  endpointLabel,
  endpointUrl,
  isRunTerminal,
  relativeTime,
  shortId,
  stateInfo,
  toYaml,
} from "~/lib/format";

describe("stateInfo", () => {
  it("maps node states", () => {
    expect(stateInfo("online").label).toBe("online");
    expect(stateInfo("online").tone).toBe("ok");
    expect(stateInfo("pending").tone).toBe("warn");
    expect(stateInfo("offline").tone).toBe("fault");
    expect(stateInfo("degraded").tone).toBe("warn");
  });

  it("maps run states", () => {
    expect(stateInfo("succeeded").tone).toBe("ok");
    expect(stateInfo("failed").tone).toBe("fault");
    expect(stateInfo("running").tone).toBe("info");
    expect(stateInfo("cancelling").tone).toBe("warn");
  });

  it("maps deployment observed states", () => {
    expect(stateInfo("healthy").tone).toBe("ok");
    expect(stateInfo("starting").tone).toBe("info");
    expect(stateInfo("stopped").tone).toBe("muted");
    expect(stateInfo("unknown").tone).toBe("muted");
  });

  it("falls back for unknown states without throwing", () => {
    expect(stateInfo("not-a-state").label).toBe("not-a-state");
    expect(stateInfo(null).tone).toBe("muted");
  });
});

describe("isRunTerminal", () => {
  it("classifies terminal and active states", () => {
    expect(isRunTerminal("succeeded")).toBe(true);
    expect(isRunTerminal("failed")).toBe(true);
    expect(isRunTerminal("cancelled")).toBe(true);
    expect(isRunTerminal("running")).toBe(false);
    expect(isRunTerminal("queued")).toBe(false);
    // Unknown state is treated as terminal: nothing to stream or cancel.
    expect(isRunTerminal(undefined)).toBe(true);
  });
});

describe("relativeTime", () => {
  it("renders seconds/minutes/hours ago", () => {
    const now = Date.parse("2026-08-19T12:00:00Z");
    expect(relativeTime("2026-08-19T11:59:18Z", now)).toMatch(/42s ago/);
    expect(relativeTime("2026-08-19T11:44:00Z", now)).toMatch(/16m ago/);
    expect(relativeTime("2026-08-19T09:00:00Z", now)).toMatch(/3h ago/);
  });

  it("handles missing timestamps", () => {
    expect(relativeTime(null)).toBe("—");
    expect(relativeTime(undefined)).toBe("—");
  });
});

describe("duration", () => {
  it("formats human-readable spans", () => {
    expect(duration("2026-08-19T12:00:00Z", "2026-08-19T12:12:04Z")).toBe("12m 04s");
    expect(duration("2026-08-19T12:00:00Z", "2026-08-19T12:00:03Z")).toBe("3s");
    expect(duration(null)).toBe("—");
  });
});

describe("bytes", () => {
  it("formats binary units", () => {
    expect(bytes(512)).toBe("512 B");
    expect(bytes(1536)).toBe("1.5 KiB");
    expect(bytes(5 * 1024 ** 3)).toBe("5.0 GiB");
    expect(bytes(null)).toBe("—");
  });
});

describe("shortId", () => {
  it("truncates ids and digests", () => {
    expect(shortId("01234567-89ab-cdef-0123-456789abcdef")).toBe("01234567…");
    expect(shortId("sha256:abcdef1234567890")).toContain("sha256:");
    expect(shortId(undefined)).toBe("—");
  });
});

describe("endpoint helpers", () => {
  const ep = { host: "10.0.0.2", port: 8080, path: "/v1", model: "test-model" };

  it("builds a full URL", () => {
    expect(endpointUrl(ep)).toBe("http://10.0.0.2:8080/v1");
    expect(endpointUrl(null)).toBe("—");
  });

  it("builds a compact label", () => {
    expect(endpointLabel(ep)).toBe("10.0.0.2:8080");
    expect(endpointLabel(null)).toBe("—");
  });
});

describe("toYaml", () => {
  it("serializes nested objects with quoting rules", () => {
    const yaml = toYaml({ a: 1, b: { c: "x" }, d: ["e", "f"], g: "true" });
    expect(yaml).toContain("a: 1");
    expect(yaml).toContain("c: x");
    expect(yaml).toContain('"true"');
    expect(yaml).toContain("- e");
  });
});
