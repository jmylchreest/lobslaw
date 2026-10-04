import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { ApiError, api, isUnavailable } from "../api";
import { Err, Spinner } from "./ui";
import { clearLocalPush } from "../pushBinding";

export function LoginGate({ children }: { children: ReactNode }) {
  const [userId, setUserId] = useState<string | null>(null);
  const [needLogin, setNeedLogin] = useState(false);
  const [error, setError] = useState<Error | null>(null);

  const refresh = useCallback(() => {
    api.session()
      .then((s) => { setUserId(s.user_id); setNeedLogin(false); setError(null); })
      .catch((e: Error) => {
        if (e instanceof ApiError && e.status === 401) {
          setNeedLogin(true);
          setUserId(null);
          setError(null);
          return;
        }
        setError(e);
      });
  }, []);
  useEffect(refresh, [refresh]);
  useEffect(() => {
    window.addEventListener("online", refresh);
    return () => window.removeEventListener("online", refresh);
  }, [refresh]);
  useEffect(() => {
    function bounce() { void clearLocalPush().catch(() => {}); setNeedLogin(true); setUserId(null); setError(null); }
    window.addEventListener("lobslaw:unauthorized", bounce);
    return () => window.removeEventListener("lobslaw:unauthorized", bounce);
  }, []);

  if (needLogin) return <SignIn onIn={refresh} error={error} onClearError={() => setError(null)} />;
  if (!userId && error) return <div className="center"><div className="empty" role="status">
    <b>{navigator.onLine ? "Node did not answer" : "You’re offline"}</b>
    <span>Reconnect to load your conversations. Your sign-in will be checked when the node is reachable.</span>
    <button className="btn" onClick={refresh}>Retry connection</button>
  </div></div>;
  if (!userId) return <div className="center"><Spinner /></div>;
  return <>{children}</>;
}

function SignIn({ onIn, error, onClearError }: {
  onIn: () => void;
  error: Error | null;
  onClearError: () => void;
}) {
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [formError, setFormError] = useState<Error | null>(null);
  const [showJwt, setShowJwt] = useState(false);
  const [jwt, setJwt] = useState("");
  const submitting = useRef(false);
  const codeInput = useRef<HTMLInputElement>(null);
  const tokenInput = useRef<HTMLTextAreaElement>(null);
  const tokenToggle = useRef<HTMLButtonElement>(null);
  const normalizedCode = code.replace(/\s/g, "");
  const codeReady = /^\d{6}$/.test(normalizedCode);
  const shown = formError || error;

  useEffect(() => {
    if (showJwt) tokenInput.current?.focus();
  }, [showJwt]);

  async function run(fn: () => Promise<unknown>) {
    if (submitting.current) return;
    submitting.current = true;
    setBusy(true); setFormError(null); onClearError();
    try { await fn(); onIn(); }
    catch (e) { setFormError(e as Error); }
    finally { submitting.current = false; setBusy(false); }
  }

  return (
    <div className="login">
      <section className="login-card" aria-labelledby="login-title">
        <div className="login-brand">
          <img src="/logo-64.png" width={48} height={48} alt="" />
          <div>
            <div className="login-name">lobslaw</div>
            <div className="login-kicker">Your agent console</div>
          </div>
        </div>
        <div className="login-intro">
          <h1 id="login-title">Welcome back</h1>
          <p className="sub">Sign in to this node to pick up with your agents.</p>
        </div>

        <form className="login-form" aria-busy={busy} onSubmit={(e) => {
          e.preventDefault();
          if (codeReady) void run(() => api.loginCode(normalizedCode));
        }}>
          <div className="field">
            <label htmlFor="code">One-time code</label>
            <input
              id="code"
              ref={codeInput}
              className="in login-code"
              inputMode="numeric"
              autoComplete="one-time-code"
              autoFocus
              disabled={busy}
              aria-describedby="code-hint"
              placeholder="000 000"
              value={code}
              onChange={(e) => setCode(e.target.value)}
            />
            <div className="hint" id="code-hint">Enter the six-digit code from your assistant. It works once.</div>
          </div>
          <button
            className="btn primary login-wide"
            type="submit"
            disabled={busy || !codeReady}
          >
            {busy ? <><span className="btn-spinner" aria-hidden="true" />Signing in…</> : <>Sign in with code <span aria-hidden="true">→</span></>}
          </button>
        </form>

        <details className="login-help">
          <summary>Need a code?</summary>
          <div className="hint">
            Ask your assistant for a console sign-in code, or use your enrolled JWT with the CLI:
            <pre className="login-cmd">lobslaw login --config /path/to/config.toml</pre>
            Set <code>LOBSLAW_LOGIN_TOKEN</code> before running this command, then enter the code it prints.
          </div>
        </details>

        {shown && (
          <div className="login-error" role="alert">
            {isUnavailable(shown)
              ? <div className="empty"><b>Node did not answer</b><span>Try again when it is reachable.</span></div>
              : <Err error={shown} />}
            <button className="btn ghost login-wide" type="button" onClick={() => {
              setFormError(null); onClearError();
              (showJwt ? tokenInput.current : codeInput.current)?.focus();
            }}>
              Try again
            </button>
          </div>
        )}

        <div className="login-alternative">
          <button ref={tokenToggle} className="btn ghost login-wide" type="button" disabled={busy} aria-expanded={showJwt} aria-controls="token-sign-in" onClick={() => setShowJwt((v) => !v)}>
            {showJwt ? "Hide token sign-in" : "I have a JWT"}
          </button>
          {showJwt && (
            <form id="token-sign-in" className="field login-token" aria-busy={busy} onSubmit={(e) => {
              e.preventDefault();
              if (jwt) void run(() => api.login(jwt));
            }} onKeyDown={(e) => {
              if (e.key === "Escape") { setShowJwt(false); tokenToggle.current?.focus(); }
            }}>
              <label htmlFor="jwt">JWT</label>
              <textarea
                id="jwt"
                ref={tokenInput}
                className="ta"
                rows={3}
                value={jwt}
                autoComplete="off"
                spellCheck={false}
                disabled={busy}
                onChange={(e) => setJwt(e.target.value.trim())}
              />
              <div className="hint">
                A Bearer token whose sub matches an enrolled [[user]]. Prefer the code above.
              </div>
              <button
                className="btn login-wide"
                type="submit"
                disabled={busy || !jwt}
              >
                {busy ? "Signing in…" : "Sign in with JWT"}
              </button>
            </form>
          )}
        </div>
      </section>
    </div>
  );
}
