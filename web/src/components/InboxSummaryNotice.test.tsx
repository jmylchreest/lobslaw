import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { InboxItem } from "../api";
import { InboxSummaryNotice } from "./InboxSummaryNotice";

const item: InboxItem = { id: "item", recipient: "worker", sender: "operator", kind: "task", subject: "Work", priority: 0, status: "failed", attempts: 1 };

describe("activity projection disclosure", () => {
  it("labels omitted links and shortened evidence and links to the authoritative item", () => {
    const html = renderToStaticMarkup(<InboxSummaryNotice item={{ ...item, truncated_fields: ["session_id", "tools_used", "error"], detail_path: "https://untrusted.example" }} />);
    expect(html).toContain("Shortened activity summary (session_id, tools_used, error)");
    expect(html).toContain('href="/v1/inbox/worker/item"');
    expect(html).toContain("View full record");
    expect(html).not.toContain("untrusted.example");
  });

  it("does not imply a full detail response is truncated", () => {
    expect(renderToStaticMarkup(<InboxSummaryNotice item={item} />)).toBe("");
  });
});
