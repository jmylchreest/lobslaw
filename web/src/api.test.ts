import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, api, isUnavailable, streamBotChat, streamChat, cancelActiveStreams } from "./api";

afterEach(() => { cancelActiveStreams(); vi.unstubAllGlobals(); });

describe("bot streams", () => {
  it("reports folded acceptance rather than a failed turn", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ error: "covered by active turn" }), { status: 202 })));
    const event = vi.fn();
    await streamBotChat("worker", "second message", event);
    expect(event).toHaveBeenCalledWith("accepted", { message: "covered by active turn" });
  });

  it("passes explicit cancellation to the outstanding request", async () => {
    const controller = new AbortController();
    let signal: AbortSignal | undefined;
    const fetch = vi.fn((_path: string, init: RequestInit) => {
      signal = init.signal as AbortSignal;
      return Promise.resolve(new Response(new ReadableStream()));
    });
    vi.stubGlobal("fetch", fetch);
    const event = vi.fn();
    const running = streamBotChat("worker", "hello", event, controller.signal);
    const stopped = expect(running).rejects.toMatchObject({ name: "AbortError" });
    await vi.waitFor(() => expect(signal).toBeDefined());
    controller.abort();
    await stopped;
    expect(signal?.aborted).toBe(true);
    expect(event).not.toHaveBeenCalled();
  });

  it("propagates a stream consumer failure instead of silently losing it", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response('event: error\ndata: {"message":"provider unavailable"}\n\n')));
    await expect(streamBotChat("worker", "hello", (_event, data) => { throw new Error(String(data.message)); })).rejects.toThrow("provider unavailable");
  });

  it("ignores malformed data while still delivering the next valid frame", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response('event: delta\ndata: invalid json\n\nevent: reply\ndata: {"text":"Hello!"}\n\n')));
    const event = vi.fn();
    await streamBotChat("worker", "hello", event);
    expect(event).toHaveBeenCalledExactlyOnceWith("reply", { text: "Hello!" });
  });

  it("logout cancels every remaining bot and single-chat stream after another finishes", async () => {
    const signals: AbortSignal[] = [];
    const cancelled = vi.fn();
    vi.stubGlobal("fetch", vi.fn((path: string, init: RequestInit) => {
      if (path === "/v1/session") return Promise.resolve(new Response('{"status":"ok"}'));
      signals.push(init.signal as AbortSignal);
      if (path.includes("finished")) return Promise.resolve(new Response('event: reply\ndata: {"text":"done"}\n\n'));
      return Promise.resolve(new Response(new ReadableStream({ cancel: cancelled })));
    }));
    const event = vi.fn();
    const bot = streamBotChat("worker", "hello", event);
    const single = streamChat("hello", event);
    const botStopped = expect(bot).rejects.toMatchObject({ name: "AbortError" });
    const singleStopped = expect(single).rejects.toMatchObject({ name: "AbortError" });
    await streamBotChat("finished", "hello", vi.fn());
    await api.logout();
    await Promise.all([botStopped, singleStopped]);
    expect(signals.map((signal) => signal.aborted)).toEqual([true, true, false]);
    expect(cancelled).toHaveBeenCalledTimes(2);
    expect(event).not.toHaveBeenCalled();
  });
});

describe("durable task decisions", () => {
  it("preserves the exact revision, bounded extra budget and explicit recovery acknowledgement", async () => {
    const fetch = vi.fn().mockImplementation(() => Promise.resolve(new Response('{"record":{}}')));
    vi.stubGlobal("fetch", fetch);
    const task = { id: "task", actor: "bot:worker", state: "TASK_APPROVAL_STATE_WAITING", revision: "9007199254740993" } as const;
    await api.decideTask(task, "budget_extension", { tool_calls: 3, spend_usd: 0.5, egress_bytes: 4096 });
    expect(JSON.parse(fetch.mock.calls[0][1].body)).toEqual({ revision: task.revision, choice: "budget_extension", extra_budget: { tool_calls: 3, spend_usd: 0.5, egress_bytes: 4096 } });
    await api.recoverTask(task, true);
    expect(JSON.parse(fetch.mock.calls[1][1].body)).toEqual({ revision: task.revision, acknowledge_duplicate_risk: true });
  });
});

function respond(status: number, body: string, ok = status < 400) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => ({
      ok,
      status,
      statusText: `status ${status}`,
      text: async () => body,
    })),
  );
}

describe("the API client's error handling", () => {
  it("surfaces the node's own message", async () => {
    respond(409, JSON.stringify({ error: "bot changed; read it again and retry" }));
    await expect(api.capabilities()).rejects.toThrow("bot changed; read it again and retry");
  });

  it("carries the status, so a conflict is distinguishable from a typo", async () => {
    respond(409, JSON.stringify({ error: "conflict" }));
    await expect(api.capabilities()).rejects.toMatchObject({ status: 409 });
    respond(404, JSON.stringify({ error: "no such bot" }));
    await expect(api.capabilities()).rejects.toBeInstanceOf(ApiError);
  });

  it("keeps a non-JSON body instead of a parser error", async () => {
    respond(502, "<html><body>Bad Gateway</body></html>");
    await expect(api.capabilities()).rejects.toThrow(/Bad Gateway/);
  });

  it("falls back to the status text when there is no body at all", async () => {
    respond(500, "");
    await expect(api.capabilities()).rejects.toThrow("status 500");
  });
});

describe("discovery", () => {
  it("treats a missing groups route as no teams, not a failure", async () => {
    respond(404, JSON.stringify({ error: "not found" }));
    await expect(api.listGroups()).resolves.toEqual([]);
  });

  it("does not treat an unavailable backend as an empty team list", async () => {
    respond(503, JSON.stringify({ error: "agent not configured on this node" }));
    await expect(api.listGroups()).rejects.toMatchObject({ status: 503 });
  });

  it("flags network and 503 errors as unavailable", () => {
    expect(isUnavailable(new TypeError("Failed to fetch"))).toBe(true);
    expect(isUnavailable(new ApiError(503, "down"))).toBe(true);
    expect(isUnavailable(new ApiError(404, "missing"))).toBe(false);
  });
});

describe("REST revision compatibility", () => {
 it.each([42, "9007199254740993", "18446744073709551615"])("returns revision %s unchanged on writes", async (revision) => {
  const fetch = vi.fn().mockImplementation(() => Promise.resolve(new Response("{}")));
  vi.stubGlobal("fetch", fetch);
  await api.updateBot("worker", {revision, enabled:false,tools:[]});
  expect(JSON.parse(fetch.mock.calls[0][1].body)).toEqual({revision,enabled:false,tools:[]});
  await api.renameGroup("team","renamed",revision);
  expect(JSON.parse(fetch.mock.calls[1][1].body)).toEqual({revision,name:"renamed"});
 });
});
