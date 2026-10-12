# mosaicast-edge

The reverse proxy in front of [Mosaicast](https://github.com/Mosaicast/mosaicast-core) core. Core decides
who may see the site (open, scheduled, maintenance, closed); the edge applies that policy to every
request and renders the pages a visitor sees instead of the site:

| page | when | status |
|---|---|---|
| countdown (wait → final 10 s → party → handoff) | `scheduled` | 200, no-store |
| maintenance (shows the announced end, never acts on it) | `maintenance` | 503 + Retry-After |
| closed | `closed` | 503 + Retry-After |
| unavailable ("back in a moment", then "taking longer") | core does not answer | 503 + Retry-After: 5 |
| `/_a/login` (staff sign-in) | always | 200 |
| `/_a/legal/{slug}` (the site's legal pages) | always | 200 / 404 |
| `/legal/{slug}` (core's own legal URL, from bookmarks, search, links in legal text) | not open | 302 to `/_a/legal/{slug}` |

Paths under `/_a/` belong to the edge in every state: its assets, `/_a/login`, `/_a/legal/{slug}`,
`/_a/calendar.ics` (a calendar entry for a scheduled opening, 404 otherwise), `/_a/healthz`, and
`/_a/state`, which the pages use to follow the policy live.

Staff whose role meets the policy's pass floor go through to the real site. Anonymous visitors cost core
nothing: no session cookie, no question to core. The edge keeps its last good policy and a copy of the
site's look and legal pages on disk, so its pages render while core is down, and it opens a scheduled
site on its own clock, on the second.

One static Go binary (standard library only) with its default look and runtime embedded; distroless
image, ~13 MB, 128 MB memory limit. The contract with core is [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) §18; how to
build a theme is [`docs/THEMING.md`](docs/THEMING.md).

## Run

```sh
cp .env.example .env          # EDGE_CORE_TOKEN, EDGE_THEME_PATH, CORE_NETWORK
docker compose up -d          # always builds from this folder; core's stack must be up first
docker compose logs edge      # "mosaicast-edge up … build=<hash> theme=…"
```

| Variable | Default | |
|---|---|---|
| `EDGE_UPSTREAM` | — (required; compose: `http://app:8080`) | core |
| `EDGE_LISTEN` | `:8080` | |
| `EDGE_CORE_TOKEN` | — | equal to core's `MOSAICAST_EDGE_TOKEN`; core's admin page then shows the edge as connected |
| `EDGE_CACHE_DIR` | `/cache` in the image | last good policy + site copy; empty = memory only |
| `EDGE_THEME` | — | the theme: a folder, a `.tgz`/`.zip`, `https://…/theme.tgz` or `owner/repo@tag`, with `#sha256:<digest>` for downloads ([THEMING.md](docs/THEMING.md#deploying-a-theme)); empty = the default look |
| `EDGE_THEME_DIR` | — | the older name for a theme folder (compose: set `EDGE_THEME_PATH` on the host) |
| `EDGE_THEME_TOKEN` | — | GitHub token for a private theme repo: fine-grained, that one repo, Contents read-only, with an expiry; never logged |
| `EDGE_THEME_ALLOW_UNVERIFIED` | `false` | download a theme without a `#sha256` pin |
| `EDGE_PUBLIC_URL` | from `X-Forwarded-Proto/-Host` | origin for share tags and the calendar entry |
| `EDGE_TIMEZONE` | `UTC` | dates for readers without JavaScript (with it: the visitor's own zone) |
| `EDGE_POLL_INTERVAL` | `2s` | how often the policy is fetched (a 304 when unchanged) |
| `EDGE_SITE_REFRESH` | `10m` | the site copy also refreshes on every policy change |
| `EDGE_LONGPOLL_MAX` | `2500` | pages held open waiting for a change; beyond this they are answered at once and poll gently (every 3 s) |
| `EDGE_MAX_CONNS` | `3500` | open connections; beyond this new ones wait to be accepted instead of costing memory |
| `GOMEMLIMIT` | `96MiB` in the image | the Go runtime's memory target; keep it below the container's limit |

Memory: about 8 MB idle plus about 21 KB per open connection (measured). The defaults fit the compose
file's 128 MB limit with headroom; scale all three together for a bigger crowd.

A broken theme stops the edge from starting. Check one before deploying:

```sh
docker run --rm -v "$PWD/my-theme:/theme:ro" <image> check-theme /theme
```

### Behind nginx / Plesk

```
TLS terminator  public domain  ──>  127.0.0.1:8082  mosaicast-edge  ──>  app:8080  core
```

```nginx
location / {
    proxy_pass http://127.0.0.1:8082;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header X-Forwarded-Host $host;
    proxy_buffering off;      # pass streamed responses through
    proxy_read_timeout 60s;   # the pages' live updates hold a request for up to 25 s
}
```

- Keep request buffering on (nginx's default; do not add `proxy_request_buffering off`): the edge has no
  read timeout, because proxied responses stream, so nginx should be the one absorbing slow uploads
  rather than holding edge connections against `EDGE_MAX_CONNS`.
- Do not let the terminator cache the domain: the edge's pages are `no-store`, and a cached countdown
  after opening would hide the site.
- The edge forwards `Host` and `X-Forwarded-*` unchanged and appends itself to `X-Forwarded-For`, so core
  builds its absolute URLs (the OAuth redirect among them) as before.
- The edge reads the visitor's address for its rate limits from the last `X-Forwarded-For` entry when
  the connection comes from loopback or a private network (the terminator), otherwise from the socket.
- Staff sign in through `/_a/login`, which works in every state; no separate admin host is needed.

## Deployment runbook

For a site moving from a standalone launch page (such as `announcer`) to the edge. The old page stays
deployed, untouched, until after the launch: it is the rollback.

### 1. Before (days ahead)

1. **Core** runs a release with site access (ARCHITECTURE §18) and `MOSAICAST_EDGE_TOKEN` set, e.g.
   `openssl rand -hex 24`. Admin → Site access: set the policy (`scheduled`, the opening time, pass floor,
   whether the staff sign-in link shows). Core refuses new sign-ups from now on until the site opens.
2. **Edge** next to it, not yet public:
   ```sh
   git clone … mosaicast-edge && cd mosaicast-edge
   cp .env.example .env
   #   EDGE_CORE_TOKEN = core's MOSAICAST_EDGE_TOKEN
   #   EDGE_THEME_PATH = /path/to/theme (checked out on the host; read-only mount). This folder mount is
   #                     the launch configuration. EDGE_THEME (a pinned download) is an alternative once a
   #                     rehearsal has run with it; with either, the log line names where the theme came from.
   #   EDGE_TIMEZONE   = Europe/Berlin
   #   EDGE_PORT       = 8082 (the old page keeps its port)
   #   CORE_NETWORK    = core's compose network (docker network ls)
   docker compose run --rm edge check-theme /theme     # must say "is valid"
   docker compose up -d
   docker compose logs edge                            # "mosaicast-edge up version=… build=<hash> theme=… themeFrom=…"
   ```
   The cache lives on the `edge-cache` volume; the root filesystem is read-only; the limit is 128 MB.
3. **Check it before anyone sees it**, straight on the port, as the terminator will call it:
   ```sh
   H='-H Host:<domain> -H X-Forwarded-Proto:https -H X-Forwarded-Host:<domain>'
   curl -s $H http://127.0.0.1:8082/_a/healthz                                # ok
   curl -s $H http://127.0.0.1:8082/_a/state                                  # "page":"countdown", opensAt as set
   curl -s $H -H 'Accept: text/html' http://127.0.0.1:8082/ | grep -o 'data-page="[a-z]*"'
   curl -s -o /dev/null -w '%{http_code} %{redirect_url}\n' $H http://127.0.0.1:8082/oauth2/authorization/discord
   #   302 to discord.com with redirect_uri=https://<domain>/login/oauth2/code/discord
   for p in $(curl -s $H -H 'Accept: text/html' http://127.0.0.1:8082/ | grep -o '/_a/legal/[a-z0-9-]*' | sort -u); do
     curl -s -o /dev/null -w "%{http_code} $p\n" $H "http://127.0.0.1:8082$p"; done   # every legal page: 200
   docker compose restart edge && sleep 3 && docker compose logs edge | grep 'site copy loaded from cache'
   ```
   Core's Admin → Site access shows the edge as connected, with the same build hash as the log.

   The legal check matters: the edge serves the imprint and the other legal pages from its own copy of
   core's, kept on the `edge-cache` volume, so they stay reachable while core is down. A first start
   while core is down has no copy yet (no legal links, `/_a/legal/…` 404) until core answers. The loop
   must list the imprint and answer 200 for every page, and the log line after the restart proves the
   copy survives restarts. Don't delete the `edge-cache` volume afterwards; if it is ever lost, repeat
   this check while core is up.

### 2. Cutover

In Plesk, the public domain's proxy target changes from the old page's port to the edge's:
**Apache & nginx settings → additional nginx directives** (or the Docker proxy rule), `proxy_pass
http://127.0.0.1:8081;` → `proxy_pass http://127.0.0.1:8082;`, with the directives above. Apply. If an
admin host was routed straight to core for staff before the launch, that route is no longer needed:
staff use `/_a/login` on the public domain.

### 3. Verify after cutover

```sh
D=https://<domain>
curl -sI $D/ | grep -iE '^(HTTP|cache-control|content-security-policy)'      # 200, no-store, CSP
curl -s  $D/_a/state                                                        # page countdown, opensAt
curl -s -o /dev/null -w '%{http_code} %header{retry-after}\n' -H 'Accept: application/json' $D/api/site   # 503 + Retry-After
curl -s  $D/robots.txt                                                      # Allow share image + favicon, Disallow /_a/
curl -s -o /dev/null -w '%{http_code} %{redirect_url}\n' $D/oauth2/authorization/discord   # redirect_uri on $D
```

Then in a browser: the countdown on a phone and a desktop; staff sign-in through `$D/_a/login` lands on
the real site; a fan account sees the countdown ("signed in").

### Rollback

Point the Plesk proxy target back at the old page's port (`8082` → `8081`) and apply. Nothing else
changes: the edge keeps running unused, the old page never stopped, core's policy stays as it is
(sign-ups stay closed until the opening either way). Roll forward again the same way.

Two things keep a rollback honest:

- **Keep the old page's launch time in step.** A standalone launch page such as `announcer` takes its
  opening time from its own environment only. While a rollback is possible, mirror every change of
  `opensAt` in core (a postponement, "open now") into the old page's `.env` and run
  `docker compose up -d` there, or a rollback shows a countdown to the wrong moment.
- **Staff access during a rollback.** With no admin host routed to core any more, staff have no way into
  core from outside while the old page holds the domain. The way in is an SSH tunnel to core's loopback
  port: `ssh -L 8080:127.0.0.1:8080 <server>`, then http://localhost:8080. Signing in there needs
  `http://localhost:8080/login/oauth2/code/discord` registered as a redirect in the Discord app;
  register it before the day.

### On the day

- The edge opens the site on its own clock at `opensAt`, even if core is slow; "open now" in core's
  admin opens it twelve seconds later (the last ten still play).
- `docker compose logs -f edge` shows policy changes and any trouble reaching core
  ("keeping the last good policy").
- If core restarts, visitors of the open site see the unavailable page, which returns them to the site
  by itself.

## Develop

Go is not required locally: `dev/go` runs the toolchain in Docker.

```sh
dev/go test -race ./...                  # unit and end-to-end tests (a fake core)
dev/go vet ./...
dev/go run . check-theme examples/minimal
```

`dev/smoke.mjs` is the rehearsal: it builds the image and runs it read-only and memory-limited in front
of core's dev instance and prints a pass/fail table
(`PW=<playwright> node dev/smoke.mjs --theme <dir> --restart-core [--core-ref v0.8.4]`, ~12 min). It
covers every state and role; live changes with a page left open; the opening at `opensAt` with nobody
pressing anything, "open now", and core's own Admin → Site access page in a browser; core unreachable
(during the countdown, across an edge restart, at a first start) and restarting; legal pages and
links; light/dark, phone/desktop, en/de; cookie abuse and a crowd. `--only <regex>` runs a subset.

Against a real core, use core's dev instances (`dev/instance.sh --name edge up --core <sha> --admin`
in the core checkout) and point the edge at it; `dev/policy` drives that instance's policy as admin
(`dev/policy scheduled 70`, `open-now`, `maintenance 3600`, `closed`, `open`, `session fan`).

## Releases

`VERSION` is the version anchor; the binary embeds it, so `mosaicast-edge version` and the startup log
(`mosaicast-edge up version=… build=…`) show it, next to the build hash that core's admin page shows.

1. Bump `VERSION` and turn `## Unreleased` in `CHANGELOG.md` into `## [<version>] – <date>`, in a PR.
2. After the merge, publish a GitHub Release from `main` with the tag `v<version>`.
3. `.github/workflows/release.yml` checks that the tag equals `VERSION` and that the changelog has the
   heading (otherwise it fails and nothing is published), then builds `linux/amd64` and `linux/arm64`
   and pushes `ghcr.io/mosaicast/mosaicast-edge:<version>` and `:latest`, and checks the pushed image
   reports the version.

The compose file builds from the checkout (`pull_policy: build`); check out the release tag there, or
replace `build: .` with `image: ghcr.io/mosaicast/mosaicast-edge:<version>` to run the published image.

## License

AGPL-3.0-or-later, see [LICENSE](LICENSE).
