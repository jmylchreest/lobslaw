import { Link, useNavigate } from "react-router-dom";
import { useState } from "react";
import { api, type Bot, type Group, type InboxItem } from "../api";
import { Mascot } from "../components/Mascot";
import { Err, Spinner, useLoad, when } from "../components/ui";
import { botVars } from "../theme";

/** Your desk.
 *
 * The console had no view of the organisation — only a conversation
 * with whoever you last clicked. That is a chat app with several
 * participants, not a company: you could see what one bot said but
 * never who reports to whom, who is busy, or what is waiting on you.
 *
 * Laid out the way somebody running a team reads it: the roster on the
 * left because that is the thing you act on, and what the team has
 * actually been doing down the right, because a desk with no activity
 * on it makes a working team look like an idle one.
 */
export function Company({ group, onRenamed }: { group?: Group; onRenamed: () => void }) {
  const nav = useNavigate();
  const { data: bots, error, loading } = useLoad(() => api.listBots());
  const { data: feed } = useLoad(() => api.activity(200));

  if (error) return <div className="wrap wide"><Err error={error} /></div>;
  if (loading && !bots) return <Spinner />;
  if (!bots) return null;

  const mine = group ? bots.filter((b) => b.group_id === group.id) : bots;
  const memberIds = new Set(mine.map((b) => b.id));
  const items = (feed ?? []).filter((item) => memberIds.has(item.recipient));
  const byBot = new Map<string, InboxItem[]>();
  for (const i of items) byBot.set(i.recipient, [...(byBot.get(i.recipient) ?? []), i]);

  const name = new Map(bots.map((b) => [b.id, b.display_name || b.id]));
  const blocked = items.filter((i) => i.status === "failed");
  const working = items.filter((i) => i.status === "claimed");
  const queued = items.filter((i) => i.status === "pending");
  // Only this team. A bot belongs to exactly one, and the desk is a
  // view of one team at a time — showing every bot would put the
  // switcher's whole purpose back in the bin.
  const lead = mine.find((b) => b.is_coordinator);
  const staff = mine.filter((b) => !b.is_coordinator);

  return (
    <div className="wrap wide">
      <div className="head">
        <div className="grow">
          <GroupName group={group} onRenamed={onRenamed} />
          <div className="sub">
            {staff.length} {staff.length === 1 ? "person" : "people"}
            {lead && <> reporting to {lead.display_name || lead.id}</>} ·{" "}
            {working.length} working · {queued.length} queued
          </div>
        </div>
        {lead && (
          <button className="btn primary" onClick={() => nav(`/bots/${lead.id}?brief=1`)}>
            Ask for a briefing
          </button>
        )}
      </div>

      <div className="desk">
        <div className="col gap">
          {blocked.length > 0 && (
            <section className="needs">
              <div className="lbl" style={{ color: "var(--bad)" }}>Needs you</div>
              <div className="col gap-sm" style={{ marginTop: 10 }}>
                {blocked.slice(0, 4).map((i) => (
                  <Link key={i.id} to={`/bots/${i.recipient}`}>
                    <div className="needs-row" style={botVars(i.recipient)}>
                      <Mascot id={i.recipient} size={22} />
                      <b>{name.get(i.recipient) ?? i.recipient}</b>
                      <span className="grow">{i.subject}</span>
                      <span className="meta">{when(i.completed_at ?? i.created_at)}</span>
                    </div>
                  </Link>
                ))}
              </div>
            </section>
          )}

          {lead && (
            <div>
              <div className="lbl">Reports to you</div>
              <Person bot={lead} work={byBot.get(lead.id) ?? []} names={name} lead />
            </div>
          )}

          <div>
            <div className="lbl">The team</div>
            <div className="staff">
              {staff.map((b) => (
                <Person key={b.id} bot={b} work={byBot.get(b.id) ?? []} names={name} />
              ))}
              <Link to="/bots/new">
                <div className="person hire">
                  <div className="plus">+</div>
                  <div>
                    <div className="person-nm">Hire someone</div>
                    <div className="meta">Give them a role and a remit.</div>
                  </div>
                </div>
              </Link>
            </div>
          </div>
        </div>

        <Ledger items={items} names={name} />
      </div>
    </div>
  );
}

/** The team's name, editable in place.
 *
 * "Your company" was a title the console chose for you, which is
 * exactly the thing to avoid on the page that is meant to feel like
 * yours. Click it and type.
 */
