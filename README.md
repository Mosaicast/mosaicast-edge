# mosaicast-edge

Reverse proxy in front of [Mosaicast](https://github.com/Mosaicast/mosaicast-core) core. It applies the
site-access policy core publishes (open, scheduled, maintenance, closed) and renders the pages a visitor
sees instead of the site: a launch countdown, a maintenance or closed page, and a holding page while core
itself is not answering. Staff with a sufficient role pass through to the real site.

Contract with core: `docs/ARCHITECTURE.md` §18. Work in progress.

## License

AGPL-3.0-or-later, see [LICENSE](LICENSE).
