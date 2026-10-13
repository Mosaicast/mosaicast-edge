// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

// The edge runtime. Injected into every edge page, themed or not. It owns the clock, the countdown's
// phases, following the policy live and the handoff to the site; themes only react to what it says
// (THEMING.md, "Runtime"). No dependencies, no storage, no third parties.
'use strict';

(() => {
  const root = document.documentElement;
  const dataEl = document.getElementById('edge-data');
  if (!dataEl) return;
  const cfg = JSON.parse(dataEl.textContent);
  const $$ = (s, el = document) => [...el.querySelectorAll(s)];
  const emit = (name, detail = {}) => document.dispatchEvent(new CustomEvent('edge:' + name, { detail }));
  const sleep = (ms) => new Promise((res) => setTimeout(res, ms));

  // ---------- clock ----------
  // The edge decides when the site opens, so the page runs on the edge's clock and never reads the
  // device's date: it starts from the server time sent with the page, refines it via /_a/state, and
  // counts forward with performance.now(), a monotonic stopwatch that changing the system clock
  // doesn't move. Never Date.now().
  let base = cfg.now;
  let baseAt = performance.now();
  const now = () => base + (performance.now() - baseAt);

  async function sample() {
    const t0 = performance.now();
    const res = await fetch('/_a/state', { cache: 'no-store' });
    const t1 = performance.now();
    const body = await res.json();
    return { rtt: t1 - t0, server: body.now + (t1 - t0) / 2, at: t1, body };
  }
  async function sync() {
    try {
      let best = null;
      for (let i = 0; i < 3; i++) {
        const s = await sample();
        if (!best || s.rtt < best.rtt) best = s;
      }
      base = best.server;
      baseAt = best.at;
      setCount(best.body);
      return best.body;
    } catch { return null; /* offline: keep counting from the time the page came with */ }
  }

  // ---------- texts and language ----------
  let lang = cfg.lang;
  const t = (key, vars) => {
    let s = cfg.texts[lang]?.[key] ?? key;
    if (vars) for (const [k, v] of Object.entries(vars)) s = s.replaceAll('{' + k + '}', v);
    return s;
  };
  const fmt = (ms, style) => {
    const opts = style === 'date' ? { weekday: 'long', day: 'numeric', month: 'long', year: 'numeric' }
      : style === 'time' ? { hour: '2-digit', minute: '2-digit', timeZoneName: 'short' }
      : { weekday: 'long', day: 'numeric', month: 'long', year: 'numeric', hour: '2-digit', minute: '2-digit', timeZoneName: 'short' };
    return new Intl.DateTimeFormat(lang, opts).format(new Date(ms)); // formats a given instant; reads no clock
  };
  const instants = () => ({ opensAt, endsAt: cfg.endsAt, downSince: cfg.downSince });
  function renderDates() {
    const at = instants();
    $$('[data-edge-when]').forEach((el) => {
      const v = at[el.dataset.edgeWhen];
      if (v == null) return;
      el.textContent = fmt(v, el.dataset.edgeFormat);
      el.setAttribute('datetime', new Date(v).toISOString());
    });
  }
  function applyLang(next) {
    if (!cfg.texts[next]) return;
    const from = lang;
    lang = next;
    root.lang = lang;
    $$('[data-edge-text]').forEach((el) => { el.textContent = t(el.dataset.edgeText); });
    $$('[data-edge-label]').forEach((el) => el.setAttribute('aria-label', t(el.dataset.edgeLabel)));
    $$('[data-edge-lang]').forEach((el) => el.setAttribute('aria-current', String(el.dataset.edgeLang === lang)));
    // links that carry the language along
    $$('[data-edge-legal] a, a[data-edge-login], a[data-edge-calendar]').forEach((a) => {
      const u = new URL(a.href, location.href);
      u.searchParams.set('lang', lang);
      a.href = u.pathname + u.search;
    });
    renderDates();
    renderCount();
    shown = '';
    if (cfg.page === 'countdown') { render(); announce(true); }
    emit('lang', { lang, from });
  }
  document.addEventListener('click', (e) => {
    const el = e.target.closest?.('[data-edge-lang]');
    if (!el) return;
    e.preventDefault();
    applyLang(el.dataset.edgeLang);
  });

  // ---------- waiting count ----------
  // How many pages follow the policy live right now: pages, not people. It comes with every /_a/state
  // answer, so it moves about every 25 s. [data-edge-count="waiting"] gets the number,
  // [data-edge-count-text="waiting"] the sentence; any element with data-edge-count-min="N" is hidden
  // while fewer than N are waiting.
  let waiting = { value: cfg.waiting ?? 0, capped: !!cfg.waitingCapped };
  function renderCount() {
    const num = new Intl.NumberFormat(lang).format(waiting.value);
    const n = waiting.capped ? t('count.capped', { n: num }) : num;
    const sentence = t(`count.waiting.${waiting.value === 1 && !waiting.capped ? 'one' : 'other'}`, { n });
    $$('[data-edge-count="waiting"]').forEach((el) => { el.textContent = n; });
    $$('[data-edge-count-text="waiting"]').forEach((el) => { el.textContent = sentence; });
    $$('[data-edge-count-min]').forEach((el) => { el.hidden = waiting.value < Number(el.dataset.edgeCountMin); });
    root.dataset.waiting = String(waiting.value);
    root.toggleAttribute('data-waiting-capped', waiting.capped);
  }
  function setCount(s) {
    if (typeof s?.waiting !== 'number') return;
    const next = { value: s.waiting, capped: !!s.waitingCapped };
    if (next.value === waiting.value && next.capped === waiting.capped) return;
    waiting = next;
    renderCount();
    emit('count', { name: 'waiting', value: waiting.value, capped: waiting.capped });
  }

  // ---------- screen readers ----------
  // A display that changes every second must not be read out every second. The runtime hides the
  // ticking elements from assistive technology and keeps one polite live region with a sentence,
  // updated rarely: on load, every 10 minutes beyond a day, every minute below, once when the final
  // ten seconds start and once at zero. Themes get this without doing anything.
  let live = null;
  let lastSaid = 0;
  function liveRegion() {
    if (live) return live;
    live = document.createElement('p');
    live.className = 'edge-sr';
    live.setAttribute('aria-live', 'polite');
    live.setAttribute('aria-atomic', 'true');
    document.body.append(live);
    return live;
  }
  function say(text) { liveRegion().textContent = text; lastSaid = performance.now(); }
  function spoken(p) {
    const units = [['d', p.d], ['h', p.h], ['m', p.m]].filter(([, n]) => n > 0)
      .map(([u, n]) => `${n} ${t(`unit.${u}.${n === 1 ? 'one' : 'other'}`)}`);
    if (!units.length) units.push(`${p.s} ${t(`unit.s.${p.s === 1 ? 'one' : 'other'}`)}`);
    return units.length > 1 ? units.slice(0, -1).join(', ') + ' ' + t('a11y.and') + ' ' + units.at(-1) : units[0];
  }
  function announce(force = false) {
    if (cfg.page !== 'countdown' || opensAt == null) return;
    const left = opensAt - now();
    if (phase === 'final' || phase === 'party' || phase === 'handoff') return;
    const every = left > 86_400_000 ? 600_000 : 60_000;
    if (!force && performance.now() - lastSaid < every) return;
    say(t('a11y.opensIn', { time: spoken(parts(left)) }));
  }
  function hideTickers() {
    $$('[data-edge-unit], [data-edge-unit-label], [data-edge-final], [data-edge-split]').forEach((el) => el.setAttribute('aria-hidden', 'true'));
  }

  // ---------- countdown ----------
  const cd = cfg.countdown;
  const ORDER = ['wait', 'final', 'party', 'handoff'];
  let opensAt = cfg.opensAt;
  let phase = '';
  let shown = '';
  let lastFinal = null;
  const firedCues = new Set();
  let preloaded = false;

  const pad = (n) => String(n).padStart(2, '0');
  const parts = (left) => {
    const s = Math.max(0, Math.ceil(left / 1000));
    return { d: Math.floor(s / 86400), h: Math.floor(s / 3600) % 24, m: Math.floor(s / 60) % 60, s: s % 60, total: s };
  };
  const put = (el, text) => {
    if ('edgeSplit' in el.dataset) el.replaceChildren(...[...text].map((c) => Object.assign(document.createElement('i'), { textContent: c })));
    else el.textContent = text;
  };

  function render() {
    const left = opensAt - now();
    setPhase(left);
    for (const [name, ms] of Object.entries(cd.cues)) {
      if (left <= ms && !firedCues.has(name)) {
        firedCues.add(name);
        root.setAttribute('data-cue-' + name, '');
        emit('cue', { name, remaining: left });
      }
    }
    if (left <= cd.preloadMs) preload();

    const p = parts(left);
    const key = `${p.total}:${lang}:${phase}`;
    if (key === shown) return;
    shown = key;
    if (phase === 'wait') {
      for (const u of ['d', 'h', 'm', 's']) {
        $$(`[data-edge-unit="${u}"]`).forEach((el) => put(el, pad(p[u])));
        $$(`[data-edge-unit-label="${u}"]`).forEach((el) => { el.textContent = t(`unit.${u}.${p[u] === 1 ? 'one' : 'other'}`); });
      }
    } else if (phase === 'final') {
      const n = Math.max(1, p.total);
      if (n !== lastFinal) {
        lastFinal = n;
        $$('[data-edge-final]').forEach((el) => {
          put(el, String(n));
          el.classList.remove('is-tick');
          void el.offsetWidth; // restart a CSS animation
          el.classList.add('is-tick');
        });
      }
    }
    announce();
    emit('tick', { remaining: left, parts: p, phase });
  }

  function setPhase(left) {
    const next = left > cd.finalMs ? 'wait' : left > 0 ? 'final' : left > -cd.partyMs ? 'party' : 'handoff';
    // forward only: a clock re-sync that shifts by a few ms must not replay the party
    if (next === phase || ORDER.indexOf(next) < ORDER.indexOf(phase)) return;
    const from = phase;
    phase = next;
    root.dataset.phase = next;
    emit('phase', { phase: next, from });
    emit(next, { from });
    if (next === 'final') say(t('a11y.final'));
    if (next === 'party') say(t('a11y.open'));
    if (next === 'handoff') handoff();
  }

  function preload() {
    if (preloaded) return;
    preloaded = true;
    $$('[data-edge-src]').forEach((el) => { el.src = el.dataset.edgeSrc; });
  }

  // Reload only once the edge says the site is open; until then a reload would bring this page back.
  let handingOff = false;
  async function handoff() {
    if (handingOff) return;
    handingOff = true;
    if (phase !== 'handoff') { phase = 'handoff'; root.dataset.phase = 'handoff'; emit('phase', { phase, from: 'party' }); emit('handoff', { from: 'party' }); }
    const go = () => setTimeout(() => location.reload(), cd.handoffMs);
    for (let i = 0; i < 40; i++) {
      try {
        const r = await fetch('/_a/state', { cache: 'no-store' });
        if ((await r.json()).page === 'open') return go();
      } catch { /* keep trying */ }
      await sleep(750);
    }
    go(); // the edge will serve whatever is right by now
  }
  document.addEventListener('click', (e) => {
    const el = e.target.closest?.('[data-edge-handoff]');
    if (!el || cfg.page !== 'countdown') return;
    e.preventDefault();
    if (phase === 'party' || phase === 'handoff') handoff();
  });

  function loop() {
    render();
    const left = opensAt - now();
    // wake just after the next full second; more often when the final phase is close
    const ms = left > cd.finalMs + 2000 ? 1000 - (now() % 1000) + 15 : 100;
    if (phase !== 'handoff') setTimeout(loop, ms);
  }

  // ---------- following the policy live ----------
  // /_a/state?rev=N is a long-poll: it answers when anything a page reacts to has changed (the policy,
  // core going down or coming back), or after ~25 s with nothing new.
  let rev = cfg.rev;
  function onState(s) {
    rev = s.rev;
    if (cfg.page === 'countdown') {
      if (s.page === 'countdown' && s.opensAt !== opensAt && (phase === 'wait' || phase === '')) {
        const from = opensAt;
        opensAt = s.opensAt; // postponed, or "open now": follow it while still waiting
        shown = '';
        renderDates();
        emit('retime', { opensAt, from });
        render();
        announce(true);
      } else if (s.page !== 'countdown' && !(s.page === 'open' && phase !== 'wait' && phase !== '')) {
        location.reload(); // maintenance, closed, or opened outright while waiting
      }
    } else if (cfg.page === 'unavailable') {
      if (s.coreUp && s.page !== 'unavailable') { emit('reconnect'); location.reload(); }
    } else if (cfg.page === 'maintenance' || cfg.page === 'closed') {
      if (s.page !== cfg.page) location.reload();
      else if (s.endsAt !== cfg.endsAt) { cfg.endsAt = s.endsAt; renderDates(); }
    }
  }
  async function watch() {
    for (;;) {
      const t0 = performance.now();
      try {
        const r = await fetch(`/_a/state?rev=${rev}`, { cache: 'no-store' });
        const s = await r.json();
        const same = s.rev === rev;
        setCount(s);
        onState(s);
        if (same && performance.now() - t0 < 1000) await sleep(3000); // the edge is busy: poll gently
      } catch {
        await sleep(3000);
      }
      if (phase === 'handoff') return;
    }
  }

  // ---------- the parts a theme can place but not remove ----------
  function guard() {
    const hidden = (el) => el && !el.hidden && el.checkVisibility && !el.checkVisibility({ opacityProperty: true, visibilityProperty: true });
    const missing = [];
    const legal = document.querySelector('[data-edge-legal]');
    if (hidden(legal)) missing.push(legal);
    const login = document.querySelector('a[data-edge-login]');
    if (hidden(login)) missing.push(login);
    if (!missing.length) return;
    const bar = document.createElement('div');
    bar.className = 'edge-guard';
    missing.forEach((el) => bar.append(el.cloneNode(true)));
    document.body.append(bar);
  }

  // ---------- unavailable ----------
  function downTimer() {
    if (cfg.downSince == null) return;
    const longAt = cfg.downSince + 60_000 - now();
    if (longAt <= 0) root.dataset.down = 'long';
    else setTimeout(() => { root.dataset.down = 'long'; emit('down', { long: true }); }, longAt);
  }

  // ---------- resolved colour scheme ----------
  // data-mode is the site's policy (light | dark | system); data-scheme is what it resolves to for this
  // visitor, kept live. CSS should prefer data-mode plus a prefers-color-scheme query (no flash before
  // this script runs); data-scheme is for scripts and for convenience.
  const darkQuery = matchMedia('(prefers-color-scheme: dark)');
  const scheme = () => root.dataset.mode === 'dark' || (root.dataset.mode !== 'light' && darkQuery.matches) ? 'dark' : 'light';
  function applyScheme() {
    const next = scheme();
    if (root.dataset.scheme === next) return;
    const from = root.dataset.scheme;
    root.dataset.scheme = next;
    if (from) emit('scheme', { scheme: next, from });
  }
  darkQuery.addEventListener?.('change', applyScheme);
  applyScheme();

  // ---------- public API for theme scripts: read-only ----------
  window.edge = Object.freeze({
    now,
    t: (key, vars) => t(key, vars),
    get lang() { return lang; },
    get phase() { return phase; },
    get opensAt() { return opensAt; },
    get remaining() { return opensAt == null ? null : opensAt - now(); },
    get scheme() { return scheme(); },
    get waiting() { return { ...waiting }; },
    data: Object.freeze({
      page: cfg.page, viewer: cfg.viewer, site: cfg.site, endsAt: cfg.endsAt, downSince: cfg.downSince, locales: cfg.locales,
      countdown: cd ? Object.freeze({ finalMs: cd.finalMs, partyMs: cd.partyMs, handoffMs: cd.handoffMs }) : null,
    }),
  });

  // ---------- start ----------
  // After DOMContentLoaded, which fires once every deferred script (the theme's included) has run, so
  // theme listeners see the first edge:phase and edge:ready.
  function start() {
    renderDates();
    renderCount();
    hideTickers();
    guard();
    if (cfg.page === 'unavailable') downTimer();
    if (cfg.page === 'countdown' && opensAt != null) {
      render();
      announce(true);
      sync().then(() => { render(); loop(); });
      setInterval(sync, 5 * 60_000);
      document.addEventListener('visibilitychange', () => { if (!document.hidden) sync().then(render); });
    }
    if (['countdown', 'maintenance', 'closed', 'unavailable'].includes(cfg.page)) watch();
    emit('ready', { page: cfg.page, phase, lang });
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', start);
  else start();
})();
