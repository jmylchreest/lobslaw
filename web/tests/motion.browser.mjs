import assert from "node:assert/strict";
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import { chromium } from "playwright";

const dist = new URL("../../internal/gateway/ui/dist/", import.meta.url);
const bot = (id, group = "crew") => ({ id, display_name: id === "chief" ? "Chief" : id === "helper" ? "Helper" : "Research", group_id: group,
  description: "A helpful teammate", instructions: "Help the team", enabled: true, is_coordinator: id === "chief", tools: [], may_message: [], revision: 1 });
let bots = [bot("chief"), bot("research", "lab")];
const groups = [{ id: "crew", name: "Crew", bots: 1, is_default: true }, { id: "lab", name: "Lab", bots: 1 }];
let feed = [{ id: "work-1", recipient: "chief", sender: "operator", subject: "Prepare the briefing", body: "Collect the latest updates", status: "claimed", kind: "task", attempts: 1, created_at: new Date().toISOString() }];
const inboxMessage = '<untrusted source="inbox:bot:chief">\nSubject: Review the proposed work\n\nPlease keep **all details** in the report.\n</untrusted>';
let task = { id: "task-1", actor: "bot:chief", state: "TASK_APPROVAL_STATE_WAITING", revision: "1", operation: { summary: "Review the proposed work" }, transcript: [{ role: "user", content: `You have work from another bot.\n\n${inboxMessage}` }, { role: "assistant", content: `Recorded task evidence\n\n${inboxMessage}` }] };
let rejectSave = false;
let stream;
let chatMode = "manual";
const turns = new Map();
const unexpected = [];
const server = createServer(async (req, res) => {
  const path = decodeURIComponent(new URL(req.url, "http://localhost").pathname);
  if (path.startsWith("/v1/")) {
    let body = "";
    for await (const chunk of req) body += chunk;
    const input = body ? JSON.parse(body) : {};
    const respond = (json, status = 200) => { res.writeHead(status, { "Content-Type": "application/json", "Cache-Control": "no-store" }); res.end(JSON.stringify(json)); };
    if (path === "/v1/session") return respond({ user_id: "alice" });
    if (path === "/v1/chat-turns") {
      if (req.method !== "POST") return respond({ turn: [...turns.values()].reverse().find((turn) => turn.bot === new URL(req.url, "http://localhost").searchParams.get("bot")) ?? null });
      const turn = { ...input, state: "running", created_at: new Date().toISOString(), updated_at: new Date().toISOString() };
      turns.set(input.id, turn);
      const update = (frame) => {
        const event = frame.match(/event: (\w+)/)?.[1];
        const data = JSON.parse(frame.match(/data: (.+)/)?.[1] || "{}");
        if (event === "delta") data.text = (turn.event === "delta" ? turn.data.text : "") + data.text;
        Object.assign(turn, { event, data, updated_at: new Date().toISOString() });
        if (event === "reply") turn.state = "completed";
        if (event === "error") turn.state = "failed";
      };
      stream = { destroyed: false, write: update, end: (frame) => { if (frame) update(frame); if (turn.state === "running") { turn.state = "failed"; turn.data = { message: "The connection closed before a reply arrived." }; } } };
      if (chatMode === "empty") stream.end();
      if (chatMode === "error") stream.end('event: error\ndata: {"message":"Provider temporarily unavailable"}\n\n');
      return respond(turn, 202);
    }
    if (path.startsWith("/v1/chat-turns/")) {
      const turn = turns.get(path.split("/").at(-1));
      if (req.method === "DELETE") Object.assign(turn, { state: "cancelled", data: { message: "Response stopped" } });
      return respond(turn);
    }
    if (path === "/v1/uploads") return respond({ enabled: false, max_bytes: 33554432, max_files: 16, media_types: [] });
    if (path === "/v1/capabilities") return respond({ compute: { available: true }, "compute-teams": { enabled: true }, "ui-web": { enabled: true } });
    if (path === "/v1/groups") return respond({ groups });
    if (path === "/v1/activity") return respond({ items: feed });
    if (path === "/v1/learned-reviews") return respond({ reviews: [] });
    if (path === "/v1/task-approvals") return respond({ records: [task] });
    if (path === "/v1/task-approvals/task-1") return respond({ record: task });
    if (path === "/v1/tools") return respond({ tools: [] });
    if (path.startsWith("/v1/sessions/")) return respond({ messages: [] });
    if (path === "/v1/bots") {
      if (req.method === "POST") {
        const created = { ...bot(input.id), ...input };
        bots = [...bots, created];
        return respond(created);
      }
      return respond({ bots });
    }
    if (path === "/v1/bots/chief/messages") {
      res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-store" });
      stream = res;
      res.write('event: working\ndata: {}\n\n');
      if (chatMode === "empty") { res.end(); stream = null; }
      if (chatMode === "error") { res.end('event: error\ndata: {"message":"Provider temporarily unavailable"}\n\n'); stream = null; }
      return;
    }
    if (path.startsWith("/v1/inbox/")) return respond(feed.find((item) => path.endsWith("/" + item.id)));
    const match = path.match(/^\/v1\/bots\/([^/]+)(?:\/(inbox|sessions|routines|memory))?$/);
    if (match) {
      if (match[2] === "inbox") return respond({ items: feed.filter((item) => item.recipient === match[1]) });
      if (match[2] === "sessions") return respond({ sessions: [] });
      if (match[2] === "routines") return respond({ routines: [] });
      if (match[2] === "memory") return respond({ records: [], total: 0 });
      if (req.method === "PATCH") {
        if (rejectSave) return respond({ error: "Save rejected" }, 409);
        bots = bots.map((entry) => entry.id === match[1] ? { ...entry, ...input, revision: entry.revision + 1 } : entry);
      }
      return respond(bots.find((entry) => entry.id === match[1]));
    }
    unexpected.push(`${req.method} ${path}`);
    return respond({ error: "Unexpected test request" }, 404);
  }
  try {
    const file = path.includes(".") ? path.slice(1) : "index.html";
    const types = { js: "text/javascript", css: "text/css", html: "text/html", png: "image/png", ico: "image/x-icon", webmanifest: "application/manifest+json" };
    res.writeHead(200, { "Content-Type": types[file.split(".").at(-1)] || "application/octet-stream" });
    res.end(await readFile(new URL(file, dist)));
  } catch { res.writeHead(404); res.end(); }
});
await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
const origin = `http://127.0.0.1:${server.address().port}`;
let browser;
let page;
try {
  browser = await chromium.launch({ executablePath: process.env.CHROME_BIN || "/usr/bin/google-chrome", args: ["--no-sandbox"] });
  page = await browser.newPage({ viewport: { width: 1440, height: 900 }, hasTouch: true, serviceWorkers: "block" });
  page.setDefaultTimeout(15000);
  await page.clock.install();
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.addInitScript(() => {
    const animate = Element.prototype.animate;
    Element.prototype.animate = function (...args) { this.motionStarts = (this.motionStarts || 0) + 1; return animate.apply(this, args); };
    document.addEventListener("animationstart", (event) => {
      if (["message-in", "sent-in"].includes(event.animationName)) event.target.entrances = (event.target.entrances || 0) + 1;
    }, true);
    window.teamTransitions = 0;
    if (document.startViewTransition) {
      const start = document.startViewTransition.bind(document);
      document.startViewTransition = (...args) => { window.teamTransitions++; return start(...args); };
    }
  });
  await page.goto(origin);
  await page.locator(".person .mascot.is-working").waitFor();
  assert.equal(await page.locator(".activity-arrival").count(), 0, "historical activity animated on load");
  assert.equal(await page.locator(".person .mascot-body").first().evaluate((node) => getComputedStyle(node).animationName), "mascot-bob");
  const mark = page.locator('[data-activity-id="work-1"] .status-mark');
  await mark.evaluate((node) => { window.originalMark = node; });
  feed = [{ ...feed[0], status: "done", result: "The briefing is ready" }, { ...feed[0], id: "work-2", subject: "Review the next milestone", status: "done", result: "Milestone reviewed" }];
  await page.clock.runFor(8000);
  await page.locator('[data-activity-id="work-2"].activity-arrival').waitFor();
  await page.waitForFunction(() => window.originalMark?.dataset.status === "done");
  assert.equal(await mark.evaluate((node) => node === window.originalMark), true, "status change remounted the timeline node");
  assert.equal(await mark.evaluate((node) => node.motionStarts), 1, "completion cue did not run exactly once");
  await page.clock.runFor(8000);
  assert.equal(await page.locator(".activity-arrival").count(), 0, "unchanged poll replayed arrival cues");
  assert.equal(await mark.evaluate((node) => node.motionStarts), 1, "unchanged poll replayed completion cue");

  await page.locator(".gp-cur").click();
  await page.getByRole("button", { name: "Lab 1", exact: true }).click();
  await page.locator(".person-nm").getByText("Research", { exact: true }).waitFor();
  assert.equal(await page.locator(".activity-arrival").count(), 0, "team switch treated history as new activity");
  assert.ok(await page.evaluate(() => window.teamTransitions > 0), "team crossfade did not start");
  await page.locator(".gp-cur").click();
  await page.getByRole("button", { name: "Crew 1", exact: true }).click();
  await page.locator(".nav-active-marker").evaluate((node) => { window.overviewMarker = node.style.transform; });
  await page.locator(".roster").getByRole("link", { name: /Chief/ }).click();
  const draft = page.getByPlaceholder("Message Chief…");
  await draft.waitFor();
  assert.notEqual(await page.locator(".nav-active-marker").evaluate((node) => node.style.transform), await page.evaluate(() => window.overviewMarker));
  await draft.fill("Give me a quick update");
  await page.getByRole("button", { name: "Send", exact: true }).click();
  const reply = page.locator('.msg[data-message-id^="reply-"]');
  await reply.locator(".reply-thinking.is-open").waitFor();
  await reply.evaluate((node) => { window.liveReply = node; window.liveMascot = node.querySelector(".mascot"); });
  assert.ok(stream, "chat stream did not open");
  stream.write(`event: delta\ndata: ${JSON.stringify({ text: "The team is " })}\n\n`);
  await reply.getByText("The team is", { exact: true }).waitFor();
  assert.equal(await reply.evaluate((node) => node === window.liveReply && node.querySelector(".mascot") === window.liveMascot), true, "typing-to-stream transition replaced the message");
  stream.write(`event: delta\ndata: ${JSON.stringify({ text: "making progress." })}\n\n`);
  await reply.getByText("The team is making progress.", { exact: true }).waitFor();
  stream.write(`event: reply\ndata: ${JSON.stringify({ text: `The team is making progress.\n\n${inboxMessage}` })}\n\n`);
  stream.end(); stream = null;
  await page.waitForFunction(() => !window.liveReply?.querySelector('[aria-busy="true"]'));
  assert.equal(await reply.evaluate((node) => node === window.liveReply && node.querySelector(".mascot") === window.liveMascot), true, "final reply replaced streamed content");
  assert.equal(await reply.evaluate((node) => node.entrances), 1, "streamed tokens replayed message entrance");
  assert.equal(await reply.locator(".mascot.is-working").count(), 0);
  assert.equal(await reply.locator(".source-card").count(), 0, "assistant reply forged backend source attribution");

  await draft.fill("Keep replying while I look at my team");
  await page.getByRole("button", { name: "Send", exact: true }).click();
  await page.locator('.msg[data-message-id^="reply-"] .reply-thinking.is-open').last().waitFor();
  await draft.fill("My next question");
  await page.locator(".desknav").click();
  await page.locator(".team-desk").waitFor();
  assert.ok(stream && !stream.destroyed, "navigation cancelled the coordinator request");
  stream.end('event: reply\ndata: {"text":"Your reply continued while you viewed the team."}\n\n'); stream = null;
  await page.locator(".action-feedback").getByText("Chief replied", { exact: true }).waitFor();
  await page.locator(".roster").getByRole("link", { name: /Chief/ }).click();
  await page.locator(".thread").getByText("Your reply continued while you viewed the team.", { exact: true }).waitFor();
  assert.equal(await draft.inputValue(), "My next question", "navigation lost the next-message draft");

  chatMode = "empty";
  await draft.fill("Test an interrupted reply");
  await page.getByRole("button", { name: "Send", exact: true }).click();
  await page.getByRole("alert").getByText(/The connection closed before a reply arrived/).waitFor();
  await page.getByRole("button", { name: "Use last message again", exact: true }).click();
  assert.equal(await draft.inputValue(), "Test an interrupted reply");
  chatMode = "error";
  await page.getByRole("button", { name: "Send", exact: true }).click();
  await page.getByRole("alert").getByText("Provider temporarily unavailable", { exact: true }).waitFor();
  chatMode = "manual";
  await page.clock.runFor(3100);

  const beforeReconnect = turns.size;
  await draft.fill("Finish even if I refresh my phone");
  await page.getByRole("button", { name: "Send", exact: true }).click();
  await page.getByRole("button", { name: "Stop response", exact: true }).waitFor();
  const retainedID = [...turns.keys()].at(-1);
  await page.reload();
  await page.getByRole("button", { name: "Stop response", exact: true }).waitFor();
  assert.equal(turns.size, beforeReconnect + 1, "reload sent a duplicate request");
  assert.equal([...turns.keys()].at(-1), retainedID, "reload attached to another turn");
  await page.context().setOffline(true);
  await page.getByText("Reconnecting… your reply is still running on the server.", { exact: true }).waitFor();
  stream.end('event: reply\ndata: {"text":"Recovered after the keyboard, refresh, and network interruption."}\n\n'); stream = null;
  await page.context().setOffline(false);
  await page.locator(".thread").getByText("Recovered after the keyboard, refresh, and network interruption.", { exact: true }).waitFor();
  await page.reload();
  await page.locator(".thread").getByText("Recovered after the keyboard, refresh, and network interruption.", { exact: true }).waitFor();
  assert.equal(turns.size, beforeReconnect + 1, "completed reply recovery reran the turn");

  const expand = page.getByRole("button", { name: "Expand: Prepare the briefing", exact: true });
  const detailId = await expand.getAttribute("aria-controls");
  await expand.click();
  const detail = page.locator(`[id="${detailId}"]`);
  await detail.getByText("Collect the latest updates", { exact: true }).waitFor();
  await page.getByRole("button", { name: "Collapse: Prepare the briefing", exact: true }).click();
  assert.equal(await detail.getAttribute("inert"), "");
  assert.equal(await detail.getAttribute("aria-hidden"), "true");

  await page.getByRole("button", { name: "Settings", exact: true }).click();
  await page.getByLabel("Description", { exact: true }).fill("Keeps everyone informed");
  rejectSave = true;
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await page.getByText("Save rejected", { exact: true }).waitFor();
  assert.equal(await page.locator(".action-feedback").count(), 0, "failed save showed a success confirmation");
  rejectSave = false;
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await page.getByRole("button", { name: "Saved", exact: true }).waitFor();
  await page.locator(".action-feedback").getByText("Bot settings saved", { exact: true }).waitFor();
  await page.getByRole("button", { name: "Close", exact: true }).click();
  assert.equal(await page.locator("#bot-settings").getAttribute("inert"), "");
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  assert.equal(await page.getByLabel("Description", { exact: true }).inputValue(), "Keeps everyone informed", "closing settings discarded the form");
  assert.equal(await page.locator('.thread[aria-label="Bot settings"]').evaluate((node) => node.scrollTop), 0, "settings opened below the start of the form");
  if (process.env.MOTION_SCREENSHOTS) await page.screenshot({ path: `${process.env.MOTION_SCREENSHOTS}/lobslaw-animated-settings.png`, animations: "disabled" });

  await page.locator(".roster").getByRole("link", { name: /New bot$/ }).click();
  await page.getByLabel("Id", { exact: true }).fill("helper");
  await page.getByLabel("Display name", { exact: true }).fill("Helper");
  await page.getByLabel("Instructions", { exact: true }).fill("Help with the next milestone");
  await page.getByRole("button", { name: "Create", exact: true }).click();
  await page.getByPlaceholder("Message Helper…").waitFor();
  await page.locator(".action-feedback").getByText("Helper created", { exact: true }).waitFor();
  await page.setViewportSize({ width: 390, height: 844 });
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true, "mobile conversation overflowed");
  const composer = await page.locator(".composer").boundingBox();
  assert.ok(composer && composer.y + composer.height <= 844, "mobile composer fell below the viewport");
  const mobileDraft = page.getByPlaceholder("Message Helper…");
  await mobileDraft.fill("Line one");
  await mobileDraft.press("Enter");
  assert.equal(await mobileDraft.inputValue(), "Line one\n", "mobile Return submitted instead of inserting a new line");
  await page.evaluate(() => {
    const viewport = window.visualViewport;
    Object.defineProperty(viewport, "height", { configurable: true, value: 430 });
    Object.defineProperty(viewport, "offsetTop", { configurable: true, value: 50 });
    viewport.dispatchEvent(new Event("resize"));
  });
  await page.clock.runFor(50);
  await page.waitForFunction(() => document.documentElement.style.getPropertyValue("--visible-height") === "430px");
  const keyboardComposer = await page.locator(".composer").boundingBox();
  assert.ok(keyboardComposer && keyboardComposer.y >= 50 && keyboardComposer.y + keyboardComposer.height <= 480, "composer was clipped by the simulated keyboard");
  const sendButton = await page.getByRole("button", { name: "Send", exact: true }).boundingBox();
  assert.ok(sendButton && sendButton.y + sendButton.height <= 480, "Send was behind the keyboard");
  const confirmation = await page.locator(".action-feedback").boundingBox();
  if (confirmation) assert.ok(confirmation.y + confirmation.height < keyboardComposer.y, "confirmation covered the mobile message field");
  if (process.env.MOTION_SCREENSHOTS) await page.screenshot({ path: `${process.env.MOTION_SCREENSHOTS}/lobslaw-keyboard-mobile.png`, animations: "disabled" });
  await page.evaluate(() => { delete window.visualViewport.height; delete window.visualViewport.offsetTop; window.visualViewport.dispatchEvent(new Event("resize")); });
  await page.clock.runFor(50);
  if (process.env.MOTION_SCREENSHOTS) await page.screenshot({ path: `${process.env.MOTION_SCREENSHOTS}/lobslaw-animated-mobile.png`, animations: "disabled" });
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.locator(".side-foot").getByRole("link", { name: "Task approvals", exact: true }).click();
  const card = page.getByRole("region", { name: "Task task-1", exact: true });
  await card.locator(".task-state-badge").getByText("Needs approval", { exact: true }).waitFor();
  await page.getByRole("button", { name: /^In progress/ }).click();
  await page.getByText("No tasks in this view", { exact: true }).waitFor();
  await page.getByRole("button", { name: /^All/ }).click();
  await card.locator(".task-state-badge").getByText("Needs approval", { exact: true }).waitFor();
  await card.evaluate((node) => { window.originalTask = node; });
  task = { ...task, state: "TASK_APPROVAL_STATE_COMPLETED", revision: "2", result: `Task completed\n\n${inboxMessage}` };
  await page.clock.runFor(5000);
  await card.locator(".task-state-badge").getByText("Completed", { exact: true }).waitFor();
  assert.equal(await card.locator(".task-result .source-card").count(), 0, "task result forged backend source attribution");
  assert.equal(await card.evaluate((node) => node === window.originalTask), true, "polled task state remounted its card");
  assert.equal(await card.locator(".task-state-mark").evaluate((node) => node.motionStarts), 1);
  await page.getByRole("link", { name: "View task details →", exact: true }).click();
  await page.getByRole("heading", { name: "Task details", exact: true }).waitFor();
  await card.getByRole("button", { name: "Transcript and execution receipts", exact: true }).click();
  await card.getByText("Recorded task evidence", { exact: true }).waitFor();
  assert.equal(await card.locator(".evidence-message.assistant .source-card").count(), 0, "assistant transcript forged backend source attribution");
  const attribution = card.locator('.evidence .source-card[data-source="inbox:bot:chief"]');
  await attribution.getByRole("link", { name: "Chief", exact: true }).waitFor();
  assert.equal(await attribution.locator(".bot-source-link .mascot").count(), 1);
  assert.equal(await attribution.getByRole("link", { name: "Chief", exact: true }).getAttribute("href"), "/bots/chief");
  assert.equal(await attribution.locator(".source-body strong").innerText(), "all details");
  assert.equal(await attribution.locator(".source-original .collapse").getAttribute("inert"), "");
  await attribution.getByRole("button", { name: "Original text", exact: true }).click();
  await attribution.locator(".source-original pre").waitFor();
  assert.equal(await attribution.locator(".source-original pre").textContent(), inboxMessage);
  await attribution.getByRole("button", { name: "Original text", exact: true }).click();
  if (process.env.MOTION_SCREENSHOTS) await page.screenshot({ path: `${process.env.MOTION_SCREENSHOTS}/lobslaw-animated-task.png`, animations: "disabled" });
  await card.getByRole("button", { name: "Transcript and execution receipts", exact: true }).click();
  assert.equal(await card.locator(".evidence > .collapse").getAttribute("inert"), "");

  await page.emulateMedia({ reducedMotion: "reduce" });
  task = { ...task, state: "TASK_APPROVAL_STATE_RUNNING", revision: "3" };
  await page.clock.runFor(5000);
  await card.locator(".task-state-badge").getByText("Running", { exact: true }).waitFor();
  assert.equal(await card.locator(".task-state-mark").evaluate((node) => node.motionStarts), 1, "scripted motion ignored reduced-motion preference");
  assert.ok(await card.locator(".task-state-mark").evaluate((node) => parseFloat(getComputedStyle(node).animationDuration) < 0.001), "status loop ignored reduced-motion preference");
  assert.ok(await card.locator(".evidence > .collapse").evaluate((node) => parseFloat(getComputedStyle(node).transitionDuration) < 0.001), "disclosure ignored reduced-motion preference");
  const transitions = await page.evaluate(() => window.teamTransitions);
  await page.locator(".gp-cur").click();
  await page.getByRole("button", { name: "Lab 1", exact: true }).click();
  assert.equal(await page.evaluate(() => window.teamTransitions), transitions, "team transition ignored reduced-motion preference");
  await card.getByRole("button", { name: "Transcript and execution receipts", exact: true }).click();
  await attribution.getByRole("link", { name: "Chief", exact: true }).click();
  await page.waitForURL(`${origin}/bots/chief`);
  await page.getByPlaceholder("Message Chief…").waitFor();
  assert.equal(page.context().pages().length, 1, "bot source opened an external tab instead of the conversation");
  assert.deepEqual(unexpected, []);
  assert.deepEqual(errors, []);
  console.log("UI/motion: stable replies across navigation, preserved drafts, visible stream failures, keyboard-safe composer, task details, live cues, disclosures, action feedback and reduced motion passed.");
} catch (error) {
  if (page && !page.isClosed()) console.error(await page.locator("body").innerText());
  throw error;
} finally {
  stream?.end();
  await browser?.close();
  await new Promise((resolve) => server.close(resolve));
}
