import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { chromium } from "playwright";

const origin = "http://127.0.0.1:4180";
const server = spawn(process.execPath, ["node_modules/vite/bin/vite.js", "preview", "--host", "127.0.0.1", "--port", "4180", "--strictPort"], { stdio: "pipe" });
let browser;
let page;
let releaseLogin;
try {
  for (let attempt = 0; ; attempt++) {
    try { if ((await fetch(origin)).ok) break; } catch { /* server is starting */ }
    if (attempt >= 100 || server.exitCode !== null) throw new Error("preview did not start");
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  browser = await chromium.launch({ executablePath: process.env.CHROME_BIN || "/usr/bin/google-chrome", args: ["--no-sandbox"] });
  page = await browser.newPage({ viewport: { width: 390, height: 844 }, isMobile: true, serviceWorkers: "block" });
  page.setDefaultTimeout(15000);
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  let signedIn = false;
  let rejectCode = true;
  const submissions = [];
  const pendingLogin = new Promise((resolve) => { releaseLogin = resolve; });
  await page.route("**/v1/**", async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    if (path === "/v1/session") {
      if (request.method() === "POST") {
        submissions.push({ body: request.postDataJSON(), authorization: request.headers().authorization });
        if (submissions.length === 1) await pendingLogin;
        if (rejectCode) {
          await route.fulfill({ status: 401, json: { error: "Code expired. Ask for a new code." } });
          return;
        }
        signedIn = true;
      }
      await route.fulfill({ status: signedIn ? 200 : 401, json: signedIn ? { user_id: "alice" } : { error: "Sign in required" } });
      return;
    }
    const responses = {
      "/v1/capabilities": { compute: { available: true }, "compute-teams": { enabled: true }, "ui-web": { enabled: true } },
      "/v1/bots": { bots: [] },
      "/v1/groups": { groups: [] },
      "/v1/activity": { items: [] },
      "/v1/learned-reviews": { reviews: [] },
      "/v1/task-approvals": { records: [] },
      "/v1/chat-turns": { turn: null },
    };
    assert.ok(path in responses, `Unexpected API request: ${path}`);
    await route.fulfill({ json: responses[path] });
  });

  await page.goto(origin);
  const code = page.getByLabel("One-time code");
  await code.waitFor();
  assert.equal(await page.evaluate(() => window.isSecureContext), true);
  assert.equal(await page.getByRole("button", { name: "Install Lobslaw" }).count(), 0, "install control appeared on login");
  assert.equal(await page.locator(".login-cmd").isVisible(), false);
  await page.getByText("Need a code?", { exact: true }).click();
  assert.equal(await page.locator(".login-cmd").isVisible(), true);
  await page.getByText("Need a code?", { exact: true }).click();

  await code.fill("123");
  assert.equal(await page.getByRole("button", { name: "Sign in with code" }).isDisabled(), true);
  await code.press("Enter");
  assert.equal(submissions.length, 0, "incomplete code was submitted");
  await code.fill("123 456");
  await code.press("Enter");
  await page.waitForFunction(() => document.querySelector(".login-form")?.getAttribute("aria-busy") === "true");
  await page.getByRole("button", { name: "Signing in…", exact: true }).waitFor();
  // Repeated submit events must not exchange the same one-time code twice.
  await page.locator(".login-form").evaluate((form) => {
    form.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    form.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
  });
  releaseLogin();
  await page.getByRole("alert").getByText("Code expired. Ask for a new code.", { exact: true }).waitFor();
  assert.equal(submissions.length, 1);
  assert.deepEqual(submissions[0].body, { code: "123456" });
  assert.equal(await code.inputValue(), "123 456", "failed sign-in lost the entered code");
  await page.getByRole("button", { name: "Try again", exact: true }).click();
  assert.equal(await code.evaluate((input) => input === document.activeElement), true);
  rejectCode = false;
  await code.press("Enter");
  await page.locator(".shell").waitFor();
  assert.equal(submissions.length, 2);

  const menu = page.getByRole("button", { name: "Open menu", exact: true });
  const sidebar = page.locator("#sidebar");
  assert.equal(await sidebar.isVisible(), false, "closed drawer remains keyboard-accessible");
  await menu.click();
  await page.getByRole("button", { name: "Close menu", exact: true }).click();
  await page.waitForFunction(() => getComputedStyle(document.querySelector("#sidebar")).visibility === "hidden");
  assert.equal(await menu.evaluate((button) => button === document.activeElement), true);
  await menu.click();
  await page.keyboard.press("Escape");
  assert.equal(await menu.getAttribute("aria-expanded"), "false");
  await menu.click();
  await sidebar.getByRole("link", { name: "Task approvals", exact: true }).click();
  await page.waitForFunction(() => document.querySelector(".topbar-nm")?.textContent === "Task approvals");
  assert.equal(await menu.getAttribute("aria-expanded"), "false", "drawer did not close after navigation");
  await menu.click();
  assert.equal(await sidebar.getByRole("link", { name: "Task approvals", exact: true }).getAttribute("class"), "on");
  await page.keyboard.press("Escape");

  // A fresh sign-in still exposes the token fallback with predictable focus.
  signedIn = false;
  await page.evaluate(() => window.dispatchEvent(new Event("lobslaw:unauthorized")));
  await code.waitFor();
  const tokenToggle = page.getByRole("button", { name: "I have a JWT", exact: true });
  await tokenToggle.click();
  const token = page.getByLabel("JWT", { exact: true });
  assert.equal(await token.evaluate((input) => input === document.activeElement), true);
  await token.press("Escape");
  assert.equal(await token.count(), 0);
  assert.equal(await tokenToggle.evaluate((button) => button === document.activeElement), true);
  await tokenToggle.click();
  await token.fill("test-enrolled-token");

  for (const width of [320, 390, 1440]) {
    await page.setViewportSize({ width, height: 844 });
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true, `login overflow at ${width}px`);
  }
  await page.emulateMedia({ reducedMotion: "reduce" });
  assert.ok(await page.locator(".login-card").evaluate((card) => parseFloat(getComputedStyle(card).animationDuration) < 0.001), "reduced-motion preference ignored");
  await page.getByRole("button", { name: "Sign in with JWT", exact: true }).click();
  await page.locator(".shell").waitFor();
  assert.equal(submissions.at(-1).authorization, "Bearer test-enrolled-token");
  assert.equal(await page.getByRole("button", { name: "Install Lobslaw", exact: true }).isVisible(), true, "signed-in install action disappeared");
  assert.deepEqual(errors, []);
  console.log("UI: clean login, code/token submission, duplicate protection, error recovery, mobile navigation, responsive layout and reduced motion passed.");
} catch (error) {
  if (page && !page.isClosed()) console.error(await page.locator("body").innerText());
  throw error;
} finally {
  releaseLogin?.();
  await browser?.close();
  server.kill("SIGTERM");
  if (server.exitCode === null) await once(server, "exit");
}
