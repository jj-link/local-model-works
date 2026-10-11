import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { streamEvents, type SseEvent, type StreamHandle, type StreamState } from "~/lib/api/sse";

const auth = vi.hoisted(() => ({ expired: vi.fn() }));
vi.mock("~/lib/api/client", () => ({ notifyUnauthorized: auth.expired }));

const handles: StreamHandle[] = [];
const encoder = new TextEncoder();

function response(parts: Uint8Array[], status = 200, contentType = "text/event-stream") {
  return new Response(new ReadableStream<Uint8Array>({
    start(controller) {
      for (const part of parts) controller.enqueue(part);
      controller.close();
    },
  }), { status, headers: { "content-type": contentType } });
}

beforeEach(() => {
  vi.useFakeTimers();
  auth.expired.mockReset();
});

afterEach(() => {
  for (const handle of handles.splice(0)) handle.close();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("SSE consumer delivery", () => {
  it("resumes only complete frames across split CRLF and UTF-8, without replaying consumed output", async () => {
    const snapshot = `id: 5:5\r\nevent: snapshot\r\ndata: ${JSON.stringify({ stdout: "old\n", stderr: "err\n" })}\r\n\r\n`;
    const stdout = `id: 15:5\r\ndata: ${JSON.stringify({ stream: "stdout", data: "界 stdout\n" })}\r\n\r\n`;
    const stderr = `id: 15:15\r\ndata: ${JSON.stringify({ stream: "stderr", data: "界 stderr\n" })}\r\n\r\n`;
    const bytes = encoder.encode(snapshot + stdout + stderr.slice(0, -3));
    const fetcher = vi.fn()
      .mockResolvedValueOnce(response(Array.from(bytes, (byte) => new Uint8Array([byte]))))
      .mockResolvedValueOnce(response([encoder.encode(stderr + "id: 15:15\nevent: end\ndata: {}\n\nid: 99\ndata: must not arrive after close\n\n")]));
    vi.stubGlobal("fetch", fetcher);
    const events: SseEvent[] = [];
    const states: StreamState[] = [];
    const handle = streamEvents("/logs", {
      onEvent: (event) => {
        events.push(event);
        if (event.event === "end") handle.close();
      },
      onState: (state) => states.push(state),
    });
    handles.push(handle);
    await vi.advanceTimersByTimeAsync(0);
    expect(events.map((event) => event.id)).toEqual(["5:5", "15:5"]);
    expect(states.at(-1)).toMatchObject({ connected: false, reconnecting: true });
    await vi.advanceTimersByTimeAsync(500);
    expect(fetcher).toHaveBeenNthCalledWith(2, "/logs", expect.objectContaining({
      headers: expect.objectContaining({ "last-event-id": "15:5" }),
    }));
    expect(events.map((event) => event.id)).toEqual(["5:5", "15:5", "15:15", "15:15"]);
    expect(JSON.parse(events[1].data).data).toBe("界 stdout\n");
    expect(JSON.parse(events[2].data).data).toBe("界 stderr\n");
    await vi.advanceTimersByTimeAsync(10000);
    expect(fetcher).toHaveBeenCalledTimes(2);
  });

  it("does not acknowledge a frame whose consumer failed", async () => {
    const frame = encoder.encode('id: 8\ndata: "single-stream build output"\n\n');
    const fetcher = vi.fn().mockResolvedValueOnce(response([frame])).mockResolvedValueOnce(response([frame]));
    vi.stubGlobal("fetch", fetcher);
    let calls = 0;
    const consumed: string[] = [];
    const handle = streamEvents("/run/logs", {
      lastEventId: "4",
      onEvent: (event) => {
        if (++calls === 1) throw new Error("consumer did not accept this event");
        consumed.push(JSON.parse(event.data));
        handle.close();
      },
    });
    handles.push(handle);
    await vi.advanceTimersByTimeAsync(500);
    expect(fetcher).toHaveBeenNthCalledWith(2, "/run/logs", expect.objectContaining({
      headers: expect.objectContaining({ "last-event-id": "4" }),
    }));
    expect(consumed).toEqual(["single-stream build output"]);
  });

  it("reports expired authentication after a connected stream rather than staying connected", async () => {
    const fetcher = vi.fn()
      .mockResolvedValueOnce(response([encoder.encode(": keepalive\n\n")]))
      .mockResolvedValueOnce(response([], 401));
    vi.stubGlobal("fetch", fetcher);
    const states: StreamState[] = [];
    handles.push(streamEvents("/logs", { onEvent: vi.fn(), onState: (state) => states.push(state) }));
    await vi.advanceTimersByTimeAsync(500);
    expect(states[0]).toMatchObject({ connected: true, reconnecting: false });
    expect(states.at(-1)).toMatchObject({ connected: false, reconnecting: false, status: 401, error: "Stream unavailable (HTTP 401)" });
    expect(auth.expired).toHaveBeenCalledOnce();
    await vi.advanceTimersByTimeAsync(10000);
    expect(fetcher).toHaveBeenCalledTimes(2);
  });

  it.each([400, 403, 404, 410])("reports HTTP %s as unavailable without retrying forever", async (status) => {
    const fetcher = vi.fn().mockResolvedValue(response([], status));
    vi.stubGlobal("fetch", fetcher);
    const state = vi.fn();
    handles.push(streamEvents("/logs", { onEvent: vi.fn(), onState: state }));
    await vi.advanceTimersByTimeAsync(10000);
    expect(state).toHaveBeenLastCalledWith(expect.objectContaining({ connected: false, reconnecting: false, status }));
    expect(fetcher).toHaveBeenCalledOnce();
  });

  it("does not call an HTML login response a connected stream", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(response([encoder.encode("<html>sign in</html>")], 200, "text/html")));
    const state = vi.fn();
    handles.push(streamEvents("/logs", { onEvent: vi.fn(), onState: state }));
    await vi.advanceTimersByTimeAsync(0);
    expect(state).toHaveBeenCalledExactlyOnceWith(expect.objectContaining({ connected: false, reconnecting: false, error: "Invalid stream response" }));
  });

  it("ignores a fetch that completes after its consumer has closed", async () => {
    let resolve!: (response: Response) => void;
    vi.stubGlobal("fetch", vi.fn().mockImplementation(() => new Promise<Response>((done) => { resolve = done; })));
    const event = vi.fn();
    const state = vi.fn();
    const handle = streamEvents("/old-rank", { onEvent: event, onState: state });
    handles.push(handle);
    handle.close();
    resolve(response([encoder.encode('id: 10\ndata: "obsolete output"\n\n')]));
    await vi.advanceTimersByTimeAsync(0);
    expect(event).not.toHaveBeenCalled();
    expect(state).not.toHaveBeenCalled();
  });
});
