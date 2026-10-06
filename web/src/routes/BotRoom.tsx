import { useEffect, useId, useRef, useState } from "react";
import { Link, useParams, useSearchParams } from "react-router-dom";
import { api, type Bot, type InboxItem, type TranscriptMessage } from "../api";
import { InboxSummaryNotice } from "../components/InboxSummaryNotice";
import { TaskEvidence } from "../components/TaskEvidence";
import { Markdown } from "../components/Markdown";
import { Mascot } from "../components/Mascot";
import { MultiSelect } from "../components/MultiSelect";
import { Err, Spinner, useLoad, when } from "../components/ui";
import { botVars } from "../theme";
import { CheckIcon, Chevron, Collapse, Disclosure, StateText, StatusMark, useFeedback, useNewItems } from "../components/Motion";
import { useBotChat, type ChatMessage } from "../components/ChatSessions";
import { Composer } from "../components/Composer";
import { TranscriptText } from "../components/SourceContent";

/** One bot, one room.
 *
 * Clicking a bot opens the CONVERSATION, not a settings form. That is
 * the whole shape of the change: the previous version made a bot
 * something you configure, with talking to it behind a separate page,
 * and the sidebar previews made that split read as a mistake.
 *
 * Its queue is woven into the same thread rather than living on a tab.
 * A bot that answered you at 14:02 and worked a scheduled task at
 * 14:05 did those things in one timeline, and splitting them across
 * two views makes you reconstruct the order by hand.
 */

type Entry =
  | ChatMessage
  | { kind: "work"; item: InboxItem; at: number }
  | { kind: "sent"; item: InboxItem; at: number };

