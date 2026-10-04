import { renderToStaticMarkup } from "react-dom/server";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api";
import { ReviewContent } from "./LearnedReviews";

afterEach(() => vi.unstubAllGlobals());

describe("learned proposal review", () => {
  it("renders all reference files as literal inspection data", () => {
    const html = renderToStaticMarkup(<ReviewContent content={{ body: '<img src="https://example.test/private">', files: { "b.txt": "second file", "a.txt": "first file" } }} />);
    expect(html).not.toContain("<img");
    expect(html).toContain("&lt;img");
    expect(html).toContain("first file");
    expect(html).toContain("second file");
    expect(html.indexOf("a.txt")).toBeLessThan(html.indexOf("b.txt"));
  });

  it("submits only the inspected revision, digest and explicit rejection", async () => {
    const fetch = vi.fn().mockResolvedValue(new Response('{"message":"Amendment denied"}'));
    vi.stubGlobal("fetch", fetch);
    await api.decideLearnedReview({ id: "skill:worker", name: "worker", author: "bot:worker", revision: "9007199254740993", digest: "inspected-content" }, false);
    expect(fetch.mock.calls[0][0]).toBe("/v1/learned-reviews/skill%3Aworker/decide");
    expect(JSON.parse(fetch.mock.calls[0][1].body)).toEqual({ revision: "9007199254740993", digest: "inspected-content", approve: false });
  });

  it("does not turn an unavailable review service into an empty queue", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response('{"error":"review unavailable"}', { status: 503 })));
    await expect(api.learnedReviews()).rejects.toMatchObject({ status: 503 });
  });
});
