# Project: mosaicast-edge

Reverse proxy in front of Mosaicast core (Go, stdlib only): applies core's site-access policy and renders
countdown / maintenance / closed / unavailable / login / legal pages, themeable. Contract with core:
`docs/ARCHITECTURE.md` §18 (a copy of core's; core owns it). Theme contract: `docs/THEMING.md` (ours,
public). See `README.md` for run/deploy.

## Rules (binding)
- **Team setup:** the core session owns §18 and the core side, reviews this repo's single PR; the user
  merges. Contract questions (core endpoints, theme.json, runtime data/events) go to core before changing.
- **Never push to `main`**, never force-push it, never merge. Work on `feat/edge`, one PR.
- **No real podcast's theme content (name, artwork, texts) in this repo, ever, not even in history.**
  Production themes live in their own private repos; this repo ships only the default look and
  `examples/`.
- `/home/ned/mosaicast/announcer` (the launch fallback) and core are **read-only**: no writes, no
  state-changing git, don't run announcer's scripts in place. Read core files with
  `git -C ../mosaicast-core show <branch>:<path>`. Only ever touch core's dev instance named `edge`.
- SPDX header on every source file, `git commit -s`, no agent session links in commits or PR text
  (the DCO workflow rejects them). DE + EN texts with identical keys (tested).

## Commands
```sh
dev/go test -race ./...    # Go 1.27 via Docker (bookworm image: -race needs cgo)
dev/go vet ./... ; docker run --rm -u $(id -u) -v $PWD:/src -w /src golang:1.27-bookworm gofmt -l .
dev/go run . check-theme examples/minimal
dev/go build -o tmp/edge . && EDGE_UPSTREAM=$MC_APP_URL EDGE_CORE_TOKEN=dev-edge-token \
  EDGE_CACHE_DIR=$PWD/tmp/cache EDGE_LISTEN=127.0.0.1:8190 ./tmp/edge
source <(../mosaicast-core/dev/instance.sh --name edge env)   # MC_APP_URL etc. never hard-code ports
dev/policy scheduled 70 | open-now | maintenance [secs] | closed | open | show | session <role>
PW=~/.npm/_npx/e41f203b7505f1fb/node_modules/playwright node dev/smoke.mjs --theme <theme-dir> --restart-core
                           # the rehearsal: real image, every check, pass/fail table (~12 min);
                           # --core-ref <tag|sha> moves core first, --image <ref> tests a published
                           # image, --only <regex> runs a subset
./tmp/edge preview --theme <dir>   # http://127.0.0.1:8090/_preview ; dev/go mounts only this repo, so
                                   # for paths outside it build tmp/edge and run it directly
```
Core's dev instance can't have a configured OAuth provider next to dev-login (core refuses it on
purpose): the provider list / redirect_uri half is core's OAuthBehindProxyIntegrationTest; the smoke
proves the edge's header forwarding against an echo upstream.
Never `pkill -f` the server (matches the tool's own shell); use `tmp/edge.pid`.

## Code map
- `main.go` subcommands; `version.go` embeds `VERSION` (release anchor, `.github/workflows/release.yml`); `listener.go` connection cap (memory: ~21 KB per open connection);
  `preview.go` the theme authors' preview (fake core); `serve.go` wiring, `loadThemes` (startup validation), `buildID`, `check-theme`;
  `config.go` env.
- `internal/policy` — `Access` (parse/validate, `Effective` on the edge's clock, `Passes`), `Store`
  (last good policy, disk cache, core up/down + downSince, `Seq` + `Wait` for the long-poll, flip timer).
- `internal/core` — HTTP client; `Poller` (2 s, ETag/304; core down only after 2 failed polls in a row; any other status, 404 included, keeps the last good policy: a 404 means a wrong upstream, never "open"); `Roles` (`/api/me`: cookie shape
  check `LooksLikeSession`, PAT `TokenRole` (`LooksLikeToken`, `BearerToken`; gate caps admin → podcaster), per-IP token bucket, in-flight cap, 30 s / 10 s negative cache, fail closed),
  `ClientIP`.
- `internal/sitecopy` — copy of `/api/site` tokens, branding images, legal pages per locale; disk cache.
- `internal/gate` — root handler: `/_a/*`, OAuth pass-through, open → proxy, cookie → role check,
  pages with the edge's HTTP semantics (503 + Retry-After, plain 503 for non-HTML/non-GET),
  `/_a/state` long-poll (capped), proxy `rewrite`, `login_error` → `/_a/login`, `/legal/{slug}` → `/_a/legal/{slug}`
  while not open (`legalSlug`, §12.6 grammar), circuit breaker.
- `internal/themesrc` — `EDGE_THEME`: folder / archive / https / `owner/repo@tag`, `#sha256` pins, cache
  (keyed by spec+pin, re-verified), in-memory `Unpack` (plain files only, limits while reading);
  `packtheme.go` writes the reproducible `theme.tgz`. The folder mount stays the launch configuration.
- `internal/theme` — `theme.json` (`Load`, strict), hashed assets (CSS `url()` rewritten, unknown ref
  fails), texts, `Lint` (check-theme warnings).
- `internal/pages` — renderer: plan (full / restyle / default), text layers, `Data` = template contract,
  `enforce` (inserts html attrs / head / legal / login / credits a template left out), site-copy
  assets (`/_a/site/`), calendar, `Validate` (dry-render every page × locale × fixture state).
- `web/default/` — the default theme (also the reference for restyling): templates, texts, `edge.css`,
  `base.css`, **`edge.js` = the runtime** (server clock, phases forward only, cues, long-poll, handoff,
  placeholders guard; never `Date.now()`).
- `examples/minimal` — the example theme CI validates.
