// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors
//
// Watches an edge page in a real browser and logs what the visitor sees: phase changes, the big digit,
// reloads, and when the real site arrives. Screenshots at each phase. Drive the policy from a shell meanwhile.
//   PW=<playwright> node dev/launch.mjs <url> <outdir> [seconds=60]
import { mkdirSync } from 'node:fs';
import { createRequire } from 'node:module';
const { chromium } = createRequire(import.meta.url)(process.env.PW ?? 'playwright');
const [url, out, secs = '60'] = process.argv.slice(2);
mkdirSync(out, { recursive: true });
const browser = await chromium.launch();
const ctx = await browser.newContext({ viewport: { width: 1100, height: 760 }, locale: 'en-US' });
if (process.env.SESSION) await ctx.addCookies([{ name: 'MOSAICAST_SESSION', value: process.env.SESSION, url }]);
const page = await ctx.newPage();
const t0 = Date.now(); // the test harness's own stopwatch, not the page's clock
const log = (...a) => console.log(((Date.now() - t0) / 1000).toFixed(1).padStart(6) + 's', ...a);
page.on('pageerror', (e) => log('PAGE ERROR', e.message));
page.on('framenavigated', (f) => { if (f === page.mainFrame()) log('navigated', f.url()); });
await page.goto(url);
let last = '';
const end = t0 + Number(secs) * 1000;
while (Date.now() < end) {
  const s = await page.evaluate(() => {
    const d = document.documentElement;
    if (!document.getElementById('edge-data')) return 'SITE: ' + document.title;
    const big = document.querySelector('[data-edge-final]')?.textContent?.trim();
    const units = [...document.querySelectorAll('[data-edge-unit]')].map((e) => e.textContent).join(':');
    return `${d.dataset.page}/${d.dataset.phase ?? '-'} ${d.dataset.phase === 'final' ? 'big=' + big : units}`;
  }).catch((e) => 'eval failed: ' + e.message.split('\n')[0]);
  const key = s.replace(/\d\d:\d\d:\d\d:\d\d/, 'HH');
  if (key !== last) {
    log(s);
    const tag = s.split(' ')[0].replace(/[^a-z-]/gi, '_');
    await page.screenshot({ path: `${out}/${String(Date.now() - t0).padStart(6, '0')}-${tag}.png` }).catch(() => {});
    last = key;
  }
  if (s.startsWith('SITE')) break;
  await page.waitForTimeout(200);
}
await browser.close();
