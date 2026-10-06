import { useEffect, useRef, useState, type ReactNode } from "react";
import { NavLink, Navigate, Route, Routes, useLocation } from "react-router-dom";
import { api, isUnavailable, type Bot, type Group, type InboxItem } from "./api";
import { LoginGate } from "./components/LoginGate";
import { InstallApp } from "./components/Pwa";
import { PushControls } from "./components/PushControls";
import { Markdown } from "./components/Markdown";
import { Mascot } from "./components/Mascot";
import { Empty, Err, Spinner, useLoad, when } from "./components/ui";
import { Approval, BotRoom } from "./routes/BotRoom";
import { setRoster } from "./theme";
import { Company } from "./routes/Company";
import { Config } from "./routes/Config";
import { NewBot } from "./routes/NewBot";
import { TaskApprovals } from "./routes/TaskApprovals";
import { LearnedReviews, LearnedReviewNotice } from "./routes/LearnedReviews";
import { FeedbackProvider, NavigationMarker, switchView, useFeedback } from "./components/Motion";
import { ChatSessionsProvider, useBotChat, useChatSessions } from "./components/ChatSessions";
import { Composer } from "./components/Composer";
import { BotDirectory } from "./components/BotDirectory";

export function App() {
  return <FeedbackProvider><LoginGate><ChatSessionsProvider><Console /></ChatSessionsProvider></LoginGate></FeedbackProvider>;
}

/** Console decides which console this node can render.
 *
 * The full team console needs the bot and team registries, which are an
 * opt-in node function. A node without them is not broken — it is a
 * node whose assistant has no team — so it gets the chat surface
 * rather than a wall of 404s.
 */
function Console() {
  const caps = useLoad(() => api.capabilities());
  if (caps.loading) return <div className="center"><Spinner /></div>;
  if (caps.error) return <><DiscoveryError error={caps.error} /><button className="btn" onClick={caps.reload}>Retry discovery</button></>;
  if (!caps.data) return null;

  if (!caps.data["compute-teams"].enabled) {
    return <Routes>
      <Route path="/learned" element={<Frame><LearnedReviews /></Frame>} />
      <Route path="*" element={<SingleChat computeOn={caps.data.compute.available} />} />
    </Routes>;
  }
  return <Shell />;
}

/** The sidebar is a conversation list, not a roster.
 *
 * Every row carries the bot's most recent line. That is what makes a
 * sidebar feel populated rather than like a nav menu, and it answers
 * "what is everyone up to" before you click anything — the first
 * version listed five names and left the question to a separate page.
 */
