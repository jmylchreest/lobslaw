import { useEffect, useRef, useState } from "react";
import { Link, useParams, useSearchParams } from "react-router-dom";
import { api, streamBotChat, type Bot, type InboxItem, type TranscriptMessage } from "../api";
import { Markdown } from "../components/Markdown";
import { Mascot } from "../components/Mascot";
import { MultiSelect } from "../components/MultiSelect";
import { Err, Spinner, useLoad, when } from "../components/ui";
import { botVars } from "../theme";

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
  | {
      kind: "said"; from: "me" | "bot"; text: string; at: number; notice?: boolean;
      // What the turn actually ran, carried alongside what it said.
      tools?: string[]; tokens?: number; cost?: number; sessionId?: string;
    }
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
  const [said, setSaid] = useState<Entry[]>([]);
  const [draft, setDraft] = useState("");
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
  const [busy, setBusy] = useState(false);
  const [working, setWorking] = useState(false);
  // Text as it is generated. Held apart from `said` until the turn
  // ends: a partial reply is not yet a message, and committing it
  // early would leave a half-sentence in the thread if the turn then
  // failed.
  const [partial, setPartial] = useState("");
  // One sentence, announced to assistive tech when the turn changes
  // state. See the region it renders into, below.
  const [notice, setNotice] = useState("");
  const [sendErr, setSendErr] = useState<Error | null>(null);
  // The turn is blocked on the other end of the open stream, so this
  // is a live question rather than a record of one. Cleared as soon as
  // it is answered — a stale set of Approve/Deny buttons invites you
  // to answer a question that has already timed out.
  const [ask, setAsk] = useState<{ id: string; reason: string; action?: string; resource?: string } | null>(null);
  const [settings, setSettings] = useState(false);
  const end = useRef<HTMLDivElement>(null);
  const activeStream = useRef<AbortController | null>(null);

  // A new bot is a new room. Carrying the spoken lines across would
  // show a conversation the bot you just opened has never had.
  //
  // Then load the one it HAS had. The thread is durable — the turn
  // runner writes it and reads it back, so the bot remembers too —
  // and not showing it made a refresh look like the conversation had
  // been thrown away.
  useEffect(() => {
    setSaid([]); setSendErr(null); setSettings(false); setPartial(""); setAsk(null);
    setBusy(false); setWorking(false);
    let live = true;
    // The console files a bot's own conversation under bot:<id>, the
    // same address the turn runner writes to.
    const id = `bot:${botId}`;
    Promise.all([api.transcript(id), api.botSessions(botId)])
      .then(([msgs, sessions]) => {
        if (!live || activeStream.current) return;
        // Stored messages carry a sequence number and no timestamp, so
        // they cannot be placed on the same clock as queue items
        // without one. Anchoring to when the session was last written
        // and spacing backwards puts the conversation at roughly the
        // right point among the work, which is what the single
        // timeline promises. Sequence still decides their order
        // relative to each other, which is the part that must be exact.
        const anchor = sessions.find((x) => x.id === id)?.updated_at;
        const end = anchor ? new Date(anchor).getTime() : Date.now();
        const entries = msgs.map(toEntry).filter((e): e is Entry => e !== null);
        const spacing = 30_000;
        entries.forEach((e, i) => { e.at = end - (entries.length - 1 - i) * spacing; });
        setSaid(entries);
      })
      // A bot nobody has spoken to yet has no transcript, and a 404
      // here is that — not a failure worth showing.
      .catch(() => {});
    return () => {
      live = false;
      activeStream.current?.abort();
      activeStream.current = null;
    };
  }, [botId]);

  // The queue moves without you. Polling keeps the thread honest
  // rather than frozen at whatever it was when you arrived.
  useEffect(() => {
    const t = setInterval(() => { reloadWork(); reloadFeed(); }, 8000);
    return () => clearInterval(t);
  }, [reloadWork, reloadFeed]);

  // `ask` belongs in here: a confirmation can arrive taller than the
  // space left and land with its buttons below the fold, so the one
  // message that REQUIRES an action was the one you could not see.
  useEffect(() => {
    // The reduced-motion rule in the stylesheet cannot reach this:
    // `behavior` is a JS argument, and an explicit "smooth" wins over
    // any `scroll-behavior` the CSS sets. Asked directly instead.
    const still = window.matchMedia?.("(prefers-reduced-motion: reduce)").matches;
    end.current?.scrollIntoView({ behavior: still ? "auto" : "smooth" });
  }, [said, work, working, partial, ask]);

  async function send() {
    const text = draft.trim();
    if (!text || busy || activeStream.current) return;
    const controller = new AbortController();
    activeStream.current = controller;
    setNotice("");
    setSaid((p) => [...p, { kind: "said", from: "me", text, at: Date.now() }]);
    setDraft("");
    // Immediately, not when the server's heartbeat arrives. That
    // heartbeat is on a ten-second ticker and exists to hold the
    // connection open; waiting for it meant you hit Send and the
    // console sat there saying nothing for ten seconds, which reads
    // as "it didn't register my message".
    setBusy(true); setWorking(true); setSendErr(null);
    try {
      await streamBotChat(botId, text, (event, data) => {
        if (activeStream.current !== controller) return;
        if (event === "accepted") setNotice(String(data.message ?? "Message accepted by the active turn."));
        if (event === "working") setWorking(true);
        if (event === "delta") {
          setWorking(false);
          setPartial((prev) => prev + String(data.text ?? ""));
        }
        if (event === "reply") {
          setWorking(false); setAsk(null); setPartial("");
          setNotice(`${bot?.display_name || botId} replied.`);
          setSaid((p) => [...p, {
            kind: "said", from: "bot", text: String(data.text ?? ""), at: Date.now(),
            tools: (data.tools_used as string[]) ?? [],
            tokens: Number(data.tokens_used ?? 0),
            cost: Number(data.cost_usd ?? 0),
            sessionId: String(data.session_id ?? ""),
          }]);
        }
        if (event === "needs_confirmation") {
          setNotice(`${bot?.display_name || botId} needs your approval to continue.`);
          setAsk({
            id: String(data.prompt_id ?? ""),
            reason: String(data.reason ?? ""),
            action: data.action ? String(data.action) : undefined,
            resource: data.resource ? String(data.resource) : undefined,
          });
        }
        if (event === "error") {
          setWorking(false); setAsk(null); setPartial("");
          setNotice("The turn failed.");
          setSendErr(new Error(String(data.message ?? "the turn failed")));
        }
      }, controller.signal);
      if (activeStream.current === controller) reloadWork();
    } catch (e) {
      if (activeStream.current === controller && !controller.signal.aborted) setSendErr(e as Error);
    } finally {
      if (activeStream.current === controller) {
        activeStream.current = null;
        setBusy(false); setWorking(false); setPartial(""); setAsk(null);
      }
    }
  }

  if (error) return <div className="wrap"><Err error={error} /></div>;
  if (loading && !bot) return <Spinner />;
  if (!bot) return null;

  // One timeline. Queue items and spoken lines interleave by time,
  // because that is the order they happened in.
  const sentByMe = (feed ?? []).filter((i) => i.sender === `bot:${bot.id}`);
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
  ].sort((a, b) => a.at - b.at);

  return (
    <div className="chat" style={botVars(bot.id)}>
      <header className="room-head">
        <Mascot id={bot.id} size={30} dim={!bot.enabled} />
        <div className="grow">
          <div className="room-nm">
            {bot.display_name || bot.id}
            {bot.is_coordinator && <span className="tag brand">coordinator</span>}
            {!bot.enabled && <span className="tag warn">disabled</span>}
          </div>
          <div className="meta">{bot.description || `bot:${bot.id}`}</div>
        </div>
        <button className="btn ghost sm" onClick={() => setSettings((v) => !v)}>
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

      {settings ? (
        <div className="thread" tabIndex={0}><div className="thread-in">
          <Settings bot={bot} onSaved={() => { reload(); onChanged(); setSettings(false); }} />
          <Routines botId={bot.id} />
          <Knows botId={bot.id} />
        </div></div>
      ) : (
        /* tabIndex because this region scrolls. Without it the pane
           cannot take focus, so there is no way to scroll the
           transcript from the keyboard — and the :focus-visible ring
           written for it could never match. */
        <div className="thread" tabIndex={0}>
          <div className="thread-in">
            {thread.length === 0 && !working && (
              <div className="opening">
                <Mascot id={bot.id} size={56} />
                <div className="nm">{bot.display_name || bot.id}</div>
                <div className="desc">{bot.description || "Ask it something."}</div>
              </div>
            )}
            {thread.map((e, i) =>
              e.kind === "said" ? <Said key={i} e={e} bot={bot} />
              : e.kind === "sent" ? <Handoff key={`s-${e.item.id}`} item={e.item} names={names} />
              : <Work key={e.item.id} item={e.item} botId={bot.id} onChanged={reloadWork} />)}
            {ask && <Approval ask={ask} onAnswered={() => setAsk(null)} />}
            {partial && (
              <div className="msg">
                <Mascot id={bot.id} size={30} />
                <div className="grow">
                  <div className="from">{bot.display_name || bot.id}</div>
                  {/* Rendered as markdown while incomplete, so the
                      text does not reflow when the turn ends. A half
                      table looks odd either way; a paragraph that
                      suddenly re-lays-out looks broken. */}
                  {/* aria-live on the finished reply, not on this:
                      announcing a token at a time is unusable. The
                      caret is decorative and hidden. */}
                  <div className="txt" aria-busy="true">
                    <Markdown>{partial}</Markdown>
                    <span className="caret" aria-hidden="true" />
                  </div>
                </div>
              </div>
            )}
            {working && !ask && !partial && <Working bot={bot} />}
            {sendErr && <Err error={sendErr} />}
            <div ref={end} />
          </div>
        </div>
      )}

      {!settings && (
        <div className="composer">
          <div className="composer-in">
            <textarea className="ta" rows={1} value={draft}
              placeholder={`Message ${bot.display_name || bot.id}…`}
              onChange={(e) => setDraft(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); void send(); }
              }} />
            <button className="btn primary" onClick={send} disabled={busy || !draft.trim()}
              style={{ height: 44 }}>Send</button>
          </div>
        </div>
      )}
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
  return { kind: "said", from: m.role === "user" ? "me" : "bot", text, at: 0 };
}

