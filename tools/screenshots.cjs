// Renders the README screenshots from the real app UI (demo mode) and the
// real phone pairing page served by the daemon.
//
//   cd daemon && go build -o /tmp/norrsken-host . && cd ..
//   DAEMON_BIN=/tmp/norrsken-host node tools/screenshots.cjs
//
// Needs the "playwright" package (npm i -D playwright) with Chromium installed.
'use strict';
const { chromium } = require('playwright');
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn, execFileSync } = require('child_process');

const ROOT = path.resolve(__dirname, '..');
const APP = path.join(ROOT, 'app');
const OUT = path.join(ROOT, 'docs', 'screenshots');
const TYPES = { '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.png': 'image/png', '.woff2': 'font/woff2', '.json': 'application/json' };

function serve() {
  return new Promise((resolve) => {
    const srv = http.createServer((req, res) => {
      const p = path.join(APP, decodeURIComponent(req.url.split('?')[0]).replace(/^\/$/, '/index.html'));
      if (!p.startsWith(APP) || !fs.existsSync(p)) { res.writeHead(404); res.end(); return; }
      res.writeHead(200, { 'Content-Type': TYPES[path.extname(p)] || 'application/octet-stream' });
      fs.createReadStream(p).pipe(res);
    });
    srv.listen(0, '127.0.0.1', () => resolve(srv));
  });
}

// Deterministic "random" numbers so every run renders the same servers.
const SEED = () => {
  let a = 0x2f6b9e1d;
  Math.random = function () {
    a |= 0; a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
};
const FREEZE = '*{transition:none!important;caret-color:transparent!important}.demo-badge{display:none!important}';

async function main() {
  fs.mkdirSync(OUT, { recursive: true });
  const srv = await serve();
  const base = `http://127.0.0.1:${srv.address().port}/`;
  const browser = await chromium.launch();
  const tv = await browser.newContext({ viewport: { width: 1920, height: 1080 }, deviceScaleFactor: 1 });
  await tv.addInitScript(SEED);
  const page = await tv.newPage();
  const key = async (k, n = 1) => { for (let i = 0; i < n; i++) { await page.keyboard.press(k); await page.waitForTimeout(120); } };
  const shot = async (name) => {
    await page.waitForTimeout(500);
    await page.screenshot({ path: path.join(OUT, name + '.jpg'), type: 'jpeg', quality: 92 });
    console.log('wrote', name);
  };
  const open = async (q) => {
    await page.goto(base + 'index.html' + q);
    await page.addStyleTag({ content: FREEZE });
    await page.waitForTimeout(900);
  };

  // Status, connected.
  await open('?demo=connected');
  await shot('status');

  // Locations: OK on "Choose location", then one step right.
  await key('ArrowRight');
  await key('Enter');
  await page.waitForTimeout(400);
  await key('ArrowRight');
  await shot('locations');

  // City picker for the focused country.
  await key('Enter');
  await key('ArrowRight', 2);
  await shot('city-picker');
  await key('Escape');

  // Settings (rail), then Diagnostics.
  await page.click('.rail-item[data-screen="settings"]');
  await page.mouse.move(0, 0);
  await page.waitForTimeout(300);
  await shot('settings');
  await page.click('#diag-open');
  await page.mouse.move(0, 0);
  await page.waitForTimeout(500);
  await shot('diagnostics');

  // Connecting state.
  await open('?demo=');
  await key('Enter'); // Quick connect
  await page.waitForTimeout(300);
  await shot('connecting');

  // Account linking.
  await open('?demo=unlinked');
  await shot('link');

  // Phone pairing page, served by the real daemon.
  if (process.env.DAEMON_BIN) {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'norrsken-shot-'));
    const env = { ...process.env, NORRSKEN_STATE: path.join(tmp, 's'), NORRSKEN_RUN: path.join(tmp, 'r'), NORRSKEN_INITD: path.join(tmp, 'i'), NORRSKEN_FAKE_TUNNEL: '1' };
    const d = spawn(process.env.DAEMON_BIN, ['daemon'], { env, stdio: 'ignore' });
    await new Promise((r) => setTimeout(r, 800));
    const p = JSON.parse(execFileSync(process.env.DAEMON_BIN, ['ctl-json', '{"cmd":"pairStart"}'], { env }).toString()).data;
    const port = p.display.split(':').pop();
    const phone = await browser.newContext({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true });
    const pp = await phone.newPage();
    await pp.goto(`http://127.0.0.1:${port}/#482913`);
    await pp.fill('#token', 'e9f1c0a4d2b8…');
    await pp.waitForTimeout(300);
    await pp.screenshot({ path: path.join(OUT, 'phone.jpg'), type: 'jpeg', quality: 90 });
    console.log('wrote phone');
    execFileSync(process.env.DAEMON_BIN, ['ctl-json', '{"cmd":"shutdown"}'], { env });
    d.kill();
  }

  await browser.close();
  srv.close();
}

main().catch((e) => { console.error(e); process.exit(1); });