function Shell() {
  const { sessions: chats } = useChatSessions();
  const { data: bots, reload } = useLoad(() => api.listBots());
  const { data: groups, reload: reloadGroups } = useLoad(() => api.listGroups());
  // Which team you are looking at. Kept in the URL-less shell state
  // rather than the path because it scopes a whole session's view, and
  // a link to a bot should open that bot whichever team you were in.
  const [groupId, setGroupId] = useState<string>("");
  // One activity call feeds every preview. Per-bot requests would be
  // N round-trips for a sidebar that is not the point of the page.
  const { data: feed, reload: reloadFeed } = useLoad(() => api.activity(120));
  const { pathname } = useLocation();
  // On a phone the sidebar is a drawer. It starts closed, and it closes
  // itself on navigation — a drawer you have to dismiss by hand after
  // every tap is worse than no drawer.
  const [nav, setNav] = useState(false);
  const menuButton = useRef<HTMLButtonElement>(null);
  const sidebar = useRef<HTMLElement>(null);
  useEffect(() => {
    const timer = setInterval(reloadFeed, 8000);
    return () => clearInterval(timer);
  }, [reloadFeed]);
  useEffect(() => { setNav(false); }, [pathname]);
  useEffect(() => {
    if (!nav) return;
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key === "Escape") { setNav(false); menuButton.current?.focus(); }
    };
    window.addEventListener("keydown", closeOnEscape);
    return () => window.removeEventListener("keydown", closeOnEscape);
  }, [nav]);

  // Colours are assigned across the whole roster rather than per id,
  // so two bots cannot render the same hue. Seeded here because this
  // is the one component that always has the full list.
  if (bots) setRoster(bots.map((b) => b.id));

  const latest = new Map<string, InboxItem>();
  for (const item of feed ?? []) {
    if (!latest.has(item.recipient)) latest.set(item.recipient, item);
  }

  const refresh = () => { reload(); reloadFeed(); reloadGroups(); };
  useEffect(() => {
    const completed = () => { reloadFeed(); reload(); };
    window.addEventListener("lobslaw:chat-completed", completed);
    return () => window.removeEventListener("lobslaw:chat-completed", completed);
  }, [reloadFeed, reload]);

  // Default to the team the server marks as default, so the console
  // opens on whichever one a channel would reach.
  const current = groups?.find((g) => g.id === groupId)
    ?? groups?.find((g) => g.is_default)
    ?? groups?.[0];
  const mine = (bots ?? []).filter((b) => !current || b.group_id === current.id);

  const here = bots?.find((b) => pathname === `/bots/${b.id}`);

  return (
    <BotDirectory bots={bots}><div className={`shell${nav ? " nav-open" : ""}`}>
      {/* First in the tab order, invisible until focused. The roster
          below is a tab stop per bot, on every page. */}
      <a className="skip" href="#main">Skip to content</a>

      {/* Only rendered on small screens. It carries the current
          location as well as the toggle, because once the sidebar is
          hidden there is nothing else saying which bot you are in. */}
      <div className="topbar">
        <button ref={menuButton} className="burger" onClick={() => setNav(true)} aria-label="Open menu" aria-expanded={nav} aria-controls="sidebar">
          <i /><i /><i />
        </button>
        <div className="topbar-nm">
          {here ? (here.display_name || here.id)
            : pathname === "/config" ? "Config"
            : pathname === "/bots/new" ? "Hire someone"
            : pathname.startsWith("/approvals/") ? "Task details"
            : pathname === "/approvals" ? "Task approvals"
            : pathname === "/learned" ? "Learned reviews"
            : current?.name ?? "Overview"}
        </div>
      </div>

      {/* Tapping away closes the drawer. Rendered rather than relying
          on a body listener so it cannot outlive the open state. */}
      <div className="scrim" onClick={() => setNav(false)} />

      <aside ref={sidebar} className="side" id="sidebar" aria-label="Main navigation">
        <NavigationMarker container={sidebar} location={pathname} layoutKey={`${current?.id}:${mine.length}:${nav}`} />
        <div className="brand">
          <img src="/logo-64.png" alt="" width={26} height={26} />
          lobslaw
          <button className="nav-close btn ghost" aria-label="Close menu" onClick={() => { setNav(false); menuButton.current?.focus(); }}>×</button>
        </div>

        <NavLink to="/" end className={`desknav${pathname === "/" ? " on" : ""}`}>
          <span className="deskicon">◆</span>
          <div className="txt"><div className="nm">Overview</div></div>
        </NavLink>

        {groups && groups.length > 0 && (
          <GroupPicker
            groups={groups}
            current={current}
            onPick={(id) => switchView(() => setGroupId(id))}
            onChanged={() => { reloadGroups(); reload(); }}
          />
        )}

        <h6>Team</h6>
        <div className="roster roster-pane">
          {mine.map((b) => {
            const on = pathname === `/bots/${b.id}`;
            const item = latest.get(b.id);
            return (
              <NavLink key={b.id} to={`/bots/${b.id}`}
                className={`${on ? "on" : ""} ${b.enabled ? "" : "off"}`}>
                <Mascot id={b.id} size={30} dim={!b.enabled} working={!!chats[b.id]?.busy || (feed ?? []).some((entry) => entry.recipient === b.id && entry.status === "claimed")} />
                <div className="txt">
                  <div className="nm">
                    <span style={{ overflow: "hidden", textOverflow: "ellipsis" }}>
                      {b.display_name || b.id}
                    </span>
                    {b.is_coordinator && <i className="lead" title="coordinator" />}
                  </div>
                  <div className="last">
                    {chats[b.id]?.ask ? "Needs your approval" : chats[b.id]?.busy ? "Writing a reply…" : chats[b.id]?.messages.at(-1)?.text || (item
                      ? `${item.result || item.error || item.subject}`
                      : b.description || "Nothing yet")}
                  </div>
                </div>
              </NavLink>
            );
          })}
          <NavLink to="/bots/new" className="newbot">
            <span className="plus">+</span>
            <div className="txt"><div className="nm">New bot</div></div>
          </NavLink>
        </div>

        <div className="side-foot">
          <LearnedReviewNotice />
          <NavLink to="/approvals" className={({ isActive }) => isActive ? "on" : ""}>Task approvals</NavLink>
          <NavLink to="/config" className={({ isActive }) => isActive ? "on" : ""}>Config</NavLink>
          <Signed />
        </div>
      </aside>

      <main className="main" id="main" tabIndex={-1}>
        <Routes>
          {/* Chat-first: opening the console lands you in a
              conversation with the coordinator, and clicking a bot
              opens ITS conversation. There is no separate activity
              page — a bot's queue is woven into its own thread, where
              the ordering against what you said still means
              something. */}
          {/* The desk is the landing surface. Opening straight into a
              conversation answered "what did the last bot say" — useful,
              but not the question somebody running a team opens with.
              That one is "what is everyone doing and what needs me",
              and it has nowhere to live in a list of chats. Chat is one
              click away and the sidebar never leaves. */}
          <Route path="/" element={<Company group={current} onRenamed={reloadGroups} />} />
          <Route path="/coordinator" element={<Landing bots={bots} />} />
          <Route path="/config" element={<Config />} />
          <Route path="/approvals" element={<TaskApprovals />} />
          <Route path="/approvals/:taskId" element={<TaskApprovals />} />
          <Route path="/learned" element={<LearnedReviews />} />
          <Route path="/bots/new" element={<NewBot group={current} onCreated={refresh} />} />
          <Route path="/bots/:botId" element={<BotRoom onChanged={refresh} />} />
          <Route path="*" element={<div className="empty"><b>Nothing here</b><span>That page does not exist.</span></div>} />
        </Routes>
      </main>
    </div></BotDirectory>
  );
}

