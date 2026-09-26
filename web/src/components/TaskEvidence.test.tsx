import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { botReply } from "../api";
import { TaskEvidence } from "./TaskEvidence";

describe("durable execution evidence", () => {
  it("keeps generated JSON names and exact transcript sequence numbers", () => {
    const reply = botReply({ text: "done", sessionId: "bot:chief.task.t", toolsUsed: ["read"], transcript: [{ seq: "9007199254740993", role: "assistant", toolCalls: [{ id: "call", name: "read", arguments: "{}" }] }], receipts: [{ callId: "call", toolName: "read", executionStatus: "executed", output: "result" }] });
    expect(reply.transcript?.[0].seq).toBe("9007199254740993");
    expect(reply.sessionId).toBe("bot:chief.task.t");
    expect(reply.receipts?.[0].executionStatus).toBe("executed");
  });

  it("shows resumed transcripts and distinguishes every attempt from execution", () => {
    const html = renderToStaticMarkup(<TaskEvidence
      transcript={[{ role: "user", content: "original question" }, { role: "assistant", toolCalls: [{ id: "call", name: "write", arguments: "{}" }] }, { role: "tool", content: "resumed output", toolCallId: "call" }, { role: "assistant", content: "resumed final reply" }]}
      receipts={["executed", "refused", "approval_required", "budget_required", "outcome_unknown", ""].map((executionStatus) => ({ toolName: "write", executionStatus, exitCode: 1, error: "detail" }))} />);
    for (const text of ["original question", "resumed output", "resumed final reply", "Requested tool", "Executed — not proof of success", "Refused — not executed", "Awaiting approval — not executed", "Awaiting budget — not executed", "Outcome unknown", "Exit code: 1"]) expect(html).toContain(text);
    expect(html.match(/Executed — not proof of success/g)).toHaveLength(1);
  });
});
