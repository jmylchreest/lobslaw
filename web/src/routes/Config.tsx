import { api } from "../api";
import { Page } from "../App";
import { Err, Spinner, useLoad } from "../components/ui";

/** What this node believes about itself.
 *
 * Read-only. Editing configuration from a browser would mean a running
 * node rewriting its own config.toml, and every "this section needs a
 * restart" caveat becomes a race somebody triggers by clicking Save.
 *
 * The node assembles this from an allowlist rather than dumping its
 * config, so what is absent is absent deliberately — endpoints and
 * credentials among it.
 */
export function Config() {
  const { data, error, loading } = useLoad(() => api.config());

  if (error) return <Page title="Config"><Err error={error} /></Page>;
  if (loading && !data) return <Spinner />;
  if (!data) return null;

  const g = data.gateway;
  const exposed = !g.require_auth && g.bind_address !== "127.0.0.1" && g.bind_address !== "localhost";

  return (
    <Page title="Config"
      sub={`${data.node_id}${data.version ? ` · ${data.version}` : ""} · ${data.functions.join(", ")}`}>
      {exposed && (
        <div className="err" style={{ marginBottom: 14 }}>
          <b>This console is unauthenticated</b>
          <div className="hint">
            Anyone who can reach {g.bind_address || "this node"} can rewrite your bots.
            Set <code>[auth] require_auth</code> and <code>[gateway.ui] token_ref</code>,
            or bind 127.0.0.1.
          </div>
        </div>
      )}

      <div className="grid2">
        <Card title="Gateway">
          <KV k="Bind" v={g.bind_address || "every interface"} />
          <KV k="Port" v={String(g.http_port)} />
          <KV k="Requires auth" v={g.require_auth ? "yes" : "no"} warn={!g.require_auth} />
          <KV k="Console login" v={g.login_configured ? "configured" : "none"} />
          <KV k="Queue mode" v={g.queue_mode || "serial"} />
          <KV k="Timezone" v={g.default_timezone || "UTC"} />
        </Card>
        <Card title="Bots">
          <KV k="Queue depth limit" v={String(data.bots.max_pending)} />
          <KV k="Bots work their queues" v={data.bots.drain_enabled ? "yes" : "no"}
            warn={!data.bots.drain_enabled} />
        </Card>
        <Card title="Memory">
          <KV k="Enabled" v={data.memory.enabled ? "yes" : "no"} />
          <KV k="Dream schedule" v={data.memory.dream_schedule || "default"} />
          {/* The vector space the corpus is in — the one thing an
              operator debugging bad recall actually needs. */}
          <KV k="Embedding model" v={data.memory.embedding_model || "none — lexical recall"} />
        </Card>
        <Card title="Compute">
          <KV k="Tool calls per turn" v={String(data.compute.max_tool_calls_per_turn)} />
          <KV k="Self-learning" v={data.compute.self_learning_mode || "off"} />
          <KV k="Channels" v={data.channels.map((c) => c.type).join(", ") || "none"} />
        </Card>
      </div>

      {data.compute.providers.length > 0 && (
        <div style={{ marginTop: 30 }}>
          <div className="lbl">Providers</div>
          <div className="sub" style={{ marginBottom: 12 }}>
            Labels and roles only — no endpoints, no model names, no credentials.
          </div>
          <div className="feed">
            {data.compute.providers.map((p) => (
              <div key={p.label} className="card pad row" style={{ justifyContent: "space-between" }}>
                <span className="row gap-sm">
                  <b className="mono" style={{ fontSize: 13 }}>{p.label}</b>
                  {p.trust_tier && <span className="meta">{p.trust_tier}</span>}
                </span>
                <span className="row gap-sm">
                  {(p.roles ?? []).length === 0
                    ? <span className="meta">council only</span>
                    : p.roles!.map((r) => (
                        <span key={r} className="tag" style={{ color: "var(--brand)" }}>{r}</span>
                      ))}
                </span>
              </div>
            ))}
          </div>
        </div>
      )}
    </Page>
  );
}

function Card({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="card pad">
      <div className="lbl" style={{ marginBottom: 12 }}>{title}</div>
      <div className="col">{children}</div>
    </div>
  );
}

function KV({ k, v, warn }: { k: string; v: string; warn?: boolean }) {
  return (
    <div className="kv">
      <span>{k}</span>
      <span style={warn ? { color: "var(--warn)" } : undefined}>{v}</span>
    </div>
  );
}
