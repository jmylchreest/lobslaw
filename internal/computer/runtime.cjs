// Executed only inside the Go subprocess sandbox. Stdio is the sole control
// channel; there is no CDP listener and no externally reachable viewer.
const { chromium } = require(process.env.COMPUTER_PLAYWRIGHT);
const http = require('node:http');
const net = require('node:net');
const readline = require('node:readline');
const { execFileSync } = require('node:child_process');
const fs = require('node:fs');
const path = require('node:path');
const actionTimeout = 20000;
const viewport = { width: 1280, height: 800 };
const roleHeaders = { 'X-Lobslaw-Role': 'computer', 'Proxy-Authorization': 'Basic ' + Buffer.from('computer:_').toString('base64') };
// Structural selectors contain no field values, labels, IDs, or page text.
function structuralSelector(node) {
  const segments = [];
  while (node && node.nodeType === Node.ELEMENT_NODE) {
    const tag = node.tagName.toLowerCase();
    const siblings = node.parentElement ? [...node.parentElement.children].filter(n => n.tagName === node.tagName) : [node];
    segments.unshift(`${tag}:nth-of-type(${siblings.indexOf(node) + 1})`); node = node.parentElement;
  }
  return segments.join(' > ');
}

async function main() {
  execFileSync(process.env.COMPUTER_IP, ['link', 'set', 'lo', 'up']);
  const socketPath = process.env.COMPUTER_PROXY;
  const proxy = http.createServer((req, res) => {
    const headers = { ...req.headers, ...roleHeaders };
    delete headers['proxy-authorization'];
    delete headers['x-lobslaw-role'];
    Object.assign(headers, roleHeaders);
    const upstream = http.request({ socketPath, method: req.method, path: req.url, headers }, reply => {
      res.writeHead(reply.statusCode, reply.headers); reply.pipe(res);
    });
    upstream.on('error', () => { res.writeHead(502); res.end(); });
    req.pipe(upstream);
  });
  proxy.on('connect', (req, client, head) => {
    const upstream = net.connect(socketPath);
    upstream.on('connect', () => {
      upstream.write(`CONNECT ${req.url} HTTP/1.1\r\nHost: ${req.url}\r\nX-Lobslaw-Role: computer\r\nProxy-Authorization: ${roleHeaders['Proxy-Authorization']}\r\n\r\n`);
      if (head.length) upstream.write(head);
      client.pipe(upstream); upstream.pipe(client);
    });
    upstream.on('error', () => client.destroy()); client.on('error', () => upstream.destroy());
    client.on('close', () => upstream.destroy());
  });
  await new Promise(resolve => proxy.listen(0, '127.0.0.1', resolve));
  const context = await chromium.launchPersistentContext(path.join(process.env.COMPUTER_PROFILE, 'profile'), {
    executablePath: process.env.COMPUTER_CHROMIUM, headless: true, viewport,
    acceptDownloads: false, serviceWorkers: 'block',
    proxy: { server: `http://127.0.0.1:${proxy.address().port}`, bypass: '<-loopback>' },
    args: ['--disable-quic', '--disable-dev-shm-usage', '--disable-gpu', '--in-process-gpu', '--force-webrtc-ip-handling-policy=disable_non_proxied_udp'],
  });
  context.setDefaultTimeout(actionTimeout);
  let page = context.pages()[0] || await context.newPage();
  context.on('page', opened => { page = opened; });
  // Only http(s) may reach the network. The netns has no network interfaces
  // other than loopback; Chromium cannot bypass the UDS proxy using UDP/DNS.
  await context.route('**/*', route => {
    const scheme = new URL(route.request().url()).protocol;
    return ['http:', 'https:'].includes(scheme) ? route.continue() : route.abort();
  });
  const locationFile = path.join(process.env.COMPUTER_PROFILE, 'location');
  try {
    const location = fs.readFileSync(locationFile, 'utf8');
    if (/^https?:\/\//.test(location)) await page.goto(location, { waitUntil: 'domcontentloaded' });
  } catch { /* First launch has no last location. */ }
  const input = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
  for await (const line of input) {
    let ok = false, result = {};
    try {
      const step = JSON.parse(line);
      switch (step.action) {
        case 'start': break;
        case 'navigate': {
          const response = await page.goto(step.url, { waitUntil: 'domcontentloaded' });
          if (response && !response.ok()) throw new Error('navigation refused');
          break;
        }
        case 'click':
          if (typeof step.x === 'number' && typeof step.y === 'number') {
            result.selector = await page.evaluate(({ x, y }) => {
              let node = document.elementFromPoint(x, y); const segments = [];
              while (node && node.nodeType === Node.ELEMENT_NODE) {
                const tag = node.tagName.toLowerCase();
                const siblings = node.parentElement ? [...node.parentElement.children].filter(n => n.tagName === node.tagName) : [node];
                segments.unshift(`${tag}:nth-of-type(${siblings.indexOf(node) + 1})`); node = node.parentElement;
              }
              return segments.join(' > ');
            }, step);
            await page.mouse.click(step.x, step.y);
          } else {
            const target = page.locator(step.selector);
            result.selector = await target.evaluate(structuralSelector);
            await target.click();
          }
          break;
        case 'fill': await page.locator(step.selector || ':focus').fill(step.value || ''); break;
        case 'press':
          if (!/^(Enter|Tab|Escape|Backspace|Delete|ArrowUp|ArrowDown|ArrowLeft|ArrowRight|Space|Control\+[a-z])$/.test(step.value)) throw new Error('unsupported key');
          await page.keyboard.press(step.value); break;
        case 'wait': {
          const target = page.locator(step.selector);
          await target.waitFor({ state: 'visible' });
          result.selector = await target.evaluate(structuralSelector);
          break;
        }
        case 'capture': result.screenshot = (await page.screenshot({ type: 'png' })).toString('base64'); break;
        default: throw new Error('unknown action');
      }
      // Local-only restore metadata. Never part of a recording or API response.
      if (/^https?:\/\//.test(page.url())) fs.writeFileSync(locationFile, page.url(), { mode: 0o600 });
      ok = true;
    } catch { /* Deliberately redact Playwright diagnostics and entered values. */ }
    process.stdout.write(JSON.stringify({ ok, result }) + '\n');
  }
  await context.close(); proxy.close();
}
main().catch(() => { process.exit(1); });
