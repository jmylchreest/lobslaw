import { useEffect, useRef, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { api, type TaskApproval, type TaskChoice } from "../api";
import { Empty, Err, Spinner, useLoad } from "../components/ui";
import { TaskEvidence } from "../components/TaskEvidence";
import { Disclosure, StateText, StatusMark, useFeedback } from "../components/Motion";
import { Markdown } from "../components/Markdown";
import { Mascot } from "../components/Mascot";
import { BotSource } from "../components/SourceContent";
import { inboxBotId, sourceBlocks, sourceSubject } from "../sourceBlocks";

const refreshIntervalMS = 5000;
const statePrefix = "TASK_APPROVAL_STATE_";
const stateLabels: Record<string, string> = {
  waiting: "Needs approval", ready: "Queued", running: "Running", resuming: "Resuming",
  completed: "Completed", cancelled: "Cancelled", denied: "Denied", expired: "Expired", failed: "Failed", outcome_unknown: "Outcome unknown",
};
const stateHelp: Record<string, string> = {
  waiting: "Review what is being requested, then choose how this task can proceed.",
  ready: "Approved, waiting for the worker to resume.", running: "The agent is carrying out this task.", resuming: "The agent is continuing from its saved checkpoint.",
  completed: "This task has finished. Review the result and execution evidence below.", cancelled: "This task was closed. No further work will be authorised.",
  denied: "The requested permission was denied.", expired: "The task's authority has expired.", outcome_unknown: "The last operation has an uncertain outcome. Review its evidence before deciding what happens next.",
};
export function taskState(task: TaskApproval): string {
  return task.state?.replace(statePrefix, "").toLowerCase() || "unknown";
}

export function TaskApprovals() {
  const { taskId } = useParams();
  const [after, setAfter] = useState("");
  const [filter, setFilter] = useState("all");
  const { data: bots } = useLoad(() => api.listBots());
  const { data, loading, error, reload } = useLoad<{ records?: TaskApproval[]; nextAfterId?: string }>(
    () => taskId ? api.taskApproval(taskId).then(({ record }) => ({ records: [record] })) : api.taskApprovals(after), [after, taskId]);
  useEffect(() => {
    const timer = setInterval(reload, refreshIntervalMS);
    return () => clearInterval(timer);
  }, [reload]);
  const records = (data?.records ?? []).filter((task) => !taskId || task.id === taskId);
  const category = (task: TaskApproval) => {
    const state = taskState(task);
    return ["waiting", "outcome_unknown"].includes(state) ? "needs" : ["ready", "running", "resuming"].includes(state) ? "active" : "finished";
  };
  const visible = records.filter((task) => taskId || filter === "all" || category(task) === filter);
  const names = new Map((bots ?? []).map((bot) => [bot.id, bot.display_name || bot.id]));
  return <div className="wrap tasks-page">
    {taskId && <Link className="back-link" to="/approvals">← All tasks</Link>}
    <div className="head">
      <div className="grow"><h1>{taskId ? "Task details" : "Task approvals"}</h1><p className="sub">{taskId ? "The request, its current progress, and the recorded outcome." : "See what is running, what needs a decision, and what has finished."}</p></div>
      <button className="btn" disabled={loading} onClick={() => { setAfter(""); void reload(); }}>{taskId ? "Refresh task" : "Refresh from start"}</button>
    </div>
    {!taskId && <div className="chips task-filters" aria-label="Filter tasks">
      {[["all", "All"], ["needs", "Needs you"], ["active", "In progress"], ["finished", "Finished"]].map(([key, label]) => <button key={key} className={`chip${filter === key ? " on" : ""}`} aria-pressed={filter === key} onClick={() => setFilter(key)}>
        {label}<span className="filter-count">{key === "all" ? records.length : records.filter((task) => category(task) === key).length}</span>
      </button>)}
    </div>}
    {error && <Err error={error} />}
    {loading && !data && <Spinner />}
    {!error && data && !visible.length && <Empty title={records.length ? "No tasks in this view" : "No tasks on this page"} hint="Assigned work appears here with its progress, result, and any decisions it needs." />}
    {!error && visible.map((task) => <TaskCard key={task.id} task={task} reload={reload} compact={!taskId} actorName={names.get(task.actor.replace(/^bot:/, ""))} />)}
    {data?.nextAfterId && <div className="task-pagination"><span className="meta">Showing tasks from this page</span><button className="btn" onClick={() => setAfter(data.nextAfterId!)}>Next page</button></div>}
    <p className="tasks-footnote">Decisions apply to this task and actor only. Approving queues work for the worker; it does not mean the action has run.</p>
  </div>;
}

export function TaskCard({ task, reload, compact = false, actorName }: { task: TaskApproval; reload: () => void | Promise<void>; compact?: boolean; actorName?: string }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<Error | null>(null);
  const [acknowledge, setAcknowledge] = useState(false);
  const [calls, setCalls] = useState("0");
  const [spend, setSpend] = useState("0");
  const [bytes, setBytes] = useState("0");
  const notify = useFeedback();
  const acting = useRef(false);
  useEffect(() => {
    setAcknowledge(false); setCalls("0"); setSpend("0"); setBytes("0"); setError(null);
  }, [task.revision, task.state, task.recoverable]);
  const state = taskState(task);
  const operation = task.operation;
  async function act(action: () => Promise<unknown>) {
    if (acting.current) return;
    acting.current = true;
    setBusy(true); setError(null);
    try { await action(); await reload(); notify("Task decision saved"); } catch (err) { setError(err as Error); }
    finally { acting.current = false; setBusy(false); }
  }
  function decide(choice: TaskChoice) {
    void act(() => api.decideTask(task, choice, choice === "budget_extension"
      ? { tool_calls: Number(calls), spend_usd: Number(spend), egress_bytes: Number(bytes) } : undefined));
  }
  const validExtra = [Number(calls), Number(spend), Number(bytes)].every((n) => Number.isFinite(n) && n >= 0)
    && Number.isSafeInteger(Number(calls)) && Number.isSafeInteger(Number(bytes))
    && (Number(calls) > 0 || Number(spend) > 0 || Number(bytes) > 0);
  const name = actorName || task.actor.replace(/^(bot|user):/, "");
  const request = task.transcript?.find((message) => message.role === "user" && message.content)?.content || "";
  const origin = sourceBlocks(request).find((block) => block.kind === "source" && inboxBotId(block.source));
  const sender = origin?.kind === "source" ? inboxBotId(origin.source) : null;
  const title = sourceSubject(operation?.summary || request) || (task.coordinatorConversation ? "Conversation" : "Assigned work");
  return <section className={`task-card ${state}${compact ? " compact" : ""}`} aria-label={`Task ${task.id}`}>
    <header className="task-card-head">
      <div className="grow"><div className="task-actor">{task.actor.startsWith("bot:") && <Mascot id={task.actor.slice(4)} size={24} working={["running", "resuming"].includes(state)} />}<span>{name}</span>{task.coordinatorConversation && <span className="meta">Conversation</span>}</div><h2 className="task-title">{title}</h2></div>
      <span className={`task-state-badge ${state}`}><StatusMark status={state} className="task-state-mark" /><StateText value={state}>{stateLabels[state] || state.replaceAll("_", " ")}</StateText></span>
    </header>
    {sender && <div className="task-origin"><span className="source-caption">From</span><BotSource id={sender} /><span className="source-kind">Inbox assignment</span></div>}
    {stateHelp[state] && <p className="task-help">{stateHelp[state]}</p>}
    {(operation?.action || operation?.resource) && <section className="task-section"><h3 className="lbl">Requested operation</h3><pre className="task-operation">{operation.action} {operation.resource}</pre></section>}
    {(task.budgetSpent || task.budgetLimits) && <section className="task-section"><h3 className="lbl">Budget used</h3><div className="budget-grid">
      <Budget label="Tool calls" spent={task.budgetSpent?.toolCalls ?? 0} limit={task.budgetLimits?.toolCalls ?? 0} />
      <Budget label="Spend (USD)" spent={task.budgetSpent?.spendUsd ?? 0} limit={task.budgetLimits?.spendUsd ?? 0} prefix="$" />
      <Budget label="Egress (bytes)" spent={task.budgetSpent?.egressBytes ?? "0"} limit={task.budgetLimits?.egressBytes ?? "0"} />
    </div><p className="meta">A zero limit means uncapped. Consumption is retained when adding allowance.</p></section>}
    {task.result && <section className="task-section task-result"><h3 className="lbl">{compact ? "Result preview" : "Result"}</h3><Markdown>{task.result}</Markdown></section>}
    <TaskEvidence transcript={task.transcript} receipts={task.receipts} />
    {error && <><Err error={error} /><button className="btn" onClick={reload}>Reload current task state</button></>}
    {state === "waiting" && <fieldset className="task-decision" disabled={busy}>
      <legend>Your decision</legend>
      {operation?.requiresBudgetExtension ? <>
        <p>Additional allowance only; existing consumption is retained. Zero adds nothing.</p>
        <div className="budget-grid">
          <label className="field">Extra calls <input className="in" type="number" min="0" step="1" value={calls} onChange={(e) => setCalls(e.target.value)} /></label>
          <label className="field">Extra USD <input className="in" type="number" min="0" step="any" value={spend} onChange={(e) => setSpend(e.target.value)} /></label>
          <label className="field">Extra egress bytes <input className="in" type="number" min="0" step="1" value={bytes} onChange={(e) => setBytes(e.target.value)} /></label>
        </div>
        <button className="btn primary" disabled={!validExtra} onClick={() => decide("budget_extension")}>Extend budget and queue resume</button>
      </> : <>
        <button className="btn primary" onClick={() => decide("once")}>Approve once</button>
        {operation?.grantable && <button className="btn" onClick={() => decide("operation")}>Allow this operation for this task</button>}
        {operation?.grantable && !!operation.labels?.length && <button className="btn" onClick={() => decide("risk_labels")}>Allow {operation.labels.join(", ")} for this task</button>}
      </>}
      <button className="btn danger" onClick={() => decide("deny")}>Deny</button>
    </fieldset>}
    {state === "outcome_unknown" && <fieldset className="task-decision" disabled={busy}>
      <legend>Outcome uncertain</legend>
      <p>The previous action may have run. Closing this task does not undo external effects.</p>
      {task.recoverable === true ? <>
        <p>A saved checkpoint is available. Recovery requires a new approval before resuming.</p>
        <label className="task-ack"><input type="checkbox" checked={acknowledge} onChange={(e) => setAcknowledge(e.target.checked)} /> <span>I acknowledge retrying may duplicate external effects.</span></label>
        <button className="btn" disabled={!acknowledge} onClick={() => void act(() => api.recoverTask(task, acknowledge))}>Recover for fresh approval</button>
      </> : <p>No recoverable checkpoint is available. Close this task; any further work requires a fresh assignment.</p>}
      <button className="btn danger" onClick={() => void act(() => api.cancelTask(task))}>Close without replay</button>
    </fieldset>}
    {["waiting", "ready", "running", "resuming"].includes(state) && <>
      {["running", "resuming"].includes(state) && <p>Cancellation stops further authorisation; it cannot undo an action already started.</p>}
      <button className="btn" disabled={busy} onClick={() => void act(() => api.cancelTask(task))}>Cancel task</button>
    </>}
    <footer className="task-card-foot">
      {compact && <Link className="btn sm" to={`/approvals/${encodeURIComponent(task.id)}`}>View task details →</Link>}
      <Disclosure title="Task reference"><dl className="task-reference"><div><dt>Task ID</dt><dd>{task.id}</dd></div>{task.parentId && <div><dt>Parent task</dt><dd>{task.parentId}</dd></div>}{task.sessionId && <div><dt>Conversation</dt><dd>{task.sessionId}</dd></div>}{task.expiresAt && <div><dt>Authority expires</dt><dd>{task.expiresAt}</dd></div>}</dl></Disclosure>
    </footer>
  </section>;
}

function Budget({ label, spent, limit, prefix = "" }: { label: string; spent: number | string; limit: number | string; prefix?: string }) {
  const bounded = Number(limit) > 0;
  return <div className="budget-metric"><span className="meta">{label}</span><div><b>{prefix}{spent}</b><span className="meta"> / {bounded ? `${prefix}${limit}` : "uncapped"}</span></div>{bounded && <progress aria-label={label} value={Math.min(Number(spent), Number(limit))} max={Number(limit)} />}</div>;
}
