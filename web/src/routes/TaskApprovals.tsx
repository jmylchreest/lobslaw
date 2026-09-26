import { useEffect, useState } from "react";
import { api, type TaskApproval, type TaskChoice } from "../api";
import { Err, Spinner, useLoad } from "../components/ui";

const refreshIntervalMS = 5000;
const statePrefix = "TASK_APPROVAL_STATE_";
export function taskState(task: TaskApproval): string {
  return task.state?.replace(statePrefix, "").toLowerCase() || "unknown";
}

export function TaskApprovals() {
  const [after, setAfter] = useState("");
  const { data, loading, error, reload } = useLoad(() => api.taskApprovals(after), [after]);
  useEffect(() => {
    const timer = setInterval(reload, refreshIntervalMS);
    return () => clearInterval(timer);
  }, [reload]);
  return <div className="wrap">
    <h1>Task approvals</h1>
    <p>Decisions apply to this task and actor only. Approving queues the saved task for its worker; it does not mean the action has run.</p>
    {error && <Err error={error} />}
    {loading && !data && <Spinner />}
    {data && !(data.records?.length) && <p>No tasks on this page.</p>}
    {(data?.records ?? []).map((task) => <TaskCard key={`${task.id}:${task.revision}`} task={task} reload={reload} />)}
    <button className="btn" onClick={() => { setAfter(""); reload(); }}>Refresh from start</button>
    {data?.nextAfterId && <button className="btn" onClick={() => setAfter(data.nextAfterId!)}>Next page</button>}
  </div>;
}

export function TaskCard({ task, reload }: { task: TaskApproval; reload: () => void }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<Error | null>(null);
  const [acknowledge, setAcknowledge] = useState(false);
  const [calls, setCalls] = useState("0");
  const [spend, setSpend] = useState("0");
  const [bytes, setBytes] = useState("0");
  const state = taskState(task);
  const operation = task.operation;
  async function act(action: () => Promise<unknown>) {
    setBusy(true); setError(null);
    try { await action(); reload(); } catch (err) { setError(err as Error); }
    finally { setBusy(false); }
  }
  function decide(choice: TaskChoice) {
    void act(() => api.decideTask(task, choice, choice === "budget_extension"
      ? { tool_calls: Number(calls), spend_usd: Number(spend), egress_bytes: Number(bytes) } : undefined));
  }
  const validExtra = [Number(calls), Number(spend), Number(bytes)].every((n) => Number.isFinite(n) && n >= 0)
    && Number.isSafeInteger(Number(calls)) && Number.isSafeInteger(Number(bytes))
    && (Number(calls) > 0 || Number(spend) > 0 || Number(bytes) > 0);
  return <section className="pad" aria-label={`Task ${task.id}`}>
    <h2>{task.actor} · {state.replaceAll("_", " ")}</h2>
    <p><code>{task.id}</code>{task.parentId && <> · parent <code>{task.parentId}</code></>}</p>
    {operation && <><p>{operation.summary || operation.toolName}</p><pre>{operation.action} {operation.resource}</pre></>}
    {task.expiresAt && <p>Authority expires: {task.expiresAt}</p>}
    {task.budgetSpent && <p>Consumed: {task.budgetSpent.toolCalls ?? 0} calls · ${task.budgetSpent.spendUsd ?? 0} · {task.budgetSpent.egressBytes ?? "0"} egress bytes</p>}
    {task.budgetLimits && <p>Current limits: {task.budgetLimits.toolCalls ?? 0} calls · ${task.budgetLimits.spendUsd ?? 0} · {task.budgetLimits.egressBytes ?? "0"} egress bytes (zero means uncapped)</p>}
    {task.result && <pre>{task.result}</pre>}
    {error && <><Err error={error} /><button className="btn" onClick={reload}>Reload current task state</button></>}
    {state === "waiting" && <fieldset disabled={busy}>
      <legend>Owner decision</legend>
      {operation?.requiresBudgetExtension ? <>
        <p>Additional allowance only; existing consumption is retained. Zero adds nothing.</p>
        <label>Extra calls <input type="number" min="0" step="1" value={calls} onChange={(e) => setCalls(e.target.value)} /></label>
        <label>Extra USD <input type="number" min="0" step="any" value={spend} onChange={(e) => setSpend(e.target.value)} /></label>
        <label>Extra egress bytes <input type="number" min="0" step="1" value={bytes} onChange={(e) => setBytes(e.target.value)} /></label>
        <button className="btn" disabled={!validExtra} onClick={() => decide("budget_extension")}>Extend budget and queue resume</button>
      </> : <>
        <button className="btn" onClick={() => decide("once")}>Approve once</button>
        {operation?.grantable && <button className="btn" onClick={() => decide("operation")}>Allow this operation for this task</button>}
        {operation?.grantable && !!operation.labels?.length && <button className="btn" onClick={() => decide("risk_labels")}>Allow {operation.labels.join(", ")} for this task</button>}
      </>}
      <button className="btn" onClick={() => decide("deny")}>Deny</button>
    </fieldset>}
    {state === "outcome_unknown" && <fieldset disabled={busy}>
      <legend>Outcome uncertain</legend>
      <p>The previous action may have run. Recovery requires a new approval before resuming.</p>
      <label><input type="checkbox" checked={acknowledge} onChange={(e) => setAcknowledge(e.target.checked)} /> I acknowledge retrying may duplicate external effects.</label>
      <button className="btn" disabled={!acknowledge} onClick={() => void act(() => api.recoverTask(task, acknowledge))}>Recover for fresh approval</button>
    </fieldset>}
    {["waiting", "ready", "running", "resuming"].includes(state) && <>
      {["running", "resuming"].includes(state) && <p>Cancellation stops further authorisation; it cannot undo an action already started.</p>}
      <button className="btn" disabled={busy} onClick={() => void act(() => api.cancelTask(task))}>Cancel task</button>
    </>}
    {state === "ready" && <p>Approved, waiting for the worker to resume.</p>}
  </section>;
}
