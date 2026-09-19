import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { api, type Group } from "../api";
import { Page } from "../App";
import { Err } from "../components/ui";
import { Mascot } from "../components/Mascot";
import { botVars } from "../theme";

export function NewBot({ group, onCreated }: { group?: Group; onCreated: () => void }) {
  const nav = useNavigate();
  const [f, setF] = useState({ id: "", display_name: "", description: "", instructions: "" });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<Error | null>(null);

  async function submit() {
    setBusy(true); setError(null);
    try {
      const bot = await api.createBot({
        group_id: group?.id,
        id: f.id.trim(), display_name: f.display_name.trim() || f.id.trim(),
        description: f.description.trim(), instructions: f.instructions.trim(),
      });
      onCreated();
      nav(`/bots/${bot.id}`);
    } catch (e) { setError(e as Error); } finally { setBusy(false); }
  }

  return (
    <Page title="New bot" sub={group ? `A specialist for ${group.name}.` : "A specialist with its own memory, tools and queue."}>
      <div className="card pad col gap-lg" style={{ maxWidth: 620, ...botVars(f.id || "new") }}>
        {/* The avatar updates as they type. It is the fastest way to
            convey that a bot is an identity rather than a row. */}
        <div className="row gap">
          <Mascot id={f.id || "new"} size={46} />
          <div>
            <div style={{ fontWeight: 600, fontSize: 16 }}>{f.display_name || f.id || "Unnamed"}</div>
            <div className="meta mono">bot:{f.id || "…"}</div>
          </div>
        </div>

        <div className="field">
          <label>Id</label>
          <input className="in mono" value={f.id} placeholder="engineering"
            onChange={(e) => setF({ ...f, id: e.target.value.toLowerCase().replace(/[^a-z0-9-]/g, "") })} />
          <div className="hint">Lowercase, hyphens allowed. Becomes the bot's identity and cannot be changed.</div>
        </div>
        <div className="field">
          <label>Display name</label>
          <input className="in" value={f.display_name} placeholder="Engineering"
            onChange={(e) => setF({ ...f, display_name: e.target.value })} />
        </div>
        <div className="field">
          <label>Description</label>
          <input className="in" value={f.description} placeholder="Builds and ships the product."
            onChange={(e) => setF({ ...f, description: e.target.value })} />
          <div className="hint">One line. Other bots read this to decide who to hand work to.</div>
        </div>
        <div className="field">
          <label>Instructions</label>
          <textarea className="ta" rows={6} value={f.instructions}
            placeholder="You are the engineer for XYZ. You own the build, the deploy pipeline and the cluster."
            onChange={(e) => setF({ ...f, instructions: e.target.value })} />
          <div className="hint">Its standing brief — the role, not a task. Rides on every turn it takes.</div>
        </div>

        {error && <Err error={error} />}

        <div className="row gap-sm">
          <button className="btn primary" onClick={submit} disabled={busy || !f.id || !f.instructions}>
            Create
          </button>
          <span className="meta">Starts with every tool this node has. Narrow that on its page.</span>
        </div>
      </div>
    </Page>
  );
}
