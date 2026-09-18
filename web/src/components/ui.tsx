import { useCallback, useEffect, useState } from "react";
import { ApiError, type BotStatus } from "../api";
import { botVars, initials } from "../theme";

/** useLoad wraps the three states every screen has: loading, an error
 * worth showing, and data.
 *
 * A hook rather than a per-screen effect because getting the third one
 * wrong renders an empty list when the node is unreachable — and "no
 * bots yet" and "the API is down" need different responses from the
 * person reading it. */
export function useLoad<T>(fn: () => Promise<T>, deps: unknown[] = []) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<Error | null>(null);
  const [loading, setLoading] = useState(true);

  const reload = useCallback(() => {
    let live = true;
    setLoading(true);
    fn()
      .then((v) => { if (live) { setData(v); setError(null); } })
      .catch((e: Error) => { if (live) setError(e); })
      .finally(() => { if (live) setLoading(false); });
    return () => { live = false; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);

  useEffect(() => reload(), [reload]);
  return { data, error, loading, reload };
}

export function Avatar({ id, name, size = 30, dim }: {
  id: string; name?: string; size?: number; dim?: boolean;
}) {
  return (
    <div
      className={dim ? "av dim" : "av"}
      style={{ ...botVars(id), width: size, height: size, fontSize: Math.round(size * 0.37) }}
    >
      {initials(name || id)}
    </div>
  );
}

const LABEL: Record<BotStatus, string> = {
  pending: "Queued", claimed: "Working", done: "Done",
  failed: "Failed", cancelled: "Cancelled",
};

/** A dot plus a word. The dot is scannable down a column in a way a
 * pill is not, and the word means the colour never carries the meaning
 * alone — which matters for the people who see red and green the
 * same. */
export function Status({ status }: { status: BotStatus }) {
  return <span className={`st ${status}`}><i />{LABEL[status] ?? status}</span>;
}

export function Spinner() { return <div className="spin" />; }

/** Says what went wrong in the API's own words.
 *
 * Deliberately not a generic apology: the node's messages are written
 * for a person — "engineering has 200 pending items (cap 200); it is
 * not keeping up" — and replacing them with something friendlier
 * throws away the only useful part. */
export function Err({ error }: { error: Error }) {
  const status = error instanceof ApiError ? error.status : undefined;
  const hint =
    status === 401 ? "Run `lobslaw login` on this machine and enter the code, or try again."
    : status === 503 ? "This node cannot serve that yet. It is unavailable, not gone."
    : undefined;
  return (
    <div className="err">
      <b>{status ? `Error ${status}` : "Error"}</b>
      <p>{error.message}</p>
      {hint && <div className="hint">{hint}</div>}
    </div>
  );
}

export function Empty({ title, hint }: { title: string; hint?: string }) {
  return <div className="empty"><b>{title}</b>{hint && <span>{hint}</span>}</div>;
}

/** Relative time, because "4 minutes ago" is the question somebody
 * actually asks of a queue. Falls back to a date once that stops
 * being useful. */
export function when(ts?: string): string {
  if (!ts) return "";
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  const s = Math.floor((Date.now() - d.getTime()) / 1000);
  if (s < 10) return "just now";
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  if (s < 604800) return `${Math.floor(s / 86400)}d ago`;
  return d.toLocaleDateString();
}
