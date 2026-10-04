import { useEffect, useState } from "react";
import { NavLink } from "react-router-dom";
import { api, type LearnedChange } from "../api";
import { Err, Spinner, useLoad } from "../components/ui";

const reviewPollMS = 15000;
const reviewChangedEvent = "lobslaw:learned-reviewed";

// The notification uses the policy-filtered review service, not an unscoped
// store count. Its link remains available when discovery is unavailable.
export function LearnedReviewNotice() {
  const { data, error, reload } = useLoad(api.learnedReviews);
  useEffect(() => {
    const refresh = () => { reload(); };
    const timer = setInterval(refresh, reviewPollMS);
    window.addEventListener(reviewChangedEvent, refresh);
    return () => { clearInterval(timer); window.removeEventListener(reviewChangedEvent, refresh); };
  }, [reload]);
  return <NavLink to="/learned" title={error ? "Review service unavailable or access denied" : undefined}>
    Learned proposals{!error && !!data?.length && <span role="status"> · {data.length} awaiting review</span>}
  </NavLink>;
}

export function LearnedReviews() {
  const { data, loading, error, reload } = useLoad(api.learnedReviews);
  const [selected, setSelected] = useState("");
  const [receipt, setReceipt] = useState("");
  function decided(message: string) {
    setReceipt(message); setSelected(""); reload();
    window.dispatchEvent(new Event(reviewChangedEvent));
  }
  return <div className="wrap">
    <h1>Learned proposals</h1>
    <p>Inspect proposed skills and amendments before approving or rejecting them. This review controls learned-content activation, not task execution approval.</p>
    {receipt && <p role="status">{receipt}</p>}
    <button className="btn" onClick={reload}>Refresh proposals</button>
    {error && <Err error={error} />}
    {loading && <Spinner />}
    {!error && !loading && data?.length === 0 && <p>No proposals awaiting your review.</p>}
    {!error && (data ?? []).map((row) => <p key={row.id}>
      <button className="btn" onClick={() => { setSelected(row.id); setReceipt(""); }}>Inspect {row.name}</button>
      {row.pending ? " · amendment" : " · new proposal"}
      {row.author && ` · by ${row.author}`}
    </p>)}
    {selected && <ReviewInspector key={selected} id={selected} onDecided={decided} />}
  </div>;
}

function ReviewInspector({ id, onDecided }: { id: string; onDecided: (message: string) => void }) {
  const { data, loading, error, reload } = useLoad(() => api.learnedReview(id), [id]);
  const [inspected, setInspected] = useState(false);
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<Error | null>(null);
  function refresh() { setInspected(false); setFailure(null); reload(); }
  async function decide(approve: boolean) {
    if (!data || !inspected || busy || failure) return;
    setBusy(true);
    try { onDecided((await api.decideLearnedReview(data, approve)).message); }
    catch (err) { setFailure(err as Error); setInspected(false); }
    finally { setBusy(false); }
  }
  if (loading) return <Spinner />;
  if (error) return <><Err error={error} /><button className="btn" onClick={refresh}>Reload proposal</button></>;
  if (!data) return null;
  return <section className="pad" aria-label="Proposal inspection">
    <h2>{data.name}</h2>
    <p>Author: {data.author || "not recorded"}</p>
    <p>Revision {data.revision} · <code>{data.digest}</code></p>
    <p>Source turn: {data.turnId || "not recorded"}</p>
    <h3>{data.pending ? (data.active ? "Current approved version" : "Original proposal (not active)") : "Proposed content (not active)"}</h3>
    <ReviewContent content={data} />
    {data.pending && <><h3>Proposed amendment</h3>
      <p>Rationale: {data.pending.rationale}</p><p>Source turn: {data.pending.turnId || "not recorded"}</p>
      <ReviewContent content={data.pending} />
    </>}
    {failure && <><Err error={failure} /><p>Reload and inspect the current proposal before deciding again. The previous decision is not retried automatically.</p></>}
    <button className="btn" disabled={busy} onClick={refresh}>Reload proposal</button>
    <fieldset disabled={busy || !!failure}>
      <legend>Human review decision</legend>
      <label><input type="checkbox" checked={inspected} onChange={(e) => setInspected(e.target.checked)} /> I have inspected the instructions and reference files shown above.</label>
      <button className="btn" disabled={!inspected} onClick={() => void decide(true)}>Approve reviewed proposal</button>
      <button className="btn" disabled={!inspected} onClick={() => void decide(false)}>Reject reviewed proposal</button>
    </fieldset>
  </section>;
}

// Proposal text is untrusted data. Plain text preserves exact commands and
// filenames without interpreting embedded links, markup or model instructions.
export function ReviewContent({ content }: { content: LearnedChange }) {
  return <><p>{content.description}</p><h4>Instructions</h4><pre>{content.body}</pre>
    {Object.entries(content.files ?? {}).sort(([a], [b]) => a.localeCompare(b)).map(([name, body]) =>
      <section key={name}><h4>File: {name}</h4><pre>{body}</pre></section>)}
  </>;
}
