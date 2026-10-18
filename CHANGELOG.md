# Changelog

## [0.2.1] – 2026-10-18

Wording only. The default theme's English and German texts follow the suite's writing guide (mosaicast-core
`docs/WRITING.md`): contractions in English, no "Please", and the sign-in errors match core's new wording.
Keys and placeholders are unchanged, so themes that override texts keep working.

## [0.2.0] – 2026-10-13

Anonymous counting (§18.1/§18.2 `edgeCounting`, §18.5, §19; core 0.9.0). Counts, never people.

- The waiting count: the `/_a/state` long-polls the edge holds (pages, not people), the highest of the
  last 30 s, clamped to `EDGE_LONGPOLL_MAX` and then shown as capped ("2,500+"). In `/_a/state` and the
  page data, never part of `rev`: a change in the count wakes nobody. Themes place it with
  `data-edge-count="waiting"` / `data-edge-count-text="waiting"` (`data-edge-count-min` hides it below a
  number); `edge:count`, `edge.waiting`, `.Waiting` / `.WaitingCapped` / `.WaitingText`, texts
  `count.waiting.*` and `count.capped` in DE and EN. Themes that don't place it are unchanged.
- Page views per page kind and UTC hour, kept in `stats.json` in the cache dir across restarts; only
  completed hours leave the edge, at most 168 pending. Counted: a page rendered for a GET with 200 or
  503. Not counted: HEAD, redirects, 404s, plain 503s, anything proxied, crawlers (§19's list),
  `Sec-GPC: 1`. GPC long-polls stay out of the count core gets; the page's own display includes them.
- The report: `POST /api/site/edge/report` every 30 s and on shutdown, at most 48 hours each, acknowledged
  hours dropped; 401/404 (an older core) asked again every 10 minutes; after three refusals in a row the
  count goes alone and the hours are tried again every 10 minutes.
- `edgeCounting` (Admin → Site access): off counts nothing, deletes `stats.json` and sends nothing; the
  page keeps its waiting count. Missing means on (an older core); before the first policy nothing counts.
- Pages wake only for what they show (state, `opensAt`, `endsAt`): an admin save that changes only the
  staff link, the pass floor, `edgeCounting` or the revision no longer wakes every open page.
- Shutdown answers held long-polls at once instead of letting them sit out their hold.
- `dev/smoke.mjs`: three counting rows against core; `dev/policy` takes `EDGE_COUNTING`.

## [0.1.1] – 2026-10-12

- Staff automation passes before the opening: a request with a personal access token
  (`Authorization: Bearer mcp_…`, §18 / core §8.5) and no session that signs it in is checked against
  `/api/me` like a session cookie: shape check first, the same cache, per-client budget and in-flight cap,
  only that header forwarded, never logged. A token counts as a podcaster at most, as in core, so a pass
  floor of `admin` keeps every token out. Before, the edge answered such requests with a 503 until the
  opening.
- `dev/smoke.mjs`: `--image <ref>` rehearses a published image (a release) instead of building one;
  `--plugin-dir <dir>` loads built plugins into core and adds their rows (gated while not open, rendered
  through the edge once open, no console error core does not show itself); rows for tokens and for core's
  rate limits counting per visitor behind the edge. Without `--plugin-dir`, core runs without plugins.

## [0.1.0] – 2026-10-12

First version.

- The edge: policy poller with ETag and disk cache, opening on the edge's own clock, role check against
  `/api/me` (cached, rate-limited, fail closed), reverse proxy with `Host` and forwarded headers kept,
  countdown / maintenance / closed / unavailable / login / legal pages from the site's own tokens and
  logo, live updates via a long-poll, themes (`theme.json`, `check-theme`), a minimal example theme.
- `mosaicast-edge preview --theme <dir>`: every page and state with a fake core, theme hot reload.
- `docs/THEMING.md`: the theme contract (pages, levels, `theme.json`, template data, runtime attributes,
  events and `window.edge`, CSS hooks, accessibility, rules, preview/check/deploy).
- Runtime: screen-reader live region, `data-edge-label`, `data-scheme`, cues, `edge.data.countdown`.
- Memory sized to measured load: `EDGE_MAX_CONNS`, `EDGE_LONGPOLL_MAX`, 128 MB limit, `GOMEMLIMIT`.
- `dev/smoke.mjs`: the rehearsal against the real image; deployment runbook in the README.
- `VERSION` as the version anchor, `mosaicast-edge version`, the version in the startup log; releases
  publish a linux/amd64 + linux/arm64 image to GHCR once the tag matches `VERSION` and the changelog.
- `{when}` in theme texts (the opening or the end of maintenance, from the policy); core's legal URLs
  reach the edge's copy while the site is not open; core counts as down only after two failed polls.
- `EDGE_THEME`: a theme from a folder, a `.tgz`/`.zip`, an `https://` address or a GitHub release
  (`owner/repo@tag`), pinned with `#sha256:`, kept in the cache volume and re-checked on every load;
  `EDGE_THEME_TOKEN` for private theme repos. `mosaicast-edge pack-theme` (reproducible `theme.tgz`),
  `check-theme` on archives, and `dev/templates/release-theme.yml` for theme repositories.
