import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { chromium } from "playwright";

const port = 4178;
const origin = `http://127.0.0.1:${port}`;
const server = spawn(process.execPath, ["node_modules/vite/bin/vite.js", "preview", "--host", "127.0.0.1", "--port", String(port), "--strictPort"], { stdio: "pipe" });
let browser;
let page;
try {
  for (let attempt = 0; ; attempt++) {
    try { if ((await fetch(origin)).ok) break; } catch { /* server is starting */ }
    if (attempt >= 100 || server.exitCode !== null) throw new Error("preview did not start");
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  browser = await chromium.launch({ executablePath: process.env.CHROME_BIN || "/usr/bin/google-chrome", args: ["--no-sandbox"] });
  page = await browser.newPage();
  page.setDefaultTimeout(15000);
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  let task = { id: "task-budget", actor: "bot:worker", revision: "3", state: "TASK_APPROVAL_STATE_WAITING", budgetSpent: { toolCalls: 2 }, budgetLimits: { toolCalls: 1 }, operation: { requiresBudgetExtension: true, summary: "Continue the assigned task" } };
  let decision;
  let teams = true;
  const imageRequests = [];
  page.on("request", (request) => { if (request.url().startsWith("https://example.test/")) imageRequests.push(request.url()); });
  await page.route("**/v1/**", async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    let json;
    if (path === "/v1/session") json = { user_id: "alice" };
    else if (path === "/v1/capabilities") json = { compute: { available: true }, "compute-teams": { enabled: teams }, "ui-web": { enabled: true } };
    else if (path === "/v1/messages") {
      await route.fulfill({ contentType: "text/event-stream", body: `event: final\ndata: ${JSON.stringify({ reply: "![private](https://example.test/pixel?context=private)" })}\n\n` });
      return;
    }
    else if (path === "/v1/bots") json = { bots: [] };
    else if (path === "/v1/groups") json = { groups: [] };
    else if (path === "/v1/activity") json = { items: [] };
    else if (path === "/v1/task-approvals") json = { records: [task] };
    else if (path.endsWith("/decide")) {
      decision = request.postDataJSON();
      task = { ...task, state: "TASK_APPROVAL_STATE_READY", revision: "4" };
      json = { record: task };
    } else if (path.endsWith("/recover")) {
      assert.equal(request.postDataJSON().acknowledge_duplicate_risk, true);
      task = { ...task, state: "TASK_APPROVAL_STATE_WAITING", revision: "6", operation: { summary: "Review external effects before proceeding", grantable: true } };
      json = { record: task };
    } else throw new Error(`unexpected API request ${request.method()} ${path}`);
    await route.fulfill({ json });
  });
  await page.goto(`${origin}/approvals`);
  await page.getByRole("heading", { name: "Task approvals", exact: true }).waitFor();
  const extend = page.getByRole("button", { name: "Extend budget and queue resume" });
  assert.equal(await extend.isDisabled(), true);
  await page.getByLabel("Extra calls").fill("3");
  await extend.click();
  await page.getByText("Approved, waiting for the worker to resume.").waitFor();
  assert.equal(decision.choice, "budget_extension");
  assert.equal(decision.revision, "3");
  assert.deepEqual(decision.extra_budget, { tool_calls: 3, spend_usd: 0, egress_bytes: 0 });
  assert.equal(Object.hasOwn(decision, "owner"), false);
  task = { ...task, state: "TASK_APPROVAL_STATE_OUTCOME_UNKNOWN", revision: "5" };
  await page.getByRole("button", { name: "Refresh from start" }).click();
  const recover = page.getByRole("button", { name: "Recover for fresh approval" });
  await recover.waitFor();
  assert.equal(await recover.isDisabled(), true);
  await page.getByRole("checkbox").check();
  await recover.click();
  await page.getByRole("button", { name: "Approve once", exact: true }).waitFor();
  teams = false;
  await page.goto(origin);
  await page.getByLabel("Message", { exact: true }).fill("Show the result");
  await page.getByLabel("Message", { exact: true }).press("Enter");
  await page.getByRole("link", { name: "private (open image)" }).waitFor();
  assert.equal(await page.locator(".md img").count(), 0);
  assert.deepEqual(imageRequests, []);
  assert.deepEqual(errors, []);
  console.log("Browser task approval, bounded budget, explicit recovery, and image-egress checks passed.");
} catch (error) {
  if (page && !page.isClosed()) console.error(await page.locator("body").innerText());
  throw error;
} finally {
  await browser?.close();
  server.kill("SIGTERM");
  if (server.exitCode === null) await once(server, "exit");
}
