import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import { useRegisterSW } from "virtual:pwa-register/react";

interface InstallPrompt extends Event {
  prompt(): Promise<void>;
  userChoice: Promise<{ outcome: "accepted" | "dismissed" }>;
}

const InstallContext = createContext<{
  installed: boolean;
  prompt: InstallPrompt | null;
  clear: () => void;
}>({ installed: false, prompt: null, clear: () => {} });

export function PwaProvider({ children }: { children: ReactNode }) {
  const [prompt, setPrompt] = useState<InstallPrompt | null>(null);
  const [installed, setInstalled] = useState(false);
  const [online, setOnline] = useState(navigator.onLine);
  const [registration, setRegistration] = useState<ServiceWorkerRegistration>();
  const { needRefresh: [needRefresh], updateServiceWorker } = useRegisterSW({
    onRegisteredSW(_url, value) { setRegistration(value); },
    onRegisterError(error) { console.warn("Lobslaw offline support unavailable", error); },
  });

  useEffect(() => {
    const standalone = window.matchMedia("(display-mode: standalone)");
    const checkInstalled = () => setInstalled(standalone.matches ||
      Boolean((navigator as Navigator & { standalone?: boolean }).standalone));
    const capture = (event: Event) => { event.preventDefault(); setPrompt(event as InstallPrompt); };
    const onInstalled = () => { setInstalled(true); setPrompt(null); };
    const connectivity = () => setOnline(navigator.onLine);
    checkInstalled();
    standalone.addEventListener("change", checkInstalled);
    window.addEventListener("beforeinstallprompt", capture);
    window.addEventListener("appinstalled", onInstalled);
    window.addEventListener("online", connectivity);
    window.addEventListener("offline", connectivity);
    return () => {
      standalone.removeEventListener("change", checkInstalled);
      window.removeEventListener("beforeinstallprompt", capture);
      window.removeEventListener("appinstalled", onInstalled);
      window.removeEventListener("online", connectivity);
      window.removeEventListener("offline", connectivity);
    };
  }, []);

  useEffect(() => {
    const checkUpdate = () => {
      if (navigator.onLine && document.visibilityState === "visible") void registration?.update().catch(() => {});
    };
    document.addEventListener("visibilitychange", checkUpdate);
    window.addEventListener("online", checkUpdate);
    return () => {
      document.removeEventListener("visibilitychange", checkUpdate);
      window.removeEventListener("online", checkUpdate);
    };
  }, [registration]);

  return <InstallContext.Provider value={{ installed, prompt, clear: () => setPrompt(null) }}>
    {children}
    {(!online || needRefresh) && <div className="pwa-status" role="status">
      {!online ? <span>Offline — reconnect to chat and manage tasks. Messages are not queued.</span>
        : <><span>A Lobslaw update is ready. Finish your chat and save drafts before reloading.</span>
          <button className="btn" onClick={() => void updateServiceWorker(true)}>Reload to update</button></>}
    </div>}
  </InstallContext.Provider>;
}

export function InstallApp() {
  const { installed, prompt, clear } = useContext(InstallContext);
  const [help, setHelp] = useState(false);
  const [busy, setBusy] = useState(false);
  if (installed) return null;

  async function install() {
    if (!prompt) { setHelp(!help); return; }
    setBusy(true);
    try { await prompt.prompt(); await prompt.userChoice; }
    catch { setHelp(true); }
    finally { clear(); setBusy(false); }
  }

  return <div className="pwa-install">
    <button className="btn ghost" disabled={busy} onClick={() => void install()} aria-expanded={help}>
      Install Lobslaw
    </button>
    {help && <p className="hint">
      {!window.isSecureContext
        ? "Open Lobslaw over HTTPS to install it on your phone."
        : "On iPhone or iPad, open in Safari, tap Share, then Add to Home Screen. On Android, choose Install app or Add to Home screen in your browser menu."}
    </p>}
  </div>;
}