export function BotRoom({ onChanged }: { onChanged: () => void }) {
  const { botId = "" } = useParams();
  const { data: bot, error, loading, reload } = useLoad(() => api.getBot(botId), [botId]);
  const { data: work, reload: reloadWork } = useLoad(() => api.listInbox(botId, "all"), [botId]);
  // What this bot HANDED OUT. A bot's own inbox is only half the
  // story: the coordinator delegating the launch to marketing wrote a
  // row in marketing's queue and nothing in its own, so the delegation
  // was invisible in the one room where you were watching it happen.
  // The activity feed is the only cross-bot view, so the outgoing side
  // is filtered out of it.
  const { data: feed, reload: reloadFeed } = useLoad(() => api.activity(200), [botId]);
  // For names. "handed to marketing" is a record; "handed to
  // Marketing" is a sentence about a colleague, and the whole point of
  // this screen is that these read as people.
  const { data: roster } = useLoad(() => api.listBots(), []);
  const { session, send: sendMessage, stop, history, answered, draft: saveDraft } = useBotChat(botId);
  const { messages: said, draft, busy, working, partial, notice, error: sendErr, ask, liveReply } = session;
  const setDraft = (value: string) => saveDraft(botId, value);
  // The company view's "Ask for a briefing" arrives as a query
  // parameter rather than a sent message: it drops the words in the
  // box and leaves the send to you. An action that fires a turn on
  // navigation is one you cannot change your mind about, and this is
  // the manager's question — you should get to word it.
  const [params] = useSearchParams();
  useEffect(() => {
    if (params.get("brief") === "1") {
      setDraft("Brief me. What is the team working on, what landed since I last asked, and what needs a decision from me?");
    }
  }, [params]);
  const [settings, setSettings] = useState(false);
  const [visitedSettings, setVisitedSettings] = useState(false);
  const notify = useFeedback();
  const arrivals = useNewItems(work, botId);
  const threadPane = useRef<HTMLDivElement>(null);
  const end = useRef<HTMLDivElement>(null);

  // A new bot is a new room. Carrying the spoken lines across would
  // show a conversation the bot you just opened has never had.
  //
  // Then load the one it HAS had. The thread is durable — the turn
  // runner writes it and reads it back, so the bot remembers too —
  // and not showing it made a refresh look like the conversation had
  // been thrown away.
  useEffect(() => {
    setSettings(false); setVisitedSettings(false);
    let live = true;
    // The console files a bot's own conversation under bot:<id>, the
    // same address the turn runner writes to.
    const id = `bot:${botId}`;
    Promise.all([api.transcript(id), api.botSessions(botId)])
      .then(([msgs, sessions]) => {
        if (!live) return;
        // Stored messages carry a sequence number and no timestamp, so
        // they cannot be placed on the same clock as queue items
        // without one. Anchoring to when the session was last written
        // and spacing backwards puts the conversation at roughly the
        // right point among the work, which is what the single
        // timeline promises. Sequence still decides their order
        // relative to each other, which is the part that must be exact.
        const anchor = sessions.find((x) => x.id === id)?.updated_at;
        const end = anchor ? new Date(anchor).getTime() : Date.now();
        const entries = msgs.map(toEntry).filter((e): e is ChatMessage => e !== null);
        const spacing = 30_000;
        entries.forEach((e, i) => { e.at = end - (entries.length - 1 - i) * spacing; });
        history(botId, entries);
      })
      // A bot nobody has spoken to yet has no transcript, and a 404
      // here is that — not a failure worth showing.
      .catch(() => {});
    return () => {
      live = false;
    };
  }, [botId, history]);

  // The queue moves without you. Polling keeps the thread honest
  // rather than frozen at whatever it was when you arrived.
  useEffect(() => {
    const t = setInterval(() => { reloadWork(); reloadFeed(); }, 8000);
    const completed = (event: Event) => { if ((event as CustomEvent<string>).detail === botId) { reloadWork(); reloadFeed(); } };
    window.addEventListener("lobslaw:chat-completed", completed);
    return () => { clearInterval(t); window.removeEventListener("lobslaw:chat-completed", completed); };
  }, [botId, reloadWork, reloadFeed]);

  // `ask` belongs in here: a confirmation can arrive taller than the
  // space left and land with its buttons below the fold, so the one
  // message that REQUIRES an action was the one you could not see.
  useEffect(() => {
    if (settings) return;
    // The reduced-motion rule in the stylesheet cannot reach this:
    // `behavior` is a JS argument, and an explicit "smooth" wins over
    // any `scroll-behavior` the CSS sets. Asked directly instead.
    const still = window.matchMedia?.("(prefers-reduced-motion: reduce)").matches;
    end.current?.scrollIntoView({ behavior: still ? "auto" : "smooth" });
  }, [said, work, working, partial, ask, settings]);
  useEffect(() => {
    if (settings) threadPane.current?.scrollTo({ top: 0, behavior: "auto" });
  }, [settings]);

  const send = () => void sendMessage(botId, draft.trim(), bot?.display_name || botId);

  if (error) return <div className="wrap"><Err error={error} /></div>;
  if (loading && !bot) return <Spinner />;
  if (!bot) return null;

  // One timeline. Queue items and spoken lines interleave by time,
  // because that is the order they happened in.
  // Self-created tasks already appear in this bot's work queue.
  const sentByMe = (feed ?? []).filter((i) => i.sender === `bot:${bot.id}` && i.recipient !== bot.id);
  const names = new Map((roster ?? []).map((b) => [b.id, b.display_name || b.id]));
  const thread: Entry[] = [
    ...(work ?? []).map((item) => ({
      kind: "work" as const, item,
      at: new Date(item.completed_at ?? item.created_at ?? 0).getTime(),
    })),
    ...sentByMe.map((item) => ({
      kind: "sent" as const, item,
      at: new Date(item.created_at ?? 0).getTime(),
    })),
    ...said,
    ...(busy && !ask && liveReply && !said.some((e) => e.kind === "said" && e.id === liveReply.id) ? [{
      kind: "said" as const, id: liveReply.id, from: "bot" as const, text: partial,
      at: liveReply.at, animate: true, pending: true,
    }] : []),
  ].sort((a, b) => a.at - b.at);

  return (
    <div className="chat" style={botVars(bot.id)}>
      <header className="room-head">
        <Mascot id={bot.id} size={30} dim={!bot.enabled} working={busy || (work ?? []).some((item) => item.status === "claimed")} />
        <div className="grow">
          <div className="room-nm">
            {bot.display_name || bot.id}
            {bot.is_coordinator && <span className="tag brand">coordinator</span>}
            {!bot.enabled && <span className="tag warn">disabled</span>}
          </div>
          <div className="meta">{bot.description || `bot:${bot.id}`}</div>
        </div>
        <button className="btn ghost sm" aria-expanded={settings} aria-controls="bot-settings" onClick={() => { setSettings((v) => !v); setVisitedSettings(true); }}>
          {settings ? "Close" : "Settings"}
        </button>
      </header>

      {/* What a screen reader is told, and the only thing on this
          screen that is live.

          Not the thread itself: `partial` grows a token at a time, and
          a live transcript would read every prefix of the reply aloud.
          So the region carries ONE short sentence per event — replied,
          needs approval, failed — and the reply itself stays in the
          thread to be read at the user's pace. */}
      <div className="sr-only" role="status" aria-live="polite">{notice}</div>

      <div ref={threadPane} className="thread" tabIndex={0} aria-label={settings ? "Bot settings" : "Conversation"}>
        <div className="thread-in">
          <Collapse open={settings} id="bot-settings">
            {visitedSettings && <div className="col gap-lg" key={bot.id}>
              <Settings bot={bot} onSaved={() => { reload(); onChanged(); notify("Bot settings saved"); }} />
              <Routines botId={bot.id} active={settings} />
              <Knows botId={bot.id} />
            </div>}
          </Collapse>
          <Collapse open={!settings}>
            <div className="col gap-lg">
              <BotHistory key={bot.id} botId={bot.id} />
              {thread.length === 0 && !working && (
                <div className="opening">
                  <Mascot id={bot.id} size={56} />
                  <div className="nm">{bot.display_name || bot.id}</div>
                  <div className="desc">{bot.description || "Ask it something."}</div>
                </div>
              )}
              {thread.map((e) =>
                e.kind === "said" ? <Said key={e.id} e={e} bot={bot} />
                : e.kind === "sent" ? <Handoff key={`s-${e.item.id}`} item={e.item} names={names} />
                : <Work key={e.item.id} item={e.item} botId={bot.id} onChanged={reloadWork} arrived={arrivals.has(e.item.id)} />)}
              {ask && <Approval ask={ask} onAnswered={() => answered(botId)} />}
              {sendErr && <div className="chat-error" role="alert"><Err error={sendErr} /><button className="btn ghost sm" onClick={() => {
                const last = [...said].reverse().find((message) => message.from === "me");
                if (last) setDraft(last.text);
              }}>Use last message again</button></div>}
              <div ref={end} />
            </div>
          </Collapse>
        </div>
      </div>

      <Collapse open={!settings} className="composer-reveal">
        <Composer draft={draft} onChange={setDraft} onSend={send} busy={busy} onStop={() => stop(botId)} placeholder={`Message ${bot.display_name || bot.id}…`} />
      </Collapse>
    </div>
  );
}

