import assert from "node:assert/strict";
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import { chromium } from "playwright";

const dist = new URL("../../internal/gateway/ui/dist/", import.meta.url);
let teams = true;
let hold;
let rejectUpload = false;
let rejectMessage = false;
const uploads = [];
const discarded = [];
const messages = [];
const turns = new Map();
const server = createServer(async (req, res) => {
  const path = decodeURIComponent(new URL(req.url, "http://localhost").pathname);
  if (path.startsWith("/v1/")) {
    const respond = (json, status = 200) => { res.writeHead(status, { "Content-Type": "application/json", "Cache-Control": "no-store" }); res.end(JSON.stringify(json)); };
    if (path === "/v1/uploads" && req.method === "GET") return respond({ enabled: true, max_bytes: 1024, max_files: 3, media_types: ["image/png", "application/pdf", "text/plain", "text/markdown", "text/csv", "application/json"] });
    if (path.startsWith("/v1/uploads/") && req.method === "DELETE") { discarded.push(path.split("/").at(-1)); res.writeHead(204); res.end(); return; }
    let raw = Buffer.alloc(0);
    for await (const chunk of req) raw = Buffer.concat([raw, chunk]);
    if (path === "/v1/uploads") {
      const id = `upload-${uploads.length + 1}`;
      uploads.push({ id, bytes: raw, mime: req.headers["content-type"], filename: decodeURIComponent(req.headers["x-upload-name"]) });
      if (hold) { await hold.promise; hold = null; }
      if (rejectUpload) return respond({ error: "Test upload interrupted" }, 503);
      return respond({ upload_id: id, filename: uploads.at(-1).filename, mime_type: uploads.at(-1).mime, size: raw.length, expires_at: new Date(Date.now() + 3600000).toISOString() }, 201);
    }
    if (path === "/v1/chat-turns") {
      if (req.method === "GET") {
        const bot = new URL(req.url, "http://localhost").searchParams.get("bot");
        return respond({ turn: [...turns.values()].reverse().find((turn) => turn.bot === bot) ?? null });
      }
      if (rejectMessage) return respond({ error: "Test message rejected before admission" }, 403);
      const input = JSON.parse(raw.toString()); messages.push(input);
      const turn = { ...input, state: "completed", event: input.bot ? "reply" : "final", data: input.bot ? { text: "Coordinator received the files." } : { reply: "Assistant received the file." },
        files: input.upload_ids.map((id) => { const file = uploads.find((file) => file.id === id); return { name: file.filename, mime: file.mime, size: file.bytes.length }; }),
        created_at: new Date().toISOString(), updated_at: new Date().toISOString() };
      turns.set(input.id, turn); return respond(turn, 202);
    }
    if (path === "/v1/bots/chief/messages" || path === "/v1/messages") {
      const input = JSON.parse(raw.toString()); messages.push(input);
      res.writeHead(200, { "Content-Type": "text/event-stream" });
      res.end(`event: ${path === "/v1/messages" ? "final" : "reply"}\ndata: ${JSON.stringify(path === "/v1/messages" ? { reply: "Assistant received the file." } : { text: "Coordinator received the files." })}\n\n`);
      return;
    }
    const bot = { id: "chief", display_name: "Coordinator", is_coordinator: true, description: "Keeps the team moving", enabled: true, tools: [], may_message: [], revision: 1 };
    const routes = {
      "/v1/session": { user_id: "alice" },
      "/v1/capabilities": { compute: { available: true }, "compute-teams": { enabled: teams }, "ui-web": { enabled: true } },
      "/v1/bots": { bots: [bot] }, "/v1/bots/chief": bot, "/v1/groups": { groups: [] }, "/v1/activity": { items: [] },
      "/v1/bots/chief/inbox": { items: [] }, "/v1/bots/chief/sessions": { sessions: [] }, "/v1/sessions/bot:chief": { messages: [] },
      "/v1/learned-reviews": { reviews: [] }, "/v1/task-approvals": { records: [] },
    };
    assert.ok(path in routes, `Unexpected API request ${path}`);
    return respond(routes[path]);
  }
  try {
    const file = path.includes(".") ? path.slice(1) : "index.html";
    const type = { js: "text/javascript", css: "text/css", html: "text/html", png: "image/png", ico: "image/x-icon", webmanifest: "application/manifest+json" }[file.split(".").at(-1)];
    res.writeHead(200, { "Content-Type": type || "application/octet-stream" }); res.end(await readFile(new URL(file, dist)));
  } catch { res.writeHead(404); res.end(); }
});
await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
const origin = `http://127.0.0.1:${server.address().port}`;
let browser;
let page;
try {
  browser = await chromium.launch({ executablePath: process.env.CHROME_BIN || "/usr/bin/google-chrome", args: ["--no-sandbox"] });
  page = await browser.newPage({ viewport: { width: 390, height: 844 }, hasTouch: true, serviceWorkers: "block" });
  page.setDefaultTimeout(15000);
  const errors = []; page.on("pageerror", (error) => errors.push(error.message));
  await page.goto(`${origin}/bots/chief`);
  const picker = page.getByLabel("Choose attachments");
  const send = page.getByRole("button", { name: "Send", exact: true });
  await page.getByRole("button", { name: "Attach files", exact: true }).waitFor();
  await page.waitForFunction(() => !document.querySelector('.attach-button').disabled);
  let release;
  hold = { promise: new Promise((resolve) => { release = resolve; }) };
  const png = Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aI1kAAAAASUVORK5CYII=", "base64");
  await picker.setInputFiles([{ name: "photo.png", mimeType: "image/png", buffer: png }, { name: "notes.txt", mimeType: "text/plain", buffer: Buffer.from("A note for the team") }]);
  await page.getByRole("progressbar", { name: "Uploading photo.png" }).waitFor();
  assert.equal(await send.isDisabled(), true, "Send became enabled before uploads finished");
  assert.equal(await page.locator(".attachment-card img").count(), 1);
  assert.ok((await page.locator(".attachment-card img").getAttribute("src")).startsWith("blob:"), "preview made a network request instead of using the selected file");
  release();
  await page.waitForFunction(() => document.querySelectorAll(".attachment-card.ready").length === 2);
  if (process.env.ATTACHMENT_SCREENSHOTS) await page.screenshot({ path: `${process.env.ATTACHMENT_SCREENSHOTS}/lobslaw-attachments-mobile.png`, animations: "disabled" });
  assert.equal(uploads.length, 2);
  assert.equal(uploads[0].bytes.compare(png), 0);
  assert.equal(uploads[1].filename, "notes.txt");
  await page.getByRole("button", { name: "Open menu", exact: true }).click();
  await page.locator(".side-foot").getByRole("link", { name: "Task approvals", exact: true }).click();
  await page.getByRole("button", { name: "Open menu", exact: true }).click();
  await page.locator(".roster").getByRole("link", { name: /Coordinator/ }).click();
  await page.getByPlaceholder("Message Coordinator…").waitFor();
  assert.equal(await page.locator(".attachment-card.ready").count(), 2, "navigation lost selected attachments");
  await page.getByRole("button", { name: "Remove notes.txt", exact: true }).click();
  await page.waitForFunction(() => document.querySelectorAll(".attachment-card").length === 1);
  await send.click();
  await page.locator(".thread").getByText("Coordinator received the files.", { exact: true }).waitFor();
  assert.deepEqual(messages[0].upload_ids, ["upload-1"]);
  assert.equal(messages[0].message, "", "attachment-only sending added an unexpected prompt");
  assert.equal(await page.locator(".message-files img").count(), 1);
  assert.equal(await page.locator(".attachment-card").count(), 0);
  assert.ok(discarded.includes("upload-2"));

  rejectUpload = true;
  await picker.setInputFiles({ name: "retry.pdf", mimeType: "application/pdf", buffer: Buffer.from("%PDF-1.7 test document") });
  await page.getByRole("alert").getByText("Test upload interrupted", { exact: true }).waitFor();
  assert.equal(await send.isDisabled(), true);
  assert.equal(await page.locator(".attachment-card").count(), 1, "failure discarded the selected file");
  rejectUpload = false;
  await page.getByRole("button", { name: "Retry upload", exact: true }).click();
  await page.locator(".attachment-card.ready").waitFor();
  await page.getByPlaceholder("Message Coordinator…").fill("Read the document");
  rejectMessage = true;
  await send.click();
  await page.getByRole("alert").getByText("Test message rejected before admission", { exact: true }).waitFor();
  assert.equal(await page.locator(".attachment-card.ready").count(), 1, "rejected message discarded its attachments");
  assert.equal(await page.getByPlaceholder("Message Coordinator…").inputValue(), "Read the document");
  rejectMessage = false;
  await send.click();
  await page.waitForFunction(() => document.querySelectorAll(".message-file").length === 2);
  assert.deepEqual(messages[1].upload_ids, ["upload-4"]);
  await page.reload();
  await page.locator(".message-file").getByText("retry.pdf", { exact: true }).waitFor();
  assert.equal(uploads.length, 4, "reconnect uploaded the file again");

  await picker.setInputFiles({ name: "large.txt", mimeType: "text/plain", buffer: Buffer.alloc(1025, "a") });
  await page.getByRole("alert").getByText(/at most 1 KB/).waitFor();
  await picker.setInputFiles({ name: "unsafe.svg", mimeType: "image/svg+xml", buffer: Buffer.from("<svg/>") });
  await page.getByRole("alert").getByText(/unsupported file type/).waitFor();
  assert.equal(uploads.length, 4, "invalid files reached the server");

  teams = false;
  await page.goto(origin);
  await page.waitForFunction(() => !document.querySelector('.attach-button')?.disabled);
  await page.getByLabel("Choose attachments").setInputFiles({ name: "standalone.txt", mimeType: "text/plain", buffer: Buffer.from("A standalone assistant note") });
  await page.locator(".attachment-card.ready").waitFor();
  await page.getByRole("button", { name: "Send", exact: true }).click();
  await page.locator(".thread").getByText("Assistant received the file.", { exact: true }).waitFor();
  assert.deepEqual(messages.at(-1).upload_ids, ["upload-5"]);
  await page.getByLabel("Message", { exact: true }).evaluate((input, data) => {
    const transfer = new DataTransfer();
    transfer.items.add(new File([new Uint8Array(data)], "pasted.png", { type: "image/png" }));
    input.dispatchEvent(new ClipboardEvent("paste", { bubbles: true, cancelable: true, clipboardData: transfer }));
  }, [...png]);
  await page.locator(".attachment-card.ready").getByText("pasted.png", { exact: true }).waitFor();
  await page.getByRole("button", { name: "Remove pasted.png", exact: true }).click();
  await page.locator(".composer").evaluate((form) => {
    const transfer = new DataTransfer();
    transfer.items.add(new File(["Dropped note"], "dropped.txt", { type: "text/plain" }));
    form.dispatchEvent(new DragEvent("drop", { bubbles: true, cancelable: true, dataTransfer: transfer }));
  });
  await page.locator(".attachment-card.ready").getByText("dropped.txt", { exact: true }).waitFor();
  await page.getByRole("button", { name: "Remove dropped.txt", exact: true }).click();
  await page.getByLabel("Choose attachments").setInputFiles([1, 2, 3, 4].map((index) => ({ name: `limit-${index}.txt`, mimeType: "text/plain", buffer: Buffer.from("note") })));
  await page.getByRole("alert").getByText("Up to 3 files per message.", { exact: true }).waitFor();
  assert.equal(await page.locator(".attachment-card").count(), 3);
  while (await page.locator(".attachment-remove").count()) await page.locator(".attachment-remove").first().click();
  assert.ok(!uploads.some((upload) => upload.filename === "limit-4.txt"), "file-count limit failed");
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true, "attachments overflowed mobile layout");
  assert.deepEqual(errors, []);
  console.log("Attachments: raw uploads/progress, local image previews, draft navigation, remove, retry, size/type validation, attachment-only bot and assistant messages passed.");
} catch (error) {
  if (page && !page.isClosed()) console.error(await page.locator("body").innerText());
  throw error;
} finally {
  if (hold) { /* test timeout cleanup */ server.closeAllConnections(); }
  await browser?.close(); await new Promise((resolve) => server.close(resolve));
}
