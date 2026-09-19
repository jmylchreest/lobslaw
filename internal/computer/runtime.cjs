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
const observationLimits = { elements: 80, text: 6000, label: 160, nodes: 2000 };
const privateEntryLimit = 100;
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

// Runs in the page, with no access to Node, cookies, storage, or form values.
// The same target classification gates automatic fill and redacts observations.
function inspectDOM(root, options) {
  const privateValues = [...document.querySelectorAll('input[type=password],input[autocomplete=current-password],input[autocomplete=one-time-code]')].map(node => node.value).filter(Boolean);
  function selector(node) {
    const parts = [];
    while (node && node.nodeType === Node.ELEMENT_NODE) {
      const siblings = node.parentElement ? [...node.parentElement.children].filter(n => n.tagName === node.tagName) : [node];
      parts.unshift(`${node.tagName.toLowerCase()}:nth-of-type(${siblings.indexOf(node) + 1})`); node = node.parentElement;
    }
    return parts.join(' > ');
  }
  function credentialField(node) {
    if (!node.matches('input,textarea,select,[contenteditable]')) return false;
    const type = (node.getAttribute('type') || '').toLowerCase();
    const autocomplete = (node.getAttribute('autocomplete') || '').toLowerCase();
    const identity = ['id', 'name', 'aria-label', 'placeholder'].map(k => node.getAttribute(k) || '').join(' ');
    return ['password', 'email', 'tel'].includes(type) || /password|username|one-time-code|email|tel|cc-/.test(autocomplete)
      || /password|passcode|secret|token|api[_ -]?key|credential|login|sign.?in|otp|username|email|auth|credit|card.?number|cvv|cvc|\bpin\b/i.test(identity);
  }
  function sensitive(node) {
    const form = node.closest('form');
    return credentialField(node) || !!form && [...form.querySelectorAll('input,textarea,select,[contenteditable]')].some(credentialField);
  }
  function redact(text) {
    for (const value of privateValues) text = text.replaceAll(value, '[redacted]');
    return text.replace(/\b(password|passcode|secret|token|api[_ -]?key|authorization)\s*[:=]\s*\S+/gi, '$1: [redacted]')
      .replace(/\b[A-Za-z0-9_/-]{32,}\b/g, '[redacted]');
  }
  function describe(node) {
    const secret = sensitive(node);
    const editable = node.matches('input,textarea,select,[contenteditable]');
    const label = editable ? (node.getAttribute('aria-label') || [...(node.labels || [])].map(label => label.textContent).join(' ') || node.getAttribute('placeholder') || '') : (node.textContent || '');
    return { selector: selector(node), tag: node.tagName.toLowerCase(), type: node instanceof HTMLInputElement ? node.type : '', text: options.saturated ? '' : secret ? 'Manual credential entry' : redact(label.trim()).slice(0, options.label), sensitive: secret };
  }
  if (options.target) return describe(root);
  const controls = [...root.querySelectorAll('a,button,input,textarea,select,[role=button],[contenteditable]')].filter(node => node.checkVisibility());
  const manual_required = options.saturated || controls.some(sensitive);
  const elements = controls.slice(0, options.elements).map(describe);
  let text = '';
  if (!manual_required) {
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    let node, count = 0;
    while ((node = walker.nextNode()) && count++ < options.nodes && text.length < options.text) {
      const parent = node.parentElement;
      if (!parent || parent.closest('script,style,noscript,input,textarea,select,[contenteditable],[data-private],[data-sensitive]') || !parent.checkVisibility()) continue;
      text += ' ' + redact(node.textContent.trim());
    }
  }
  return { origin: location.origin, text: manual_required ? 'Sensitive form: ask the owner to take control for credential entry.' : text.trim().slice(0, options.text), elements, manual_required };
}

async function main() {
  const privateValues = []; let saturated = false;
  // A page can replace its JavaScript builtins and observe their arguments.
  // Never inject values entered on another origin into that realm for redaction.
  function redactObservation(value) {
    if (typeof value === 'string') {
      for (const secret of privateValues) value = value.replaceAll(secret, '[redacted]');
      return value;
    }
    if (Array.isArray(value)) return value.map(redactObservation);
    if (value && typeof value === 'object') return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, redactObservation(item)]));
    return value;
  }
  execFileSync(process.env.COMPUTER_IP, ['link', 'set', 'lo', 'up']);
  const socketPath = process.env.COMPUTER_PROXY;
  const proxy = http.createServer((req, res) => {
    const headers = { ...req.headers };
    delete headers['proxy-authorization'];
    delete headers['x-lobslaw-role'];
    const upstream = http.request({ socketPath, method: req.method, path: req.url, headers }, reply => {
      res.writeHead(reply.statusCode, reply.headers); reply.pipe(res);
    });
    upstream.on('error', () => { res.writeHead(502); res.end(); });
    req.pipe(upstream);
  });
  proxy.on('connect', (req, client, head) => {
    const upstream = net.connect(socketPath);
    upstream.on('connect', () => {
      upstream.write(`CONNECT ${req.url} HTTP/1.1\r\nHost: ${req.url}\r\n\r\n`);
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
    let ok = false, result = {}, code = '';
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
            const info = await target.evaluate(inspectDOM, { ...observationLimits, target: true });
            if (step.automated && info.sensitive) { const err = new Error('manual'); err.manual = true; throw err; }
            result.selector = info.selector;
            await target.click();
          }
          break;
        case 'fill': {
          const target = page.locator(step.selector || ':focus');
          const info = await target.evaluate(inspectDOM, { ...observationLimits, target: true });
          if (step.automated && info.sensitive) { const err = new Error('manual'); err.manual = true; throw err; }
          result.selector = info.selector;
          await target.fill(step.value || '');
          if (!step.automated && step.value && !privateValues.includes(step.value)) {
            if (privateValues.length < privateEntryLimit) privateValues.push(step.value); else saturated = true;
          }
          break;
        }
        case 'press':
          if (!/^(Enter|Tab|Escape|Backspace|Delete|ArrowUp|ArrowDown|ArrowLeft|ArrowRight|Space|Control\+a)$/.test(step.value)) throw new Error('unsupported key');
          if (step.automated && (await page.locator('body').evaluate(inspectDOM, observationLimits)).manual_required) { const err = new Error('manual'); err.manual = true; throw err; }
          await page.keyboard.press(step.value); break;
        case 'wait': {
          const target = page.locator(step.selector);
          await target.waitFor({ state: 'visible' });
          result.selector = await target.evaluate(structuralSelector);
          break;
        }
        case 'capture':
          if (!step.automated) result.screenshot = (await page.screenshot({ type: 'png' })).toString('base64');
          break;
        default: throw new Error('unknown action');
      }
      // Local-only restore metadata. Never part of a recording or API response.
      if (/^https?:\/\//.test(page.url())) fs.writeFileSync(locationFile, page.url(), { mode: 0o600 });
      if (step.automated) result.observation = redactObservation(await page.locator('body').evaluate(inspectDOM, { ...observationLimits, saturated }));
      ok = true;
    } catch (err) { if (err.manual) code = 'manual'; /* Never emit diagnostic values. */ }
    process.stdout.write(JSON.stringify({ ok, result, code }) + '\n');
  }
  await context.close(); proxy.close();
}
main().catch(() => { process.exit(1); });