/** A question the turn is blocked on, with the two answers.
 *
 * The console used to print "approve it on the channel you normally
 * use" — which was true only because this channel could not ask. It
 * can now: the turn is held open on the stream while you decide, so
 * answering here is what releases it.
 */
export function Approval({ ask, onAnswered }: {
  ask: { id: string; reason: string; action?: string; resource?: string };
  onAnswered: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<Error | null>(null);
  const approve = useRef<HTMLButtonElement>(null);

  // Move focus here when the question appears.
  //
  // It interrupts the thread to demand a decision, which is the one
  // case where taking focus is right rather than rude: a keyboard user
  // would otherwise have to hunt for a control that arrived without
  // warning, and a screen reader would not be told at all.
  //
  // Approve is focused rather than Deny because it is the affirmative
  // action of the pair, and both are one Tab apart. Neither fires on
  // Enter without the button being focused first.
  useEffect(() => { approve.current?.focus(); }, [ask.id]);

  async function answer(approve: boolean) {
    setBusy(true); setError(null);
    try { await api.resolvePrompt(ask.id, approve); onAnswered(); }
    catch (e) { setError(e as Error); setBusy(false); }
  }

  return (
    // role="group" with aria-labelledby, not role="dialog": this is
    // inline in the thread rather than modal, and claiming to be a
    // dialog would promise a focus trap and an Escape-to-dismiss that
    // do not exist — and should not, because dismissing the question
    // is not the same as answering it.
    <div
      className="ask"
      role="group"
      aria-labelledby={`ask-${ask.id}-title`}
      aria-describedby={`ask-${ask.id}-reason`}
    >
      {/* Announced the moment it arrives, for anyone not watching. */}
      <div className="ask-hd" id={`ask-${ask.id}-title`} role="status">
        Needs your approval
      </div>
      <div className="ask-reason" id={`ask-${ask.id}-reason`}>{ask.reason}</div>
      {ask.resource && (
        // The exact operation, verbatim and monospaced. An approval
        // dialog that paraphrases what it is asking about is how you
        // end up approving something else.
        <pre className="ask-what">{ask.action ? `${ask.action}: ` : ""}{ask.resource}</pre>
      )}
      {error && <Err error={error} />}
      <div className="ask-btns">
        <button
          ref={approve} className="btn primary sm"
          onClick={() => answer(true)} disabled={busy}
          aria-label={`Approve: ${ask.reason}`}
        >
          {busy ? "…" : "Approve"}
        </button>
        <button
          className="btn ghost sm"
          onClick={() => answer(false)} disabled={busy}
          aria-label={`Deny: ${ask.reason}`}
        >
          Deny
        </button>
      </div>
    </div>
  );
}

/** One stored message as a thread entry.
 *
 * Tool and system messages are dropped: they are the turn's internal
 * workings, and the receipt already says which tools ran. An assistant
 * message with no text is a tool-call step, which has nothing to show.
 */
function toEntry(m: TranscriptMessage): Entry | null {
  const text = (m.content ?? "").trim();
  if (!text) return null;
  if (m.role !== "user" && m.role !== "assistant") return null;
  // `at` is filled in by the caller, which knows when the session was
  // last written; a sequence number alone cannot be compared against
  // the epoch timestamps everything else on the timeline uses.
  return { kind: "said", id: `stored-${m.seq}`, from: m.role === "user" ? "me" : "bot", text, at: 0 };
}

/** Shown from the moment you hit Send until the reply lands.
 *
 * A turn against a real model runs thirty to sixty seconds — far past
 * the point where a bare animation stops reassuring and starts looking
 * hung. So it says WHO is working and HOW LONG it has been, which is
 * the difference between "still going" and "something broke".
 */
/** Returned tool results prove dispatch, not successful external effects.
 * Historical inbox names lack outcome evidence and remain labelled attempts. */
export function Receipt({ tools, attempts, tokens, cost }: { tools?: string[]; attempts?: string[]; tokens?: number; cost?: number }) {
  const ran = tools ?? [];
  if (tools === undefined && attempts === undefined && !tokens && !cost) return null;
  return (
    <div className="receipt">
      {ran.length > 0 ? (
        <>
          <span className="receipt-lbl">returned a result (not proof of success)</span>
          {ran.map((t) => <code key={t}>{t}</code>)}
        </>
      ) : (
        <span className="receipt-lbl">no confirmed tool execution</span>
      )}
      {!!attempts?.length && <><span className="receipt-lbl">attempted · may be refused, pending or failed</span>{attempts.map((t) => <code key={t}>{t}</code>)}</>}
      {!!tokens && <span className="receipt-num">{tokens.toLocaleString()} tokens</span>}
      {!!cost && cost > 0 && <span className="receipt-num">${cost.toFixed(4)}</span>}
    </div>
  );
}

function Working({ active }: { active: boolean }) {
  const [secs, setSecs] = useState(0);
  useEffect(() => {
    if (!active) { setSecs(0); return; }
    const t = setInterval(() => setSecs((s) => s + 1), 1000);
    return () => clearInterval(t);
  }, [active]);

  return (
    <div className="waiting">
      <span className="dots"><i /><i /><i /></span>
      <span className="waiting-txt">
        {secs < 12 ? "Thinking…" : secs < 40 ? "Preparing your reply…" : "Still waiting for the model…"}
      </span>
      {secs >= 5 && <span className="waiting-secs">{secs}s</span>}
    </div>
  );
}

function Said({ e, bot }: { e: Extract<Entry, { kind: "said" }>; bot: Bot }) {
  if (e.from === "me") {
    return <div className={`msg me${e.animate ? " message-arrival" : ""}`} data-message-id={e.id}><div className="bubble">{e.text}</div></div>;
  }
  return (
    <div className={`msg${e.animate ? " message-arrival" : ""}`} data-message-id={e.id}>
      <Mascot id={bot.id} size={30} working={e.pending} />
      <div className="grow">
        <div className="from">{bot.display_name || bot.id}</div>
        {/* A notice is our own generated sentence, not model output —
            rendering it as markdown would be pretending otherwise. */}
        <Collapse open={!!e.pending && !e.text} className="reply-thinking"><Working active={!!e.pending && !e.text} /></Collapse>
        <Collapse open={!!e.text} className="reply-text">
          {e.notice
            ? <div className="txt notice">{e.text}</div>
            : <div className="txt" aria-busy={e.pending || undefined}><Markdown>{e.text}</Markdown>{e.pending && <span className="caret" aria-hidden="true" />}</div>}
        </Collapse>
        {!e.notice && !e.pending && <>
          {!e.receipts?.length && <Receipt tools={e.tools} attempts={e.attempts} tokens={e.tokens} cost={e.cost} />}
          <TaskEvidence transcript={e.transcript} receipts={e.receipts} />
          {e.sessionId && !e.transcript?.length && <Transcript sessionId={e.sessionId} />}
        </>}
      </div>
    </div>
  );
}

/** A queue item, rendered inline in the thread.
 *
 * Visually distinct from a spoken line — indented, quieter, with its
 * own affordances — because "you asked me this" and "something made me
 * do this" are different events and flattening them would misrepresent
 * who started what.
 */
/** Work this bot handed to someone else.
 *
 * Rendered as an outgoing line — indented the other way from the
 * bot's own queue — because the direction is the whole point. Without
 * it a manager bot looks like it answered you itself, when what it
 * actually did was break the ask up and give it to somebody.
 */
function Handoff({ item, names }: { item: InboxItem; names: Map<string, string> }) {
  return (
    <Link to={`/bots/${item.recipient}`} className="ev handoff" style={botVars(item.recipient)}>
      <span className="ev-node hand" aria-hidden="true">
        <Mascot id={item.recipient} size={18} />
      </span>
      <div className="ev-main">
        <div className="ev-meta">
          <b>passed to {names.get(item.recipient) ?? item.recipient}</b>
          <span className="ev-dot">·</span>
          <span className={`ev-state ${item.status}`}>{item.status === "done" ? "they finished" : item.status}</span>
          <span className="ev-dot">·</span>
          {when(item.created_at)}
        </div>
        <div className="ev-ttl sm">{item.subject}</div>
      </div>
    </Link>
  );
}

function Work({ item, botId, onChanged, arrived }: { item: InboxItem; botId: string; onChanged: () => void; arrived: boolean }) {
  const [open, setOpen] = useState(false);
  const detailId = useId();
  const [full, setFull] = useState<InboxItem | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<Error | null>(null);
  const notify = useFeedback();

  useEffect(() => {
    let live = true;
    setFull(null);
    if (open) api.readItem(botId, item.id).then((value) => {
      if (live) setFull(value);
    }).catch((e) => { if (live) setError(e as Error); });
    return () => { live = false; };
  }, [open, botId, item.id, item.status, item.attempts, item.completed_at]);

  function toggle() { setOpen((value) => !value); }
  async function act(action: "retry" | "cancel") {
    setBusy(true); setError(null);
    try { setFull(await api.actOnItem(botId, item.id, action)); onChanged(); notify(action === "retry" ? "Task queued for another attempt" : "Task withdrawn"); }
    catch (e) { setError(e as Error); } finally { setBusy(false); }
  }

  const d = full ?? item;

  // What happened, as a sentence rather than a status enum. "Cancelled"
  // in a chip tells you the state; "withdrawn before anyone picked it
  // up" tells you whether to care.
  const LINE: Record<string, string> = {
    pending: "queued — nobody has picked this up yet",
    claimed: "working on it now",
    done: "done",
    failed: `failed after ${item.attempts} ${item.attempts === 1 ? "try" : "tries"}`,
    cancelled: "withdrawn before it was started",
  };

  return (
    <div className={`ev ${item.status}${arrived ? " activity-arrival" : ""}`}>
      {/* The rail node. Status lives here as shape and colour, so the
          text beside it can be plain language. */}
      <StatusMark status={item.status} className="ev-node" />
      <div className="ev-main">
        {/* A button, because it is one: a div with onClick cannot be
            reached by Tab and does not respond to Enter or Space. */}
        <button
          type="button"
          className={`ev-head${open ? " open" : ""}`}
          onClick={toggle}
          aria-expanded={open}
          aria-controls={detailId}
          aria-label={`${open ? "Collapse" : "Expand"}: ${item.subject || "untitled item"}`}
        >
          <span className="ev-ttl">{item.subject || "(no subject)"}</span>
          <Chevron className="ev-chevron" />
        </button>
        <div className="ev-meta">
          <StateText value={item.status} className="ev-state">{LINE[item.status] ?? item.status}</StateText>
          <span className="ev-dot">·</span>
          {item.sender === "operator" ? "you asked for this" : `asked by ${item.sender.replace(/^bot:/, "")}`}
          <span className="ev-dot">·</span>
          {when(item.created_at)}
          {d.task_id && <Link className="ev-act" to={`/approvals/${encodeURIComponent(d.task_id)}`}>Review task</Link>}
          {!d.task_id && !d.truncated_fields?.includes("task_id") && (item.status === "failed" || item.status === "cancelled") && (
            <button className="ev-act" onClick={(e) => { e.stopPropagation(); act("retry"); }} disabled={busy}>
              Try again
            </button>
          )}
          {item.status === "pending" && (
            <button className="ev-act" onClick={(e) => { e.stopPropagation(); act("cancel"); }} disabled={busy}>
              Withdraw
            </button>
          )}
        </div>
        <InboxSummaryNotice item={d} />
        {item.result && !open && <div className="ev-body"><Markdown>{item.result}</Markdown></div>}
        {item.status === "done" && (
          <Receipt attempts={d.tools_used} tokens={d.tokens_used == null ? undefined : Number(d.tokens_used)} cost={d.cost_usd == null ? undefined : Number(d.cost_usd)} />
        )}
        {/* A failed item keeps its error, visibly. A task that vanished
            quietly is the failure the queue exists to prevent. */}
        {item.error && !open && <div className="ev-body bad">{item.error}</div>}
        {error && <Err error={error} />}
        <Collapse open={open} id={detailId}>
          <div className="ev-detail">
            <Block label="Asked" body={d.body} />
            {d.result && <Block label="Result" body={d.result} />}
            {d.error && <Block label="Error" body={d.error} bad />}
            {d.session_id && <Transcript sessionId={d.session_id} />}
          </div>
        </Collapse>
      </div>
    </div>
  );
}

/** What this bot has set itself to do on a schedule.
 *
 * Shown because its absence is the interesting case. A coordinator
 * turn reported setting a reminder and never set one, and nobody
 * caught it for the simple reason that there was nowhere to look —
 * the routine either existed or did not, and both looked identical.
 */
function Routines({ botId, active }: { botId: string; active: boolean }) {
  const { data, error, loading, reload } = useLoad(() => api.routines(botId), [botId]);
  useEffect(() => {
    if (!active) return;
    const timer = setInterval(reload, 8000);
    return () => clearInterval(timer);
  }, [reload, active]);
  if (loading && !data) return null;

  return (
    <div className="panel">
      <div className="lbl">Routines</div>
      <p className="hint">Ask this agent to pause, resume, change, or stop a routine. Results appear in the conversation; use schedule_get to inspect its last dispatch and checkpoint.</p>
      {error ? <Err error={error} />
        : !data || data.length === 0 ? (
          <div className="panel-empty">
            Nothing scheduled. This bot can set its own routines with
            <code>schedule_create</code> — ask it to.
          </div>
        ) : (
          <div className="col gap-sm" style={{ marginTop: 9 }}>
            {data.map((rt) => (
              <div key={rt.id} className={`routine${rt.enabled ? "" : " off"}`}>
                <div className="routine-top">
                  <b>{rt.name || rt.id}</b>
                  <code>{rt.schedule}</code>
                  {!rt.enabled && <span className="tag warn">paused</span>}
                  <span className="grow" />
                  <span className="meta">{rt.next_run ? `next ${when(rt.next_run)}` : "not scheduled"}</span>
                </div>
                {rt.prompt && <div className="routine-body">{rt.prompt}</div>}
                <div className="hint">ID: {rt.id}</div>
                {rt.last_run && <div className="meta" style={{ marginTop: 4 }}>last ran {when(rt.last_run)}</div>}
              </div>
            ))}
          </div>
        )}
    </div>
  );
}

/** What this bot remembers.
 *
 * Read-only on purpose: the question it answers is "why did it say
 * that", and a viewer that also let you rewrite the evidence would be
 * a worse answer to it.
 */
function Knows({ botId }: { botId: string }) {
  const { data, error, loading } = useLoad(() => api.memory(botId), [botId]);
  if (loading && !data) return null;

  const records = data?.records ?? [];
  return (
    <div className="panel">
      <div className="lbl">
        What it remembers
        {data && data.total > records.length && (
          <span className="meta"> · showing {records.length} of {data.total}</span>
        )}
      </div>
      {error ? <Err error={error} />
        : records.length === 0 ? (
          <div className="panel-empty">
            Nothing yet. This bot remembers what it is told to remember.
          </div>
        ) : (
          <div className="col gap-sm" style={{ marginTop: 9 }}>
            {records.map((rec) => (
              <div key={rec.id} className="know">
                <div className="know-text">{rec.text}</div>
                <div className="know-meta">
                  {(rec.tags ?? []).map((t) => <span key={t} className="tag">{t}</span>)}
                  <span className="grow" />
                  <span className="meta">{when(rec.created_at)}</span>
                </div>
              </div>
            ))}
          </div>
        )}
    </div>
  );
}

function Block({ label, body, bad }: { label: string; body?: string; bad?: boolean }) {
  if (!body) return null;
  return (
    <div>
      <div className="lbl" style={bad ? { color: "var(--bad)" } : undefined}>{label}</div>
      {/* An error is ours and stays literal — its formatting is the
          evidence. Everything else came from a model and is markdown. */}
      {bad ? (
        <div style={{ fontSize: 13.5, lineHeight: 1.7, marginTop: 5,
          whiteSpace: "pre-wrap", color: "var(--bad)" }}>{body}</div>
      ) : (
        <div style={{ marginTop: 5, color: "var(--mid)" }}><Markdown>{body}</Markdown></div>
      )}
    </div>
  );
}

function Transcript({ sessionId }: { sessionId: string }) {
  const [open, setOpen] = useState(false);
  const [msgs, setMsgs] = useState<TranscriptMessage[] | null>(null);
  const [error, setError] = useState<Error | null>(null);
  async function toggle() {
    const next = !open; setOpen(next);
    if (next && !msgs) {
      try { setMsgs(await api.transcript(sessionId)); } catch (e) { setError(e as Error); }
    }
  }
  return (
    <div>
      <button className="btn ghost sm" style={{ padding: 0 }} onClick={toggle} aria-expanded={open} aria-label={`${open ? "▾" : "▸"} what it did`}>
        <Chevron /> what it did
      </button>
      {error && <Err error={error} />}
      <Collapse open={open}>
        {msgs && (
        <div className="col gap-sm" style={{ marginTop: 8, paddingLeft: 12, borderLeft: "1px solid var(--edge)" }}>
          {msgs.map((m) => (
            <div key={m.seq}>
              <div className="lbl">{m.role}{m.tool_calls ? ` · ${m.tool_calls} tool calls` : ""}</div>
              <div style={{ fontSize: 13, color: "var(--mid)", whiteSpace: "pre-wrap", marginTop: 2 }}>
                {m.content ? <TranscriptText role={m.role}>{m.content}</TranscriptText> : "(tool calls only)"}
              </div>
            </div>
          ))}
        </div>
        )}
      </Collapse>
    </div>
  );
}

function BotHistory({ botId }: { botId: string }) {
  return <Disclosure title="Conversation and task history">
    {(hasOpened) => hasOpened && <BotHistoryList botId={botId} />}
  </Disclosure>;
}

function BotHistoryList({ botId }: { botId: string }) {
  const { data, loading, error, reload } = useLoad(() => api.botSessions(botId), [botId]);
  return <div className="pad">
    <button className="btn" onClick={reload}>Refresh history</button>
    {loading && <Spinner />}{error && <Err error={error} />}
    {!error && (data ?? []).map((session) => <section key={session.id}>
      <p>{session.title || session.id}</p><Transcript key={`${session.id}:${session.messages}`} sessionId={session.id} />
    </section>)}
    {!loading && !error && data?.length === 0 && <p>No recorded history yet.</p>}
  </div>;
}

function Settings({ bot, onSaved }: { bot: Bot; onSaved: () => void }) {
  const [f, setF] = useState({
    display_name: bot.display_name, description: bot.description,
    instructions: bot.instructions,
  });
  const [tools, setTools] = useState<string[]>(bot.tools);
  const [edges, setEdges] = useState<string[]>(bot.may_message);
  const catalogue = useLoad(() => api.tools());
  const roster = useLoad(() => api.listBots());
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<Error | null>(null);
  const [saved, setSaved] = useState(false);
  useEffect(() => { setSaved(false); }, [f, tools, edges]);
  useEffect(() => {
    if (!saved) return;
    const timer = setTimeout(() => setSaved(false), 2500);
    return () => clearTimeout(timer);
  }, [saved]);

  async function save(patch?: Partial<Bot>) {
    if (busy) return;
    setSaved(false);
    setBusy(true); setError(null);
    try {
      await api.updateBot(bot.id, { revision: bot.revision, ...(patch ?? {
        display_name: f.display_name, description: f.description, instructions: f.instructions,
        tools, may_message: edges,
      }) });
      setSaved(!patch); onSaved();
    } catch (e) { setError(e as Error); } finally { setBusy(false); }
  }

  return (
    <div className="card pad col gap-lg">
      <div className="field">
        <label htmlFor="bot-display-name">Display name</label>
        <input id="bot-display-name" className="in" value={f.display_name} onChange={(e) => setF({ ...f, display_name: e.target.value })} />
      </div>
      <div className="field">
        <label htmlFor="bot-description">Description</label>
        <input id="bot-description" className="in" value={f.description} onChange={(e) => setF({ ...f, description: e.target.value })} />
      </div>
      <div className="field">
        <label htmlFor="bot-instructions">Instructions</label>
        <textarea id="bot-instructions" className="ta" rows={7} value={f.instructions}
          onChange={(e) => setF({ ...f, instructions: e.target.value })} />
        <div className="hint">Rides on every turn this bot takes. The role, not a task.</div>
      </div>
      <MultiSelect
        label="Tools"
        options={(catalogue.data ?? []).map((t) => ({ value: t.name, hint: t.description }))}
        value={tools}
        onChange={setTools}
        placeholder="Search tools…"
        hint="Empty means every tool this node has. A tool left out is one this bot is never even shown — that is how you limit what it can do."
      />
      {bot.is_coordinator ? (
        <div className="field">
          <label>Can message</label>
          <div className="hint">
            The coordinator reaches every bot in its team automatically; the list is kept up to
            date as bots are added.
          </div>
        </div>
      ) : (
        <MultiSelect
          label="Can message"
          options={(roster.data ?? []).map((b) => ({ value: b.id, hint: b.display_name }))}
          value={edges}
          onChange={setEdges}
          placeholder="Search bots…"
          exclude={[bot.id]}
          hint="Who this bot may hand work to. Loops are refused — the error names the path."
        />
      )}
      {error && <Err error={error} />}
      <div className="row gap-sm settings-actions">
        <button className={`btn primary${saved ? " action-saved" : ""}`} onClick={() => save()} disabled={busy}>
          {busy ? "Saving…" : saved ? <><CheckIcon />Saved</> : "Save"}
        </button>
        {/* The coordinator answers your Telegram messages and the API
            refuses to remove it, so the control is absent rather than
            present and failing. */}
        {!bot.is_coordinator && (
          <button className="btn" onClick={() => save({ enabled: !bot.enabled })} disabled={busy}>
            {bot.enabled ? "Disable" : "Enable"}
          </button>
        )}
        <div className="grow" />
        <Link className="btn ghost sm" to="/config">Node config →</Link>
      </div>
    </div>
  );
}
