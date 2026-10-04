/* Loaded by the production service worker. Only explicit server events reach
   this handler; ordinary chat replies and quiet routine runs never send push. */
async function readPushBinding() {
  const db = await new Promise((resolve, reject) => {
    const request = indexedDB.open("lobslaw-push", 1);
    request.onupgradeneeded = () => { request.result.createObjectStore("state"); };
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error);
  });
  try {
    return await new Promise((resolve, reject) => {
      const request = db.transaction("state", "readonly").objectStore("state").get("audience");
      request.onsuccess = () => resolve(request.result);
      request.onerror = () => reject(request.error);
    });
  } finally { db.close(); }
}

self.addEventListener("push", (event) => {
  const show = async () => {
    let message = {};
    try { message = event.data?.json() || {}; } catch { /* show a generic notice */ }
    let binding;
    try {
      binding = await readPushBinding();
    } catch { /* missing/unreadable binding must never expose private payloads */ }
    if (!self.navigator.locks || !binding || binding.id !== message.audience || Date.parse(binding.expires) <= Date.now() || !Number.isFinite(Date.parse(binding.expires))) {
      await self.registration.showNotification("Lobslaw", {
        body: "Open Lobslaw to view your updates.", icon: "/logo-512.png",
        tag: "lobslaw-session-changed", data: { url: "/" },
      });
      return;
    }
    let icon = "/logo-512.png";
    if (typeof message.icon === "string" && message.icon.startsWith("/__agent-icons/")) {
      const cached = await (await caches.open("lobslaw-agent-icons")).match(message.icon);
      if (cached) {
        const candidate = await cached.text();
        if (candidate.startsWith("data:image/png;base64,")) icon = candidate;
      }
    }
    const target = new URL(typeof message.url === "string" ? message.url : "/", self.location.origin);
    const safeURL = target.origin === self.location.origin && /^\/(bots|approvals)\//.test(target.pathname)
      ? target.pathname + target.search : "/";
    await self.registration.showNotification(String(message.title || "Lobslaw").slice(0, 150), {
      body: String(message.body || "Open Lobslaw to view the update.").slice(0, 400),
      icon, badge: "/logo-64.png", tag: String(message.id || "lobslaw-update"),
      data: { url: safeURL },
    });
  };
  event.waitUntil(self.navigator.locks ? self.navigator.locks.request("lobslaw-push-audience", show) : show());
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  event.waitUntil((async () => {
    const target = new URL(event.notification.data?.url || "/", self.location.origin);
    const url = target.origin === self.location.origin ? target.href : self.location.origin + "/";
    const windows = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
    const existing = windows.find((client) => new URL(client.url).origin === self.location.origin);
    if (existing) { await existing.navigate(url); await existing.focus(); }
    else await self.clients.openWindow(url);
  })());
});
