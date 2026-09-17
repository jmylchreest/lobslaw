import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, api } from "./api";

afterEach(() => vi.unstubAllGlobals());

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
  // The node writes errors a person can read — "engineering has 200
  // pending items (cap 200); it is not keeping up". Replacing that
  // with something generic throws away the only useful part.
  it("surfaces the node's own message", async () => {
    respond(409, JSON.stringify({ error: "bot changed; read it again and retry" }));
    await expect(api.listBots()).rejects.toThrow("bot changed; read it again and retry");
  });

  it("carries the status, so a conflict is distinguishable from a typo", async () => {
    respond(409, JSON.stringify({ error: "conflict" }));
    await expect(api.listBots()).rejects.toMatchObject({ status: 409 });
    respond(404, JSON.stringify({ error: "no such bot" }));
    await expect(api.listBots()).rejects.toBeInstanceOf(ApiError);
  });

  // A reverse proxy's 502 is HTML, not the API's JSON envelope. Parsing
  // it and failing would replace a useful body with "unexpected token".
  it("keeps a non-JSON body instead of a parser error", async () => {
    respond(502, "<html><body>Bad Gateway</body></html>");
    await expect(api.listBots()).rejects.toThrow(/Bad Gateway/);
  });

  it("falls back to the status text when there is no body at all", async () => {
    respond(500, "");
    await expect(api.listBots()).rejects.toThrow("status 500");
  });

  it("treats 204 as success with nothing to parse", async () => {
    respond(204, "");
    await expect(api.deleteBot("devops")).resolves.toBeUndefined();
  });
});