/** Shown from the moment you hit Send until the reply lands.
 *
 * A turn against a real model runs thirty to sixty seconds — far past
 * the point where a bare animation stops reassuring and starts looking
 * hung. So it says WHO is working and HOW LONG it has been, which is
 * the difference between "still going" and "something broke".
 */
/** What a turn actually ran, printed under what it said.
 *
 * A coordinator turn reported setting a reminder it never set, and
 * nothing in the console contradicted it — the reply was the only
 * record, and a reply is a claim. These are the tools the turn really
 * invoked, so a claim and the evidence for it sit in the same place.
 *
 * Quiet by design: it should be glanceable when you are checking and
 * ignorable when you are not.
 */
function Receipt({ tools, tokens, cost }: { tools?: string[]; tokens?: number; cost?: number }) {
  const ran = tools ?? [];
  if (ran.length === 0 && !tokens) return null;
  return (
    <div className="receipt">
      {ran.length > 0 ? (
        <>
          <span className="receipt-lbl">ran</span>
          {ran.map((t) => <code key={t}>{t}</code>)}
        </>
      ) : (
        <span className="receipt-lbl">no tools used</span>
      )}
      {!!tokens && <span className="receipt-num">{tokens.toLocaleString()} tokens</span>}
      {!!cost && cost > 0 && <span className="receipt-num">${cost.toFixed(4)}</span>}
    </div>
  );
}

