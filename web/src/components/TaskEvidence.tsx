import type { TaskMessage, ToolReceipt } from "../api";

const receiptLabels: Record<string, string> = {
  executed: "Executed — not proof of success",
  refused: "Refused — not executed",
  approval_required: "Awaiting approval — not executed",
  budget_required: "Awaiting budget — not executed",
  outcome_unknown: "Outcome unknown",
};

export function TaskEvidence({ transcript, receipts }: { transcript?: TaskMessage[]; receipts?: ToolReceipt[] }) {
  if (!transcript?.length && !receipts?.length) return null;
  return <details className="pad">
    <summary>Transcript and execution receipts</summary>
    <h3>Recorded transcript</h3>
    {(transcript ?? []).map((message, index) => <section key={index}>
      <h4>{message.role || "unknown role"}{message.toolCallId && ` · ${message.toolCallId}`}</h4>
      {message.content && <pre>{message.content}</pre>}
      {(message.toolCalls ?? []).map((call, callIndex) => <div key={callIndex}>
        <p>Requested tool: {call.name} · {call.id}</p><pre>{call.arguments}</pre>
      </div>)}
    </section>)}
    <h3>Per-attempt receipts</h3>
    <p>A requested call is not execution evidence. An executed handler can still fail or have an uncertain external result.</p>
    {(receipts ?? []).map((receipt, index) => <section key={index}>
      <h4>{receipt.toolName || "unknown tool"} · {receiptLabels[receipt.executionStatus ?? ""] ?? "Outcome unknown"}</h4>
      <p>Call: {receipt.callId || "not recorded"}</p>
      {receipt.args && <><h5>Arguments</h5><pre>{receipt.args}</pre></>}
      {receipt.output && <><h5>Output</h5><pre>{receipt.output}</pre></>}
      {receipt.error && <><h5>Error</h5><pre>{receipt.error}</pre></>}
      {receipt.executionStatus === "executed" && <p>Exit code: {receipt.exitCode ?? 0}</p>}
    </section>)}
  </details>;
}
