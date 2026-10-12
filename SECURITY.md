# Security Policy

Please report vulnerabilities **privately** via GitHub Security Advisories on this repository ("Security"
tab → "Report a vulnerability"), not as public issues. We aim to respond within 14 days. Please include
reproduction steps and the affected version (`mosaicast-edge version`, or `version=` in the startup log).

The edge sits in front of Mosaicast core on the public internet. Reports about the core itself belong to
[Mosaicast core](https://github.com/Mosaicast/mosaicast-core/security).

## What the edge is meant to guarantee

- **Nothing behind it is reachable before it should be.** While the site is not open, only `/_a/*` (the
  edge's own pages and assets), the OAuth sign-in paths, core's legal URLs (redirected to the edge's copy)
  and signed-in staff whose role core confirms reach core, along with their automation tokens (checked
  the same way, never above podcaster). Everything else gets the edge's page.
- **A wrong upstream or a broken answer never opens the site.** Only a 200 or 304 for the policy counts;
  anything else keeps the last good policy, and with none the page is "unavailable".
- **Visitors cannot make the edge hammer core.** Role lookups need a cookie of the right shape, are rate
  limited per client and capped in flight, cached, and fail closed.
- **A theme cannot weaken the pages.** Themes are validated at startup; their assets are served only
  under hashed names; every edge page carries a strict Content Security Policy (`script-src 'self'`, no
  inline script or style); a downloaded theme must be pinned by its sha256 and is re-checked on every load.

## Known residuals

Things a review will find, that we know about and have decided to keep. If you can show the trade is
wrong, that is worth a report.

### A theme is trusted code for visitors

A theme's scripts run on the site's own origin, for every visitor of the countdown and the other edge
pages. The CSP keeps them to the theme's own files, but it cannot make a malicious theme harmless. Treat a
theme like a plugin: deploy only themes you trust, and pin downloads (`EDGE_THEME=…#sha256:…`; unpinned
downloads are refused unless `EDGE_THEME_ALLOW_UNVERIFIED=true`).

### The legal pages are core's HTML, inserted as they are

The edge shows core's rendering of the legal pages without sanitising it again: core sanitises it, and
the edge trusts core. The CSP still applies, so script in that HTML would not run.

### The client address comes from `X-Forwarded-For` behind a local proxy

The per-client rate limit uses the rightmost `X-Forwarded-For` entry when the connection comes from a
loopback or private address, which is how the TLS terminator in front of the edge reaches it. Exposing the
edge's port directly to the internet would let a client choose its own address; the compose file binds it
to loopback for that reason.
