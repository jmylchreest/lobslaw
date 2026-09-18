import { useEffect, useRef, useState, type ReactNode } from "react";
import { LoginGate } from "./components/LoginGate";
import { Markdown } from "./components/Markdown";
import { Empty, Err, Spinner, useLoad } from "./components/ui";
import { api, isUnavailable, streamChat } from "./api";

export function App() {
  return <LoginGate><Shell /></LoginGate>;
}

function Shell() {
  const caps = useLoad(() => api.capabilities());
  if (caps.loading) return <div className="center"><Spinner /></div>;
  if (caps.error) return <DiscoveryError error={caps.error} />;
  if (!caps.data) return null;

  if (!caps.data["compute-teams"].enabled) {
    return <SingleChat computeOn={caps.data.compute.available} />;
  }
  return <TeamGate computeOn={caps.data.compute.available} />;
}

function TeamGate({ computeOn }: { computeOn: boolean }) {
  const groups = useLoad(() => api.listGroups());
  if (groups.loading) return <div className="center"><Spinner /></div>;
  if (groups.error) return <DiscoveryError error={groups.error} />;
  if (!groups.data?.length) {
    // Teams are on but none belong to this account. Chat with the
    // node's own assistant rather than inventing a default team; the
    // team chrome simply is not shown.
    return <SingleChat computeOn={computeOn} />;
  }
  return (
    <Frame>
      <Empty
        title="Teams"
        hint="This console hides team chrome until a team API exists on this node."
      />
    </Frame>
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
      <main className="main" id="main" tabIndex={-1}>{children}</main>
    </div>
  );
}

interface ChatLine {
  role: "user" | "assistant";
  text: string;
}

function SingleChat({ computeOn }: { computeOn: boolean }) {
  const [lines, setLines] = useState<ChatLine[]>([]);
  const [draft, setDraft] = useState("");
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState("");
  const [error, setError] = useState<Error | null>(null);
  const bottom = useRef<HTMLDivElement>(null);

  useEffect(() => {
    bottom.current?.scrollIntoView({ block: "end" });
  }, [lines, notice]);

  async function send() {
    const text = draft.trim();
    if (!text || busy) return;
    setDraft("");
    setError(null);
    setLines((cur) => [...cur, { role: "user", text }]);
    setBusy(true);
    setNotice("Sending");
    let reply = "";
    try {
      await streamChat(text, (event, data) => {
        if (event === "typing") setNotice("Working");
        if (event === "interim" && typeof data.text === "string") setNotice(data.text);
        if (event === "final") {
          const body = typeof data.reply === "string" ? data.reply : "";
          reply = body;
        }
        if (event === "error" && typeof data.error === "string") {
          setError(new Error(data.error));
        }
      });
      if (reply) setLines((cur) => [...cur, { role: "assistant", text: reply }]);
    } catch (e) {
      setError(e as Error);
    } finally {
      setBusy(false);
      setNotice("");
    }
  }

  return (
    <Frame>
      {!computeOn && (
        <Empty
          title="Assistant unavailable"
          hint="This node is not running compute. The console is up; chat will wait until an agent is reachable."
        />
      )}
      <div className="thread" tabIndex={0} aria-label="Conversation">
        {lines.map((line, i) => (
          <div key={i} className="msg" data-role={line.role}>
            {line.role === "assistant" ? <Markdown>{line.text}</Markdown> : line.text}
          </div>
        ))}
        {busy && <div className="msg" aria-live="polite">{notice || "Working"}</div>}
        <div ref={bottom} />
      </div>
      <div className="sr-only" role="status" aria-live="polite">{notice}</div>
      {error && (
        <div className="pad">
          {isUnavailable(error)
            ? <Empty title="Assistant unavailable" hint="The agent did not answer. It is not gone." />
            : <Err error={error} />}
        </div>
      )}
      <form
        className="composer"
        onSubmit={(e) => { e.preventDefault(); void send(); }}
      >
        <div className="composer-in">
          <label className="sr-only" htmlFor="draft">Message</label>
          <textarea
            id="draft"
            className="ta"
            rows={2}
            value={draft}
            disabled={!computeOn || busy}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && !e.shiftKey) {
                e.preventDefault();
                void send();
              }
            }}
          />
          <button className="btn primary" type="submit" disabled={!computeOn || busy || !draft.trim()}>
            Send
          </button>
        </div>
      </form>
    </Frame>
  );
}
