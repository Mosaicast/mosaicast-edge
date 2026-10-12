# Changelog

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
