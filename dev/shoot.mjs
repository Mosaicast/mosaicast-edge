// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors
//
// Screenshots of edge pages in light/dark × phone/desktop.
//   PW=<path to playwright> node dev/shoot.mjs <base-url> <outdir> <name>=<path>[~waitms] ...
// e.g. node dev/shoot.mjs http://127.0.0.1:8190 tmp/shots countdown=/ login=/_a/login~300
import { mkdirSync } from 'node:fs';
import { createRequire } from 'node:module';
const { chromium } = createRequire(import.meta.url)(process.env.PW ?? 'playwright');
const [base, out, ...shots] = process.argv.slice(2);
mkdirSync(out, { recursive: true });
const browser = await chromium.launch();
const errors = [];
for (const scheme of ['light', 'dark']) {
  for (const [vp, size] of [['desktop', { width: 1280, height: 860 }], ['phone', { width: 390, height: 844 }]]) {
    const ctx = await browser.newContext({ viewport: size, colorScheme: scheme, locale: process.env.LOCALE ?? 'en-US',
      deviceScaleFactor: vp === 'phone' ? 2 : 1 });
    if (process.env.SESSION) await ctx.addCookies([{ name: 'MOSAICAST_SESSION', value: process.env.SESSION, url: base }]);
    for (const spec of shots) {
      const i = spec.indexOf('=');
      const name = spec.slice(0, i), rest = spec.slice(i + 1);
      const [path, wait] = rest.split('~');
      const page = await ctx.newPage();
      // the document's own 503/404 is the point of those pages, not an error
      page.on('console', (m) => { if (m.type() === 'error' && !/status of (503|404)/.test(m.text())) errors.push(`${name}: ${m.text()}`); });
      page.on('pageerror', (e) => errors.push(`${name}: ${e.message}`));
      await page.goto(base + path, { waitUntil: 'load' });
      await page.waitForTimeout(Number(wait ?? 400));
      await page.screenshot({ path: `${out}/${name}-${scheme}-${vp}.png`, fullPage: true });
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth > innerWidth);
      if (overflow) errors.push(`${name} ${scheme} ${vp}: horizontal overflow`);
      await page.close();
    }
    await ctx.close();
  }
}
await browser.close();
if (errors.length) { console.log(errors.join('\n')); process.exitCode = 1; } else console.log('no console errors, no overflow');
