import type { TaskMessage, ToolReceipt } from "../api";
import { Disclosure } from "./Motion";
import { Markdown } from "./Markdown";
import { SourceText } from "./SourceContent";

const receiptLabels: Record<string, string> = {
  executed: "Executed — not proof of success",
  refused: "Refused — not executed",
  approval_required: "Awaiting approval — not executed",
  budget_required: "Awaiting budget — not executed",
  outcome_unknown: "Outcome unknown",
};

export function TaskEvidence({ transcript, receipts }: { transcript?: TaskMessage[]; receipts?: ToolReceipt[] }) {
  if (!transcript?.length && !receipts?.length) return null;
  return <Disclosure className="pad evidence" title="Transcript and execution receipts">
    <p className="evidence-summary">{transcript?.length ?? 0} recorded messages · {receipts?.length ?? 0} execution receipts</p>
    {!!transcript?.length && <h3>Recorded transcript</h3>}
    {(transcript ?? []).map((message, index) => <section className={`evidence-message ${message.role || "unknown"}`} key={index}>
      <h4>{message.role || "unknown role"}{message.toolCallId && ` · ${message.toolCallId}`}</h4>
      {message.content && (message.role === "user" || message.role === "assistant" ? <div className="evidence-prose"><Markdown>{message.content}</Markdown></div> : <SourceText>{message.content}</SourceText>)}
      {(message.toolCalls ?? []).map((call, callIndex) => <div key={callIndex}>
        <p>Requested tool: {call.name} · {call.id}</p><pre>{call.arguments}</pre>
      </div>)}
    </section>)}
    {!!receipts?.length && <><h3>Per-attempt receipts</h3><p>A requested call is not execution evidence. An executed handler can still fail or have an uncertain external result.</p></>}
    {(receipts ?? []).map((receipt, index) => <section className={`evidence-receipt ${receipt.executionStatus || "unknown"}`} key={index}>
      <h4>{receipt.toolName || "unknown tool"} · {receiptLabels[receipt.executionStatus ?? ""] ?? "Outcome unknown"}</h4>
      <p>Call: {receipt.callId || "not recorded"}</p>
      {receipt.args && <Disclosure title="Arguments"><pre>{receipt.args}</pre></Disclosure>}
      {receipt.output && <><h5>Output</h5><SourceText>{receipt.output}</SourceText></>}
      {receipt.error && <><h5>Error</h5><pre>{receipt.error}</pre></>}
      {receipt.executionStatus === "executed" && <p>Exit code: {receipt.exitCode ?? 0}</p>}
    </section>)}
  </Disclosure>;
}