/** Who is signed in.
 *
 * Worth a line now that the console can tell people apart: with one
 * shared token every session was the same anonymous subject, and the
 * question "who changed this bot's brief" had no answer to show.
 * Silent for the anonymous case, which genuinely is nobody in
 * particular.
 */
function Signed() {
  const { data } = useLoad(() => api.session());
  const id = data?.user_id ?? "";
  return <><InstallApp /><PushControls userId={id} />{id && id !== "anon" && <div className="signed">{id.startsWith("user:") ? id.slice("user:".length) : id}</div>}</>;
}

/** The team switcher.
 *
 * Sits above the roster rather than in a settings page because
 * switching teams changes everything below it — which people you see,
 * whose work the desk shows — and a control that changes the whole
 * view belongs next to the view.
 *
 * Hidden entirely when there is one team: a picker with a single
 * option is a control that only ever teaches you it does nothing.
 */
function GroupPicker({ groups, current, onPick, onChanged }: {
  groups: Group[];
  current?: Group;
  onPick: (id: string) => void;
  onChanged: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [adding, setAdding] = useState(false);
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);
  const notify = useFeedback();

  async function create() {
    const label = name.trim();
    if (!label) return;
    setBusy(true);
    try {
      // The id is derived once, from the name you typed, and then
      // never changes — renaming a team must not rewrite every bot's
      // group_id.
      const id = label.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "").slice(0, 60);
      await api.createGroup(id || `team-${groups.length + 1}`, label);
      setName(""); setAdding(false); setOpen(false); onChanged(); notify("Team created");
    } finally { setBusy(false); }
  }

  if (groups.length <= 1 && !adding) {
    return (
      <div className="gp">
        <button className="gp-cur" onClick={() => setAdding(true)}>
          <span className="gp-nm">{current?.name ?? "Your team"}</span>
          <span className="gp-add">+ team</span>
        </button>
      </div>
    );
  }

  return (
    <div className="gp">
      <button className="gp-cur" onClick={() => setOpen((v) => !v)}>
        <span className="gp-nm">{current?.name ?? "Pick a team"}</span>
        <span className="gp-caret">{open ? "▴" : "▾"}</span>
      </button>
      {open && (
        <div className="gp-menu">
          {groups.map((g) => (
            <button
              key={g.id}
              className={`gp-item${g.id === current?.id ? " on" : ""}`}
              onClick={() => { onPick(g.id); setOpen(false); }}
            >
              <span className="grow">{g.name}</span>
              <span className="meta">{g.bots}</span>
            </button>
          ))}
          <button className="gp-item new" onClick={() => setAdding(true)}>+ New team</button>
        </div>
      )}
      {adding && (
        <div className="gp-new">
          <input
            className="in" autoFocus placeholder="Team name"
            value={name} onChange={(e) => setName(e.target.value)}
            onKeyDown={(e) => { if (e.key === "Enter") create(); if (e.key === "Escape") setAdding(false); }}
          />
          <button className="btn primary sm" onClick={create} disabled={busy || !name.trim()}>Create</button>
        </div>
      )}
    </div>
  );
}

