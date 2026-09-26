import { describe, expect, it } from "vitest";
import { renderToStaticMarkup } from "react-dom/server";
import { TaskCard } from "./TaskApprovals";
import type { TaskApproval } from "../api";

function render(state: string, operation?: TaskApproval["operation"], recoverable?: boolean) {
  return renderToStaticMarkup(<TaskCard task={{ id: "task", actor: "bot:worker", revision: "1", state: `TASK_APPROVAL_STATE_${state}`, operation, recoverable }} reload={() => {}} />);
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
    const html = render("OUTCOME_UNKNOWN", undefined, true);
    expect(html).toContain("may duplicate external effects");
    expect(html).toContain("disabled=\"\">Recover for fresh approval");
    expect(html).not.toContain("Approve once");
    expect(html).toContain("Close without replay");
  });

  it.each([false, undefined])("only offers closure when recoverable is %s", (recoverable) => {
    const html = render("OUTCOME_UNKNOWN", undefined, recoverable);
    expect(html).toContain("No recoverable checkpoint");
    expect(html).toContain("Close without replay");
    expect(html).not.toContain("Recover for fresh approval");
    expect(html).not.toContain("checkbox");
  });

  it.each(["DENIED", "EXPIRED", "COMPLETED", "CANCELLED"])("does not offer stale decisions on %s", (state) => {
    const html = render(state);
    expect(html).not.toContain("Approve once");
    expect(html).not.toContain("Recover for fresh approval");
    expect(html).not.toContain("Cancel task");
  });
});