function GroupName({ group, onRenamed }: { group?: Group; onRenamed: () => void }) {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState("");
  const [busy, setBusy] = useState(false);

  if (!group) return <h1>Your team</h1>;
  // Somebody else's team is shown, not edited. The server refuses
  // either way; this just stops the console offering a click that
  // only ever produces an error.
  if (!group.mine) return <h1 title={`Owned by ${group.owner}`}>{group.name}</h1>;

  async function save() {
    const name = draft.trim();
    // An unchanged or empty name is a no-op, not an error: pressing
    // Enter on a name you decided not to change should just close.
    if (!name || !group || name === group.name) { setEditing(false); return; }
    setBusy(true);
    try { await api.renameGroup(group.id, name, group.revision); onRenamed(); }
    finally { setBusy(false); setEditing(false); }
  }

  if (editing) {
    return (
      <input
        className="in h1-in" autoFocus value={draft} disabled={busy}
        onChange={(e) => setDraft(e.target.value)}
        onBlur={save}
        onKeyDown={(e) => {
          if (e.key === "Enter") save();
          if (e.key === "Escape") setEditing(false);
        }}
      />
    );
  }
  return (
    <h1 className="h1-edit">
      {/* The heading stays a heading; the control inside it is a
          button, so it is reachable by keyboard and announces what it
          does rather than looking like inert text. */}
      <button
        type="button"
        className="h1-btn"
        onClick={() => { setDraft(group.name); setEditing(true); }}
        aria-label={`Rename team, currently ${group.name}`}
      >
        {group.name}
      </button>
    </h1>
  );
}

function Person({ bot, work, names, lead }: {
  bot: Bot; work: InboxItem[]; names: Map<string, string>; lead?: boolean;
}) {
  const active = work.find((w) => w.status === "claimed");
  const queued = work.filter((w) => w.status === "pending").length;
  const latest = work.find((w) => w.status === "done" || w.status === "failed");

  // "Idle" is a real and useful state. A dashboard that only shows
  // activity makes an idle team look identical to a broken one.
  const state = active
    ? { cls: "busy", text: active.subject }
    : queued > 0
      ? { cls: "queued", text: `${queued} waiting` }
      : { cls: "idle", text: "Idle" };

  return (
    <Link to={`/bots/${bot.id}`}>
      <div className={`person${lead ? " lead" : ""}`} style={botVars(bot.id)}>
        <Mascot id={bot.id} size={lead ? 42 : 36} dim={!bot.enabled} />
        <div className="grow">
          <div className="person-nm">
            {bot.display_name || bot.id}
            {!bot.enabled && <span className="tag warn">off duty</span>}
          </div>
          <div className="person-role">{bot.description || "No remit set."}</div>
          <div className={`person-state ${state.cls}`}><i />{state.text}</div>
          {latest?.result && !active && (
            <div className="person-last">{latest.result}</div>
          )}
          {bot.may_message.length > 0 && (
            // The reporting line, drawn from the message graph rather
            // than a second structure that could disagree with it.
            // Named, not just coloured: a row of 18px silhouettes is
            // decoration, and the question "who can this bot go to"
            // deserves an answer you can read.
            <div className="reports">
              <span className="meta">can brief</span>
              {bot.may_message.map((t, i) => (
                <span key={t} className="rep" style={botVars(t)}>
                  {names.get(t) ?? t}{i < bot.may_message.length - 1 ? "," : ""}
                </span>
              ))}
            </div>
          )}
        </div>
      </div>
    </Link>
  );
}

/** What the team has actually done.
 *
 * Deliberately the whole team in one column rather than per-bot tabs:
 * the useful reading is chronological, because that is where you spot
 * that engineering answered marketing's question ten minutes after it
 * was asked.
 */
function Ledger({ items, names }: { items: InboxItem[]; names: Map<string, string> }) {
  const done = items.filter((i) => i.status !== "pending").slice(0, 25);
  if (done.length === 0) {
    return (
      <aside className="ledger" tabIndex={0}>
        <div className="lbl">Recent work</div>
        <div className="empty sm"><b>Nothing yet</b><span>Assign someone a task and it shows up here.</span></div>
      </aside>
    );
  }
  return (
    <aside className="ledger" tabIndex={0}>
      <div className="lbl">Recent work</div>
      <div className="col" style={{ marginTop: 10 }}>
        {done.map((i) => (
          <Link key={i.id} to={`/bots/${i.recipient}`}>
            <div className={`led ${i.status}`} style={botVars(i.recipient)}>
              {/* A node on the rail, in the bot's colour. The mascot
                  was doing avatar duty in a list where the only
                  question is "who and when" — the colour answers it
                  in a ninth of the space. */}
              <span className="led-node" aria-hidden="true" />
              <div className="grow">
                <div className="led-top">
                  <b>{names.get(i.recipient) ?? i.recipient}</b>
                  {/* Where the work came FROM is the delegation story.
                      An item posted by another bot is a handoff, and
                      showing it is the difference between a team and
                      five things you talk to separately. */}
                  {i.sender.startsWith("bot:") && (
                    <span className="from">← {names.get(i.sender.slice(4)) ?? i.sender.slice(4)}</span>
                  )}
                  <span className="grow" />
                  <span className="meta">{when(i.completed_at ?? i.created_at)}</span>
                </div>
                <div className="led-sub">{i.subject}</div>
                {(i.result || i.error) && (
                  <div className={`led-res${i.error ? " bad" : ""}`}>{i.error || i.result}</div>
                )}
              </div>
            </div>
          </Link>
        ))}
      </div>
    </aside>
  );
}
