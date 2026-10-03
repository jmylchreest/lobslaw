/* Loaded by the production service worker. Only explicit server events reach
   this handler; ordinary chat replies and quiet routine runs never send push. */
self.addEventListener("push", (event) => {
  event.waitUntil((async () => {
    let message = {};
    try { message = event.data?.json() || {}; } catch { /* show a generic notice */ }
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
  })());
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
