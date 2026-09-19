import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, api, isUnavailable, streamBotChat } from "./api";

afterEach(() => vi.unstubAllGlobals());

describe("bot streams", () => {
  it("reports folded acceptance rather than a failed turn", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ error: "covered by active turn" }), { status: 202 })));
    const event = vi.fn();
    await streamBotChat("worker", "second message", event);
    expect(event).toHaveBeenCalledWith("accepted", { message: "covered by active turn" });
  });

  it("passes navigation cancellation to the outstanding request", async () => {
    const controller = new AbortController();
    const fetch = vi.fn().mockResolvedValue(new Response('event: reply\ndata: {"text":"done"}\n\n'));
    vi.stubGlobal("fetch", fetch);
    const event = vi.fn();
    await streamBotChat("worker", "hello", event, controller.signal);
    expect(fetch).toHaveBeenCalledWith("/v1/bots/worker/messages", expect.objectContaining({ signal: controller.signal }));
    expect(event).toHaveBeenCalledWith("reply", { text: "done" });
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
