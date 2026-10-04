import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { chromium } from "playwright";

const port = 4179;
const origin = `http://127.0.0.1:${port}`;
const server = spawn(process.execPath, ["node_modules/vite/bin/vite.js", "preview", "--host", "127.0.0.1", "--port", String(port), "--strictPort"], { stdio: "pipe" });
let browser;
let page;
try {
  for (let attempt = 0; ; attempt++) {
    try { if ((await fetch(origin)).ok) break; } catch { /* startup */ }
    if (attempt >= 100 || server.exitCode !== null) throw new Error("preview did not start");
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  browser = await chromium.launch({ executablePath: process.env.CHROME_BIN || "/usr/bin/google-chrome", args: ["--no-sandbox"] });
  page = await browser.newPage();
  page.setDefaultTimeout(15000);
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  let actor = "owner-user";
  let teams = true;
  let decided = false;
  let conflict = false;
  const decisions = [];
  let review;
  const fresh = () => ({ id: "skill:worker-guide", name: "worker-guide", revision: "9007199254740993", digest: "inspected-digest", active: true, body: "Current private instructions", files: { "removed.txt": "Removed reference content" }, pending: { body: "Proposed private instructions <img src='https://example.test/pixel'>", rationale: "Improve worker steps", files: { "new.txt": "New reference content" }, turnId: "worker-turn" } });
  await page.route("**/v1/**", async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    let json;
    const allowed = actor === "owner-user" || actor === "owner-operator";
    if (path === "/v1/session") json = { user_id: actor };
    else if (path === "/v1/capabilities") json = { compute: { available: true }, "compute-teams": { enabled: teams }, "ui-web": { enabled: true } };
    else if (path === "/v1/bots") json = { bots: [] };
    else if (path === "/v1/groups") json = { groups: [] };
    else if (path === "/v1/activity") json = { items: [] };
    else if (path === "/v1/learned-reviews") {
      if (actor === "ungranted-owner") { await route.fulfill({ status: 403, json: { error: "not authorised to review learned skills" } }); return; }
      json = { reviews: allowed && !decided ? [review] : [] };
    } else if (path.startsWith("/v1/learned-reviews/")) {
      if (!allowed) { await route.fulfill({ status: 404, json: { error: "proposal not found for this user" } }); return; }
      if (request.method() === "GET") json = review;
      else {
        const body = request.postDataJSON();
        decisions.push(body);
        assert.deepEqual(Object.keys(body).sort(), ["approve", "digest", "revision"]);
        if (conflict) {
          conflict = false;
          review = { ...review, revision: "9007199254740994", digest: "changed-digest" };
          await route.fulfill({ status: 409, json: { error: "proposal changed; reload before reviewing again" } }); return;
        }
        assert.equal(body.revision, review.revision);
        assert.equal(body.digest, review.digest);
        decided = true;
        json = { message: body.approve ? "Approval recorded. Skill activation is pending on a compute node." : "Amendment denied. The existing approved version is unchanged." };
      }
    } else throw new Error(`unexpected request ${request.method()} ${path}`);
    await route.fulfill({ json });
  });

  for (const owner of ["owner-user", "owner-operator"]) {
    actor = owner; teams = owner === "owner-user"; decided = false; review = fresh(); conflict = true;
    await page.setViewportSize({ width: teams ? 1280 : 390, height: 900 });
    await page.goto(`${origin}/learned`);
    await page.getByRole("heading", { name: "Learned proposals", exact: true }).waitFor();
    await page.getByRole("status").filter({ hasText: "awaiting review" }).waitFor();
    assert.equal(await page.getByRole("button", { name: "Approve reviewed proposal" }).count(), 0);
    await page.getByRole("button", { name: "Inspect worker-guide" }).click();
    await page.getByText("Removed reference content", { exact: true }).waitFor();
    await page.getByText("New reference content", { exact: true }).waitFor();
    assert.equal(await page.locator("section[aria-label='Proposal inspection'] img").count(), 0);
    const approve = page.getByRole("button", { name: "Approve reviewed proposal" });
    assert.equal(await approve.isDisabled(), true);
    await page.getByRole("checkbox").check();
    await approve.click();
    await page.getByText("Reload and inspect the current proposal", { exact: false }).waitFor();
    assert.equal(await approve.isDisabled(), true);
    assert.equal(await page.getByRole("checkbox").isChecked(), false);
    await page.getByRole("button", { name: "Reload proposal", exact: true }).click();
    await page.getByText("changed-digest", { exact: true }).waitFor();
    assert.equal(await approve.isDisabled(), true);
    await page.getByRole("checkbox").check();
    await approve.click();
    await page.getByRole("status").filter({ hasText: "activation is pending" }).waitFor();
    await page.getByText("No proposals awaiting your review.", { exact: true }).waitFor();
    decided = false; review = fresh();
    await page.getByRole("button", { name: "Refresh proposals" }).click();
    await page.getByRole("button", { name: "Inspect worker-guide" }).click();
    await page.getByRole("checkbox").check();
    await page.getByRole("button", { name: "Reject reviewed proposal" }).click();
    await page.getByRole("status").filter({ hasText: "Amendment denied" }).waitFor();
    assert.equal(decisions.at(-1).approve, false);
  }
  for (const other of ["other-operator", "ungranted-owner"]) {
    await page.setViewportSize({ width: 1280, height: 900 });
    actor = other; decided = false; teams = true; review = fresh();
    await page.goto(`${origin}/learned`);
    await page.getByRole("heading", { name: "Learned proposals", exact: true }).waitFor();
    await page.getByText(other === "other-operator" ? "No proposals awaiting your review." : "not authorised to review learned skills", { exact: true }).waitFor();
    assert.equal(await page.getByRole("button", { name: "Inspect worker-guide" }).count(), 0);
    const read = await page.evaluate(async () => { const r = await fetch("/v1/learned-reviews/skill%3Aworker-guide"); return { status: r.status, body: await r.text() }; });
    assert.equal(read.status, 404);
    assert.equal(read.body.includes("private instructions"), false);
  }
  assert.deepEqual(errors, []);
  console.log("Browser learned review: inspection, notifications, owner user/operator, denial, conflict reload and exact decisions passed.");
} catch (error) {
  if (page && !page.isClosed()) console.error(await page.locator("body").innerText());
  throw error;
} finally {
  await browser?.close();
  server.kill("SIGTERM");
  if (server.exitCode === null) await once(server, "exit");
}
