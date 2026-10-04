import { useEffect, useRef, useState } from "react";
import { api } from "../api";
import { setRoster } from "../theme";
import { mascotSVG } from "./Mascot";
import { clearLocalPush, pushBindingEpoch, savePushBinding } from "../pushBinding";

async function cacheAgentIcons() {
  const bots = await api.listBots();
  setRoster(bots.map((b) => b.id));
  const cache = await caches.open("lobslaw-agent-icons");
  for (const bot of bots) {
    const image = new Image();
    image.src = "data:image/svg+xml;charset=utf-8," + encodeURIComponent(mascotSVG(bot.id));
    await image.decode();
    const canvas = document.createElement("canvas"); canvas.width = 192; canvas.height = 192;
    canvas.getContext("2d")?.drawImage(image, 0, 0);
    await cache.put(`/__agent-icons/${encodeURIComponent(bot.id)}`, new Response(canvas.toDataURL("image/png")));
  }
}

export function PushControls({ userId }: { userId: string }) {
  const supported = window.isSecureContext && "serviceWorker" in navigator && "PushManager" in window && "Notification" in window;
  const [enabled, setEnabled] = useState(false);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState("");
  const operation = useRef(0);
  useEffect(() => {
    if (!supported || !userId || userId === "anon") return;
    let live = true;
    const version = ++operation.current;
    async function renew() {
      const reg = await navigator.serviceWorker.ready;
      const sub = await reg.pushManager.getSubscription();
      if (localStorage.getItem("lobslaw:push-owner") !== userId) return;
      if (!sub) return;
      const epoch = await pushBindingEpoch();
      const binding = await api.subscribePush(sub.toJSON());
      if (!live || version !== operation.current) return;
      if (binding.user_id !== userId) throw new Error("Your sign-in changed; reload Lobslaw.");
      if (!await savePushBinding(binding.binding_id, binding.expires_at, epoch)) return;
      setEnabled(true);
      void cacheAgentIcons().catch(() => {});
    }
    void renew().catch((error) => { if (live) setMessage(error.message); });
    return () => { live = false; operation.current++; };
  }, [supported, userId]);
  if (!userId || userId === "anon") return null;

  async function toggle() {
    if (!supported) {
      setMessage(!window.isSecureContext ? "Notifications require HTTPS." : "On iPhone/iPad, install Lobslaw on your Home Screen, then open it there to enable notifications (iOS 16.4+).");
      return;
    }
    setBusy(true); setMessage("");
    const version = ++operation.current;
    try {
      if (!enabled && await Notification.requestPermission() !== "granted") throw new Error("Notifications are blocked. You can change this in your browser or device settings.");
      const reg = await navigator.serviceWorker.ready;
      let sub = await reg.pushManager.getSubscription();
      if (enabled) {
        await clearLocalPush();
        setEnabled(false);
        if (sub) await api.unsubscribePush(sub.endpoint);
      } else {
        const epoch = await pushBindingEpoch();
        const config = await api.pushConfig();
        const key = Uint8Array.from(atob(config.public_key.replace(/-/g, "+").replace(/_/g, "/")), (c) => c.charCodeAt(0));
        sub ??= await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: key });
        const binding = await api.subscribePush(sub.toJSON());
        if (version !== operation.current) return;
        if (binding.user_id !== userId) throw new Error("Your sign-in changed; reload Lobslaw.");
        if (!await savePushBinding(binding.binding_id, binding.expires_at, epoch)) return;
        localStorage.setItem("lobslaw:push-owner", userId); setEnabled(true);
        void cacheAgentIcons().catch(() => {});
      }
    } catch (error) { setMessage((error as Error).message); }
    finally { setBusy(false); }
  }

  return <div className="pwa-install">
    <button className="btn ghost" disabled={busy} onClick={() => void toggle()}>{enabled ? "Disable notifications" : "Enable notifications"}</button>
    {enabled && <p className="hint">Attention requests and outcomes you asked for. This device only.</p>}
    {message && <p className="hint" role="status">{message}</p>}
  </div>;
}
