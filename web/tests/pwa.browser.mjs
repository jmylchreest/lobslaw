import assert from "node:assert/strict";
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import { chromium } from "playwright";

// Exercise the production worker, including offline navigation and its waiting
// update lifecycle. API responses come from the server, not Playwright routes.
const dist = new URL("../../internal/gateway/ui/dist/", import.meta.url);
let revision = 1;
let sessionStatus = 401;
const server = createServer(async (req, res) => {
  const path = new URL(req.url, "http://localhost").pathname;
  if (path.startsWith("/v1/")) {
    res.writeHead(sessionStatus, { "Content-Type": "application/json", "Cache-Control": "no-store" });
    res.end(JSON.stringify({ error: "sign in required" }));
    return;
  }
  try {
    const file = path.includes(".") ? path.slice(1) : "index.html";
    let body = await readFile(new URL(file, dist));
    if (path === "/sw.js") body = Buffer.concat([body, Buffer.from(`\n// deployment ${revision}\n`)]);
    const ext = file.split(".").at(-1);
    const types = { js: "text/javascript", css: "text/css", html: "text/html", png: "image/png", webmanifest: "application/manifest+json", ico: "image/x-icon" };
    res.writeHead(200, { "Content-Type": types[ext] || "application/octet-stream", "Cache-Control": "no-cache" });
    res.end(body);
  } catch { res.writeHead(404); res.end(); }
});
await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
const origin = `http://127.0.0.1:${server.address().port}`;
let browser;
try {
  browser = await chromium.launch({ executablePath: process.env.CHROME_BIN || "/usr/bin/google-chrome", args: ["--no-sandbox"] });
  const context = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true });
  const page = await context.newPage();
  await context.grantPermissions(["notifications"], { origin });
  const cdp = await context.newCDPSession(page);
  let registrationID;
  cdp.on("ServiceWorker.workerRegistrationUpdated", ({ registrations }) => {
    registrationID = registrations.find((r) => r.scopeURL === origin + "/")?.registrationId || registrationID;
  });
  await cdp.send("ServiceWorker.enable");
  page.setDefaultTimeout(15000);
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.goto(origin);
  await page.getByLabel("One-time code").waitFor();
  const manifest = await (await fetch(`${origin}/manifest.webmanifest`)).json();
  assert.equal(manifest.display, "standalone");
  assert.equal(manifest.name, "Lobslaw");
  for (const icon of manifest.icons) assert.equal((await fetch(origin + icon.src)).status, 200);
  await page.evaluate(() => navigator.serviceWorker.ready);
  await page.reload();
  await page.waitForFunction(() => navigator.serviceWorker.controller !== null);
  assert.ok(registrationID, "service worker registration missing");
  const worker = context.serviceWorkers()[0];
  await worker.evaluate(() => {
    self.pushTestMessages = [];
    const show = self.registration.showNotification.bind(self.registration);
    self.registration.showNotification = async (title, options) => {
      self.pushTestMessages.push({ title, ...options });
      return show(title, options);
    };
  });
  const sendPush = (message) => cdp.send("ServiceWorker.deliverPushMessage", { origin, registrationId: registrationID, data: JSON.stringify(message) });
  const iconData = "data:image/png;base64," + (await readFile(new URL("logo-64.png", dist))).toString("base64");
  await page.evaluate(async (data) => { await (await caches.open("lobslaw-agent-icons")).put("/__agent-icons/tester", new Response(data)); }, iconData);
  await sendPush({ id: "attention-1", title: "Tester needs your attention", body: "Review the proposed operation", url: "/approvals/task-1", icon: "/__agent-icons/tester" });
  async function waitForPush(count) {
    for (let i = 0; i < 100; i++) {
      if (await worker.evaluate((n) => self.pushTestMessages.length >= n, count)) return;
      await new Promise((resolve) => setTimeout(resolve, 50));
    }
    throw new Error("push handler did not show a notification");
  }
  await waitForPush(1);
  const notices = await worker.evaluate(() => self.pushTestMessages.map((n) => ({ title: n.title, body: n.body, url: n.data.url })));
  assert.deepEqual(notices, [{ title: "Tester needs your attention", body: "Review the proposed operation", url: "/approvals/task-1" }]);
  assert.equal(await worker.evaluate(() => self.pushTestMessages[0].icon), iconData, "agent avatar not used");
  await sendPush({ id: "attention-1", title: "Tester needs your attention", body: "Same event retried", url: "https://evil.test/" });
  await waitForPush(2);
  assert.deepEqual(await worker.evaluate(() => self.pushTestMessages.map((n) => n.tag)), ["attention-1", "attention-1"]);
  assert.equal(await worker.evaluate(() => self.pushTestMessages[1].data.url), "/");
  await page.evaluate(async () => { for (const n of await (await navigator.serviceWorker.ready).getNotifications()) n.close(); });
  await page.getByLabel("One-time code").fill("123 456");

  revision++;
  await page.evaluate(async () => (await navigator.serviceWorker.ready).update());
  await page.getByRole("button", { name: "Reload to update" }).waitFor();
  assert.equal(await page.getByLabel("One-time code").inputValue(), "123 456", "update interrupted a draft");
  await page.getByRole("button", { name: "Reload to update" }).click();
  await page.waitForFunction(() => document.querySelector("#code")?.value === "");

  await context.setOffline(true);
  await page.goto(`${origin}/bots/chief`);
  await page.getByText("You’re offline", { exact: true }).waitFor();
  assert.equal(await page.getByLabel("One-time code").count(), 0, "network failure masqueraded as logout");
  const cached = await page.evaluate(async () => {
    const urls = [];
    for (const name of await caches.keys()) {
      for (const request of await (await caches.open(name)).keys()) urls.push(new URL(request.url).pathname);
    }
    return urls;
  });
  assert.ok(cached.includes("/index.html"));
  assert.ok(!cached.some((path) => path.startsWith("/v1/")), "cached authenticated API data");
  assert.equal(await page.evaluate(async () => {
    try { await fetch("/v1/session"); return true; } catch { return false; }
  }), false, "worker answered an offline API request");
  await context.setOffline(false);
  await page.getByLabel("One-time code").waitFor();

  sessionStatus = 503;
  await page.reload();
  await page.getByText("Node did not answer", { exact: true }).waitFor();
  assert.equal(await page.getByLabel("One-time code").count(), 0);
  sessionStatus = 401;
  await page.getByRole("button", { name: "Retry connection" }).click();
  await page.getByLabel("One-time code").waitFor();
  assert.deepEqual(errors, []);
  console.log("PWA: manifest, push rendering, stable notification tags and safe links, offline deep links, API isolation, reconnect and non-disruptive updates passed");
} finally {
  await browser?.close();
  await new Promise((resolve) => server.close(resolve));
}
