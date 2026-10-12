# Changelog

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
