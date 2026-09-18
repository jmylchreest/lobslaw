import { useCallback, useEffect, useState, type ReactNode } from "react";
import { ApiError, api, isUnavailable } from "../api";
import { Err, Spinner } from "./ui";

export function LoginGate({ children }: { children: ReactNode }) {
  const [userId, setUserId] = useState<string | null>(null);
  const [needLogin, setNeedLogin] = useState(false);
  const [error, setError] = useState<Error | null>(null);

  const refresh = useCallback(() => {
    api.session()
      .then((s) => { setUserId(s.user_id); setNeedLogin(false); setError(null); })
      .catch((e: Error) => {
        setNeedLogin(true);
        setUserId(null);
        if (e instanceof ApiError && e.status === 401) {
          setError(null);
          return;
        }
        setError(e);
      });
  }, []);
  useEffect(refresh, [refresh]);
  useEffect(() => {
    function bounce() { setNeedLogin(true); setUserId(null); setError(null); }
    window.addEventListener("lobslaw:unauthorized", bounce);
    return () => window.removeEventListener("lobslaw:unauthorized", bounce);
  }, []);

  if (needLogin) return <SignIn onIn={refresh} error={error} onClearError={() => setError(null)} />;
  if (!userId) return <div className="center"><Spinner /></div>;
  return <>{children}</>;
}

function onLoopback(): boolean {
  const h = window.location.hostname;
  return h === "localhost" || h === "127.0.0.1" || h === "[::1]" || h === "::1";
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
  const local = onLoopback();
  const shown = formError || error;

  async function run(fn: () => Promise<unknown>) {
    setBusy(true); setFormError(null); onClearError();
    try { await fn(); onIn(); }
    catch (e) { setFormError(e as Error); }
    finally { setBusy(false); }
  }

  return (
    <div className="login">
      <div className="login-card">
        <div className="login-brand">
          <img src="/logo-64.png" width={48} height={48} alt="" />
          <div>
            <div className="login-name">lobslaw</div>
            <div className="hint">Sign in to this node</div>
          </div>
        </div>

        {local && (
          <button
            className="btn primary login-wide"
            disabled={busy}
            onClick={() => void run(() => api.loginLoopback())}
          >
            Continue on this computer
          </button>
        )}

        <div className="field">
          <label htmlFor="code">One-time code</label>
          <input
            id="code"
            className="in login-code"
            inputMode="numeric"
            autoComplete="one-time-code"
            autoFocus={!local}
            placeholder="000 000"
            value={code}
            onChange={(e) => setCode(e.target.value)}
            onKeyDown={(e) => { if (e.key === "Enter") void run(() => api.loginCode(code)); }}
          />
          <div className="hint">
            On the machine that runs this node, in a terminal:
            <pre className="login-cmd">lobslaw login --config /path/to/config.toml</pre>
            Type the six-digit code it prints. It works once and lasts a few minutes.
          </div>
        </div>
        <button
          className="btn primary login-wide"
          disabled={busy || !code.trim()}
          onClick={() => void run(() => api.loginCode(code))}
        >
          Sign in with code
        </button>

        {shown && (
          <div>
            {isUnavailable(shown)
              ? <div className="empty"><b>Node did not answer</b><span>Try again when it is reachable.</span></div>
              : <Err error={shown} />}
            <button className="btn ghost login-wide" type="button" onClick={() => { setFormError(null); onClearError(); }}>
              Try again
            </button>
          </div>
        )}

        <button className="btn ghost login-wide" type="button" onClick={() => setShowJwt((v) => !v)}>
          {showJwt ? "Hide token sign-in" : "I have a JWT"}
        </button>
        {showJwt && (
          <div className="field">
            <label htmlFor="jwt">JWT</label>
            <textarea
              id="jwt"
              className="ta"
              rows={3}
              value={jwt}
              autoComplete="off"
              onChange={(e) => setJwt(e.target.value.trim())}
            />
            <div className="hint">
              A Bearer token whose sub matches an enrolled [[user]]. Prefer the code above.
            </div>
            <button
              className="btn login-wide"
              disabled={busy || !jwt}
              onClick={() => void run(() => api.login(jwt))}
            >
              Sign in with JWT
            </button>
          </div>
        )}
      </div>
    </div>
  );
}
