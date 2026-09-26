import { describe, expect, it } from "vitest";
import { renderToStaticMarkup } from "react-dom/server";
import { TaskCard } from "./TaskApprovals";
import type { TaskApproval } from "../api";

function render(state: string, operation?: TaskApproval["operation"]) {
  return renderToStaticMarkup(<TaskCard task={{ id: "task", actor: "bot:worker", revision: "1", state: `TASK_APPROVAL_STATE_${state}`, operation }} reload={() => {}} />);
}

describe("owner task approval states", () => {
  it("does not present a ready task as execution complete", () => {
    const html = render("READY");
    expect(html).toContain("waiting for the worker to resume");
    expect(html).not.toContain("Approve once");
  });

  it("requires a positive bounded extension rather than offering an unlimited approval", () => {
    const html = render("WAITING", { requiresBudgetExtension: true });
    expect(html).toContain("Additional allowance only");
    expect(html).toContain("disabled=\"\">Extend budget");
    expect(html).not.toContain("Approve once");
  });

  it("requires explicit duplicate-effect acknowledgement for unknown outcomes", () => {
    const html = render("OUTCOME_UNKNOWN");
    expect(html).toContain("may duplicate external effects");
    expect(html).toContain("disabled=\"\">Recover for fresh approval");
    expect(html).not.toContain("Approve once");
  });

  it.each(["DENIED", "EXPIRED", "COMPLETED", "CANCELLED"])("does not offer stale decisions on %s", (state) => {
    const html = render(state);
    expect(html).not.toContain("Approve once");
    expect(html).not.toContain("Recover for fresh approval");
    expect(html).not.toContain("Cancel task");
  });
});