function Working({ bot }: { bot: Bot }) {
  const [secs, setSecs] = useState(0);
  useEffect(() => {
    const t = setInterval(() => setSecs((s) => s + 1), 1000);
    return () => clearInterval(t);
  }, []);

  return (
    <div className="msg" aria-live="polite">
      <Mascot id={bot.id} size={30} />
      <div className="grow">
        <div className="from">{bot.display_name || bot.id}</div>
        <div className="waiting">
          <span className="dots"><i /><i /><i /></span>
          <span className="waiting-txt">
            {secs < 12 ? "thinking" : secs < 40 ? "working on it" : "still working"}
          </span>
          {secs >= 5 && <span className="waiting-secs">{secs}s</span>}
        </div>
      </div>
    </div>
  );
}

function Said({ e, bot }: { e: Extract<Entry, { kind: "said" }>; bot: Bot }) {
  if (e.from === "me") {
    return <div className="msg me"><div className="bubble">{e.text}</div></div>;
  }
  return (
    <div className="msg">
      <Mascot id={bot.id} size={30} />
      <div className="grow">
        <div className="from">{bot.display_name || bot.id}</div>
        {/* A notice is our own generated sentence, not model output —
            rendering it as markdown would be pretending otherwise. */}
        {e.notice
          ? <div className="txt notice">{e.text}</div>
          : <div className="txt"><Markdown>{e.text}</Markdown></div>}
        {!e.notice && <Receipt tools={e.tools} tokens={e.tokens} cost={e.cost} />}
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

function Work({ item, botId, onChanged }: { item: InboxItem; botId: string; onChanged: () => void }) {
  const [open, setOpen] = useState(false);
  const [full, setFull] = useState<InboxItem | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<Error | null>(null);

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
    try { setFull(await api.actOnItem(botId, item.id, action)); onChanged(); }
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
    <div className={`ev ${item.status}`}>
      {/* The rail node. Status lives here as shape and colour, so the
          text beside it can be plain language. */}
      <span className="ev-node" aria-hidden="true" />
      <div className="ev-main">
        {/* A button, because it is one: a div with onClick cannot be
            reached by Tab and does not respond to Enter or Space. */}
        <button
          type="button"
          className={`ev-head${open ? " open" : ""}`}
          onClick={toggle}
          aria-expanded={open}
          aria-label={`${open ? "Collapse" : "Expand"}: ${item.subject || "untitled item"}`}
        >
          <span className="ev-ttl">{item.subject || "(no subject)"}</span>
          <span className="ev-chevron" aria-hidden="true">{open ? "▾" : "▸"}</span>
        </button>
        <div className="ev-meta">
          <span className="ev-state">{LINE[item.status] ?? item.status}</span>
          <span className="ev-dot">·</span>
          {item.sender === "operator" ? "you asked for this" : `asked by ${item.sender.replace(/^bot:/, "")}`}
          <span className="ev-dot">·</span>
          {when(item.created_at)}
          {(item.status === "failed" || item.status === "cancelled") && (
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
        {item.result && !open && <div className="ev-body"><Markdown>{item.result}</Markdown></div>}
        {item.status === "done" && (
          <Receipt tools={item.tools_used} tokens={item.tokens_used} cost={item.cost_usd} />
        )}
        {/* A failed item keeps its error, visibly. A task that vanished
            quietly is the failure the queue exists to prevent. */}
        {item.error && !open && <div className="ev-body bad">{item.error}</div>}
        {error && <Err error={error} />}
        {open && (
          <div className="ev-detail">
            <Block label="Asked" body={d.body} />
            {d.result && <Block label="Result" body={d.result} />}
            {d.error && <Block label="Error" body={d.error} bad />}
            {d.session_id && <Transcript sessionId={d.session_id} />}
          </div>
        )}
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
function Routines({ botId }: { botId: string }) {
  const { data, error, loading } = useLoad(() => api.routines(botId), [botId]);
  if (loading && !data) return null;

  return (
    <div className="panel">
      <div className="lbl">Routines</div>
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
      <button className="btn ghost sm" style={{ padding: 0 }} onClick={toggle}>
        {open ? "▾" : "▸"} what it did
      </button>
      {error && <Err error={error} />}
      {open && msgs && (
        <div className="col gap-sm" style={{ marginTop: 8, paddingLeft: 12, borderLeft: "1px solid var(--edge)" }}>
          {msgs.map((m) => (
            <div key={m.seq}>
              <div className="lbl">{m.role}{m.tool_calls ? ` · ${m.tool_calls} tool calls` : ""}</div>
              <div style={{ fontSize: 13, color: "var(--mid)", whiteSpace: "pre-wrap", marginTop: 2 }}>
                {m.content || "(tool calls only)"}
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  );
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

  async function save(patch?: Partial<Bot>) {
    setBusy(true); setError(null);
    try {
      await api.updateBot(bot.id, { revision: bot.revision, ...(patch ?? {
        display_name: f.display_name, description: f.description, instructions: f.instructions,
        tools, may_message: edges,
      }) });
      onSaved();
    } catch (e) { setError(e as Error); } finally { setBusy(false); }
  }

  return (
    <div className="card pad col gap-lg">
      <div className="field">
        <label>Display name</label>
        <input className="in" value={f.display_name} onChange={(e) => setF({ ...f, display_name: e.target.value })} />
      </div>
      <div className="field">
        <label>Description</label>
        <input className="in" value={f.description} onChange={(e) => setF({ ...f, description: e.target.value })} />
      </div>
      <div className="field">
        <label>Instructions</label>
        <textarea className="ta" rows={7} value={f.instructions}
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
      <div className="row gap-sm">
        <button className="btn primary" onClick={() => save()} disabled={busy}>Save</button>
        {/* The coordinator answers your Telegram messages and the API
            refuses to remove it, so the control is absent rather than
            present and failing. */}
        {!bot.is_coordinator && (
          <button className="btn" onClick={() => save({ enabled: !bot.enabled })} disabled={busy}>
            {bot.enabled ? "Disable" : "Enable"}
          </button>
        )}
        <div className="grow" />
        <Link to="/config"><button className="btn ghost sm">Node config →</button></Link>
      </div>
    </div>
  );
}
