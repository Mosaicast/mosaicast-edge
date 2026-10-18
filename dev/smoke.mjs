// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors
//
// Rehearsal: the real artifact (the Docker image, read-only, memory-limited, cache on a volume, a theme
// mounted read-only) in front of core's dev instance named "edge", driven through dev/policy, checked
// in a real browser and over HTTP. Prints a pass/fail table at the end.
//
//   PW=<path to playwright> node dev/smoke.mjs [--theme DIR] [--core-ref REF] [--restart-core] [--skip-launch] [--skip-expiry]
//
//   --theme DIR      theme to mount (default: none, the edge's own look)
//   --image REF      test that published image (e.g. ghcr.io/mosaicast/mosaicast-edge:0.1.0) instead of
//                    building one from this checkout
//   --core-ref REF   move core's "edge" instance to REF first (a commit or a release tag such as v0.8.4;
//                    the same source the release image is built from); --core is the old name
//   --discord        start core with dummy Discord credentials, so the provider list and the OAuth
//                    redirect are real (needs a core that allows this next to dev-login; without it the
//                    two OAuth checks are reported as SKIP)
//   --restart-core   also restart core mid-countdown and while open (~15 s each)
//   --skip-launch    skip the launch runs (open now, the opening at opensAt, the admin page; ~5 min)
//   --skip-expiry    skip the ~100 s artwork-expiry run
//   --only REGEX     run only the checks whose name matches (case-insensitive), e.g. --only 'admin page'
//   --plugin-dir DIR a built plugin (its plugin.tgz unpacked) to load into core; repeatable. Adds the
//                    plugin rows: gated while not open, rendered through the edge once open
//
// Between the edge and core sits a small counting proxy, so the run can tell how often the edge asked
// core who someone is, and can make core unreachable for the edge (coreDown) without stopping it. Leaves the policy at "open" and removes its containers when done.
import { createHash } from 'node:crypto';
import { execFile, execFileSync, spawn, spawnSync } from 'node:child_process';
import { chmodSync, cpSync, mkdtempSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync } from 'node:fs';
import http from 'node:http';
import { createRequire } from 'node:module';
import { Worker } from 'node:worker_threads';
import { tmpdir } from 'node:os';
import { dirname, extname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const { chromium } = createRequire(import.meta.url)(process.env.PW ?? 'playwright');
const here = dirname(fileURLToPath(import.meta.url));
const repo = resolve(here, '..');
const args = process.argv.slice(2);
const opt = (name) => { const i = args.indexOf(name); return i >= 0 ? args[i + 1] : undefined; };
const flag = (name) => args.includes(name);
const CORE = process.env.CORE ?? resolve(repo, '../mosaicast-core');
const TOKEN = process.env.EDGE_CORE_TOKEN ?? 'dev-edge-token';
const PORT = Number(opt('--port') ?? 8290);
const EDGE = `http://127.0.0.1:${PORT}`;
const theme = opt('--theme') ? resolve(opt('--theme')) : '';
const pluginDirs = args.flatMap((a, i) => (a === '--plugin-dir' ? [resolve(args[i + 1])] : []));
const plugins = pluginDirs.map((d) => JSON.parse(readFileSync(join(d, 'plugin.json'), 'utf8')));
const coreArgs = ['--app-arg', `--mosaicast.edge.token=${TOKEN}`,
  ...(flag('--discord') ? ['--app-arg', '--DISCORD_CLIENT_ID=dummy', '--app-arg', '--DISCORD_CLIENT_SECRET=dummy'] : []),
  ...(pluginDirs.length ? pluginDirs.flatMap((d) => ['--plugin-dir', d]) : ['--no-plugins'])]; // never a previous run's plugins

const sh = (cmd, a, o = {}) => execFileSync(cmd, a, { encoding: 'utf8', ...o }).trim();
const policy = (...a) => sh(join(here, 'policy'), a);
const policyWith = (env, ...a) => sh(join(here, 'policy'), a, { env: { ...process.env, ...env } });
const instance = (...a) => sh(join(CORE, 'dev/instance.sh'), ['--name', 'edge', ...a], { stdio: ['ignore', 'pipe', 'inherit'] });
// without blocking this process, so the browser checks keep running while core restarts
const instanceAsync = (...a) => new Promise((res, rej) => {
  spawn(join(CORE, 'dev/instance.sh'), ['--name', 'edge', ...a], { stdio: 'ignore' }).on('close', (c) => (c === 0 ? res() : rej(new Error(`instance.sh exited ${c}`))));
});
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const settle = () => sleep(3000); // the edge polls every 2 s
let ipN = 10;
const freshIP = () => `198.51.100.${ipN++}`; // each check its own client address, so rate limits stay per check

// ---------- results ----------
const results = [];
class Skip extends Error {}
const only = opt('--only') ? new RegExp(opt('--only'), 'i') : null;
async function check(name, fn) {
  if (only && !only.test(name)) return;
  let detail = '';
  try {
    detail = (await fn()) ?? '';
    results.push({ name, result: 'PASS', detail });
  } catch (e) {
    results.push({ name, result: e instanceof Skip ? 'SKIP' : '**FAIL**', detail: e.message });
  }
  const r = results.at(-1);
  console.log(`${r.result.replaceAll('*', '').padEnd(5)} ${name}${r.detail ? ' — ' + r.detail : ''}`);
}
const skip = (why) => { throw new Skip(why); };
// a SKIP whose reason says another test covers it is reported as such
const covered = (r) => r.result === 'SKIP' && r.detail.startsWith('covered by');
const expect = (cond, msg) => { if (!cond) throw new Error(msg); };

// ---------- core, image, proxy, container ----------
const coreRef = opt('--core-ref') ?? opt('--core');
if (coreRef) { console.log(`core → ${coreRef} …`); instance('restart', '--core', coreRef, ...coreArgs); }
else { console.log(pluginDirs.length ? 'core: loading the plugins …' : 'core: without plugins …'); instance('restart', ...coreArgs); }
const env = Object.fromEntries(instance('env').split('\n').filter((l) => l.includes('=')).map((l) => {
  const [k, ...v] = l.replace(/^export /, '').split('='); return [k, v.join('=').replace(/^['"]|['"]$/g, '')];
}));
const coreURL = new URL(env.MC_APP_URL);
const coreSHA = env.MC_CORE_SHA ?? '?';
console.log(`core ${coreURL.origin} @ ${coreSHA}; building the image …`);
// --image REF tests a published image (the release) instead of one built from this checkout
const IMAGE = opt('--image') ?? 'mosaicast-edge:rehearsal';
if (opt('--image')) {
  console.log(`pulling ${IMAGE} …`);
  sh('docker', ['pull', '-q', IMAGE], { stdio: ['ignore', 'pipe', 'inherit'] });
} else {
  sh('docker', ['build', '-q', '-t', IMAGE, repo], { stdio: ['ignore', 'pipe', 'inherit'] });
}
const imageVersion = sh('docker', ['run', '--rm', IMAGE, 'version']);

let meCalls = 0;
let coreDown = false; // true: the edge's connections to core are cut, as when core is gone
const proxy = http.createServer((req, res) => {
  if (coreDown) { req.socket.destroy(); return; }
  if (req.url.split('?')[0] === '/api/me') meCalls++;
  const up = http.request({ host: coreURL.hostname, port: coreURL.port, method: req.method, path: req.url, headers: req.headers }, (r) => {
    res.writeHead(r.statusCode, r.headers);
    r.pipe(res);
  });
  up.on('error', () => { res.destroy(); });
  req.pipe(up);
});
await new Promise((r) => proxy.listen(0, '127.0.0.1', r));
const proxyURL = `http://127.0.0.1:${proxy.address().port}`;

const containers = [];
function startEdge(name, port, themeDir, extraEnv = {}) {
  spawnSync('docker', ['rm', '-f', name]); spawnSync('docker', ['volume', 'rm', '-f', name + '-cache']);
  const e = { EDGE_UPSTREAM: proxyURL, EDGE_LISTEN: `127.0.0.1:${port}`, EDGE_CORE_TOKEN: TOKEN, EDGE_TIMEZONE: 'Europe/Berlin',
    ...(themeDir ? { EDGE_THEME_DIR: '/theme' } : {}), ...extraEnv };
  sh('docker', ['run', '-d', '--name', name, '--network', 'host', '--read-only', '--memory', '128m', '--memory-swap', '128m',
    '--security-opt', 'no-new-privileges:true', '--cap-drop', 'ALL', '-v', `${name}-cache:/cache`,
    ...(themeDir ? ['-v', `${themeDir}:/theme:ro`] : []),
    ...Object.entries(e).flatMap(([k, v]) => ['-e', `${k}=${v}`]), IMAGE]);
  containers.push(name);
}
async function healthy(url) {
  for (let i = 0; i < 40; i++) {
    try { if ((await fetch(url + '/_a/healthz')).ok) return; } catch { /* starting */ }
    await sleep(250);
  }
  throw new Error('edge did not become healthy: ' + sh('docker', ['logs', '--tail', '20', containers.at(-1)]));
}
// asynchronous: a blocking call here would stall the counting proxy on this thread
const memMB = (name) => new Promise((res) => execFile('docker', ['stats', '--no-stream', '--format', '{{.MemUsage}}', name],
  { encoding: 'utf8' }, (_, stdout) => res(parseMem((stdout ?? '').trim()))));
const parseMem = (out) => {
  const m = out.match(/^([\d.]+)\s*([KMG]i?B)/);
  if (!m) return NaN;
  return Number(m[1]) * ({ KiB: 1 / 1024, KB: 1 / 1000, MiB: 1, MB: 1, GiB: 1024, GB: 1000 }[m[2]] ?? 1);
};

const cleanup = () => { for (const c of containers) { spawnSync('docker', ['rm', '-f', c]); spawnSync('docker', ['volume', 'rm', '-f', c + '-cache']); } };
process.on('exit', cleanup);
process.on('SIGINT', () => process.exit(130));
startEdge('edge-smoke', PORT, theme, { EDGE_LONGPOLL_MAX: '1500' });
await healthy(EDGE);


// ---------- browser helpers ----------
const browser = await chromium.launch();
const sessions = { admin: policy('session', 'admin'), podcaster: policy('session', 'podcaster'), fan: policy('session', 'fan') };
async function visit(session, o = {}) {
  const ctx = await browser.newContext({ locale: o.locale ?? 'en-GB', viewport: o.viewport ?? { width: 1100, height: 760 },
    extraHTTPHeaders: { 'X-Forwarded-For': o.ip ?? freshIP() } });
  if (session) await ctx.addCookies([{ name: 'MOSAICAST_SESSION', value: session, url: EDGE, httpOnly: true, sameSite: 'Lax' }]);
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  page.on('console', (m) => {
    if (m.type() !== 'error' || /status of (503|404)/.test(m.text())) return;
    const at = m.location()?.url ?? '';
    if (/status of 401/.test(m.text()) && new URL(at, EDGE).pathname === '/api/me') return; // core's shell asking who an anonymous visitor is
    errors.push(`${m.text()}${at ? ' @ ' + at : ''}`);
  });
  const res = await page.goto(EDGE + (o.path ?? '/'));
  return { ctx, page, res, errors };
}
const state = (page) => page.evaluate(() => {
  const d = document.documentElement;
  return { edge: !!document.getElementById('edge-data'), page: d.dataset.page, phase: d.dataset.phase,
    units: [...document.querySelectorAll('[data-edge-unit]')].map((e) => e.textContent).join(':'),
    final: document.querySelector('[data-edge-final]')?.textContent?.trim() };
});
async function waitForSite(page, ms) {
  const end = performance.now() + ms;
  while (performance.now() < end) {
    const s = await state(page).catch(() => null);
    if (s && !s.edge) return true;
    await sleep(250);
  }
  return false;
}
async function waitFor(page, pred, ms) {
  const end = performance.now() + ms;
  let s = null;
  while (performance.now() < end) {
    s = await state(page).catch(() => null);
    if (s && pred(s)) return s;
    await sleep(250);
  }
  throw new Error(`waited ${ms / 1000}s, last ${JSON.stringify(s)}`);
}
const edgeState = async (base = EDGE) => (await fetch(base + '/_a/state')).json();
const get = (path, headers = {}, method = 'GET', port = PORT) => new Promise((res) => {
  const r = http.request({ host: '127.0.0.1', port, path, method, headers }, (resp) => {
    let body = ''; resp.setEncoding('utf8'); resp.on('data', (c) => { body += c; }); resp.on('end', () => res({ status: resp.statusCode, headers: resp.headers, body }));
  });
  r.on('error', (e) => res({ status: 'ERR ' + e.code, headers: {}, body: '' })); r.end();
});
const cookie = (v) => ({ Cookie: `MOSAICAST_SESSION=${v}` });
const html = { Accept: 'text/html' };

// ---------- checks ----------
await check('built image forwards Host and X-Forwarded-* unchanged, appends to X-Forwarded-For (echo upstream)', async () => {
  // a 10-line upstream that answers with the request it got, and an open policy
  const echo = http.createServer((req, res) => {
    if (req.url === '/api/site/access') {
      res.writeHead(200, { 'Content-Type': 'application/json' }).end('{"state":"open","passFloor":"podcaster","staffLogin":{"visible":false,"providers":[]},"revision":1}');
      return;
    }
    res.writeHead(200, { 'Content-Type': 'application/json' }).end(JSON.stringify({ url: req.url, headers: req.headers }));
  });
  await new Promise((r) => echo.listen(0, '127.0.0.1', r));
  try {
    const saved = proxyURL;
    startEdge('edge-smoke-echo', PORT + 2, '', { EDGE_UPSTREAM: `http://127.0.0.1:${echo.address().port}` });
    await healthy(`http://127.0.0.1:${PORT + 2}`);
    await sleep(500);
    const plesk = { Host: 'launch.example', 'X-Forwarded-Proto': 'https', 'X-Forwarded-Host': 'launch.example', 'X-Forwarded-Port': '443', 'X-Forwarded-For': '203.0.113.9' };
    for (const path of ['/oauth2/authorization/discord', '/p/wiki/start?x=1']) {
      const r = await get(path, { ...plesk, Accept: 'text/html' }, 'GET', PORT + 2);
      expect(r.status === 200, `${path}: ${r.status}`);
      const { url, headers: h } = JSON.parse(r.body);
      expect(url === path, `path ${url}`);
      expect(h.host === 'launch.example' && h['x-forwarded-proto'] === 'https' && h['x-forwarded-host'] === 'launch.example' && h['x-forwarded-port'] === '443',
        `${path}: ${JSON.stringify(h)}`);
      expect(h['x-forwarded-for'] === '203.0.113.9, 127.0.0.1', `${path}: X-Forwarded-For ${h['x-forwarded-for']}`);
    }
    void saved;
  } finally {
    spawnSync('docker', ['rm', '-f', 'edge-smoke-echo']);
    echo.close();
  }
  return 'Host, Proto, Host, Port kept; XFF "203.0.113.9, 127.0.0.1"';
});

await check('open: anonymous visitors get the site through the edge', async () => {
  policy('open'); await settle();
  const v = await visit(); const s = await state(v.page); await v.ctx.close();
  expect(!s.edge && v.res.status() === 200, `edge page ${s.page} / ${v.res.status()}`);
});

const providers = (await (await fetch(coreURL.origin + '/api/site/access')).json()).staffLogin.providers;
const byCore = 'covered by core test OAuthBehindProxyIntegrationTest (dev-login and a configured provider cannot coexist on a dev instance)';
await check('core lists the Discord provider; /_a/login links it', async () => {
  if (!providers.length) skip(byCore);
  const acc = await (await fetch(coreURL.origin + '/api/site/access')).json();
  expect(acc.staffLogin.providers.some((p) => p.id === 'discord'), `providers: ${JSON.stringify(acc.staffLogin.providers)}`);
  const r = await get('/_a/login', { 'Accept-Language': 'en' });
  expect(r.status === 200 && r.body.includes('href="/oauth2/authorization/discord"'), `login: ${r.status}`);
});

await check('/_a/login without providers: renders "no sign-in configured"', async () => {
  if (providers.length) skip('core has providers configured');
  const r = await get('/_a/login', { 'Accept-Language': 'en' });
  expect(r.status === 200 && r.body.includes('No sign-in is configured on this site.'), `login: ${r.status}`);
});

await check('OAuth start behind TLS: redirect_uri is the public https origin', async () => {
  if (!providers.length) skip(byCore);
  const r = await get('/oauth2/authorization/discord', { Host: 'launch.example', 'X-Forwarded-Proto': 'https', 'X-Forwarded-Host': 'launch.example', 'X-Forwarded-For': freshIP() });
  expect(r.status === 302, `status ${r.status}`);
  const loc = new URL(r.headers.location);
  const redirect = loc.searchParams.get('redirect_uri');
  expect(loc.hostname.endsWith('discord.com'), `Location ${r.headers.location}`);
  expect(redirect === 'https://launch.example/login/oauth2/code/discord', `redirect_uri=${redirect}`);
  return redirect;
});

await check('scheduled: countdown for anonymous visitors, legal links reachable, no console errors', async () => {
  policy('scheduled', '3600'); await settle();
  const v = await visit();
  const s = await state(v.page);
  const legal = await v.page.$$eval('[data-edge-legal] a', (as) => as.map((a) => a.href));
  await v.ctx.close();
  expect(s.page === 'countdown' && s.phase === 'wait' && v.res.status() === 200, JSON.stringify(s));
  expect(!v.errors.length, v.errors.join('; '));
  expect(legal.length > 0, 'no legal links');
  for (const href of legal) { const r = await fetch(href); expect(r.status === 200, `${href}: ${r.status}`); }
  return `${legal.length} legal links`;
});

await check('scheduled: admin and podcaster get the site, a fan gets the page', async () => {
  for (const [role, page] of [['admin', false], ['podcaster', false], ['fan', true]]) {
    const v = await visit(sessions[role]); const s = await state(v.page);
    const signedIn = await v.page.evaluate(() => document.documentElement.dataset.viewer);
    await v.ctx.close();
    expect(s.edge === page, `${role}: edge page = ${s.edge}`);
    if (page) expect(signedIn === 'signed-in', 'fan not marked signed in');
  }
});

await check('pass floor admin: podcaster refused, admin passes', async () => {
  policy('scheduled', '3600', 'admin'); await settle();
  const ip = freshIP();
  const p = await get('/', { ...html, ...cookie(sessions.podcaster), 'X-Forwarded-For': ip });
  const a = await get('/', { ...html, ...cookie(sessions.admin), 'X-Forwarded-For': ip });
  expect(p.body.includes('data-page="countdown"'), `podcaster: ${p.status}`);
  expect(!a.body.includes('id="edge-data"') && a.status === 200, `admin: ${a.status}`);
  policy('scheduled', '3600'); await settle();
});

await check('HTTP: non-HTML GET and POST get a plain 503 with Retry-After; HEAD on a page', async () => {
  const a = await get('/api/site', { Accept: 'application/json' });
  const b = await get('/api/anything', {}, 'POST');
  const h = await get('/', html, 'HEAD');
  expect(a.status === 503 && a.headers['retry-after'] && !a.body.includes('<html'), `GET json: ${a.status}`);
  expect(b.status === 503 && b.headers['retry-after'], `POST: ${b.status}`);
  expect(h.status === 200 && h.body === '' && h.headers['cache-control'] === 'no-store', `HEAD: ${h.status}`);
});

// A personal access token, minted directly at core with the session's CSRF token (§8.5).
async function mintToken(session) {
  const jar = { MOSAICAST_SESSION: session };
  const r0 = await fetch(coreURL.origin + '/api/meta', { headers: { Cookie: `MOSAICAST_SESSION=${session}` } });
  for (const c of r0.headers.getSetCookie()) { const [kv] = c.split(';'); const [k, ...v] = kv.split('='); jar[k] = v.join('='); }
  const headers = { Cookie: Object.entries(jar).map(([k, v]) => `${k}=${v}`).join('; '), 'X-XSRF-TOKEN': jar['XSRF-TOKEN'], 'Content-Type': 'application/json' };
  const r = await fetch(coreURL.origin + '/api/me/tokens', { method: 'POST', headers, body: JSON.stringify({ name: 'edge rehearsal' }) });
  expect(r.ok, `minting a token: ${r.status}`);
  const t = await r.json();
  return { secret: t.secret, revoke: () => fetch(coreURL.origin + '/api/me/tokens/' + t.id, { method: 'DELETE', headers }) };
}

await check('automation token (Authorization: Bearer) while scheduled: a podcaster\'s passes, an admin\'s is a podcaster at most, a made-up one gets the plain 503 for one lookup', async () => {
  policy('scheduled', '3600'); await settle();
  // only podcasters and admins can mint one (a fan's token is the edge's unit test)
  const pod = await mintToken(sessions.podcaster), adm = await mintToken(sessions.admin);
  try {
    const me = (t) => get('/api/me', { Accept: 'application/json', Authorization: 'Bearer ' + t, 'X-Forwarded-For': freshIP() });
    const p = await me(pod.secret), a = await me(adm.secret);
    expect(p.status === 200 && JSON.parse(p.body).role === 'podcaster', `podcaster token: ${p.status}`);
    expect(a.status === 200, `admin token, floor podcaster: ${a.status}`);
    const before = meCalls;
    const made = 'mcp_' + Buffer.from(crypto.getRandomValues(new Uint8Array(32))).toString('base64url');
    const m1 = await me(made), m2 = await me(made), junk = await me('mcp_not-a-token');
    expect([m1, m2, junk].every((r) => r.status === 503 && r.headers['retry-after']), `made-up: ${m1.status} ${m2.status} ${junk.status}`);
    expect(meCalls - before === 1, `made-up token twice + a malformed one cost core ${meCalls - before} lookups, want 1`);
    policy('scheduled', '3600', 'admin'); await settle(); // the cache holds the account's role; the cap applies per request
    const a2 = await me(adm.secret);
    expect(a2.status === 503, `admin token, floor admin: ${a2.status} (a token never counts as admin)`);
    return 'podcaster 200, admin 200 → 503 at floor admin, made-up 503 (1 lookup)';
  } finally {
    policy('scheduled', '3600'); await settle();
    await Promise.all([pod, adm].map((t) => t.revoke()));
  }
});

await check('rate limits per visitor through the edge: one client hitting /oauth2/authorization/* until 429 leaves another client unlimited', async () => {
  const A = freshIP(), B = freshIP();
  let n = 0, last = 0;
  while (n < 40 && last !== 429) { last = (await get('/oauth2/authorization/discord', { 'X-Forwarded-For': A })).status; n++; }
  expect(last === 429, `client A never limited after ${n} requests (last ${last})`);
  const b = await get('/oauth2/authorization/discord', { 'X-Forwarded-For': B });
  expect(b.status !== 429, `client B limited too (${b.status}): every visitor shares one bucket behind the edge`);
  return `A: 429 after ${n}; B: ${b.status}`;
});

if (plugins.length) {
  await check(`plugins (${plugins.map((p) => `${p.id} ${p.version}`).join(', ')}) while scheduled: pages, bundles and APIs gated for visitors; staff reach them`, async () => {
    policy('scheduled', '3600'); await settle();
    const active = await (await fetch(coreURL.origin + '/api/plugins/manifest')).json();
    for (const p of plugins) expect(active.some((a) => a.id === p.id && a.version === p.version), `core has not loaded ${p.id} ${p.version}: ${active.map((a) => a.id + ' ' + a.version).join(', ')}`);
    for (const p of plugins) {
      const ip = freshIP();
      const page = await get('/p/' + p.id, { ...html, 'X-Forwarded-For': ip });
      // a browser loads a module with Accept */*, which gets the page; a script loader gets the plain 503
      const bundle = await get(`/plugins/${p.id}/assets/${p.frontend.entry}`, { Accept: '*/*', 'X-Forwarded-For': ip });
      const bundleJS = await get(`/plugins/${p.id}/assets/${p.frontend.entry}`, { Accept: 'application/javascript', 'X-Forwarded-For': ip });
      const api = await get(`/api/plugins/${p.id}/data/site/x`, { Accept: 'application/json', 'X-Forwarded-For': ip });
      const manifest = await get('/api/plugins/manifest', { Accept: 'application/json', 'X-Forwarded-For': ip });
      expect(page.status === 200 && page.body.includes('data-page="countdown"'), `${p.id} page: ${page.status}`);
      expect(bundle.body.includes('data-page="countdown"') && !/javascript/.test(bundle.headers['content-type']), `${p.id} bundle (*/*): ${bundle.status} ${bundle.headers['content-type']}`);
      expect(bundleJS.status === 503 && api.status === 503 && manifest.status === 503, `${p.id} bundle ${bundleJS.status}, data ${api.status}, manifest ${manifest.status}`);
      const sb = await get(`/plugins/${p.id}/assets/${p.frontend.entry}`, { ...cookie(sessions.podcaster), 'X-Forwarded-For': ip });
      expect(sb.status === 200 && /javascript/.test(sb.headers['content-type']), `${p.id} bundle for staff: ${sb.status} ${sb.headers['content-type']}`);
    }
  });
  await check('plugins once open: each plugin page and an episode page with their slots render through the edge in a browser, no console error that core itself does not show', async () => {
    policy('open'); await settle();
    const out = [];
    // the same page straight from core: what it logs there is the plugin's own, not the edge's
    const direct = async (path) => {
      const ctx = await browser.newContext(); const page = await ctx.newPage(); const errs = new Set();
      page.on('console', (m) => { if (m.type() === 'error') errs.add(`${m.text().replace(/\(.*?\)/, '')} ${new URL(m.location()?.url ?? '/', coreURL).pathname}`); });
      await page.goto(coreURL.origin + path); await sleep(4000); await ctx.close();
      return errs;
    };
    for (const p of plugins) {
      const el = p.frontend.elements.find((e) => e.endsWith('-page')) ?? p.frontend.elements[0];
      const v = await visit(null, { path: '/p/' + p.id });
      try {
        await v.page.waitForFunction((n) => customElements.get(n) && document.querySelector(n), el, { timeout: 20000 });
        await sleep(4000); // let it fetch its data
        const own = await direct('/p/' + p.id);
        const norm = (e) => { const [text, url] = e.split(' @ '); return `${text.replace(/\(.*?\)/, '')} ${new URL(url ?? '/', EDGE).pathname}`; };
        const added = v.errors.filter((e) => !own.has(norm(e)));
        expect(!added.length, `${p.id}, only through the edge: ${added.join('; ')}`);
        const known = [...own].filter((e) => !e.endsWith(' /api/me'));
        out.push(`${p.id}: <${el}>${known.length ? ` (core shows the same without the edge: ${known.join('; ')})` : ''}`);
      } finally { await v.ctx.close(); }
    }
    // an episode page carries every plugin's episode slot (the bingo card, the stats panel)
    const ep = (await (await fetch(coreURL.origin + '/api/episodes?size=1')).json()).items?.[0];
    const els = plugins.flatMap((p) => (p.slots ?? []).filter((x) => x.scope === 'episode' && x.placement === 'main' && x.visibleTo === 'anonymous').map((x) => x.element));
    if (ep && els.length) {
      const path = '/episodes/' + ep.slug;
      const v = await visit(null, { path });
      try {
        await v.page.waitForFunction((ns) => ns.every((n) => customElements.get(n) && document.querySelector(n)), els, { timeout: 20000 });
        await sleep(4000);
        const own = await direct(path);
        const norm = (e) => { const [text, url] = e.split(' @ '); return `${text.replace(/\(.*?\)/, '')} ${new URL(url ?? '/', EDGE).pathname}`; };
        const added = v.errors.filter((e) => !own.has(norm(e)));
        expect(!added.length, `${path}, only through the edge: ${added.join('; ')}`);
        out.push(`episode page: ${els.map((e) => `<${e}>`).join(' ')}`);
      } finally { await v.ctx.close(); }
    }
    policy('scheduled', '3600'); await settle(); // the rows after this one expect the countdown
    return out.join(', ');
  });
}

await check('robots.txt: only the share image and favicon allowed under /_a/', async () => {
  const r = await get('/robots.txt');
  const allows = r.body.split('\n').filter((l) => l.startsWith('Allow: /_a/'));
  expect(r.body.includes('Disallow: /_a/'), r.body);
  expect(allows.length === (theme ? 2 : 0), `allow lines: ${allows.join(', ')}`);
  for (const l of allows) { const a = await get(l.slice(7)); expect(a.status === 200, `${l}: ${a.status}`); }
  return `${allows.length} allowed`;
});

await check('login_error=signups_closed → /_a/login with the text, de and en', async () => {
  const r = await get('/?login_error=signups_closed', html);
  expect(r.status === 303 && r.headers.location === '/_a/login?error=signups_closed', `${r.status} ${r.headers.location}`);
  const de = await get(r.headers.location, { 'Accept-Language': 'de-DE' });
  const en = await get(r.headers.location, { 'Accept-Language': 'en' });
  expect(de.body.includes('Du kannst noch kein Konto anlegen'), 'de text missing');
  // no apostrophe in the needle: html/template may emit it as &#39;
  expect(en.body.includes('create an account yet'), 'en text missing');
});

await check('abuse: 300 random well-shaped cookies from one client cost core ≤ 10 lookups', async () => {
  const ip = freshIP(); const before = meCalls;
  const uuid = () => crypto.randomUUID();
  await Promise.all(Array.from({ length: 300 }, () => get('/', { ...html, ...cookie(Buffer.from(uuid()).toString('base64')), 'X-Forwarded-For': ip })));
  const n = meCalls - before;
  expect(n <= 11, `${n} /api/me calls`);
  return `${n} /api/me calls`;
});

await check('abuse: 300 malformed cookies cost core nothing', async () => {
  const before = meCalls;
  await Promise.all(Array.from({ length: 300 }, (_, i) => get('/', { ...html, ...cookie('x' + i + Math.random()), 'X-Forwarded-For': freshIP() })));
  const n = meCalls - before;
  expect(n === 0, `${n} /api/me calls`);
  return '0 /api/me calls';
});

await check('crowd: 2,000 long-polls + 200 page loads, extras answered at once, memory under the limit (cap 1,500 for the test)', async () => {
  const { rev } = await (await fetch(EDGE + '/_a/state')).json();
  let peak = 0; let sampling = true;
  const sampler = (async () => { while (sampling) { peak = Math.max(peak, (await memMB('edge-smoke')) || 0); await sleep(400); } })();
  // The load runs on its own thread: on this one it would starve the counting proxy (same event loop),
  // the edge's poll of core would time out, and the harness would be measuring itself. Each request's
  // own wait counts: a held poll waits 25 s, one over the cap is answered at once.
  const crowd = new Worker(`
    const { parentPort, workerData: { port, rev } } = require('node:worker_threads');
    const http = require('node:http');
    const get = (path, headers = {}) => new Promise((res) => {
      const t0 = performance.now();
      const r = http.request({ host: '127.0.0.1', port, path, headers }, (resp) => { resp.resume(); resp.on('end', () => res([resp.statusCode, performance.now() - t0])); });
      r.on('error', (e) => res(['ERR ' + e.code, performance.now() - t0])); r.end();
    });
    (async () => {
      const polls = Array.from({ length: 2000 }, () => get('/_a/state?rev=' + rev));
      await new Promise((r) => setTimeout(r, 1500));
      const p0 = performance.now();
      const pages = await Promise.all(Array.from({ length: 200 }, () => get('/', { Accept: 'text/html' })));
      const pageMs = performance.now() - p0;
      parentPort.postMessage({ polls: await Promise.all(polls), pages, pageMs });
    })();
  `, { eval: true, workerData: { port: PORT, rev } });
  const { polls: done, pages, pageMs } = await new Promise((res, rej) => { crowd.once('message', res); crowd.once('error', rej); });
  await crowd.terminate();
  const statuses = done.map((x) => x[0]);
  const waits = done.map((x) => x[1]);
  sampling = false; await sampler;
  const quick = waits.filter((ms) => ms < 5000).length;
  expect(statuses.every((s) => s === 200), 'a long-poll failed');
  expect(pages.every((p) => p[0] === 200), 'a page load failed');
  expect(quick >= 450 && quick <= 600, `${quick} answered at once (cap 1,500 of 2,000)`);
  expect(pageMs < 5000, `200 pages took ${pageMs.toFixed(0)} ms`);
  expect(peak > 0 && peak < 128, `peak ${peak.toFixed(1)} MiB`);
  return `${quick} answered at once; 200 pages in ${pageMs.toFixed(0)} ms; peak ${peak.toFixed(1)} MiB of 128`;
});

await check('postpone and bring forward while waiting: the page follows both', async () => {
  policy('scheduled', '3600'); await settle();
  const v = await visit();
  const before = (await state(v.page)).units;
  policy('scheduled', '90000');
  await sleep(6000);
  const later = (await state(v.page)).units;
  policy('scheduled', '1800');
  await sleep(6000);
  const sooner = (await state(v.page)).units; await v.ctx.close();
  const secs = (u) => u.split(':').map(Number).reduce((a, n, i, all) => a + n * [86400, 3600, 60, 1][i + 4 - all.length], 0);
  const [b, l, n] = [before, later, sooner].map(secs);
  expect(b > 3500 && b <= 3600 && l > 89900 && l <= 90000 && n > 1700 && n <= 1800, `${before} → ${later} → ${sooner}`);
  return `${before} → ${later} → ${sooner}`;
});

await check('maintenance with endsAt: 503 + Retry-After, the end shown', async () => {
  policy('maintenance', '7200'); await settle();
  const r = await get('/', html);
  expect(r.status === 503 && Number(r.headers['retry-after']) >= 60 && r.body.includes('data-page="maintenance"'), `${r.status}`);
  expect(r.body.includes('data-edge-when="endsAt"'), 'endsAt not shown');
});

await check('closed: 503 + Retry-After', async () => {
  policy('closed'); await settle();
  const r = await get('/', html);
  expect(r.status === 503 && r.headers['retry-after'] && r.body.includes('data-page="closed"'), `${r.status}`);
});

await check('maintenance without an announced end: 503 + Retry-After, no end shown', async () => {
  policy('maintenance'); await settle();
  const r = await get('/', html);
  expect(r.status === 503 && Number(r.headers['retry-after']) > 0 && r.body.includes('data-page="maintenance"'), `${r.status}`);
  expect(!r.body.includes('data-edge-when="endsAt"'), 'an end is shown although none is set');
});

await check('staff sign-in link: shown by default, hidden when switched off, /_a/login reachable either way', async () => {
  const link = (b) => /<a[^>]*data-edge-login/.test(b) || b.includes('href="/_a/login');
  policy('closed'); await settle();
  const shown = await get('/', html);
  policyWith({ STAFF_LOGIN: 'false' }, 'closed'); await settle();
  const hidden = await get('/', html);
  const login = await get('/_a/login', html);
  policy('closed'); await settle();
  expect(link(shown.body), 'link missing while visible');
  expect(!link(hidden.body), 'link shown while switched off');
  expect(login.status === 200 && login.body.includes('data-page="login"'), `/_a/login: ${login.status}`);
});

await check('calendar entry: the opening while scheduled, 404 otherwise', async () => {
  policy('scheduled', '3600'); await settle();
  const { opensAt } = await edgeState();
  const ics = await get('/_a/calendar.ics?lang=en');
  const utc = new Date(opensAt).toISOString().replace(/[-:]/g, '').replace(/\.\d+/, '');
  policy('closed'); await settle();
  const none = await get('/_a/calendar.ics');
  expect(ics.status === 200 && /^text\/calendar/.test(ics.headers['content-type']) && ics.body.includes('BEGIN:VEVENT'), `scheduled: ${ics.status}`);
  expect(ics.body.includes('DTSTART:' + utc), `DTSTART is not ${utc}`);
  expect(none.status === 404, `closed: ${none.status}`);
});

await check('share description: its date follows the policy, with the zone name (themes that use {when})', async () => {
  const desc = async (lang) => (await get(`/?lang=${lang}`, { ...html, 'X-Forwarded-For': freshIP() })).body.match(/<meta name="description" content="([^"]*)"/)?.[1] ?? '';
  policy('scheduled', '3600'); await settle();
  const de1 = await desc('de'); const en1 = await desc('en');
  if (!/\d\d:\d\d/.test(de1 + en1)) skip('the theme writes no {when} into its description');
  policy('scheduled', '90000'); await settle();
  const en2 = await desc('en');
  policy('closed'); await settle();
  const en3 = await desc('en');
  expect(/Uhr ME(S)?Z/.test(de1) && /\bCES?T\b/.test(en1), `zone name missing: "${de1}" / "${en1}"`);
  expect(en2 !== en1 && /\d\d:\d\d/.test(en2), `postponed, unchanged: "${en2}"`);
  expect(!/\d\d:\d\d/.test(en3) && !en3.includes('{when}') && !/\s$/.test(en3), `closed: "${en3}"`);
  return `"${en1.slice(-38)}" → "${en2.slice(-38)}"; closed: no date`;
});

await check('live, one page left open: maintenance → closed → countdown → open, each followed without a reload by hand', async () => {
  policy('maintenance', '7200'); await settle();
  const v = await visit();
  const seen = [(await state(v.page)).page];
  try {
    policy('closed'); seen.push((await waitFor(v.page, (s) => s.page === 'closed', 15000)).page);
    policy('scheduled', '3600'); seen.push((await waitFor(v.page, (s) => s.page === 'countdown' && s.phase === 'wait', 15000)).page);
    policy('open');
    expect(await waitForSite(v.page, 20000), 'countdown did not hand over to the site');
    seen.push('site');
  } finally { await v.ctx.close(); }
  expect(seen[0] === 'maintenance', `started on ${seen[0]}`);
  return seen.join(' → ');
});

await check('core unreachable during the countdown: visitors keep counting, signed-in visitors get unavailable without a lookup, back on its own', async () => {
  policy('scheduled', '3600'); await settle();
  const v = await visit();
  coreDown = true;
  try {
    await sleep(5000);
    const anon = await get('/', { ...html, 'X-Forwarded-For': freshIP() });
    const before = meCalls;
    const signed = await get('/', { ...html, ...cookie(sessions.fan), 'X-Forwarded-For': freshIP() });
    const s = await state(v.page);
    expect(anon.status === 200 && anon.body.includes('data-page="countdown"'), `anonymous: ${anon.status}`);
    expect(signed.status === 503 && signed.body.includes('data-page="unavailable"') && meCalls === before, `signed in: ${signed.status}, lookups ${meCalls - before}`);
    expect(s.page === 'countdown' && s.phase === 'wait', `open page: ${JSON.stringify(s)}`);
  } finally { coreDown = false; }
  await settle();
  const st = await edgeState();
  await v.ctx.close();
  expect(st.coreUp && st.page === 'countdown', `after: ${JSON.stringify(st)}`);
});

await check('edge restarts while core is unreachable: countdown and legal pages from its cache, still opens at opensAt', async () => {
  policy('scheduled', '75'); await settle();
  const { opensAt } = await edgeState();
  coreDown = true;
  try {
    sh('docker', ['restart', '-t', '5', 'edge-smoke']);
    await healthy(EDGE);
    const page = await get('/', html);
    const legal = [...new Set([...page.body.matchAll(/\/_a\/legal\/[a-z0-9-]+/g)].map((m) => m[0]))];
    const legalStatus = await Promise.all(legal.map(async (p) => (await get(p, html)).status));
    const st = await edgeState();
    const logs = spawnSync('docker', ['logs', '--since', '30s', 'edge-smoke'], { encoding: 'utf8' }).stderr;
    expect(page.status === 200 && page.body.includes('data-page="countdown"'), `page ${page.status}`);
    expect(st.opensAt === opensAt && !st.coreUp, `state ${JSON.stringify(st)}`);
    expect(legal.length > 0 && legalStatus.every((c) => c === 200), `legal ${legal.join(',')}: ${legalStatus.join(',')}`);
    expect(logs.includes('policy loaded from cache') && logs.includes('site copy loaded from cache'), 'cache lines missing from the log');
    await sleep(Math.max(0, opensAt - Date.now()) + 1500);
    const after = await edgeState();
    expect(after.page === 'open', `after opensAt with core still unreachable: ${after.page}`);
  } finally { coreDown = false; }
  await settle();
  const back = await get('/', html);
  expect(!back.body.includes('id="edge-data"'), 'site not reached once core answers again');
  return 'policy + site copy from the volume; opened on its own clock';
});

await check('first start while core is unreachable: unavailable, no legal copy yet; all there once core answers, without a restart', async () => {
  policy('scheduled', '3600'); await settle();
  coreDown = true;
  const base = `http://127.0.0.1:${PORT + 2}`;
  let cold, legalCold;
  try {
    startEdge('edge-smoke-cold', PORT + 2, theme);
    await healthy(base);
    cold = await get('/', html, 'GET', PORT + 2);
    legalCold = cold.body.match(/\/_a\/legal\/[a-z0-9-]+/g) ?? [];
  } finally { coreDown = false; }
  let warm = null; let legal = [];
  for (let i = 0; i < 40; i++) {
    warm = await get('/', html, 'GET', PORT + 2);
    legal = [...new Set(warm.body.match(/\/_a\/legal\/[a-z0-9-]+/g) ?? [])];
    if (warm.status === 200 && legal.length) break;
    await sleep(500);
  }
  const legalStatus = await Promise.all(legal.map(async (p) => (await get(p, html, 'GET', PORT + 2)).status));
  spawnSync('docker', ['rm', '-f', 'edge-smoke-cold']); spawnSync('docker', ['volume', 'rm', '-f', 'edge-smoke-cold-cache']);
  expect(cold.status === 503 && cold.body.includes('data-page="unavailable"') && !legalCold.length, `cold: ${cold.status}, ${legalCold.length} legal links`);
  expect(warm.status === 200 && warm.body.includes('data-page="countdown"'), `after core answered: ${warm.status}`);
  expect(legal.length && legalStatus.every((c) => c === 200), `legal after core answered: ${legalStatus.join(',')}`);
  return `${legal.length} legal pages once core answered`;
});

await check('theme by download (EDGE_THEME, pinned): served; restart with the server gone runs from the cache; a wrong pin and an archive changed on the volume refuse to start', async () => {
  if (!theme) skip('needs --theme');
  const out = mkdtempSync(join(tmpdir(), 'edge-pack-'));
  chmodSync(out, 0o777); // the image runs as a non-root user
  const exited = async (name, ms = 20000) => {
    const end = performance.now() + ms;
    while (performance.now() < end) {
      const st = spawnSync('docker', ['inspect', '-f', '{{.State.Status}} {{.State.ExitCode}}', name], { encoding: 'utf8' }).stdout.trim();
      if (st.startsWith('exited')) return st;
      await sleep(300);
    }
    return 'still running';
  };
  const logsOf = (name) => { const r = spawnSync('docker', ['logs', name], { encoding: 'utf8' }); return r.stdout + r.stderr; };
  let up = true; let hits = 0; let srv;
  try {
    const packed = sh('docker', ['run', '--rm', '-v', `${theme}:/theme:ro`, '-v', `${out}:/out`, '--entrypoint', '/mosaicast-edge',
      IMAGE, 'pack-theme', '-o', '/out/theme.tgz', '/theme']);
    const sum = packed.match(/sha256:([0-9a-f]{64})/)?.[1];
    expect(sum, `pack-theme printed no digest: ${packed}`);
    const body = readFileSync(join(out, 'theme.tgz'));
    srv = http.createServer((req, res) => { hits++; if (!up || req.url !== '/theme.tgz') { res.writeHead(up ? 404 : 503).end(); return; } res.writeHead(200).end(body); });
    await new Promise((r) => srv.listen(0, '127.0.0.1', r));
    const url = `http://127.0.0.1:${srv.address().port}/theme.tgz`;
    policy('scheduled', '3600'); await settle();

    const name = 'edge-smoke-dl'; const port = PORT + 3; const base = `http://127.0.0.1:${port}`;
    startEdge(name, port, '', { EDGE_THEME: `${url}#sha256:${sum}` });
    await healthy(base);
    const first = await get('/', html, 'GET', port);
    expect(first.status === 200 && first.body.includes('/_a/t/') && hits === 1, `downloaded theme not served (${first.status}, ${hits} downloads)`);
    expect(/themeFrom="?http:\/\/127\.0\.0\.1:\d+\/theme\.tgz sha256:/.test(logsOf(name)), 'log does not name the download');

    up = false; const before = hits;
    sh('docker', ['restart', '-t', '5', name]); await healthy(base);
    const again = await get('/', html, 'GET', port);
    expect(again.status === 200 && again.body.includes('/_a/t/'), `after restart: ${again.status}`);
    expect(hits === before, `the restart asked the server ${hits - before} time(s)`);
    expect(/sha256:[0-9a-f]{64} \(cached\)/.test(logsOf(name)), 'log does not say cached');

    // the cache is a volume: change the archive there, and the edge refuses rather than loading it
    sh('docker', ['run', '--rm', '-v', `${name}-cache:/cache`, 'alpine:3.20', 'sh', '-c', 'for f in /cache/themes/*.archive; do printf x >> "$f"; done']);
    sh('docker', ['restart', '-t', '5', name]);
    const tampered = await exited(name);
    expect(/^exited [1-9]/.test(tampered) && logsOf(name).includes('changed on the volume'), `tampered cache: ${tampered}`);

    up = true;
    startEdge('edge-smoke-badpin', PORT + 4, '', { EDGE_THEME: `${url}#sha256:${'0'.repeat(64)}` });
    const bad = await exited('edge-smoke-badpin');
    expect(/^exited [1-9]/.test(bad) && logsOf('edge-smoke-badpin').includes('the pin says'), `wrong pin: ${bad}`);
    return `pinned ${sum.slice(0, 12)}…: downloaded once, restart from cache, tampered cache and wrong pin refused`;
  } finally {
    srv?.close();
    for (const c of ['edge-smoke-dl', 'edge-smoke-badpin']) { spawnSync('docker', ['rm', '-f', c]); spawnSync('docker', ['volume', 'rm', '-f', c + '-cache']); }
    rmSync(out, { recursive: true, force: true });
  }
});

await check("core's legal URL /legal/{slug}: the edge's copy while not open, core's page when open", async () => {
  const slugs = [...new Set([...(await get('/', html)).body.matchAll(/\/_a\/legal\/([a-z0-9-]+)/g)].map((m) => m[1]))];
  expect(slugs.length > 0, 'no legal links on the page');
  for (const st of [['scheduled', '3600'], ['maintenance', '7200'], ['closed']]) {
    policy(...st); await settle();
    for (const slug of slugs) for (const method of ['GET', 'HEAD']) {
      const r = await get(`/legal/${slug}?lang=en`, html, method);
      expect(r.status === 302 && r.headers.location === `/_a/legal/${slug}?lang=en`, `${st[0]} ${method} /legal/${slug}: ${r.status} ${r.headers.location}`);
      const t = await get(r.headers.location, html);
      expect(t.status === 200 && t.body.includes('data-page="legal"'), `${st[0]} ${r.headers.location}: ${t.status}`);
    }
  }
  const bad = await get('/legal/Not_A_Slug', html);
  expect(bad.status === 503 && !bad.headers.location && bad.body.includes('data-page="closed"'), `bad slug: ${bad.status}`);
  policy('open'); await settle();
  const o = await get(`/legal/${slugs[0]}`, html);
  expect(!o.headers.location && !o.body.includes('data-page="legal"') && o.status === 200, `open: ${o.status} ${o.headers.location || ''}`);
  return `${slugs.length} slugs × 3 states × GET/HEAD`;
});

await check('every page × en/de × light/dark × phone/desktop: the right page, no console errors, no sideways scroll', async () => {
  let n = 0;
  const legal = (await get('/', html)).body.match(/\/_a\/legal\/[a-z0-9-]+/)?.[0] ?? '/_a/legal/imprint';
  const views = [
    [['scheduled', '3600'], '/', 'countdown'], [['maintenance', '7200'], '/', 'maintenance'], [['closed'], '/', 'closed'],
    [['open'], '/', 'unavailable', true], [null, '/_a/login', 'login'], [null, legal, 'legal'],
  ];
  try {
    for (const [st, path, want, down] of views) {
      if (st) { policy(...st); await settle(); }
      if (down) { coreDown = true; await sleep(4000); }
      for (const locale of ['en-GB', 'de-DE']) {
        for (const scheme of ['light', 'dark']) {
          for (const viewport of [{ width: 390, height: 844 }, { width: 1280, height: 800 }]) {
            const ctx = await browser.newContext({ viewport, colorScheme: scheme, locale, extraHTTPHeaders: { 'X-Forwarded-For': freshIP() } });
            const page = await ctx.newPage(); const errs = [];
            page.on('pageerror', (e) => errs.push(e.message));
            page.on('console', (m) => { if (m.type() === 'error' && !/status of (503|404)/.test(m.text())) errs.push(m.text()); });
            await page.goto(EDGE + path); await sleep(300);
            const got = await page.evaluate(() => [document.documentElement.dataset.page, document.documentElement.lang,
              document.documentElement.scrollWidth > innerWidth + 1]);
            await ctx.close(); n++;
            const at = `${path} ${locale} ${scheme} ${viewport.width}`;
            expect(got[0] === want, `${at}: page ${got[0]}, expected ${want}`);
            expect(got[1] === locale.slice(0, 2), `${at}: lang ${got[1]}`);
            expect(!errs.length, `${at}: ${errs.join('; ')}`);
            expect(!got[2], `${at}: horizontal overflow`);
          }
        }
      }
      if (down) { coreDown = false; await settle(); }
    }
  } finally { coreDown = false; }
  return `${n} views`;
});

if (!flag('--skip-launch')) {
  await check('open now while waiting: full final, party, handoff into the site', async () => {
    policy('scheduled', '3600'); await settle();
    const v = await visit();
    await sleep(1000);
    policy('open-now');
    let firstFinal = null;
    const end = performance.now() + 20000;
    while (performance.now() < end) {
      const s = await state(v.page);
      if (s.phase === 'final' && firstFinal == null) firstFinal = Number(s.final);
      if (s.phase === 'party') break;
      await sleep(150);
    }
    const reached = await waitForSite(v.page, 30000);
    await v.ctx.close();
    expect(firstFinal >= 8, `final first seen at ${firstFinal}`);
    expect(reached, 'never reached the site');
    return `final from ${firstFinal}`;
  });

  // the launch as it will happen: nobody presses anything, the edge opens at opensAt on its own clock
  await check('opening at opensAt, nobody pressing anything: wait → final 10 → party → handoff, on time', async () => {
    policy('scheduled', '75'); await settle();
    const { opensAt } = await edgeState();
    const v = await visit();
    const phases = []; let firstFinal = null; let finalAt = null; let partyAt = null;
    try {
      const end = opensAt - Date.now() + 15000;
      const t0 = performance.now();
      while (performance.now() - t0 < end) {
        const s = await state(v.page).catch(() => null);
        if (!s || !s.edge) break;
        if (phases.at(-1) !== s.phase) phases.push(s.phase);
        if (s.phase === 'final' && firstFinal == null) { firstFinal = Number(s.final); finalAt = Date.now(); }
        if (s.phase === 'party' && partyAt == null) partyAt = Date.now();
        if (partyAt) break;
        await sleep(100);
      }
      const reached = await waitForSite(v.page, 40000);
      const siteAt = Date.now();
      const anon = await get('/', { ...html, 'X-Forwarded-For': freshIP() });
      expect(phases[0] === 'wait' && phases.includes('final') && phases.includes('party'), `phases ${phases.join(' → ')}`);
      expect(firstFinal >= 9, `final first seen at ${firstFinal}`);
      expect(Math.abs(finalAt - (opensAt - 10000)) < 1500, `final began ${((finalAt - opensAt) / 1000).toFixed(1)} s from opensAt`);
      expect(Math.abs(partyAt - opensAt) < 1500, `party began ${((partyAt - opensAt) / 1000).toFixed(1)} s from opensAt`);
      expect(reached && siteAt >= opensAt, 'never reached the site');
      expect(!anon.body.includes('id="edge-data"'), `a new visitor after opensAt still gets an edge page (${anon.status})`);
      return `final at ${((finalAt - opensAt) / 1000).toFixed(1)} s, party at ${((partyAt - opensAt) / 1000).toFixed(1)} s, site at +${((siteAt - opensAt) / 1000).toFixed(0)} s`;
    } finally { await v.ctx.close(); }
  });

  // the controls staff will really use: core's "Admin → Site access" in a browser, through the edge
  await check('admin page in a browser (Admin → Site access, through the edge): a countdown set there reaches visitors, the edge shows as connected with this build, "Open now" there launches', async () => {
    policy('open'); await settle();
    const tz = 'Europe/Berlin';
    const ctx = await browser.newContext({ locale: 'en-US', timezoneId: tz, extraHTTPHeaders: { 'X-Forwarded-For': freshIP() } });
    await ctx.addCookies([{ name: 'MOSAICAST_SESSION', value: sessions.admin, url: EDGE, httpOnly: true, sameSite: 'Lax' }]);
    let v = null;
    try {
      const a = await ctx.newPage();
      await a.goto(EDGE + '/admin/access');
      // core wants at least a minute ahead, minute precision, in the browser's zone
      const target = new Date(Math.ceil(Date.now() / 60000) * 60000 + 2 * 60000);
      const pt = Object.fromEntries(new Intl.DateTimeFormat('en-CA', { timeZone: tz, year: 'numeric', month: '2-digit', day: '2-digit',
        hour: '2-digit', minute: '2-digit', hourCycle: 'h23' }).formatToParts(target).map((x) => [x.type, x.value]));
      const value = `${pt.year}-${pt.month}-${pt.day}T${pt.hour}:${pt.minute}`;
      await a.getByRole('radio', { name: /^Countdown/ }).check();
      await a.getByLabel(/Opens at/).fill(value);
      await a.getByRole('button', { name: 'Save' }).click();
      await a.getByText('Saved.', { exact: true }).first().waitFor({ state: 'attached', timeout: 5000 }).catch(async () => {
        await a.screenshot({ path: join(tmpdir(), 'edge-smoke-admin.png'), fullPage: true }).catch(() => {});
        const err = await a.locator('.mc-error').textContent({ timeout: 1000 }).catch(() => 'no error shown');
        throw new Error(`not saved (${err}); screenshot ${join(tmpdir(), 'edge-smoke-admin.png')}`);
      });
      await settle();
      const st = await edgeState();
      expect(st.page === 'countdown' && st.opensAt === target.getTime(), `edge: ${st.page}, opensAt ${new Date(st.opensAt).toISOString()} vs ${target.toISOString()}`);
      v = await visit();
      const vs = await state(v.page);
      expect(vs.page === 'countdown' && vs.phase === 'wait', `visitor: ${JSON.stringify(vs)}`);

      await a.reload();
      const status = (await a.getByText(/The edge is enforcing these settings/).textContent({ timeout: 5000 }).catch(() => null))
        ?? `not connected: ${await a.locator('.mc-access__warning').textContent().catch(() => '?')}`;
      const shown = status.match(/build (\S+)\)/)?.[1];
      const logs = spawnSync('docker', ['logs', 'edge-smoke'], { encoding: 'utf8' }).stderr;
      const running = [...logs.matchAll(/mosaicast-edge up.*?build=(\S+)/g)].at(-1)?.[1];
      expect(shown && shown === running, `admin page: "${status}", edge log build=${running}`);
      const now = await a.locator('.mc-access__now').textContent();
      expect(/countdown/i.test(now), `admin "right now": ${now}`);

      await a.getByRole('button', { name: 'Open now' }).click();
      await a.getByRole('dialog').getByRole('button', { name: 'Open now' }).click();
      let firstFinal = null;
      const end = performance.now() + 20000;
      while (performance.now() < end) {
        const s = await state(v.page);
        if (s.phase === 'final' && firstFinal == null) firstFinal = Number(s.final);
        if (s.phase === 'party') break;
        await sleep(150);
      }
      const reached = await waitForSite(v.page, 30000);
      expect(firstFinal >= 8, `after "Open now": final first seen at ${firstFinal}`);
      expect(reached, 'after "Open now": never reached the site');
      return `opensAt ${value} (${tz}) reached the edge; build ${shown} shown; Open now → final from ${firstFinal} → site`;
    } finally { await ctx.close(); if (v) await v.ctx.close(); }
  });
}

if (flag('--restart-core')) {
  await check('core restarts during wait: the page keeps counting, no unavailable', async () => {
    policy('scheduled', '3600'); await settle();
    const v = await visit();
    instance('restart', ...coreArgs);
    const s = await state(v.page);
    await v.ctx.close();
    expect(s.page === 'countdown' && s.phase === 'wait', JSON.stringify(s));
    expect((await get('/', html)).status === 200, 'countdown gone after the restart');
  });
  await check('core restarts while open: unavailable, then back on its own', async () => {
    policy('open'); await settle();
    const restarting = instanceAsync('restart', ...coreArgs);
    await sleep(2000); // instance.sh stops the app first
    const v = await visit();
    const s = await state(v.page);
    const back = s.edge ? await waitForSite(v.page, 90000) : true;
    await restarting; await v.ctx.close();
    expect(s.edge && s.page === 'unavailable', `expected unavailable while core restarts, got ${JSON.stringify(s)}`);
    expect(back, `stuck on ${JSON.stringify(s)}`);
    return 'saw unavailable, came back to the site on its own';
  });
}

if (theme && !flag('--skip-expiry')) {
  await check('artwork expiry: gone after expireAfterOpen; share image, favicon and assets.keep stay', async () => {
    const dir = mkdtempSync(join(tmpdir(), 'edge-expiry-'));
    try {
      cpSync(theme, dir, { recursive: true, filter: (p) => !p.includes('/.git') });
      // the container runs as a non-root user: the copy must be readable by it, like a real mount
      const open = (p) => { chmodSync(p, statSync(p).isDirectory() ? 0o755 : 0o644); if (statSync(p).isDirectory()) readdirSync(p).forEach((f) => open(join(p, f))); };
      open(dir);
      const m = JSON.parse(readFileSync(join(dir, 'theme.json'), 'utf8'));
      m.assets = { ...(m.assets ?? {}), expireAfterOpen: `${(m.countdown?.partySeconds ?? 15) + 60}s` };
      writeFileSync(join(dir, 'theme.json'), JSON.stringify(m));
      startEdge('edge-smoke-expiry', PORT + 1, dir);
      const base = `http://127.0.0.1:${PORT + 1}`;
      await healthy(base);
      policy('scheduled', '3600'); await settle();
      const page = await (await fetch(base + '/', { headers: html })).text();
      // images the theme keeps (assets.keep) must stay; served names are sha256(bytes)[:8] + extension
      const keptNames = new Set((m.assets.keep ?? []).filter((k) => !k.endsWith('.css'))
        .map((k) => createHash('sha256').update(readFileSync(join(dir, 'assets', k))).digest('hex').slice(0, 16) + extname(k)));
      const webps = [...new Set([...page.matchAll(/src="(\/_a\/t\/[0-9a-f]+\.webp)"/g)].map((x) => x[1]))];
      const art = webps.filter((p) => !keptNames.has(p.split('/').pop()));
      const kept = webps.filter((p) => keptNames.has(p.split('/').pop()));
      const og = page.match(/property="og:image" content="[^"]*(\/_a\/t\/[^"]+)"/)?.[1];
      const fav = page.match(/rel="icon" href="(\/_a\/t\/[^"]+)"/)?.[1];
      expect(art.length && og && fav, 'no artwork / share image / favicon on the page');
      policy('open-now');
      const ttl = (m.countdown?.partySeconds ?? 15) + 60;
      await sleep((12 + ttl / 2) * 1000);
      expect((await fetch(base + art[0])).status === 200, 'artwork gone too early');
      await sleep((ttl / 2 + 5) * 1000);
      const st = async (p) => (await fetch(base + p)).status;
      const gone = await Promise.all(art.map(st)), stay = await Promise.all([og, fav, ...kept].map(st));
      expect(gone.every((c) => c === 404), `artwork ${gone.join(',')}`);
      expect(stay.every((c) => c === 200), `share ${stay[0]}, favicon ${stay[1]}, kept ${stay.slice(2).join(',')}`);
      return `expireAfterOpen ${ttl}s: ${art.length} artwork 404; share, favicon, ${kept.length} kept 200`;
    } finally { rmSync(dir, { recursive: true, force: true }); }
  });
}

// ---------- counting (§18.5, §19) ----------
// Its own edge (long-poll limit 5, the minimal example theme) in front of the same core; the main edge
// is paused meanwhile, so the waiting count and the hours core shows are this edge's alone.
{
  const CPORT = PORT + 7;
  const CEDGE = `http://127.0.0.1:${CPORT}`;
  const name = 'edge-count';
  const vol = `${name}-cache`;
  const busybox = (...a) => spawnSync('docker', ['run', '--rm', '-u', '65532:65532', '-v', `${vol}:/c`, 'busybox:1.37', ...a], { encoding: 'utf8' });
  const statsFile = () => { const r = busybox('cat', '/c/stats.json'); return r.status === 0 ? JSON.parse(r.stdout) : null; };
  const admin = policy('session', 'admin'); // fresh: a core restart earlier in the run ends older sessions
  const metrics = async (q = '') => {
    const r = await fetch(`${coreURL.origin}/api/admin/metrics${q}`, { headers: cookie(admin) });
    return r.status === 404 ? null : r.json();
  };
  const until = async (what, pred, ms) => {
    const end = performance.now() + ms;
    let last;
    while (performance.now() < end) {
      last = await what();
      if (pred(last)) return last;
      await sleep(1000);
    }
    throw new Error(`waited ${ms / 1000}s, last ${JSON.stringify(last)}`);
  };
  // n pages that keep a long-poll held, as the runtime does; stop() lets them go
  const holders = [];
  const hold = (n, headers = {}) => {
    for (let i = 0; i < n; i++) {
      const h = { stopped: false, req: null };
      const loop = async () => {
        while (!h.stopped) {
          const rev = (await edgeState(CEDGE)).rev;
          await new Promise((res) => {
            h.req = http.request({ host: '127.0.0.1', port: CPORT, path: `/_a/state?rev=${rev}`, headers }, (r) => { r.resume(); r.on('end', res); });
            h.req.on('error', res);
            h.req.end();
          });
          if (!h.stopped) await sleep(200);
        }
      };
      loop();
      holders.push(h);
    }
  };
  const release = () => { for (const h of holders.splice(0)) { h.stopped = true; h.req?.destroy(); } };
  const browserUA = { 'User-Agent': 'Mozilla/5.0 (X11; Linux x86_64; rv:131.0) Gecko/20100101 Firefox/131.0', Accept: 'text/html' };

  const hasReport = (await metrics()) !== null;
  const hour = new Date(); hour.setUTCMinutes(0, 0, 0);
  const hourISO = (h) => new Date(hour.getTime() + h * 3600_000).toISOString().replace('.000Z', 'Z');
  if (hasReport) {
    spawnSync('docker', ['pause', 'edge-smoke']);
    policyWith({ EDGE_COUNTING: 'true' }, 'scheduled', '3600');
    // a first start initialises the volume with the image's owner; then the seed goes in
    startEdge(name, CPORT, resolve(repo, 'examples/minimal'), { EDGE_LONGPOLL_MAX: '5' });
    await healthy(CEDGE);
    sh('docker', ['stop', '-t', '10', name]);
    const seed = { version: 1, running: { start: hourISO(0), views: { countdown: 5 } },
      pending: [{ start: hourISO(-3), views: { countdown: 7, legal: 2 } }] };
    busybox('sh', '-c', `echo '${JSON.stringify(seed)}' > /c/stats.json`);
    sh('docker', ['start', name]);
    await healthy(CEDGE);
  }

  await check('counting: the waiting count on the page and in core, GPC pages left out of core\'s, "5+" at the limit', async () => {
    if (!hasReport) skip('core without §18.5 (/api/admin/metrics)');
    hold(2);
    hold(1, { 'Sec-GPC': '1' });
    await until(() => edgeState(CEDGE), (s) => s.waiting === 3, 10_000);
    const ctx2 = await browser.newContext({ locale: 'en-GB' });
    const p2 = await ctx2.newPage();
    await p2.goto(CEDGE + '/');
    const shown = await p2.evaluate(() => { const el = document.querySelector('.mx-waiting'); return { hidden: el?.hidden, text: el?.textContent }; });
    await ctx2.close();
    expect(shown.hidden === false && /^[3-4] are waiting$/.test(shown.text), `theme element: ${JSON.stringify(shown)}`);
    const core = await until(metrics, (m) => m.edge?.waiting === 2 && !m.edge.waitingCapped, 90_000);
    hold(4);
    const capped = await until(() => edgeState(CEDGE), (s) => s.waiting === 5 && s.waitingCapped, 10_000);
    const coreCapped = await until(metrics, (m) => m.edge?.waitingCapped === true, 45_000);
    release();
    return `page "${shown.text}"; core ${core.edge.waiting} (3 held, 1 with GPC); at the limit /_a/state ${capped.waiting}+, core capped=${coreCapped.edge.waitingCapped}, peak this policy ${coreCapped.edge.peakThisPolicy}`;
  });

  await check('counting: a completed hour kept across a restart reaches core and is acknowledged; the running hour carries on; crawler, GPC, HEAD not counted', async () => {
    if (!hasReport) skip('core without §18.5 (/api/admin/metrics)');
    for (let i = 0; i < 2; i++) await get('/', browserUA, 'GET', CPORT);
    await get('/', { ...browserUA, 'User-Agent': 'Slackbot-LinkExpanding 1.0' }, 'GET', CPORT);
    await get('/', { ...browserUA, 'Sec-GPC': '1' }, 'GET', CPORT);
    await get('/', browserUA, 'HEAD', CPORT);
    const from = hourISO(-3); const to = hourISO(-2);
    const m = await until(() => metrics(`?from=${from}&to=${to}`), (x) => (x.edgeViews ?? []).some((v) => v.subject.endsWith('countdown') && v.count === 7), 45_000);
    const views = Object.fromEntries(m.edgeViews.map((v) => [v.subject, v.count]));
    // written every 30 s: wait for the two views to be in the file, the seeded hour gone from it
    const turned = () => new Date().getUTCHours() !== hour.getUTCHours();
    const f = await until(async () => statsFile(), (x) => x && x.pending.length === 0 && (turned() || x.running?.views?.countdown === 7), 45_000)
      .catch((e) => { throw new Error(`running hour: 5 seeded + 2 counted; ${e.message}`); });
    const running = f.running?.start === hourISO(0) ? f.running.views.countdown : null;
    sh('docker', ['restart', '-t', '10', name]);
    await healthy(CEDGE);
    const after = statsFile();
    if (running !== null && after.running?.start === hourISO(0)) expect(after.running.views.countdown === 7, `after a restart: ${JSON.stringify(after.running)}`);
    return `core has ${JSON.stringify(views)} for ${from}; acknowledged (pending empty); running hour ${running ?? '(the hour turned)'}, the same after a restart`;
  });

  await check('counting: edgeCounting off deletes stats.json and sends nothing; the page keeps its waiting count; on again counts afresh', async () => {
    if (!hasReport) skip('core without §18.5 (/api/admin/metrics)');
    policyWith({ EDGE_COUNTING: 'false' }, 'scheduled', '3600');
    await until(async () => busybox('test', '-e', '/c/stats.json').status, (st) => st !== 0, 45_000);
    await get('/', browserUA, 'GET', CPORT);
    const before = (await metrics()).edge.waitingAt;
    await sleep(35_000);
    const still = busybox('test', '-e', '/c/stats.json').status !== 0;
    const after = (await metrics()).edge;
    expect(still, 'stats.json came back while off');
    expect(after.waitingAt === before, `a report arrived while off (${before} → ${after.waitingAt})`);
    expect(after.counting === false, 'core shows edge counting off');
    hold(1);
    const s = await until(() => edgeState(CEDGE), (x) => x.waiting === 1, 10_000);
    release();
    policyWith({ EDGE_COUNTING: 'true' }, 'scheduled', '3600');
    await settle();
    await get('/', browserUA, 'GET', CPORT);
    const f = await until(async () => statsFile(), (x) => x?.running?.views?.countdown === 1, 45_000);
    return `off: file gone, no report for 35 s, /_a/state waiting=${s.waiting}; on: counting from ${f.running.views.countdown}`;
  });

  release();
  if (hasReport) {
    spawnSync('docker', ['rm', '-f', name]); spawnSync('docker', ['volume', 'rm', '-f', vol]);
    containers.splice(containers.indexOf(name), 1);
    spawnSync('docker', ['unpause', 'edge-smoke']);
  }
}

// ---------- done ----------
policy('open');
await browser.close();
proxy.close();
const logs = spawnSync('docker', ['logs', 'edge-smoke'], { encoding: 'utf8' }).stderr;
cleanup(); containers.length = 0;
const errorLines = logs.split('\n').filter((l) => /level=ERROR/.test(l));

const failed = results.filter((r) => r.result.includes('FAIL')).length;
const skipped = results.filter((r) => r.result === 'SKIP' && !covered(r)).length;
const byCoreN = results.filter(covered).length;
console.log(`\n### Edge rehearsal — ${new Date().toISOString().slice(0, 16)}Z`); // the harness's own clock, for the report
console.log(`image ${IMAGE} (${imageVersion}), core ${coreSHA.slice(0, 7)}, theme ${theme ? theme.split('/').at(-1) : 'default'}; read-only rootfs, 128 MiB, cache on a volume\n`);
console.log('| # | check | result | detail |');
console.log('|---|---|---|---|');
results.forEach((r, i) => console.log(`| ${i + 1} | ${r.name} | ${covered(r) ? 'CORE' : r.result} | ${r.detail.replaceAll('|', '\\|')} |`));
console.log(`\n${results.length - failed - skipped - byCoreN} passed, ${failed} failed, ${skipped} skipped, ${byCoreN} covered by core; ${errorLines.length} ERROR lines in the edge log`);
process.exitCode = failed ? 1 : 0;