/** Landing sends you straight to the coordinator, or to the first bot
 * on a node that somehow has no coordinator. Waiting on the roster
 * before redirecting avoids a flash of "nothing here" on first load.
 */
function Landing({ bots }: { bots: Bot[] | null }) {
  if (!bots) return <Spinner />;
  const first = bots.find((b) => b.is_coordinator) ?? bots[0];
  if (!first) return <Navigate to="/bots/new" replace />;
  return <Navigate to={`/bots/${first.id}`} replace />;
}

/** The common frame: a title, an optional action, and a body. Repeated
 * per-screen padding is how a console drifts into looking like several
 * applications. */
export function Page({ title, sub, action, children }: {
  title: React.ReactNode; sub?: React.ReactNode;
  action?: React.ReactNode; children: React.ReactNode;
}) {
  return (
    <div className="wrap">
      <div className="head">
        <div className="grow">
          <h1>{title}</h1>
          {sub && <div className="sub">{sub}</div>}
        </div>
        {action}
      </div>
      {children}
    </div>
  );
}

function DiscoveryError({ error }: { error: Error }) {
  if (isUnavailable(error)) {
    return (
      <Frame>
        <Empty
          title="Console unavailable"
          hint="The node did not answer. It is not gone — try again when it is reachable."
        />
      </Frame>
    );
  }
  return (
    <div className="center">
      <div style={{ maxWidth: 420, width: "100%", padding: 16 }}>
        <Err error={error} />
        <button className="btn ghost login-wide" type="button" onClick={() => window.location.reload()}>
          Try again
        </button>
      </div>
    </div>
  );
}

function Frame({ children }: { children: ReactNode }) {
  return (
    <div className="shell">
      <a className="skip" href="#main">Skip to content</a>
      <aside className="side">
        <div className="brand">
          <img src="/logo-64.png" alt="" width={26} height={26} />
          lobslaw
        </div>
      </aside>
      <main className="main assistant-main" id="main" tabIndex={-1}>
        <nav className="assistant-nav" aria-label="Assistant navigation"><NavLink to="/">Chat</NavLink> · <LearnedReviewNotice /></nav>
        {children}
      </main>
    </div>
  );
}

/** SingleChat is the console on a node with no team registry.
 *
 * One conversation with the node's own assistant, streamed to the same
 * /v1/messages endpoint Telegram and Slack use.
 */
function SingleChat({ computeOn }: { computeOn: boolean }) {
  const { session, send: sendMessage, stop, answered, draft: setDraft } = useBotChat("");
  const { messages: lines, draft, busy, notice, error, ask, reconnecting } = session;
  const bottom = useRef<HTMLDivElement>(null);

  useEffect(() => {
    bottom.current?.scrollIntoView({ block: "end" });
  }, [lines, notice]);

  const send = () => void sendMessage("", draft.trim(), "Assistant");

  return (
    <Frame>
      <div className="chat single-chat">
      {reconnecting && <div className="chat-reconnecting" role="status">Reconnecting… your reply continues on the server.</div>}
      {!computeOn && (
        <Empty
          title="Assistant unavailable"
          hint="This node is not running compute. The console is up; chat will wait until an agent is reachable."
        />
      )}
      <div className="thread" tabIndex={0} aria-label="Conversation"><div className="thread-in">
        {lines.map((line, i) => (
          <div key={i} className={`msg message-arrival${line.from === "me" ? " me" : ""}`} data-role={line.from}>
            {line.from === "bot" ? <><Mascot id="assistant" size={30} /><div className="grow txt"><Markdown>{line.text}</Markdown></div></> : <div className="bubble">{line.text}</div>}
          </div>
        ))}
        {busy && <div className="msg message-arrival"><Mascot id="assistant" size={30} working /><div className="waiting"><span className="dots" aria-hidden="true"><i /><i /><i /></span>{notice || "Working"}</div></div>}
        {ask && <Approval ask={ask} onAnswered={() => answered("")} />}
        <div ref={bottom} />
      </div></div>
      <div className="sr-only" role="status" aria-live="polite">{notice}</div>
      {error && (
        <div className="pad">
          {isUnavailable(error)
            ? <Empty title="Assistant unavailable" hint="The agent did not answer. It is not gone." />
            : <Err error={error} />}
        </div>
      )}
      <Composer draft={draft} onChange={(value) => setDraft("", value)} onSend={send} busy={busy} onStop={() => stop("")} disabled={!computeOn} />
      </div>
    </Frame>
  );
}

export { when };
