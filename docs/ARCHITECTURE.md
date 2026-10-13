# Mosaicast – Architecture Reference (Source of Truth)

> This document is the **single source of truth**. Every repo gets a copy at `docs/ARCHITECTURE.md`.
> Subproject briefs reference this file and only add repo-specific detail.
> On conflict between a brief and this file → **this file wins**, then flag it.

---

## 1. Vision & Scope

Mosaicast is an **extensible website platform for podcasts**. A podcast has several feeds (regular, news, paid) that are shown unified or filtered. Fans interact (bingos), hosts add extra info (speaking shares / stats). Everything is extensible via **plugins** at site, feed, season and episode level.

- **Now (v1):** ONE podcast, multiple RSS feeds per site. Public open-source project.
- **Scale:** v1 targets dozens of users, but built so it grows to thousands + multiple load-balanced instances (v3).
- **Far future:** multi-tenant (many podcasts centrally), possibly a native audio host with its own RSS feeds. **Only keep interfaces open, do NOT build now.**

**Guiding principle:** abstract interfaces cleanly for the future, keep the implementation simple for *today*. YAGNI on implementation, foresight on interfaces.

---

## 2. Tech Stack (binding)

| Layer | Choice |
|---|---|
| Backend | **Java 21**, **Spring Boot 3** (Web/REST, Security, Data JPA, Session) |
| Plugins | **PF4J** (`pf4j-spring`), loaded from folders at **startup** (no hot reload) |
| RSS | **Rome** |
| DB | **PostgreSQL** (JSONB for the plugin store; `pgvector` only in v2) |
| Migrations | **Flyway** |
| Scheduler locks | **ShedLock** (mandatory, for the multi-instance future) |
| Cache/Sessions | in-memory in v1 → **Redis** from v3 (config switch, no rewrite) |
| Frontend | **TypeScript + React + Vite** (host shell). Plugins ship UI as **Web Components**. **No Vaadin.** |
| Deployment | **Docker Compose** on a VPS (app, postgres, caddy/traefik; redis later) |
| Edge (optional) | **Go**, standard library only: `mosaicast-edge`, the reverse proxy that applies site access (§18) |

Naming conventions: see §15.

---

## 3. Repository Map & Dependencies

```
mosaicast-plugin-sdk     ← contract (Java plugin-api JAR + @mosaicast/plugin-sdk TS types). NO app dependency.
        ▲
        │ (compile-only)
mosaicast-core           ← host: Spring Boot backend + React/Vite shell. Depends on the SDK.
        ▲
        │ (compile-only, against the SDK – NOT against core)
mosaicast-plugin-sample  ← reference plugin + build.sh (React-specific, replaceable)
mosaicast-plugin-bingo   ┐
mosaicast-plugin-stats   ├ own repos, depend ONLY on the SDK
mosaicast-plugin-wiki    ┘

mosaicast-edge           ← optional reverse proxy in front of core (Go, §18). Talks to core over public HTTP only;
                           depends on neither the SDK nor core code.
```

**Build order:** SDK → core → sample → individual plugins.
Plugins compile **exclusively** against the SDK, **never** against core code. That hard boundary is what makes external plugin repos viable.

---

## 4. Domain Model – the two layers

The project's most important decision: **identity is separate from presentation.**

### 4.1 `EpisodeRef` – identity layer (authoritative, owned by us)
Stable internal ID that **all plugins reference**. Survives feed changes.

```
EpisodeRef
  id            UUID         -- THIS is what plugins reference
  source_id     FK           -- from which FeedSource config
  external_guid String?      -- the source's GUID (null when PLANNED)
  season        Int?         -- from itunes:season, persisted as a relation
  episode_no    Int?
  numbers_pinned Bool        -- season + episode_no set by a podcaster; polls no longer overwrite them (4.4)
  feed_season, feed_episode_no Int?  -- what the feed last declared, so un-pinning restores it at once (4.4)
  status        Enum         -- PLANNED | PUBLISHED | WITHDRAWN
  access        Access       -- PUBLIC | TIER(ref)
  first_seen_at, last_seen_at
  provisional_display Json?   -- only set when PLANNED (see 4.3)
  announce_at   Instant?     -- PLANNED only: when a quiet plan goes public; null = quiet until announced (4.3)
  client_ref    String?      -- a planner's own reference, unique per feed; makes a retried create idempotent
```

**Status lifecycle:** `PLANNED` → `PUBLISHED` → possibly `WITHDRAWN`. What plugins and the shell reason about is the **phase** derived from it (§4.3), not the stored status.

### 4.2 Display snapshot (NOT authoritative, from the feed)
Title, description, audio URL, pubDate, runtime/duration (`<itunes:duration>`/enclosure), **episode artwork** (`itunes:image`) with the **feed/show cover** (channel `itunes:image`) as a fallback (`artwork()` = episode → feed), **author** (`itunes:author`, falling back to the channel author) and **subtitle** (`itunes:subtitle`). **Overwritten** on every fetch from the raw feed, only read-through cached. So a description change in the RSS propagates automatically and never lives in the DB as truth. **The description is third-party HTML** — whatever the podcast host published, so whoever controls the feed controls it. The shell renders it only through its sanitizer, and plugins receive it **verbatim and unsanitized**, documented as such, with `descriptionText` beside it: the same prose reduced to plain text by the host (Jsoup), for cards, teasers, OG descriptions and search excerpts. It is derived rather than stored for rows written before it existed, so no backfill is needed (platformApi 0.16.0). Plugins also receive the episode's **place in the site** — `feed` (the feed's public slug), `season`, `episodeNo` — which belongs to the identity layer (`EpisodeRef`, §4.4), not to the feed's presentation: the host adds it **on read** and **never stores it in the snapshot**, so a refetch rewriting the snapshot cannot move an episode between seasons in a plugin's eyes. Each is absent when the episode does not have it, and a season scope (`Scope.season(feed, n)`) only exists for an episode that has both (platformApi 0.17.0). (Episode **tags** — `itunes:keywords`/`<category>` — are feed-derived too but stored as a relation `episode_tag`, since they are a filter/scoping axis, §6.1.)
Table: `episode_display(episode_ref_id, snapshot JSONB, fetched_at)`. Swappable for Redis later.

> **Core display vs. plugin metrics:** Runtime/date in the main UI (feed cards, detail header, player) always come from the **feed snapshot**. Metrics provided by plugins (e.g. MAT runtime, speaking shares) are **non-authoritative, possibly absent** (not every episode has stats) and are shown **only inside that plugin's UI** — never in the core display.

### 4.3 Planned episodes (PLANNED) – creating episodes before the RSS
Hosts make bingos for the **upcoming** episode. So episodes must be creatable before they appear in the feed.
- A podcaster or admin creates a planned episode → `EpisodeRef` with `status=PLANNED`, `source=manual`, `provisional_display` (title, description, planned season/episode no.). **Only here** does display data live authoritatively in the DB — until the feed takes over. It is created in the admin area or over the API with a personal access token (§8.5); the answer carries the **slug**, so a follow-up call can prepare plugin content on it straight away. An optional `client_ref` makes a retried create return the existing plan instead of a second one.
- Plugin data (bingos) attaches to the internal ID immediately.
- When the real episode appears in the RSS → **binding instead of duplicating** (see 5.3). Status flips `PLANNED → PUBLISHED`, from then on the feed snapshot rules. Plugin data is untouched because it hung on the ID, not the feed. The **slug is kept**, so links shared before the release keep working; the feed's title and description replace the planned ones everywhere.

**Quiet first, announced later.** A plan is **quiet by default**: only podcasters and admins see it, so content can be prepared without telling the audience. `announce_at` makes it public from that moment; it can also be announced by hand, or set back to quiet while it is still planned. **The feed outranks the schedule**: a feed item that binds before `announce_at` releases the episode anyway. A quiet episode stays out of **every** public surface — lists, detail, search, tags, related, previous/next, link previews, sitemap, and the scope checks plugin requests go through — and a podcaster opening it sees a notice that nobody else can. A plugin's **backend** (`FeedAccess`) sees quiet episodes, because preparing them is the point; a plugin's **frontend** sees one only when the viewer may.

**Phase, derived at read time, no scheduler** (`EpisodePhase`, platformApi 0.18.0):

| phase | stored state | who sees it |
|---|---|---|
| `PLANNED` | `PLANNED`, `announce_at` null or in the future | podcasters, admins, plugin backends |
| `UPCOMING` | `PLANNED`, `announce_at` reached | everyone — "upcoming" stub, no audio |
| `RELEASED` | `PUBLISHED` | everyone |
| `WITHDRAWN` | `WITHDRAWN` | as §5.2 |

Nothing flips at `announce_at`: every read compares it with the clock, so there is no job to miss and no window in which the episode is half-announced. Plugins read the phase and are **told** of a release, and of any *write* that changes the phase (§7.4); they never infer it from a title or a date.

**Editing and cancelling.** While an episode is planned, its title, description, numbers and `announce_at` can be changed. Once released, its details are the feed's and the planning API refuses. Cancelling a plan deletes it **together with the plugin data prepared for it**, since nothing else could ever reach that data again.

`PLANNED` = preparation (quiet), `UPCOMING` = bingo prediction phase, `RELEASED` = resolution.

### 4.4 Season as a first-class concept
Season is **not** a plugin concern. The fetcher extracts `itunes:season` and persists it as a relation on the `EpisodeRef`. A season = "all EpisodeRefs of a feed with season=N". Season is a **scope** (§6).

**A podcaster can set the numbers by hand.** A feed cannot always say what the podcaster means: Apple's spec allows only a non-zero `itunes:episode`, so the season prologue a podcaster calls episode 0 arrives with a season and no number (Acast drops the 0). A podcaster or admin may therefore set a released episode's `season` and `episode_no` — **both together**, and either may be empty — which sets `numbers_pinned`. From then on the reconciler still records what the feed declares, in `feed_season` / `feed_episode_no`, but leaves the effective numbers alone, so a poll never undoes the choice; un-pinning restores the feed's values immediately, without waiting for a poll. The pinned numbers are the episode's numbers **everywhere** — season scope and filter, listing order, labels, and the `season` / `episodeNo` plugins receive (§4.2) — and change nothing else: the slug was minted once and is kept (§4.1), and previous/next still follow release order (§6.2), so an explicit 0 is a podcaster's statement, not the coercion §6.2 rules out. A **planned** episode has no override: its numbers are edited with the plan (§4.3), and on binding the feed's numbers take over as for any new item — unless the plan is matched to an imported episode whose numbers a podcaster had already pinned, in which case the pin moves across with it, like its tags (§5.3). The host never infers a number on its own — not from a title (`5.00 Prolog`), not from a gap in the sequence.

---

## 5. Feed Pipeline

### 5.1 FeedSource SPI (the open interface for other hosts)
```java
interface FeedSource {
    String type();                        // "rss", "patreon", ...
    SourceCapabilities capabilities();
    List<RawEpisode> fetch(SourceConfig cfg) throws FetchException;
}
record SourceCapabilities(boolean providesAudio, boolean supportsTierGating,
                          boolean supportsSeasons, boolean pushBased) {}
record RawEpisode(String externalGuid, String title, String description, String audioUrl,
                  Instant publishedAt, Integer season, Integer episodeNumber,
                  Duration declaredDuration, Access access) {}
```
- `RssFeedSource`: providesAudio=true (`<enclosure>`), supportsSeasons=true, supportsTierGating=false, pushBased=false.
- `PatreonFeedSource` (v2): like RSS + supportsTierGating=true.
- Future native host: pushBased=true (no polling).

**Rule:** the rest of the platform queries **capabilities**, never the source type. New host = new implementation, otherwise zero change.

### 5.2 Reconciler – raw items become EpisodeRefs
Match by `(source_id, external_guid)`:
1. **New GUID** → create `EpisodeRef` (identity + relations only).
2. **Known GUID** → refresh season relation (only the `feed_*` record when the podcaster pinned the numbers, §4.4) + display snapshot, plugin data untouched.
3. **Ref exists, GUID now missing** → **never hard delete** (plugin data would orphan, feeds glitch). Set `status=WITHDRAWN`.

### 5.3 PLANNED binding & dedup (same machinery)
Before creating a new ref for an unknown GUID, the reconciler checks whether a `PLANNED` ref matches: first by declared season/episode no., otherwise fuzzy title → **suggestion, podcaster confirms**. On binding: set `external_guid` + `source`, `PLANNED → PUBLISHED`.
This is the **same merge machinery** as v2 dedup (multiple refs → one canonical episode, fuzzy-title merge UI), just triggered at a different time. v1: only PLANNED binding active; the model allows later merging without a rewrite.

**Manual match** covers what neither the numbers nor the title caught, after the feed has already imported the item as an episode of its own. A podcaster matches the plan to that episode: the plan **keeps its identity, slug and plugin data** and takes over the feed item. Listeners' progress, pins and tags move across, and the duplicate is removed. A duplicate that already has **plugin data of its own is refused** with a message saying so, and nothing changes: merging two plugins' worth of state is a decision the host cannot make on a plugin's behalf. Every path that releases a planned episode — binding, a confirmed suggestion, a manual match — announces it to plugins after the transaction commits (§7.4).

### 5.4 Scheduler
One periodic job per FeedSource config, **ShedLock-wrapped** (runs only once across N instances). Default 15–30 min, configurable per feed. `pushBased` sources skip polling. "Refresh now" button for podcasters. Dead feeds: backoff, last successful state stays visible. Polling uses **HTTP conditional GET** (ETag / If-Modified-Since) — polite to feed hosts; an unchanged feed costs a 304.

---

## 6. Scopes, sequential navigation & related

### 6.1 Scopes
Four: **site / feed / season / episode**. Resolving "which episodes belong to this scope" is the **host's job**:
```java
interface FeedAccess { List<String> episodesIn(Scope scope); DisplaySnapshot display(String refId); }
```
The host fills the frontend `ctx.episodes[]` from this. A plugin never figures out itself how a season is defined.

**Filter state lives in the URL** (query params, e.g. `?season=2&tag=christmas`): filtered views are shareable/bookmarkable, the back button works, and the server can read the params when rendering share metadata (§6.4). Plugins still consume filters read-only via `ctx.filter`: `current()` reflects the URL's `season` / `tag` / `order` as `{ season, tags, sort }` (only what is set), `onChange` fires when one changes, and a plugin **page** gets `{}` because its query string is its own (`ctx.route.query`). `ctx.episodes` holds every episode in the scope; the shell pages the host's resolution to the end rather than handing over its first page. Filter axes: **season** (§4.4), **tag** (§6.1.1) and ordering; **feed** is selected by the shell's **per-feed tabs** (All + one per feed; a single-feed site shows no tabs and lives at that feed's own URL). The feed view is **two-column**: a left **scope panel** (a **feed panel** — cover/title/author/description + the `feed` plugin region — on a feed tab, or a **site panel** — logo/name + the `site` region — on the All tab) beside the episode list (which infinite-scrolls). Host-defined **subfeeds** (a saved tag/search/filter as a named view) are a planned extension.

#### 6.1.1 Tags: a shared vocabulary, with provenance
Tags began as one feed's `<itunes:keywords>`/`<category>`, rewritten on every poll. They are now a **site-wide vocabulary several writers share**, because a plugin that wanted tags otherwise invented a private one — and two things labelled `lore` on the same site had no relationship to each other.

- **The host owns the key.** Every writer sends any spelling; the host canonicalises (trim, collapse internal whitespace, casefold) and keeps a **display label from first use**, so `Maritime` and `maritime ` converge without lower-casing what a visitor reads. The rule is applied to **feed ingest too** — a vocabulary normalised on one path only is not normalised. `/api/tags` therefore returns `{ tag, label }`, and an incoming `?tag=` is canonicalised so links written in any spelling keep matching.
- **Every assignment carries a source**: `feed` · `manual` (a podcaster in admin) · `plugin:<id>`. The reconciler's per-poll overwrite narrows to the rows the feed owns. Before that column existed the wipe was unqualified, so a tag a podcaster or a plugin added survived until the next poll — minutes, in practice, which reads as "it did not save".
- **A plugin's own assignments** are keyed by an opaque `subject_key` it invents inside its own namespace (`plugin_tag`), the property `SchemaStore` has for tables and `ctx.route.navigate` has for URLs.
- **Three things a writer may never do**, enforced rather than asked: remove another writer's assignment (the feed's included), delete a word from the vocabulary (it is shared; an entry outlives its last assignment), or rename one. Renaming and curation are admin's.
- **Tagging an episode is a capability, not a convenience.** It changes the shell's filter options *and* what `RelatedProvider` recommends beside that episode (§6.3), so it is a separate manifest declaration (§7.2) from tagging a plugin's own subjects, and its absence is a refusal rather than a silently dropped write.

### 6.2 Sequential navigation (always shown)
Previous/next episode are **core navigation, not related and not a plugin** — always shown (detail page + player). **Order: release order (`publishedAt`) within the feed** — the same sequence the browsable feed shows (§6.1), so `prev` is the previously released episode and `next` the next released one, and navigation always matches what the listener sees in the list. Episode numbers deliberately do **not** drive navigation: real feeds routinely omit `itunes:episode` (e.g. Acast) or number inconsistently, so a numberless trailer/bonus would otherwise be coerced to "episode 0" and jump to the front. An episode without a `pubDate` counts as the earliest point in the series; ties fall back to season + episode no. **Only released episodes are part of the sequence** — a `PLANNED` episode leads the *listing* (§6.1, upcoming first) but has no audio, so it is never anyone's `next` (auto-advance would land on an unplayable episode); its own page links back to the latest release. (The per-feed *listing* order used by `FeedAccess.episodesIn` stays season + episode no., nulls last — that is a catalogue view, not the navigation sequence.) **The player auto-advances to the next episode when one ends** (same sequence logic). Must work with zero plugins.

### 6.3 RelatedProvider (core, swappable strategy – not a plugin)
Interface: `related(EpisodeRef ctx, int limit) → List<EpisodeRef>` with capabilities.
**v1 strategy (deterministic, no ML):** pinned episodes (podcaster-curated in admin) win, otherwise a weighted blend of **same season** (recency), **shared tags/keywords** (§6.1.1 — the feed's, a podcaster's, or a plugin's, counted once per topic rather than once per writer who agreed), fuzzy title. Exclude: current episode, PLANNED, WITHDRAWN; locked ones at most as a lock stub. **Computed on-request + cached**, invalidated on new episodes.
**Future (v2):** an embedding strategy (`pgvector`, cosine) or a recommender plugin overrides the provider — the sidebar widget doesn't change. Runs over `EpisodeRef` → automatically respects dedup + tier gating.

### 6.4 Deep links & share metadata (SEO/OG)
The shell is an SPA, but link scrapers (WhatsApp/Discord/Facebook) **do not run JS**. So core injects **OpenGraph/Twitter meta tags server-side** into `index.html` per URL before serving it.
- **OgResolver per scope:** episode → episode title + episode image (fallback: podcast cover); feed/site → cover + site name + description; season → "site name – Season N". The resolver reads the **query params** (§6.1), so a shared filtered view gets matching meta.
- **Plugin deep links:** the host reserves **`/p/{pluginId}/*`** and passes the subpath to the plugin as `ctx.route` (read-only + `onChange`). That makes plugin content (e.g. a wiki page) linkable and shareable at all.
- **`ShareMetadataProvider`** (optional backend extension point, §7.4): when serving a `/p/{pluginId}/…` URL, core asks the plugin for title/description/image; no provider (or no match) → site-level OG fallback. The wiki implements it in v1 as the reference.
- **Timestamped episode links:** `/episodes/{slug}?t=754` addresses a **moment**, and the shell hands the position to the player as its pending seek. The grammar accepts what people paste — bare seconds, `12:34`, `1:02:03`, `1h02m03s`, `90m` — and an unparsable value is **dropped rather than an error**, since the meaning of the URL is its path and `t` only enriches it. One grammar, implemented on both sides (shell + server) and held to a shared table of cases: the shell decides where playback lands, the server decides what a card says, and a link that previews as one moment and plays another is worse than one carrying no timestamp.
- **`rel=canonical` and `og:url` may differ, and here they do.** They answer different questions: canonical is *which URL to index*, and a timestamp is a position within one page rather than a page of its own — so canonical stays the bare episode and the filter-normalizing rules above are untouched. `og:url` is *what was shared*; emitting the canonical form there would let a scraper normalize a shared moment back to the top of the episode, which is precisely the link the sharer did not send. Only a parsed-and-re-serialized value is echoed, so the parameter never reaches the page as text.
- **Preview tags are written for messengers, not only for search.** Beyond title/description/image: an episode declares `og:type: article` with `article:published_time`, plus `og:audio`(`:type`) when the enclosure's format is unambiguous from its URL; every view carries `og:site_name`, `og:locale` and `og:image:alt`. **`og:locale` is the language of *that URL*** (§12.7), not the install's default — as is the document's `lang` attribute; a page served in German that announces itself as English is wrong in the one place a scraper looks. Image *dimensions* are deliberately absent — artwork comes from the feed as a third-party URL whose size the host does not know.

### 6.5 Listening progress (core service)
Per-user playback progress is a **core service, not a plugin concern**: the player stores position per episode (anonymous → localStorage only; logged-in → server-side), **resumes playback** where the user left off, and exposes read access to plugins via `ctx.progress` (e.g. bingo's spoiler protection). One source of truth for "has this user heard this episode".

**An explicit `t` (§6.4) wins over stored progress for that navigation, and does not overwrite it.** Someone following a timestamped link asked for that spot; someone who was partway through the episode did not ask to lose their place, and a link they were sent should not be able to move it. The stored position is only rewritten once playback has actually advanced past the shared one — the difference between having looked and having listened — and a manual scrub ends the hold at once, being a deliberate statement about where the listener is.

### 6.6 SEO & crawlers
Same machinery as §6.4 (the server knows episodes and can ask plugins), extended four ways:
- **`sitemap.xml`** — generated dynamically: all episode URLs, feed/season views, legal pages, plus plugin URLs via the optional **`SitemapProvider`** extension point (§7.4; wiki implements it). Referenced from robots.txt.
- **`robots.txt`** — served by core: admin/API paths disallowed, sitemap referenced. **AI-crawler policy (GPTBot, ClaudeBot, PerplexityBot, …) is an admin setting**, not hardcoded — operators decide. Optional `llms.txt` alongside.
- **Structured data (JSON-LD):** `PodcastSeries` on the site/feed pages, `PodcastEpisode` on episode pages — fed by the same data the OgResolver already has.
- **Content for non-JS crawlers:** Googlebot renders JS, but many crawlers (and most AI crawlers) don't. Core renders a **simple server-side HTML content block** into the served `index.html` (episode list on feed pages; title/description/show notes on episode pages); the React shell replaces it on mount. No SSR framework — a plain template is enough.
- **Hygiene:** `rel=canonical` (normalize filter query params — they're in the URL per §6.1, so prevent duplicate content), `hreflang` once >1 UI locale is active, `<link rel="alternate" type="application/rss+xml">` for feed discovery, and **real HTTP 404s** for unknown episodes/routes (no soft-404).
  - **`hreflang` needs a URL per language, and `?lang=<code>` (§12.7) is it.** An alternate is a promise that fetching that URL returns that language, so it cannot be built on a preference the server cannot see. A path prefix (`/de/…`) is **rejected**; the parameter is the whole scheme. `sitemap.xml` carries the set as `xhtml:link` alternates — reciprocal and self-referential, every URL in a group listing every URL including itself — with **`x-default` on the bare URL**, which is the site default by construction.
  - **The site default is never carried in a canonical URL.** Same rule as `order=newest`: a parameter whose value is what its absence already means would canonicalize one view two ways, and that doubling is the cost that has to stay bounded for a query parameter to be the right scheme here. So one extra URL per *translated* view, not one per view. An unrecognised `lang` likewise canonicalizes bare, so guessing at the query string mints nothing indexable.
  - **A page is only advertised in a language it is really written in.** Where the host falls back to the default language for a reader — a legal page with no translation for that locale (§12.6) — it must *not* emit an alternate: the fallback is a kindness to a visitor and a lie to a crawler, which would be told a translation exists and handed the original. Feed content is exempt from the question: it stays in its original language (§12.7), so only the surrounding UI differs.
  - **A plugin's pages are its own to describe.** The host emits no alternates for a `SitemapProvider` entry unless the plugin declared them — inventing a translation group on a plugin's behalf would be a claim about content the host cannot read.
  - **That last rule reaches into plugin subtrees, and only the plugin can enforce it.** Core knows a plugin declared a `page` slot; it cannot know that `/p/wiki/nowhere` is not a page, so every subpath answered 200 and a crawler indexed a wiki's typos and its deleted pages while `sitemap.xml` listed only the real ones. The optional **`PageRouteProvider`** (§7.4) is the plugin answering. **Absent means yes**, so a plugin that does not implement it behaves exactly as before, and a provider that throws leaves the route at 200 — a broken plugin must not be able to turn its own working pages into 404s. It is deliberately *not* folded into `ShareMetadataProvider`: a subtree legitimately holds views with nothing to describe (a search result page should not claim to be a shareable document), so "no OpenGraph" must never mean "no page".

---

### 6.7 Site-wide search
`/api/search?q=` searches episodes **and** whatever active plugins say about their own content (`SearchProvider`, §7.4). Before it, core searched episodes and nothing else, so a plugin with searchable content grew a second search box on the same site — right for its own data, wrong for a visitor, who then had two places to type the same query and no way to learn the answer was in the other one.

- **Grouped by source, never merged into one ranking.** A plugin's score and Postgres `ts_rank` are not on one scale; interleaving them produces an order nobody can explain and one that changes meaning whenever a plugin changes how it scores. A section per source stays honest and survives that. Merging would need a shared scale first, not a sort.
- **A provider gets a budget** (per provider, on a request) and a section that misses it comes back **marked rather than dropped** — "found nothing" and "did not answer" are different answers, and a visitor given the first concludes the content is not on this site.
- **The host resolves the URL.** A hit names a subpath; the host resolves it under `/p/<pluginId>/` and drops `.`/`..` segments exactly as `ctx.route.navigate` does, so a result cannot point at a core route or another plugin.
- **Access is the plugin's job here** — the one place in this contract where it is. See §7.4.

`/api/episodes/search?q=` remains as the episode-only, paginated query for callers that want exactly that.

## 7. Plugin System (the heart)

### 7.1 Structure
A plugin = **one folder**: backend JAR (PF4J extension) + `frontend/` (built Web Component bundle) + `plugin.json`. Loaded from a plugins folder at startup. **Trusted, in-process, no sandboxing** — the admin carries responsibility, also for 3rd-party.

**Configurable plugins path (no forced folder layout):** core reads the plugins folder from `MOSAICAST_PLUGINS_DIR` (env/property, default `./plugins`). Plugins know **nothing** about it — their `build.sh` only writes to their own local `dist/` and never touches core. Distribution (`dist/` → plugins folder) stays a **separate step**; location is free to choose (sibling folder, central folder, Docker volume). An optional `install.sh` in a plugin may shortcut the copy if `MOSAICAST_PLUGINS_DIR` is set — never as a requirement.

**Installing by spec (no registry).** Copying by hand remains valid and always will, but it is not the only path: `scripts/install-plugin.sh` resolves `owner/repo[@tag][#sha256:…]`, a tarball URL or a local file, and in a container `MOSAICAST_PLUGINS` does the same **before the JVM starts** — plugins are read once at startup, so anything arriving later would stay invisible until the next restart. **GitHub Releases are the index**: a fixed `plugin.tgz` asset reached through the plain `releases/…/download/` redirect, which is what keeps this an API call, a token and a JSON parser lighter than a registry would be. A central registry is not ruled out, but nothing here needs one yet.
- The installed folder name comes from the manifest's own `id`, never from the repo name — a folder that disagrees with its id is rejected at load anyway, so guessing would only move the error somewhere less obvious.
- An unresolvable spec **fails the container** rather than booting without it: an instance that looks healthy while silently missing a plugin the operator asked for surfaces later as a broken page instead of the deploy error it is. Restarts are idempotent — an already-installed spec is recognised without re-downloading.
- **Pin a tag *and* a checksum.** Plugins are trusted, in-process and unsandboxed (above); making installation one env var away does not change that, so the documented default is the form an operator can audit. The checksum is the only integrity control this model has.

> **Known rough edge:** PF4J classloading inside a Spring Boot fat JAR is fiddly. Keeping plugins Spring-free (plain PF4J extensions against the SDK) is the deliberate mitigation; still budget real time for the classloader setup in core (E5) instead of fighting it late.

### 7.2 Manifest (`plugin.json`)
```json
{
  "id": "bingo",
  "version": "1.0.0",
  "platformApi": "0.17.0",
  "name": "Bingo",
  "license": "AGPL-3.0-or-later",
  "author": "The Mosaicast Authors",
  "homepage": "https://github.com/Mosaicast/mosaicast-plugin-bingo",
  "backend":  { "basePath": "/api/plugins/bingo", "extensions": ["dev.mosaicast.plugin.bingo.BingoPlugin"] },
  "frontend": { "entry": "bingo.es.js", "elements": ["bingo-episode-card", "bingo-host-board"] },
  "slots": [
    { "scope": "episode", "element": "bingo-episode-card", "placement": "main",    "visibleTo": "anonymous", "order": 100 },
    { "scope": "episode", "element": "bingo-host-board",   "placement": "admin",   "visibleTo": "podcaster" }
  ],
  "storage": "doc",
  "data":    { "readableBy": "fan", "writableBy": "podcaster", "backendOwned": ["stats", "agg:*"],
               "keyFloors": [ { "keys": ["import:*"], "readableBy": "podcaster" } ],
               "readsAllUsers": true },
  "config":  { "fuzzyThreshold": { "type": "number", "default": 0.85, "editableBy": "podcaster",
                                   "min": 0, "max": 1 },
               "rankBy": { "type": "string", "default": "lines", "editableBy": "admin",
                           "options": [ { "value": "lines",  "label": { "en": "Lines",  "de": "Reihen" } },
                                        { "value": "fields", "label": { "en": "Fields", "de": "Felder" } } ] } },
  "tags":    { "readsVocabulary": true, "writesEpisodes": false },
  "external": { "kinds": ["translation"], "usedBy": "podcaster" },
  "consent": { "services": [] }
}
```
- **`platformApi`**: which SDK contract version it was built against. The host **rejects incompatible plugins at startup** (stability anchor). The match is an exact `major.minor` (pre-1.0 the minor carries breaking changes); the patch floats.
- **`frontend`**: the ES module `entry` and the custom `elements` it registers. `entry` is the one manifest string that becomes a URL path (`/plugins/<id>/assets/<entry>`), so it has a grammar like every sibling path-shaped string: a relative path under the plugin's own `assets/` — `[A-Za-z0-9._-]` segments joined by `/`, no leading `/`, no `.`/`..` segment, no query or fragment. A non-matching entry **rejects the plugin at load** rather than being rewritten: loading a URL the author did not write is worse than naming the mistake (`FRONTEND_ENTRY_PATTERN` in the SDK).
- **`slots`**: scope + Web Component + `placement` (named region) + `visibleTo` (minimum role) + optional `order`.
- **`storage`**: `"doc"` = generic JSONB store. For the wiki, a schema declaration instead (§7.6), optionally with **`schemaReadableBy`** — the schema read surface's own floor, any of the four roles, defaulting to `data.readableBy` (platformApi 0.19.0). It is a sibling of `schema` rather than inside it because `schema` maps entity names, and a floor there would take one up.
- **`data`**: the access floor of the generic data surface (§7.6) — `readableBy` / `writableBy`, each `anonymous | fan | podcaster | admin`; `writableBy` may not be `anonymous`. **Declared, never derived:** the host does not infer it from slots. An absent block defaults `readableBy` to the *write* floor, not to anonymous — a **behaviour change**: a plugin that relied on an anonymous slot making its data anonymously readable must now declare `readableBy: "anonymous"` to keep it. A slot's `visibleTo` governs **rendering only**. Neither floor applies to the `USER` scope (§7.6). `backendOwned` names the keys only the plugin's **backend** may write — an exact key, a `*`-terminated prefix, or the bare `*`; clients may still read them (subject to `readableBy`) and a client `PUT`/`DELETE` is a **403** the host words apart from the role-floor one, so an author can tell which rule refused them. `keyFloors` (platformApi 0.19.0) raises the read or write floor of the keys it names — `[{ "keys": [...], "readableBy"?, "writableBy"? }]`, the same selector grammar as `backendOwned`. Together they are the only **per-key** rules on this surface; see §7.6 for why they are needed and what they do not do. `readsAllUsers: true` grants the backend's `ctx.allUsers()` — the read of **every** user's `USER` partition, owner ids included (§7.4). Absent means no. It is declared because it is the one read that crosses an ownership boundary: "can enumerate every account that ever used me" is not what the `USER` scope alone implies, and an operator should be able to read it off the manifest — the admin plugin page shows it. Until platformApi 0.16.0 every plugin had it by existing, as `DocStore.queryAcrossUsers`.
- **`blobs`**: opt-in file storage (§11.1) — `maxFileBytes`, `quotaBytes`, `mimeTypes`, and optionally its own `readableBy` / `writableBy` (defaulting to the `data` floors). Declared, never derived, like `data`: what a plugin may write to disk should be readable off its manifest. Absent = no file storage at all. The operator caps every number and intersects the type list with the install's own, so a plugin is granted the smaller of the two rather than rejected for asking. `image/svg+xml` is refused at load — SVG is never storable (§12.2).
- **`tags`**: opt-in access to the shared tag vocabulary (§6.1.1) — `{ "readsVocabulary": true, "writesEpisodes": false }`. Declared, never derived, like `data` and `blobs`; absent means **no tag surface at all** (`ctx.tags` is null, the endpoints 404). Two flags because the two acts are not alike: tagging a plugin's own subjects touches rows nobody else can name, while tagging an **episode** changes the shell's filter options and what core recommends beside that episode — a capability an operator should be able to read off a manifest before installing. A block declaring neither is refused at load, since it would produce a surface that exists and refuses everything.
- **`identity`**: opt-in resolution of user UUIDs to a name and a picture (§8.8) — `{ "resolvesUsers": true }`. Declared, never derived, like `data`, `blobs` and `tags`; absent means `ctx.users` is null and the endpoint 404s. It is a declaration and not a derivation because a plugin already *holds* user ids — the doc store's `USER` scope and the cross-user read (`ctx.allUsers()`, declared as `data.readsAllUsers`) both hand them over — so the capability being granted is not access to the ids but the turning of them into people, and that is the part an operator should be able to read off a manifest before installing.
- **`notifications`**: opt-in ability to put a message in a user's inbox (§17) — `{ "sends": true, "perUserPerDay": n }`, the number being what the plugin *asks* for and the operator's cap what it gets. Declared, never derived, like the blocks around it; absent means `ctx.notify` is null and the endpoint 404s. This is the one plugin surface that **writes into another user's view of the site**, so it is the one an operator most needs to see before installing.
- **`external`**: opt-in use of the instance's external services (§16) — `kinds` names them, `usedBy` is the lowest role that may trigger a call from the plugin's **UI** (default `podcaster`, matching `data.writableBy`'s floor). Declared, never derived, like `data`, `blobs` and `tags`; absent means no external surface at all. `kinds` is a list although translation is the only member today, so a plugin that later wants transcription adds an entry rather than a second block.
- **`consent`**: third-party services the plugin loads — one declaration each (`id`, `name`, `provider`, `category`, `privacyUrl`, `hosts`, `thirdCountryTransfer`, `storage[]`); the visitor decides per *category*, and `hosts` doubles as the CSP allow-list (§12.5). A category the plugin introduces (anything but `necessary`, `functional`, `analytics`) owes the visitor a name: **`categoryLabels`**, keyed by category id, gives each a `label` and optional `hint` in the localized-text shape a config label uses (§12.5). Omit the key entirely when the plugin loads nothing third-party.
- **`config`**: declared fields are rendered by core as a **generic admin form** (respecting `editableBy`) — plugins never build their own config UI. A field may carry **`label`** and **`description`**, which is what an operator actually reads: without them the form could only show the identifier its author chose — `ingestIntervalSeconds (podcaster)` — with no room to say what the setting does, what unit it is in, or what a sane value looks like, and the plugin cannot make up the difference because building its own UI is exactly what it may not do. Both take the same two shapes as an option's label (a plain string, or an object keyed by locale) and resolve in the browser by the same fallback chain; the field key stays visible beside the label, because a plugin's own documentation names the identifier. Prose is not a correctness concern, so the host validates only the shape and never the content, and a field that declares neither reads exactly as it did before. Purely additive, like `options` — **no `platformApi` bump**. A field may declare **`options`**, the closed set of values it accepts: core renders a select instead of a text box and **refuses anything outside the set**, at load for the manifest's own `default` and at write time for an operator's override. Without it a field whose plugin understood two words was a free-text box, where a typo validated, stored, and then fell back silently at read time — reporting a saved setting that did nothing. Each option's `label` is the one manifest string the host **localises**: either a plain string, exactly like the verbatim `nav` and `consent` labels, or an object keyed by locale (`{ "en": "Lines", "de": "Reihen" }`), resolved in the browser against the language the operator is reading in — falling back locale → base language → `en` → any label → the raw value. It is resolved there rather than server-side because the host learns the operator's language from the SPA, and switching it must not need a refetch. Purely additive: an older manifest declares no options and stays free-form, so **no `platformApi` bump**. A field may also declare **bounds** — `min` / `max` / `step` on a `number`, `minLength` / `maxLength` on a `string` (platformApi 0.16.0). Without them any number an operator could type was legal, including the `0` that switched a scheduled task off while the form said "Saved." The host **refuses** an out-of-range write with a 400 that names the bound (never a silent clamp), renders the bounds as input constraints, and refuses at load a manifest whose bounds cannot mean anything — a bound on the wrong type, `min > max`, a non-positive `step`, or a `default` outside its own bounds. A value stored **before** a bound existed that now breaks it counts as **unset**, so the default applies: what a plugin reads through `config()` always satisfies its declaration, and it needs no clamp of its own. There is no `pattern`: Java and ECMAScript regex dialects differ, and one field's regex would be two rules free to disagree.
- **`license` / `author` / `homepage` / `attribution`**: credit, shown on the public About page (§12.6). All optional and **never validated** — a plugin written before these existed must keep loading, and an oddly-spelled licence is still a working plugin; credit is not a correctness concern. `attribution` is separate from `homepage` because "where this lives" and "who deserves credit for it" are not the same link: a plugin that borrows data, artwork or an upstream library should be able to say so without giving up its own page. Purely additive in both directions — the host ignores unknown manifest fields, and the SDK's `PluginManifest` type is documentation for an author's editor with no runtime effect, the host remaining the sole validator — so **no `platformApi` bump**, which matters because that check is an exact `major.minor` match and a bump would reject every installed plugin until each one re-released.

### 7.3 Slots & placements
- Regions (`top`, `card`, `main`, `sidebar`, `player`, `feed`, `site`, `admin`, …) are defined by the **host shell** per view. Plugins only target existing names; an unknown region → startup rejection. The **`feed`** and **`site`** regions are the scope panels' plugin spaces (§6.1): `feed` on a feed tab, `site` on the All tab.
- **Multiple plugins in one region → stacked** vertically, sorted by `order` (ties: plugin ID alphabetical). No "battle royale". Admin can steer `order` via config.
- `card` placement (episode, compact): plugins show a one-liner on the feed card (stats summary, bingo badge), full rendering only in the detail page `main`. A plugin declares `card` or omits it.

### 7.4 Backend contract (in the SDK)
```java
public interface PluginBackend extends ExtensionPoint { void register(PluginContext ctx); }
public interface PluginContext {
    DocStore     store();    // (scope, key) → JSON, hard-scoped
    SchemaStore  schema();   // only present if the manifest declares schema
    PluginBlobs  blobs();    // only present if the manifest declares `blobs` (§11.1); null otherwise
    Tags         tags();     // only present if the manifest declares `tags` (§6.1.1); null otherwise
    Users        users();    // only present if the manifest declares `identity` (§8.8); null otherwise
    Notifier     notifier(); // only present if the manifest declares `notifications` (§17); null otherwise
                             // `notifier`, not `notify`: Object.notify() is final in Java
    CrossUserStore allUsers(); // only present if the manifest declares `data.readsAllUsers` (§7.2); null otherwise
    PluginConfig config();
    FeedAccess   feeds();
    void onSchedule(Supplier<Duration> every, Runnable task); // ShedLock-wrapped; period re-read per tick
    default void onSchedule(Duration every, Runnable task);   // fixed cadence, captured once
    default void onEpisodeReleased(Consumer<String> slug);    // a planned episode became RELEASED (0.18.0)
    default void onEpisodePhaseChanged(BiConsumer<String, EpisodePhase> listener); // a write moved it (0.19.0)
}   // The supplier form (platformApi 0.15.0) is what a configurable interval needs: the host consults it
    // before every fire and reschedules when the answer changes, so an operator's edit takes effect within
    // one old period instead of at the next restart. It is consulted, not trusted — null, a non-positive
    // Duration or a throw leaves the task on the last period that was valid, and only the value at
    // registration is strict. The host clamps to an operator-owned floor (`mosaicast.plugin-schedule
    // .min-period`, default 10s): the period is a request, like the manifest's other numbers.
    // onEpisodeReleased fires once a planned episode is released (§4.3, §5.3), after the commit, so a
    // listener reads the released state. Each call runs off the releasing thread, and one that throws is
    // logged in that plugin's log without affecting the others. Best effort: nothing is queued or retried,
    // and a plugin that was not loaded misses it — it reconciles by reading `DisplaySnapshot.phase()`.
    // onEpisodePhaseChanged (0.19.0) is the general case: it fires when a *write* changes an episode's
    // derived phase — compared before and after the write at one instant — with the slug and the new
    // phase: announce, an `announceAt` edit either way, every release path, a withdrawal, a withdrawn
    // episode returning, and an episode that no longer exists (phase null): a cancelled plan, the
    // duplicate a match or a confirmed suggestion removed, or every episode of a deleted feed. The clock passing
    // `announceAt` fires nothing, since no write happened and becoming visible late is harmless; becoming
    // *hidden* late is a leak, which is why the write that hides an episode must tell plugins at once. Same
    // delivery as onEpisodeReleased, and on a release those listeners run first.
    // The snapshot carries `phase` and `announceAt` beside the placement fields (§4.2): filled on read
    // from the identity layer, never stored.
interface DocStore {
    <T> Optional<T> get(Scope scope, String key, Class<T> type);
    void            put(Scope scope, String key, Object value);
    boolean         delete(Scope scope, String key);          // idempotent
    List<DocEntry>  query(Scope scope, String keyPrefix);     // keyed: (key, value)
}   // Scope = (SITE|FEED|SEASON|EPISODE|USER, id)
    // USER is host-owned: its id is always the sentinel "me" and the host substitutes the authenticated
    // caller. A plugin cannot name another user's partition, and an anonymous caller has none.
    // A backend thread has no caller, so every DocStore method above throws UnsupportedOperationException
    // for a USER scope — reads included. Aggregates go through allUsers(), which names each owner:
interface CrossUserStore {
    List<OwnedDocEntry> query(String keyPrefix);              // every USER partition; backend-only, read-only,
}                                                             // no HTTP surface (was DocStore.queryAcrossUsers)
record DocEntry(String key, JsonNode value) {}
record OwnedDocEntry(UUID userId, String key, JsonNode value) {}   // userId is host-resolved, never client-supplied

// Optional extension point (a plugin MAY implement it; wiki does in v1):
interface ShareMetadataProvider {
    Optional<OgMeta> metaFor(String subpath);   // subpath under /p/<pluginId>/
}
record OgMeta(String title, String description, String imageUrl) {}  // imageUrl nullable

interface SitemapProvider {          // optional extension point (§6.6); wiki implements it
    List<SitemapUrl> urls();         // absolute-path locs under /p/<pluginId>/…
}
record SitemapUrl(String loc, Instant lastModified) {}   // lastModified nullable

interface PageRouteProvider {        // optional extension point (§6.6); absent ⇒ every subpath renders
    boolean hasRoute(String subpath);   // subpath under /p/<pluginId>/; never null, empty at the root
}

interface SearchProvider {           // optional extension point (§6.7)
    List<SearchHit> search(String query, Role role, int limit);   // role is null for anonymous
}
record SearchHit(String subpath, String title, String snippet, double score) {}

interface UserDataHandler {          // optional extension point (§12.8)
    void eraseUser(String userId);                                    // erase OR pseudonymise — the plugin's call
    default Optional<Map<String, Object>> exportUser(String userId) { return Optional.empty(); }
}
```
**`SearchProvider` is the one place where access is the plugin's job.** Everywhere else in this document the host resolves access and the plugin consumes the result (§7, §10) — but core has no model of a plugin's objects and cannot know that a row carries a `published` flag or that a revision is visible only to its author. So the caller's role is passed through and **a provider that returns a draft to an anonymous visitor is a leak the host will not catch**. It is stated here, and not only in Javadoc, because it is a real exception to a rule the rest of the system relies on.

### 7.5 Frontend contract (in the SDK, TS) – the `ctx`
The host mounts the custom element and sets `ctx`. This is the **entire** interface a plugin author has to learn:
```ts
interface PluginContext {
  scope:    { type: 'site'|'feed'|'season'|'episode'; id: string };
  episodes: string[];                 // EpisodeRef IDs in scope (resolved by the host, for this viewer:
                                      // a quiet planned episode only for a podcaster or admin, §4.3)
  episode?: { status: 'PLANNED'|'PUBLISHED'|'WITHDRAWN';   // on episode scope (§4.3):
              phase: 'planned'|'upcoming'|'released'|'withdrawn';   // the derived phase (0.18.0)
              announceAt?: string };  // ISO instant, while planned; a phase change hands over a new ctx
  user:     { id: string; role: Role; displayName: string; avatarUrl: string } | null;
  users:    UserDirectory | null;     // resolve(ids) → who the other UUIDs are; null unless the
                                      // manifest declares `identity` (§8.8)
  notify:   NotifyClient | null;      // send(userIds, message) to users this plugin already holds
                                      // data for; null unless the manifest declares it (§17)
  api:      PluginApiClient;          // calls /api/plugins/<id>/* with auth token; rejections carry
                                      // `status` + the RFC-7807 body, and getOrNull resolves absence to null
  docs:     DocClient;                // typed doc store over the same endpoints, plus getMany(type, ids, keys);
                                      // dedupes and remembers misses (§7.6); never null
  feeds:    FeedsClient;              // display(slug) / displayMany(slugs) — the frontend half of FeedAccess;
                                      // displayMany splits past 200 slugs and merges, never clamps (0.19.0)
  sanitize(html: string | null | undefined): string;  // the shell's own HTML policy; never null
  schema:   SchemaClient | null;      // read-only; null unless the manifest declares a schema (§7.6)
  blobs:    BlobClient | null;        // upload/list/delete; null unless the manifest declares `blobs` (§11.1)
  tags:     TagsClient | null;        // null unless the manifest declares `tags` (§6.1.1)
  consent:  { has(cat: string): boolean; onChange(cb: () => void): void };
  filter:   { current(): FilterState; onChange(cb: (f: FilterState) => void): void }; // read-only
  player:   { currentTime(): number; seekTo(s: number): void; on(ev, cb): void };     // for sync plugins
  route:    { path: string; query: URLSearchParams; hash: string;        // subpath under /p/<pluginId>/ (§6.4)
              onChange(cb: (p: string) => void): void;
              navigate(subpath: string, opts?: { replace?: boolean }): void };  // within that subtree only
  links:    { episode(slug, opts?): string; feed(slug, opts?): string }; // host URL shapes, strings only
  locale:   { current(): string; onChange(cb: (l: string) => void): void;  // active UI locale (§12.7)
              available(): LocaleInfo[]; content(): LocaleInfo[] };      // the site's languages (§12.7)
  translation: TranslationClient | null;  // host-mediated MT; null unless the manifest declares the kind
                                          // *and* a provider is configured — two reasons (§16)
  progress: { get(episodeId: string): Promise<number | null> };          // core listening progress, seconds (§6.5)
  theme:    ThemeTokens;              // host colors as CSS variables, incl. accentText (§12.3)
}
```
**Important:** plugins *consume* filters, they don't *define* them. Filter axes (season, tags, sorting) belong to the host.

**`ctx.feeds` is the one plugin surface with no read floor of its own**, and the exception is worth writing down because "every plugin surface is gated by the manifest floor" is otherwise a reliable rule. It returns host data the same visitor can already read from `/api/episodes/*`; it exists so a plugin need not know that URL shape, which is the argument §6.4 makes for `ctx.links`. What it must not become is a way to see *more* than the visitor can, so the host filters the answer: a `WITHDRAWN` or gated episode is **absent rather than redacted**, and `display()` deliberately cannot tell "no snapshot" from "not visible" — distinguishing them would confirm the existence of an episode this visitor was not shown.

**`ctx.sanitize` is the shell's own policy, handed to plugins** (platformApi 0.16.0). A plugin renders HTML it did not write — a snapshot's `description` (§4.2), a podcaster's Markdown, a user's rich text — and without a sanctioned sanitizer it reached for a library's defaults. DOMPurify's defaults stop scripts but permit `<style>` and `style=`, and the plugin contract requires `style-src 'unsafe-inline'` (Web Components style their shadow roots), while `img-src` stays open to any `https:` origin for artwork: together that makes a stylesheet in someone else's HTML a full-viewport click-jacking overlay or attribute-selector CSS that exfiltrates form values. Both shipped plugins had exactly that. So the shell's allow/forbid lists live in the SDK as plain data (`FEED_HTML_POLICY`), the shell's own sanitizer imports them, and `ctx.sanitize` applies them: one policy, which a plugin cannot weaken by writing less code. It needs no declaration, because it grants nothing. **The lists are the whole policy, in both directions**: a library default that would widen them (DOMPurify keeps every `data-*` and `aria-*` attribute unless told otherwise) or narrow them (it holds non-URL attributes such as `lang` or `colspan` to the URI allow-list) is switched off, and the host's sanitizer is tested for identical output against the SDK test kit's `sanitizeLikeHost` — a plugin whose test passes in the kit sees the same result in production (core#232).

**Rejections are typed.** The host attaches the HTTP status and the problem body to what `ctx.api` throws, because the contract already words two different 403s apart on purpose — the read floor refused you, versus this key is `backendOwned` (§7.2) — and before this a plugin could not read the distinction it had been given. `getOrNull` exists for the same reason in the other direction: "nothing saved yet" is the normal state of a doc-store key, and expressing it as a rejection meant every plugin wrote a `catch` that also swallowed the 500 and the network failure.

**`ctx.scope` stays four-valued.** `USER` (§7.6) addresses storage, not a page — a slot is mounted into a named region of a view and there is no user view — so it appears in a `data/{scopeType}/{scopeId}/…` path and never in `ctx.scope`. A component reading per-user data addresses `data/user/me/…` explicitly while its `ctx.scope` remains whatever page it is on.

**`route.navigate` is the only outbound handle on this context**, and it is namespace-confined by the *host*, not by the plugin's good behaviour: the subpath is resolved under `/p/<pluginId>/`, a leading `/` is stripped and `..` segments are dropped, so another plugin's route or a core one is unnameable rather than merely refused — the same property `ctx.schema` has for tables. It exists because a `page` plugin owns a URL subtree and had no supported way to move inside it: an `<a href>` is a full document load that re-fetches the shell, the plugin registry and every plugin bundle, and the workaround that *worked* (`history.pushState` plus a synthetic `popstate`) silently coupled plugins to the host's router choice. Keep the real `href` on links — middle-click, "open in new tab" and crawlers need it; `navigate` only takes over the plain-click path.

### 7.6 Generic store + schema provider
- **Access floor:** declared in the manifest (`"data"`, §7.2), never derived from slots. The `USER` scope is exempt from **both** floors — see below.
- **Authorization is per plugin, not per document.** A floor says *who*, never *which key*: clearing `writableBy` grants every key in every **shared** scope (`SITE`/`FEED`/`SEASON`/`EPISODE`), including one another caller wrote, because nothing binds a shared document to its author. Two rules narrow that, and neither closes it in general.
  - **`data.backendOwned`** (§7.2) reserves the keys the plugin's *backend* authors. Without it a plugin cannot express "this key is mine": the host cannot tell a scheduled write from a `curl`, so any caller above the write floor could forge a value the backend computed — a site-wide aggregate, a per-episode count — and have it served to every visitor as though the plugin had produced it. Reads are untouched, since the point is to publish a value rather than hide it, and `ctx.store()` is unaffected — enforcement is on the HTTP surface only, so the `DocStore` contract does not change. It is **ignored for `USER` scopes**, even under a bare `*`: a backend cannot write a partition there at all, so reserving one would reserve it for nobody. A declaration does **not** remove a value a client wrote before it existed, so a plugin writes its computed keys in `register(ctx)` as well as on a schedule. A malformed entry **rejects the plugin at load** (§7.8) rather than being dropped — a dropped entry loads a plugin whose manifest claims a key is the backend's while the host enforces nothing, which is the worst way for a security declaration to fail.
  - **`data.keyFloors`** (§7.2, platformApi 0.19.0) raises the floor of named keys: public numbers beside private bookkeeping, an admin-only setting beside podcaster-writable ones. **Raise-only** — a floor below the plugin's own (`readableBy` against the effective read floor, `writableBy` against the write floor), `writableBy: "anonymous"`, an empty `keys`, an entry with neither floor or a malformed selector **rejects the plugin at load**, for the reason `backendOwned` does. Where several entries match a key the **strictest** wins, per direction. A write is checked plugin floor → `backendOwned` → key floor, each with its own problem type (`…/problems/forbidden`, `…/backend-owned-key`, `…/key-floor`), so an author can tell which refused them. A single `GET`/`PUT`/`DELETE` below a key's floor is a **403**; a **listing** drops such keys *before* paging, so its totals count only what the reader may see; a **batch read** leaves them absent, like a miss. Ignored for `USER` scopes and for `ctx.store()`, like `backendOwned`. It is a minor bump rather than a silent addition because the host ignores unknown manifest keys: a host that predates it would load the plugin and serve its "private" keys at the plugin floor.
  - The **`USER` scope** gives per-user data a partition no request can name (below).
  - **What remains open:** an unreserved key in a shared scope still has no owner, so on a multi-podcaster install one tenant can overwrite or delete another's plugin data. Closing it needs per-record ownership, which needs an owner in the domain model — feeds do not have one yet (§5, roadmap §14). Until then a plugin that needs the guarantee reserves the key or keeps the data in `USER` scope, and a plugin author should know the host does not otherwise make it.
- **Default store:** `plugin_data(plugin_id, scope_type, scope_id, key, value JSONB)` + GIN index. Scales comfortably to thousands in Postgres. Optional: a plugin declares **indexable fields** (expression index) as an escape hatch. Writes are **last-write-wins** — plugins needing stronger concurrency control model it in their data design. **Per-user data belongs in the `USER` scope, never in the key.** A key is client-supplied, so a convention like `mark:<userId>:cell` is an access-control decision the host cannot enforce: any caller above the plugin's read floor can address another user's key directly, and scope ids are public slugs, so nothing has to be guessed. The `USER` scope is addressed as `user/me` and resolved server-side from the session, so the partition a caller reaches is the only one they can name. **Neither floor applies to it.** `readableBy` does not, in either direction: no floor makes another user's partition readable, and none stands between a caller and their own. `writableBy` does not either — a write floor protects the *shared* surface, where one caller's write is visible to others and can overwrite theirs, and a `USER` partition is unshared by construction, so there is nobody to protect from it. Gating it would also re-create the coupling this rule exists to remove: a plugin needing a per-user feature would have to declare `writableBy: "fan"`, opening its *shared* scopes to fan writes — the same "one setting forced by an unrelated need" failure as the old minimum-across-slots read floor, moved to the write side. So any authenticated caller reads and writes their own `user/me` whatever the plugin declares; any other `user` id is a **400** (never a silent substitution) and an anonymous request a **401**, since there is no session to resolve. The partition is flat — one per user, not one per user *and* entity — so the entity goes in the key (`mark:<episodeSlug>:cell`). Aggregating across users is the backend's job via `ctx.allUsers()` (§7.4, declared as `data.readsAllUsers`), not the client's: a summary each browser reports about itself is a summary of whatever users typed.
- **"Not set" is an answer, not an error.** An absent key answers **204**; **404** keeps its meaning for a wrong address (unknown or disabled plugin, unknown scope type, a scope naming nothing). Before this a miss was a 404, which no client could cache — on one real instance 98% of a session's plugin requests said nothing more than "not set", one key asked 178 times. A **batch read** — `GET /api/plugins/{id}/data/{scopeType}?ids=…&keys=…` → `{ scopeId: { key: value } }`, misses absent, at most 100 ids and 100 keys, the same floors per id — lets a page of cards ask once. The host's client promises, per plugin and signed-in identity: identical reads in flight share one request; a miss is remembered **briefly** — for a short while (30 s) and never across a navigation, batch misses included; the plugin's own writes forget the address they touched; hits are never cached; errors are never remembered — so a plugin needs no miss cache of its own. The memory is deliberately short: keys the plugin's backend writes on its schedule and keys another session writes start out unset and appear later, and a page-long memory hid them until a reload (core#237). What it exists for — re-render bursts milliseconds apart asking for the same key — all lands inside the window.
- **Schema provider (for relational plugins like the wiki):** the manifest declares entities + indexed/FTS fields; the **platform** provisions dedicated, namespaced tables (`plugin_wiki_*`) via a **platform-managed migration runner** and cleans up on uninstall. (Implementation note: Flyway itself is static — dynamic per-plugin DDL is applied programmatically with the platform's own bookkeeping table; "never trust plugin DDL" still holds.) **The plugin never writes DDL.** Every plugin may use the mechanism; most declare nothing.
  ```json
  "storage": { "schema": { "page": {
      "slug": "string:indexed:unique", "title": "string",
      "markdown": "text:fulltext", "updatedAt": "timestamp:indexed" } } }
  ```
- **Reading a schema from the frontend:** provisioning without access is half a feature — the full-text index the platform builds exists for a search box, and the search box is in the browser. So the host serves a **read-only** surface per plugin, mapping one-to-one onto `SchemaStore` and reached through `ctx.schema` (`null` for a doc-store plugin, mirroring `ctx.schema()`):
  ```text
  GET /api/plugins/{id}/schema/{entity}?where=&orderBy=&page=&size=
  GET /api/plugins/{id}/schema/{entity}/search?field=&q=&where=&orderBy=&page=&size=
  GET /api/plugins/{id}/schema/{entity}/count?where=
  GET /api/plugins/{id}/schema/{entity}/{rowId}
  ```
  A query is *described*, never written: `where=field:op:value` and `orderBy=field:asc|desc` name **declared** fields, the host resolves them against that plugin's own manifest and builds the statement, and every value is bound as a parameter — so this adds no injection surface over what `SchemaStore` already had. Values are read against the field's declared type, so a malformed one is a **400** rather than a driver error. Access is `storage.schemaReadableBy` when declared, otherwise the same `data.readableBy` floor as the doc surface (platformApi 0.19.0) — so a plugin whose tile is anonymous can keep per-user rows, or rows naming a quiet episode, from anonymous visitors, the reasoning that gave `blobs` its own floor. It gates all four paths alike. There is no `USER` exemption to make here because schema tables are per plugin and these paths carry no scope. An undeclared **entity** is a **404** (over HTTP it is a path segment, i.e. an address); an undeclared **field**, an unreadable value, or `search` on a field that is not `:fulltext` is a **400**. Paging follows the doc surface (`page` from 0, `size` 50, capped at 200).
- **No schema writes over HTTP, and this shapes plugin design.** A v1 plugin authors no routes (§7.4), so no plugin code runs at request time — there is nowhere to enforce slug uniqueness, append a revision atomically, or reject malformed input. Exposing writes would hand clients direct row access with *no plugin code in the path*, which is worse than the doc store's position rather than better. The plugin's **backend stays the only writer of relational truth**: a frontend that must write puts a document in the doc store and the backend ingests it on its schedule. The consequence is that such a write is **eventually consistent** — an editor does not see its own save through `ctx.schema` until the next tick, and a plugin with an editing UI (the wiki) must design for that with an optimistic render or an explicit saving state. Changing this needs a request-time plugin hook, which is a v1-contract decision and not a gap to be filled quietly.
- **Not rate-limited.** `GET /api/plugins/**` is outside the rate limiter, which covers auth and upload paths, so an anonymous full-text search is capped only by `size`. Same posture as the doc-store list endpoint — but full-text is more expensive per call, so an operator exposing a large corpus should expect to front it.

### 7.7 On-request aggregation
Feed/season-level aggregates (e.g. stats sums) are computed **lazily**, the Web Component shows a loading bar meanwhile, the result is cached in the store and invalidated on new episodes.

### 7.8 Plugin lifecycle & failure isolation
Plugins are trusted, but **a broken plugin must not take the site down**:
- **Startup:** if a plugin fails to load (exception, bad manifest, `platformApi` mismatch), core **disables that plugin and keeps booting**, with a prominent admin warning — never a crashed host.
- **Frontend:** the shell wraps **every slot mount in an error boundary** — a throwing Web Component blanks only its own tile (with a small "plugin error" note), never the page.
- **Removal semantics:** deleting a plugin folder = plugin **dormant**; its data (doc store / schema tables) is **retained**. Actual data removal is an explicit admin action ("purge plugin data") — for both storage kinds. The schema provider's cleanup runs on purge, not on mere absence.

---

## 8. Auth & Identity

### 8.1 Login
Spring Security `oauth2Login`, **social-only to start**: Discord (clean OAuth2), Patreon (custom OAuth2 client via its own `OAuth2UserService`), Google optional. **No passwords** (GDPR). Password login stays cheap to add later (an extra `DaoAuthenticationProvider`).

### 8.2 Model
```
User           (id UUID, display_name, display_key, avatar_provider, role, created_at)
  ├─ LinkedIdentity  (provider, external_id, email, email_verified, avatar_ref, PK(provider, external_id))
  └─ UserNameHistory (user_id, name, set_at, set_by)
```
The stable key is `(provider, external_id)`, **not** the email. Keep the Discord `external_id` (future: bot/role sync).

`display_name` is what a reader sees and `display_key` its canonical form (§8.6); `avatar_provider` names the linked identity a picture is pulled from, or is null (§8.7). **Neither is identity.** A document, a log line, a plugin's rows and an erasure all key on the UUID, which never changes — the name is a label the person is free to replace.

### 8.3 Account merging (security rule)
On login `(provider P, external_id E, email Q, verified V)`:
1. `(P,E)` exists → log in.
2. `(P,E)` new **and a user is logged in** (linking from settings) → attach identity. **Always safe.**
3. `(P,E)` new, nobody logged in → only merge if **both emails are verified**; the conservative variant is better: **do not merge silently**, instead "please log in with the existing method and link in settings". With `V==false` **never** merge → new user.
In short: **auto-link only with two verified emails or a logged-in user, otherwise explicit.**

### 8.4 Settings: provider list
`GET /api/me/identities` → per configured provider `{provider, linked, email?, since?}`. UI shows a green check when linked, otherwise a "Connect" button (flow = merging case 2). **The last remaining identity cannot be removed** (lockout protection).

### 8.5 Sessions & roles
- **httpOnly cookie + server-side session** (Spring Session). **No JWT** (revoke/ban/role change must take effect immediately). In-memory by default in v1, or in **Postgres** (`MOSAICAST_SESSION_STORE=jdbc`) so sign-ins survive a restart → **Redis from v3** (app instances stay stateless). A session expires after `MOSAICAST_SESSION_TIMEOUT` without a visit (default 30 days), and the cookie lives as long, so closing the browser does not sign anyone out. Cookie sessions require **CSRF protection** (Spring's `XSRF-TOKEN` cookie pattern for the SPA) and `SameSite=Lax`.
- RBAC, `role` on the `User`: **ADMIN** (site config, users, plugin activation) · **PODCASTER** (bingos, wiki, episodes, feeds/Patreon sources, planned episodes) · **FAN** (fill in/view). Anonymous: read only.
- Bootstrap admin via env on first start; afterwards the admin promotes fans → podcasters.
- **Personal access tokens** (podcaster-scoped) for automation (e.g. MAT upload, planning episodes from a CMS, §4.3).

### 8.6 Display name
Prefilled from the provider at account creation and **never overwritten by a later login** — a name someone chose is not a cache of their Discord profile, and with several identities linked (§8.3) there is no non-arbitrary answer to which provider's name would win. From settings they may change it.

- **The host owns the key.** `display_key` is the canonicalised form — NFKC, zero-width stripped, whitespace collapsed, confusables folded, casefolded — and uniqueness is enforced on *it*, while `display_name` keeps the spelling that was typed. The same rule as the tag vocabulary (§6.1.1) and for the same reason: converge the spellings without lower-casing what a visitor reads.
- **Unique on the key**, because the display name is the only human-readable identity the site puts in front of other people, and a leaderboard where a fan can appear as the podcaster is worth an index. It is not a defence against lookalikes — folding confusables raises the cost, it does not close the class — which is why the answer to impersonation is §8.6.1 and not a better filter. Existing rows were prefilled from providers that never promised uniqueness, so the migration that adds the index **must resolve collisions first**.
- Refused: reserved names (`admin`, `system`, `moderator`, the site's own), and a word list held **in configuration rather than code**, because an operator's language and jurisdiction are not ours to guess and self-hosters need their own. Matching runs on `display_key`, so the normalisation that serves uniqueness serves the filter too — one function, two callers. Treat it as a speed bump: word lists lose to leetspeak and to compounds, and they produce false positives. The control is §8.6.1.
- Renames are **rate-limited** and recorded in `UserNameHistory`. That history is personal data: retention-capped and erased with the account, or the mechanism that lets someone shed a name becomes a permanent record of every name they tried to leave behind.

#### 8.6.1 Moderation: revert, not rename
An admin may **revert** a display name. An admin may not **set** one. The distinction is the whole design: an admin who never types the string cannot choose it, cannot use it to mock or to impersonate, and cannot be accused of having done either — and the act stays available to every operator without anyone having to write a policy about what an admin is allowed to type into someone else's profile.

- Revert targets the previous **self-chosen** name in `UserNameHistory`, walking further back if that one was itself reverted. The floor is a **host-generated neutral name** derived from the UUID (`Listener 4f2a`), so there is always a terminal state and never an account without a name.
- A revert **freezes renaming** for a period. Without that the user renames straight back and the act meant nothing.
- **Admin only, not podcaster.** PODCASTER is a content role (§8.5); on an install with two of them, "every podcaster may rename any listener" is a grant nobody asked for and no boundary can express (§14, feed ownership). Podcasters report.
- Reverts are logged like role changes (§8.5), and **the user is told** — a name that changes with no explanation reads as a bug or a break-in. The notice is a fixed system message rather than admin-authored text, for the same reason the admin does not type the name.

### 8.7 Avatars
Everyone starts with a **generated avatar**: an initial over a colour derived from the user UUID, drawn from the theme tokens so it is right in light and dark and re-themes with the site (§12.3). It costs no bytes, no storage and no CSP widening, and one mechanism covers every case that would otherwise each need an answer — a provider that has no avatars at all, a provider avatar that is simply absent, an account that has re-anonymised, and a user who has been deleted (§12.8).

From settings a user may instead **pick one linked identity to pull their picture from** (§8.4). `LinkedIdentity.avatar_ref` holds that provider's own reference, refreshed on each login with it; `User.avatar_provider` names the chosen one, null meaning generated. Unlinking an identity — or erasing the account — **clears `avatar_provider`**, since a picture pulled from an identity that is gone is a dangling fetch.

**The picture is always served by the host, never linked to.** `GET /api/users/{id}/avatar` answers bytes.

- **A redirect would defeat the entire point.** Discord's avatar URL contains the Discord snowflake — the `external_id` §8.2 deliberately keeps server-side — so a `302` publishes the identifier social login was supposed to hold back, to anyone who reads the page source, and hands the CDN a hit from every visitor's browser. Proxy the bytes.
- **Nothing attacker-influenced reaches the fetch.** The URL is composed in code from the provider and the stored ref, so the host is a constant per provider and there is no SSRF to filter rather than a filter to get right.
- **Cached in memory, never stored.** A picture the host keeps a copy of is a picture the host must moderate, retain and erase. TTL, so a changed provider avatar propagates; the cache bounded by **total bytes, not entry count**; a per-image byte cap; and failures cached too, briefly, or a single 404 behind a leaderboard becomes one outbound fetch per page view. Changing or unlinking the source **evicts immediately**. An `ETag` over `(avatar_provider, avatar_ref)` makes a cold start after a restart cost revalidations instead of refetches.
- Response content type whitelisted against an image list, `nosniff`, no provider headers passed through, no redirects followed.

**Uploads are deliberately absent.** Accepting arbitrary images means owning image moderation — and one illegal upload is a legal event, not a support ticket — plus decode-and-re-encode, dimension and decompression-bomb guards, and inheriting all of it to every self-hoster. Generated avatars and provider pictures meet the need without opening that.

### 8.8 What a plugin sees of a user
§10 still holds: the host resolves access and `ctx.user` stays slim. But the cross-user read (`ctx.allUsers()`, §7.4) hands a backend `OwnedDocEntry(userId, …)` and nothing more, so a plugin that aggregates across users — a bingo leaderboard, the case this is written for — holds UUIDs and has no way to render a person. The gap is filled with a **lookup, not a wider `ctx.user`**:

```ts
interface UserDirectory { resolve(ids: string[]): Promise<UserRef[]>; }
type UserRef = { id: string; displayName: string; avatarUrl: string; role: Role };
```
`Users users()` is the backend twin (§7.4), and an `identity` block in the manifest gates both — declared, never derived, like `data`, `blobs`, `tags` and `external` (§7.2). Absent means `ctx.users` is null and the endpoint 404s.

- **Never email, provider or `external_id`.** `avatarUrl` is the host's own `/api/users/{id}/avatar` (§8.7), which is the only reason a picture can be handed out here at all.
- **It resolves, it does not enumerate.** There is no list endpoint. A plugin can ask only about ids it already holds, and it only comes by them through its own scope.
- **Plugins store UUIDs and resolve at render; they do not store names.** A display name copied into a plugin's store survives the rename meant to shed it and the erasure meant to end it, and §12.8 cannot reach it — core provisioned those columns without ever learning which one is a person. The rule is written here because the host cannot enforce it.
- **Absent rather than redacted** for an id that is unknown, erased or pseudonymised — the shape `ctx.feeds` already uses (§7.5). It is also what lets a leaderboard row outlive its author as §13 requires: the aggregate stays, the person becomes a placeholder the plugin renders.

---

## 9. Patreon (v2) – three independent roles

Patreon appears in **three mutually independent places**; they die independently:
1. **Login provider** — pure identity, the most stable API surface. **Stays permanently** (for future supporter goodies).
2. **FeedSource** — pulls the **free** episodes of the campaign into the unified feed. Needs creator credentials.
3. **Tier resolver** — maps a logged-in user to their tier; enables "view on Patreon" gating. The most fragile.

**API quirk:** the user token only yields the entitled amount in cents via identity, **not** the concrete tiers — those come from the members endpoint with the creator token. So **creator-token sync**: the podcaster stores creator OAuth (encrypted at rest), picks the campaign(s) → that is both the FeedSource config (#2) and activates the resolver (#3). A periodic sync `members` → mapping `patreon_user_id → tier, status`. Login join instead of a per-request API call; updates via webhook/sync.

**Graceful degradation:** if the API dies → resolver `unavailable`, locked episodes still render as "view on Patreon" without personalized unlock; login (#1) is untouched.

---

## 10. Access / Gating

Each `EpisodeRef.access = PUBLIC | TIER(ref)`. **The host makes the access decision, not the plugin.** At render time `unlocked = userEntitlement.satisfies(access)`.
- v1 (RSS only): everything PUBLIC.
- v2: free Patreon episodes PUBLIC (always visible); paid ones as a **locked stub** with a "view on Patreon" CTA, real link on matching tier.
- PLANNED episodes: "upcoming episode" stub, no audio, bingo open — once announced. A quiet one (§4.3) is not gated but **absent**: to anyone below podcaster it does not exist, which is a visibility rule the host applies before any access decision.
Plugins only get the episode list the user may see; `ctx.user` stays slim.

---

## 11. Storage / BlobStore

One **`BlobStore` interface** in core. Branding is the first customer (Postgres behind it), audio the heavy future customer — through the **same door**.
```java
interface BlobStore {
    BlobRef put(String namespace, String key, InputStream data, String mime);
    BlobContent get(BlobRef ref);              // STREAMING, not "all bytes"
    void delete(BlobRef ref);
    List<BlobMetadata> list(String namespace, int page, int size);  // a backend answers for its own
    long count(String namespace);
    long usedBytes(String namespace);          // called on every upload — quota (§11.1)
    int  deleteNamespace(String namespace);    // purge (§7.8)
    Optional<String> directUrl(BlobRef ref, AccessContext ctx); // empty = serve it yourself
    BlobCapabilities capabilities();           // supportsRange, supportsPresignedUrls
    BlobRef putVerbatim(BlobMetadata meta, InputStream data);   // writes keeping the id — migration (§11.2)
    List<String> namespacesUnder(String prefix);                // what this backend holds below a prefix
}
```
- **Streaming-first + range requests** from day one (a 5 KB favicon like a 100 MB audio file).
- **Namespace routing:** `branding/*` may stay in Postgres forever, `audio/*` later S3/CDN. One interface, one backend per namespace, chosen in configuration (`mosaicast.blobs.namespaces`) — exact namespace first, then longest `/` prefix, so `plugin` covers every plugin without naming plugins that are not installed yet. A rule naming a backend that is not registered **fails startup**: falling through to the default would put files in a store nobody chose, with the symptom appearing long after the cause.
- **A backend is self-contained.** It answers for its own namespaces — listing, count, total size, delete-all — rather than sharing an index in Postgres. Two stores of the same truth can disagree and nothing can then say which is right, and the alternative was worse in practice: the plugin surface reached past the interface into `BlobRepository`, so a non-Postgres backend would have accepted uploads and reported an empty library with zero usage, leaving the quota unenforced and nothing failing loudly.
  - The cost is that each backend owes those answers *well*, and that is the backend's problem rather than the interface's: Postgres sums an indexed column, a filesystem walks a directory, an object store keeps a counter instead of paginating its own prefix on every upload. `usedBytes` is called on every write, which is the constraint that shapes the choice.
  - Listing is **offset-paged**, matching the plugin HTTP surface. That is a real constraint on an object store, which pages by continuation token; a media library is browsed from its first page, so walking to a deep one is a cost such a backend may cap. Moving to cursor paging is an SDK contract change and belongs with the backend that needs it, not before.
- **`directUrl` is the seam that makes an object store worth having.** A backend that can serve bytes without the app in the path says so; Postgres returns empty and callers serve the bytes themselves. Proxying every byte gives up most of the reason to move them out of the database. It is also where tier-gated audio (§10) signs a URL *after* the entitlement check, which is why it takes an `AccessContext` and cannot be cached per blob.
- Registered today: `PostgresBlobStore` (BYTEA) and `FilesystemBlobStore` (objects plus a JSON sidecar under a configured root, registered only when one is set). `S3BlobStore` is next, and is what `directUrl` was shaped for.
- Tier-gated audio (future): expiring presigned URLs only after the entitlement check (`AccessContext`).

### 11.1 Plugin file storage

Branding was the only customer for a long time, and the asymmetry that left was hard to defend: a plugin's CSP allows an image from **any** host on the web (§12.5), and there was no way to accept one from the site's own podcaster. A wiki wants diagrams; "find an image host first" is not an answer. So plugins get a scoped surface over the same store — the namespace `plugin/{id}`, computed by the host from the plugin id and never passed in, which is the property `SchemaStore` has for tables and `ctx.route.navigate` has for URLs.

```text
POST   /api/plugins/{id}/blob         multipart; → { ref, url, mime, size, filename, updatedAt }
GET    /api/plugins/{id}/blob         paged, newest first
GET    /api/plugins/{id}/blob/quota   used / allowed, as this install sees it
GET    /api/plugins/{id}/blob/{ref}   streaming; Range + ETag
DELETE /api/plugins/{id}/blob/{ref}   idempotent
```

- **Declared, never derived.** A manifest `blobs` block (`maxFileBytes`, `quotaBytes`, `mimeTypes`) is the opt-in, so a plugin's appetite for disk is readable by whoever installs it. Absent → `ctx.blobs` is `null` and every path is a **404**, indistinguishable from an unknown plugin, exactly as the schema surface answers a doc-store plugin. Reachable through `ctx.blobs` (TS) and `ctx.blobs()` (Java) — the latter mostly so a backend can collect the orphans nothing else collects.
- **Who decides how much room a plugin gets, in order:** an **admin's grant** in the plugin settings UI, else the **manifest's** ask, else the install's **default** — then clamped by an optional operator **hard ceiling**, which is unset by default. The quota endpoint is the only honest source for what came out of that.
  - An admin's grant **replaces** the manifest's ask rather than being minimised with it. A manifest says what a plugin's author guessed it would need on an install they have never seen; an admin raising it is looking at this install's real usage. Minimised, a grant of 2 GB against a manifest asking 256 MB would yield 256 MB and no explanation — a control that appears to work and does nothing. The manifest still decides when nobody has said otherwise, and remains what an installing operator reads to see what a plugin is asking for.
  - The **hard ceiling** exists because ADMIN is a role inside the application (§8.5) while the properties are infrastructure. Where those are not the same person, an operator needs a bound the UI cannot cross; where they are, leaving it unset is right. A grant past it is **clamped, not refused**, and the clamped value is what is stored — an admin is never shown a number that means something else.
  - **The MIME allow-list is not grantable.** No admin decision widens it, because what a file may *be* is a security question (§12.2) rather than a capacity one.
  - Storage is **per plugin**: a wiki accumulating diagrams and a bingo plugin storing nothing have no reason to share a ceiling, and "the wiki has outgrown its space" is an ordinary operational event rather than a redeploy.
- **Writes are the point here, and that is why this differs from §7.6.** The case against schema writes over HTTP is that no plugin code runs at request time to enforce a relational invariant. A file has none, so a write floor plus a quota is the whole authorization story. Files take the **`data` floors** unless the `blobs` block declares its own `readableBy` / `writableBy` — same vocabulary, and a write may not be `anonymous`. That default is right for a wiki, whose diagrams are as public as its pages; the override is for a plugin whose uploads are **inputs** rather than published content (raw analysis archives behind public numbers), which would otherwise be published along with the numbers. A read floor above `anonymous` also makes every download `Cache-Control: private`. `backendOwned` does not apply: it reserves *keys*, and a caller never names one — a ref is a UUID the host mints per upload, so an upload can never overwrite an existing file, including another tenant's.
- **What the bytes say is what gets stored**, in this order: size, the declared type, the *actual* type read from the leading bytes, then the quota. That order is why the SDK's `blobs.upload` normalises the **declared** type before sending: Firefox reads `File.type` from the OS MIME database and hands over `''` where that lookup fails, so `FormData` sends `application/octet-stream` and a valid PNG is refused on its declared type before anything sniffs it — in one browser only. Guessing there is safe precisely because this order keeps the byte check afterwards, so a wrong guess becomes the same 415 rather than a stored file of the wrong kind. Reordering these steps would take that safety with it. One shared sniffer serves branding and plugins, and **SVG has no case in it** — that is what makes it unstorable rather than merely undeclared (§12.2). **ZIP** is recognised and in the default allow-list: it carries no active content, the browser aliases (`application/x-zip-compressed`, …) are folded into `application/zip` on the declared type, and opening an archive safely (zip bombs, zip slip) is the job of the plugin that reads it (§13). Images and audio are served `inline`; everything else as an `attachment`, so a file that is not for display is a download everywhere. Refusals are **415** (type) and **413** (size or quota), worded apart because the fixes differ.
- **Purge removes files** alongside documents and schema tables (§7.8), matched on the namespace exactly rather than on a name prefix.
- Blobs are served **same-origin** under `/api/`, so a plugin rendering its own upload needs no CSP host and makes no consent decision — which an external image URL cannot say. This is the answer to the asymmetry above, not merely a workaround for it.

---

### 11.2 Moving a namespace between backends
Routing a namespace at a different backend does nothing to what is already stored: the old backend keeps the bytes, the new one starts empty, so every existing ref 404s and the plugin's quota reads as zero while the source is still full. The switch therefore has a second half — a migration that runs **through `BlobStore`**, as a separate entry point (`./gradlew migrateBlobs`, `dev.mosaicast.tools.blob.BlobMigratorApplication`).

- **Inside the JVM, not beside it.** An external script has to re-describe each backend's layout — a sidecar's fields, a table's columns, the rules — and that description is correct until someone adds a backend or changes a field, with nothing to say otherwise. Through the interface, a new backend is migratable the day it implements it.
- **`putVerbatim` exists because ids must survive.** `put` mints an id, and an id is the identity a plugin stores (§11.1); a copy that renumbered objects would orphan every reference a plugin ever saved. Both it and `namespacesUnder` are **required rather than defaulted**, so a backend cannot be added that quietly cannot be migrated to — a property an operator would otherwise discover halfway through moving their files.
- **Copy, verify by hash, then optionally delete.** Deleting the source is off by default and never happens before every object has been read back and compared; a re-run skips what already matches, so an interrupted migration continues rather than being something to unpick.
- **A separate entry point, not a button.** It wants the app stopped, it takes a while and it deletes things. Its context is the blob package alone — no web server, no plugin loader — and Flyway is off: schema migration is the app's decision, not a side effect of moving files.
- **`branding` cannot move.** `site_config.{logo,favicon,dark_logo}_asset_id` are foreign keys into the Postgres `blob` table and the app refuses to start when that namespace is routed elsewhere (§12.2), so the migration refuses it before reading anything.

## 12. Branding & Theming

### 12.1 SiteConfig (DB, editable by admin – no config files needed)
Single-row now, tenant-keyed later: `site_name`, `logo_asset_id`, `favicon_asset_id`, optional dark logo, **theme_seed** (accent + mode policy: light/dark/system). Editable by ADMIN only. `GET /api/site` returns name + branding + seed at boot.

### 12.2 Assets
Via `BlobStore` (namespace `branding/`). Served at `/branding/logo`, `/branding/favicon` with an **ETag** from `updated_at` (a change propagates immediately, otherwise cached). `index.html` points `<link rel="icon">` at the dynamic endpoint.
**Security:** uploaded **SVGs are an XSS vector** → either sanitize server-side or restrict to raster (PNG/ICO) and serve with `Content-Disposition`/CSP.

### 12.3 Light/dark + seed generator
- **Semantic tokens** as CSS custom properties: `--mc-bg, --mc-surface, --mc-text, --mc-text-muted, --mc-accent, --mc-accent-contrast, --mc-accent-text, --mc-border` (+ `--mc-accent-2`, `--mc-accent-2-contrast`). Light/dark = two value sets, `data-theme` on the root. **Shell AND plugins read the same properties** → they re-theme automatically.
- **`--mc-accent` is for fills; `--mc-accent-text` is for text, links and focus rings.** The accent is the admin's seed, paired with `--mc-accent-contrast` for whatever sits on top of it, and is not contrast-checked against the page — a pale seed such as `#FFF176` measured 1.12:1 as link text. `--mc-accent-text` is the same accent clamped to WCAG AA (4.5:1) against both `--mc-bg` and `--mc-surface`. It is set on `:root`, so it reaches plugins through inheritance, and since platformApi 0.16.0 it is also `ctx.theme.accentText`. Colouring text with `--mc-accent` is a bug in the shell and in a plugin alike.
- **`--mc-accent-2` is a second accent, readable as text.** Derived from the accent by rotating its OKLCH hue by 40° — backwards for warm accents (hues 35°–115°), so a gold brand gets bronze rather than olive — and clamped like `--mc-accent-text` to 4.5:1 on `--mc-bg` and `--mc-surface` in both modes (dark from the lifted accent). As a fill it pairs with `--mc-accent-2-contrast`, which is CSS-only until an SDK minor adds it to `ctx.theme` (core#281).
- **Icons ride the same channel: `--mc-icon-*`.** The shell's icon set is generated from a whitelist and a deliberate subset is published as custom properties, so a plugin's Web Component draws the shell's icons with **no SDK import, no `platformApi` bump and no version skew** — a plugin built against an older SDK picks up an icon added here the day it lands. Consumed as a **mask**, never as `background-image`, so the icon takes the caller's own colour and re-themes with everything else:
  ```css
  mask-image: var(--mc-icon-close); mask-size: contain; background: currentColor;
  ```
  The published names are a **contract**, exactly like the colour tokens: add freely, rename never. The set is deliberately provisioned ahead of demand — a plugin builds against a *released* core, so an icon that is not already published is one its author cannot add without waiting for a core release.
- **Seed:** the admin sets **one accent**, the system generates the rest. **OKLCH** (perceptually uniform) + **WCAG contrast clamp** (text/bg ≥ 4.5:1), so no unreadable theme can result.
- **No theme flash:** a tiny inline script in `index.html` sets `data-theme` + accent synchronously from the site payload before first paint.
- **Previews:** the branding panel shows the logo on a light AND a dark swatch; the theme seed renders live as the accent is dragged. Both client-side.

### 12.4 Logo→theme (stage 2, moderate)
On logo upload, **suggest** an accent: for SVG parse the `fill` values (cleaner than rasterizing), for PNG quantize (k-means/median-cut, Vibrant) → **not** the most frequent, but the **most saturated** color. Feeds the same seed generator. **Extract → suggest → admin confirms/adjusts.** Never auto-lock fully.

### 12.5 Consent (platform service)
Strictly necessary cookies (session/CSRF/LB) need **no** consent → the core runs banner-free. Plugins that load third-party content with cookies declare **services** in the manifest (§7.2) — provider, category, hosts, what each stores — → the host generates the cookie/privacy notice + admin audit from that. The **category** is the thing a visitor consents to, so it is the one plugin-authored string that cannot fall back to a developer key. The host names its own (`necessary`, `functional`, `analytics`); a category a plugin introduces is named by that plugin's `consent.categoryLabels` (label + hint, localized), a host category may not be relabelled, and when two plugins label one category the lowest plugin id wins so load order never decides what a visitor is asked. An unlabelled plugin category is shown inside a generic localized phrase ("Other services: social") — never as the bare id. A plugin loads consent-requiring resources only after `ctx.consent.has(cat)`, before that a click-to-load placeholder. Self-host fonts, cookieless analytics (Plausible/Umami).

**Server-side external services are out of scope here.** A call to a configured provider (§16) happens on the server, so no browser request reaches it, nothing enters `connect-src`, and no consent category applies. The question that *is* real — whether operator-submitted content leaves the EU — is an operator decision surfaced in admin before a provider is selected, not a visitor prompt.

**What consent actually enforces.** `ctx.consent.has()` is advisory — a plugin shares the page's JavaScript realm and can simply not call it — so the declaration is also narrowed into the **CSP**: a declined category takes its `hosts` out of `script-src`/`frame-src`/`connect-src`, and the request then fails at the network layer rather than on plugin manners. **Images and media are the exception**: `img-src`/`media-src` allow any `https:` origin, because episode artwork and audio come from whatever host a podcaster's feed points at. That leaves an `<img>` to an arbitrary origin working as a one-way beacon, past `connect-src` and past consent — a residual the host makes closable rather than closed, by narrowing both directives to the origins the site's own content references plus the consented plugin hosts (opt-in, since a derivation cannot see a host nothing references yet, and most media CDNs redirect to one it never saw).

### 12.6 Legal pages (mini-CMS, jurisdiction-agnostic)
Publicly operated sites need jurisdiction-specific legal documents (e.g. Germany: Impressum + Datenschutzerklärung; other countries: other sets). Core therefore ships a **generic legal-pages mechanism instead of hardcoded documents**:
- Admin creates any number of **static pages** (markdown; slug, title), shown automatically as **footer links**, order sortable. The slug is a URL segment (`/legal/{slug}`, the API path, the sitemap), so it follows the site's slug grammar — lowercase letters and digits in hyphen-separated runs, at most 64 characters — and anything else is refused at creation rather than stored as a page no link can reach.
- Pages are **translatable per locale** (§12.7): one logical page, one markdown body per language, served in the active UI locale with fallback to the default locale, and — when the page has neither — to whichever language it is written in: a reader would rather have the imprint in another language than no imprint, and a page that exists must not answer 404. Crawlers are still told only the truth: `hreflang` names the languages a page actually has (§6.6).
- A translation may be **machine-drafted, never machine-published** (§16). The prefill action returns an unsaved draft the admin reads, edits and saves themselves. This section ships the mechanism and deliberately no legal texts because a policy nobody read is false safety; writing a machine translation straight into the table would be that same failure with extra steps.
- A page can carry a **role marker** (`privacy`, `imprint`, `terms`, …); the consent service (§12.5) links to the page marked `privacy`.
- Markdown is rendered sanitized (same care as wiki content).
- Docs note for operators: German operators typically need Impressum + Datenschutzerklärung. **Mosaicast ships the mechanism, not legal texts** — bundling texts would be false safety. *(Not legal advice.)*

**`/about` — what this is, what it runs, what it is built on.** A shipped route, not an admin-authored page, because a visitor to a *bare* install is the one most likely to ask "what is this site?" and least likely to find an answer. Four sections, each taking up the question the one before it raises:
1. **This instance** — the operator's own words. It leads because someone arrived at *this podcast's site*, not at a piece of software; opening with a paragraph about the platform answers a question nobody asked.
2. **What Mosaicast is** — fixed, translated text with the running version and a link to the source. This is also where the AGPL's obligation to offer source becomes something a network user can actually act on, rather than a clause with nowhere to click.
3. **Plugins** — what this install runs, with whatever credit each declared (§7.2). Anonymous: what an install runs and under what terms is not privileged information.
4. **Built with** — the third-party work the project stands on, generated from one source shared with the repository's notices file so the two cannot drift. Curated and deliberately generous rather than a transitive dependency dump, and it credits build and test tooling too.

Every section is **absent-tolerant on its own**, so an install with no plugins, no About text and no legal pages still renders. The operator's blurb reuses this mini-CMS under an `about` slug — that buys per-locale markdown, sanitising, locale fallback and the existing editor for nothing, where a `SiteConfig` column could not do per-locale text without reinventing the translation table. It is **not** a legal page, so its role marker keeps it out of the footer's legal group; it gets its own link there and in the info menu.

### 12.7 Internationalization (i18n)
Built in from the start so the project can grow international and translation is an easy first contribution.
- **Catalogs:** one JSON per language (`locales/en.json`, `locales/de.json`). **English is the source language**; German ships in v1. Library: **i18next** (interpolation, plurals) in the shell.
- **Languages are a runtime registry, not a build-time constant.** The host scans two roots — the shipped catalogs, and a drop-in directory (`MOSAICAST_LOCALES_DIR`) — and `GET /api/i18n/locales` is the answer the shell and every plugin read. Contributing a language upstream is still copy `en.json`, translate, PR; **running** one is dropping the file in and enabling it, with no rebuild. A drop-in file **merges key by key** rather than replacing, so a partial `en.json` overrides exactly the strings it declares — which is also how an operator renames "Podcast" to "Show" without forking a catalog they would then maintain against every release. A malformed file is skipped with a warning, never fatal: it lands in a directory the app does not own.
- **UI languages and content languages are separate.** A language needs a catalog to be *offered* in the shell (`site_config.ui_locales`) and needs nothing at all to be one content is *authored* in (`content_locales`) — legal pages, the About blurb, per-locale plugin content. A Dutch imprint on an English-only site is a real thing to want, and one switch could not express it. The site default must be one of the content languages. English can never be switched off: it is the source language and what everything else falls back to. Admin → Languages owns all three settings.
- **Per-locale writes are validated against the content languages.** A page stored under a locale nobody offers is invisible to every reader and to the editor's own tab strip — a page that quietly does not exist. Reads stay tolerant and fall back to the default.
- **Locale resolution order:** 1) **`?lang=<code>` on the URL**, 2) explicit user choice — **works anonymously** (persisted in localStorage/cookie; additionally on the profile when logged in), 3) browser (`Accept-Language` / `navigator.language`), 4) site default from `SiteConfig`. A visible **language switcher** (footer/top bar) requires no login.
  - **`?lang=` is read, never written.** It exists so a *URL* can name a language — which is what an `hreflang` alternate is (§6.6), and what nothing else here provides, since every other step is invisible to a crawler. Persisting it would let a link someone was sent silently change the language of the whole site for them, on every later visit, with no action they would recognise as a choice. The switcher stays the only thing that writes a preference.
  - **Validated against the UI languages, and an unknown value is not an error.** A language may be one content is authored in without having a catalog, so it has no rendering to point at; and a stale alternate or a language an admin has since switched off must serve the site default rather than 404. See §6.6 for what that means for the canonical URL.
- **Plugins:** get `ctx.locale` (+ `onChange`, plus `available()` and `content()` since platformApi 0.10.0) and a small SDK helper (`createPluginI18n(catalogs)`); each plugin ships its own `locales/*.json` in its frontend bundle — same convention as the shell. A plugin's own catalogs and the site's content languages routinely disagree, and that is fine: the plugin UI falls back to English while the *content* is written in Dutch. An editor should offer `content()`, never `available()`.
- **Boundaries:** **feed content stays in its original language** (titles/descriptions are data, not UI); dates/numbers format via `Intl` per locale; backend error payloads stay English (the UI translates); legal pages are maintained per locale (§12.6).

---

### 12.8 Account deletion reaches a plugin's data
Core owns what it stored: identities, tokens, listening progress, and the `USER`-scope documents it holds on plugins' behalf (host-owned, §7.6, which is why they need no asking). It cannot touch what a plugin put in its own schema columns or files — it provisioned those tables without ever learning which column is a person, and cannot know that pseudonymising is right where deleting is not. So every installed plugin is asked, through `UserDataHandler` (§7.4), and the parts that make the promise real are all about not lying:

- **Handlers run before the account row drops.** A plugin resolving a user id against a user that no longer exists cannot pseudonymise sensibly.
- **Every plugin's part is a row written before it is asked.** A handler that throws, or one whose plugin an operator had switched off, leaves an **open record** rather than a log line — otherwise the account is gone, the person has been told it is done, and the data is still there.
- **Outstanding erasures are retried and visible in admin**, and settle immediately when a switched-off plugin is switched back on. A plugin that is *rejected* is asked only if it has ever stored anything: it did not run this boot, but "rejected" is also what a working plugin becomes after a bad upgrade, and last week's rows do not disappear because a manifest stopped parsing.
- **The API answers a receipt, not a 204** — complete, plus the plugins that have not finished. Reporting completion while a plugin still holds data would be the failure the whole record exists to prevent.

`exportUser` was defaulted to empty so erasure could ship alone; the data export below hangs off the same code.

#### 12.8.1 Data export
A person can download everything the site holds about them (GDPR Art. 15 access, Art. 20 portability), including what every plugin holds, in a form that is useful to them — so a plugin hands over **files in its own format**, not a map core has to guess a format for.

- **A job, not a response.** `POST /api/me/export` answers a **receipt** (`202`); the work runs off the request, because asking every plugin with a timeout each is not something a request should wait on. One export per account per interval (default a day, operator-configurable): an export walks every plugin, and a button pressed in a loop should not.
- **Done means told.** When it finishes the person gets an in-app notification (§17) with a link to their account page, and the download is **one ZIP**, available only to that account and only for a limited time (default seven days), after which it is deleted. Admins see the jobs and each plugin's outcome — **never the ZIPs**: the point of the archive is that it is everything about one person.
- **The layout:**
  ```text
  export-<date>.zip
    README.txt                        what each part is, generated
    core/account.json                 account, linked providers, name history, access tokens (never a secret)
    core/listening.json               listening progress
    core/notifications.json           the inbox
    core/plugin-documents/<plugin>.json   USER-scope documents core holds on plugins' behalf (§7.6)
    plugins/<id>/…                    whatever each plugin hands over, in its own format
    outcome.json                      per plugin: complete | empty | failed | outstanding | not-supported
  ```
- **Plugins are asked the way erasure asks them, and nothing goes missing silently.** Every discovered plugin that could hold data gets an outcome **recorded before it is asked**. The order is the SDK's: `exportFiles` first; if that is empty, `exportUser`, whose non-empty map is written as `plugins/<id>/data.json`; if that is empty too the plugin holds nothing on this person (`empty`). A switched-off plugin, or a rejected one that has stored data (§12.8), cannot be asked and is **`outstanding`**; a handler that throws, runs past `UserExport.TIMEOUT` or hands over more than `UserExport.MAX_BYTES` is **`failed`** — never truncated, because a partial part reads as a complete one. A plugin with no `UserDataHandler` is **`not-supported`** rather than `empty`: core cannot know it holds nothing, only that it did not say. The limits are the SDK's constants; an operator may lower them, never raise them. A plugin includes only this person's data, which the SDK tells it.
- **Erasure takes the exports with it.** An archive of a deleted account is that account's data outliving it.
- **Not here:** an admin full-site export (that is database tooling), and import — a plugin that wants a round trip reads its own format back itself.

## 13. Non-Functional

- **Scaling v3:** app instances **stateless** (Redis session, DB as the only truth), periodic jobs **ShedLock**. Moving to multiple instances behind an LB = config, not a rewrite.
- **Observability:** Spring **Actuator** `health`/`info` (compose healthcheck + uptime monitoring hook), structured logging. Nothing fancier in v1.
- **API conventions:** the REST API is **internal** in v1 — the SDK is the only public contract, so no API-versioning machinery. Errors as **RFC 7807** `application/problem+json` (stable `type` codes; the UI translates) — including the external-service vocabulary listed in §16. **List endpoints paginate from day one.**
- **GDPR:** store minimal (provider, external_id, optional email/name, and for the avatar a *reference* rather than a picture — §8.7), no passwords. Name history is retention-capped and dies with the account (§8.6). On account deletion **pseudonymize** public bingo contributions (cut the identity link, aggregates/leaderboard stay correct), don't hard delete — the mechanism is §12.8, because bingo is a plugin and core cannot keep that promise on its own. Not a lawyer — have the privacy policy reviewed.
- **Security:** creator/OAuth tokens encrypted at rest — `MOSAICAST_ENCRYPTION_KEY` is wired and used for admin-entered external-service credentials (§16); absent, such values are stored in the clear with a startup warning and an admin badge, because refusing to boot would take a site down over a feature it may not use. Outbound requests to **admin-supplied service URLs** are gated by an exact-origin private allow-list, distinct from and **not** a widening of `mosaicast.feed.allow-private-targets`. SVG sanitizing. Presigned URLs for gated audio. **Baseline security headers** (CSP, X-Content-Type-Options, Referrer-Policy). **Upload limits** (max body size; archives additionally guarded against zip-slip and zip bombs — see the stats brief). **Basic rate limiting** on auth endpoints and uploads.
- **Deployment:** Docker Compose (app, postgres, caddy/traefik; redis from v3). Secrets via `.env`, not committed. **Backups from day one:** nightly `pg_dump` of the database (host cron or sidecar) + keeping a copy off the VPS; test a restore once.

## 13.5 Testing Strategy

Tests are **layered by tier**, not dogmatically blanket. Focus on risk and contract.

- **SDK (`mosaicast-plugin-sdk`):** test only its own helpers (`defineMosaicastElement`, version constant). **Ships a test kit as part of the contract** (below).
- **core:** real unit tests at the **risk points** — reconciler (3 cases **incl. PLANNED binding**), account **merging rules** (§8.3), fuzzy match, theme seed **contrast clamp** (WCAG), BlobStore range. Plus **integration tests with Testcontainers** (Postgres + Flyway + plugin loading via PF4J). **Shell:** component tests (Vitest + Testing Library) for **slot mounting** and theme token application.
- **Plugins:** unit tests **against the test kit** — no core, no Postgres needed.

### Test kit (in the SDK, so plugin testing is trivial)
The contract ships its test doubles. Production plugin code does **not** bundle them (separate artifact / dev subpath).
- **Java `plugin-testkit`** (own artifact, `testImplementation` only): `FakePluginContext` with `InMemoryDocStore`, `FakeFeedAccess(Map<Scope,List<String>>)`, `MapPluginConfig`, synchronous `onSchedule`. → test a plugin backend without infrastructure: build the fake context, call `register`, assert store contents. Capabilities default to **absent**, as the host's do without a declaration — `allUsers()` stays `null` until `withReadsAllUsers()`, so a backend that forgot its manifest declaration fails in its tests, not at its first production load.
- **TS `@mosaicast/plugin-sdk/testing`** (dev subpath): `makeMockCtx(overrides)` returns a `PluginContext` with a fake `api` (records calls / canned responses), in-memory `consent`/`filter`/`player`, theme tokens. → mount the Web Component with the mock ctx, assert DOM. `ctx.sanitize` is `sanitizeLikeHost` — the same `FEED_HTML_POLICY` walked over a parsed tree — so a component test sees the removals the host makes; the docs double records its calls, so "one batch, not one read per card" is assertable.

---

## 14. Version Roadmap

- **v1:** RSS feeds, unified feed + filter, season, EpisodeRef/snapshot, PLANNED lifecycle, social login (Discord) + account merging, RBAC, SiteConfig/branding + theming/seed, BlobStore (Postgres), plugin system, sequential nav + RelatedProvider (tags/season/fuzzy), plugins: **bingo, stats, wiki**.
- **v2:** Patreon (login/FeedSource/tier), tier gating, **feed ownership** (below), dedup/merge UI (fuzzy), logo→theme stage 2, embedding-related (`pgvector`), transcript display (MAT transcripts are already uploaded — accessibility + SEO nearly for free), Podcasting 2.0 namespace (`<podcast:chapters/transcript/funding>`), first-party cookieless analytics, oEmbed embed player, **external services** (§16: translation shipped first, with transcription/TTS/embeddings the follow-on kinds the surface is shaped for).
- **v3:** Redis sessions/cache, multiple LB instances, multi-tenant preparation; far future: native audio host (BlobStore `audio/*` → S3, own FeedSource, custom RSS).

**Feed ownership (v2).** A `Feed` has no owner, and **PODCASTER** is a single global role (§8.5): every podcaster can edit every feed, and — because the plugin doc store authorizes per plugin rather than per document (§7.6) — tamper with plugin data on any feed, season or episode. On a single-podcaster install that is invisible. The moment there are two, it is one tenant reaching another's, with no way to express the boundary.

Deliberately **v2, not v3.** "Multi-tenant preparation" is a v3 heading, but this is not preparation: the exposure exists as soon as a v1 install promotes a second podcaster, which is an ordinary thing to do. It is also the prerequisite the doc-store gap is waiting on — per-record ownership needs an owner to point at — so scheduling it later leaves §7.6's residual open across two releases.

Shape, for whoever picks it up: an owner on `Feed` (nullable, so existing installs stay valid), ownership carried down the scope tree (a season/episode belongs to its feed's owner), enforcement on `/api/admin/feeds/**` and on shared-scope plugin writes, and ADMIN unaffected. Whether a feed may have several owners, and whether ownership implies exclusivity or merely attribution, are open — the answer decides whether §7.6 gets "only the author may overwrite" or "only the owning tenant may".

---

## 15. Naming Conventions (carried through initially, not enforced)

- **Repos:** `mosaicast-core`, `mosaicast-plugin-sdk`, `mosaicast-plugin-sample`, `mosaicast-plugin-<name>`.
- **npm:** scope `@mosaicast/...` (e.g. `@mosaicast/plugin-sdk`); community plugins unscoped `mosaicast-plugin-<name>` (the `-plugin-` infix = discoverable, like eslint).
- **Java:** `dev.mosaicast.core.*` (host), `dev.mosaicast.plugin.<name>.*` (plugins).
- **Manifest `id`:** short, unprefixed (`bingo`, `stats`, `wiki`).
- **Flavor:** plugins may be called "tiles" in UI/docs; the technical convention stays `*-plugin-*`.

## 16. External Services (admin-configured third parties)
A generic surface for services this instance may use but does not run: **one *kind*, one selected provider, or none.** Translation is the first kind; transcription, text-to-speech and embeddings are the shapes it is built to take next. Adding a kind is one enum constant, one support bean, an input and an output type — nothing in the registry, settings model, pipeline, cache, admin API or frontend changes.

- **Providers are compile-time beans, not PF4J plugins.** They ship with the host, are reviewed with it, hold operator credentials and speak to paid APIs — precisely the capabilities the plugin sandbox exists to withhold. Installing a plugin must never mean trusting it with a billing relationship. Adding a provider is a PR against core. A malformed provider descriptor **fails the boot**: a broken *plugin* manifest is skipped so the site survives a third party, but this is first-party code and a wrong one should never reach a running instance.
- **Absent means off.** No selection row means no provider and no outbound call — deliberately the opposite of plugin activation, where absent means enabled. A plugin nobody toggled should work; a third-party service nobody configured must stay silent.
- **Settings manifest.** Each provider declares typed fields (string, integer, decimal, boolean, one-of, prose, and two credential kinds) with bounds, labels and defaults. Rejections name the constraint, never the submitted value. Writes are all-or-nothing and report every bad field at once.
- **Credentials.** `ENV_SECRET` is supplied through an environment variable whose name the **host derives** (`MOSAICAST_EXTERNAL_<KIND>_<PROVIDER>_<SUFFIX>`) and is never stored — a value that does not exist cannot leak from a backup or be echoed by an endpoint. The name is derived rather than declared because the admin API answers "is this variable set?", and a descriptor free to name any variable would make that endpoint an oracle over the whole process environment. `SECRET` is the admin-typed, database-stored alternative for operators who cannot restart to add a variable, encrypted with `MOSAICAST_ENCRYPTION_KEY` when set and stored in the clear with a startup warning and an admin badge when not. **A credential is never read back**: the payload has no field one could travel in. Either kind may be **optional** — a self-hosted LibreTranslate runs with no key at all, while the same image behind a public URL enforces one.
- **The call pipeline wraps every provider**, so a provider is only ever "shape the request, speak HTTP, shape the response": cache outermost, then the per-provider rate limit, then a per-kind concurrency bound with a wall-clock ceiling. A cache hit therefore costs no permit and no token, which is the point of caching something metered.
- **The cache is in the database**, keyed by kind, provider, the non-secret settings fingerprint and the kind's own request identity. The provider is part of the key because switching provider is a quality decision; the settings are because a changed base URL is a different model on a different machine; credentials are **not**, because rotating a key says who is asking, not what the answer is. Hit accounting is throttled so a cache read stays a read.
- **Outbound targets.** A self-hosted service is a private address, which the feed SSRF filter refuses. It is granted by an **exact-origin allow-list** (`MOSAICAST_EXTERNAL_ALLOWED_PRIVATE_ORIGINS`) — scheme, host and port matched literally — and emphatically **not** by widening `mosaicast.feed.allow-private-targets`, which disables the filter on the podcaster-writable feed path whose preview endpoint reads responses back to the caller. Every refusal returns one identical message: this URL is admin-supplied and the outcome is rendered back to them, so a form that explained *why* would be an internal port scanner. Upstream response bodies are never forwarded — an error body can echo the credential that was sent.
- **No consent category and no CSP host.** Calls happen server-side, so no browser request reaches the provider and §12.5's machinery is not implicated. Third-country transfer is real but concerns operator-submitted *content*, not visitor data, and is surfaced in admin **before** a provider is selected.
- **Machine output is a draft.** Anything stored from it is marked as such and confirmed by a person (§12.6).
- **A plugin declares what it uses.** The manifest's `external` block (§7.2) names the kinds (`external.kinds`) and the lowest role that may trigger a call from the plugin's UI (`external.usedBy`, default `podcaster`). An undeclared kind is **`null` on the plugin's context and 404 on its endpoint** — the shape `blobs` and `tags` already have — and it is that **independently of whether a provider is configured**, because the manifest is checked first: a plugin that never asked must not be able to read off an error code whether this instance pays for translation. It is deliberately *not* `external-no-provider`, which tells a caller to go ask their admin about something no admin can grant. The declaration exists here and not only for storage because the browser half of this surface spends money: `translate()` in a page means anyone who can load that page can bill a metered API, and the pipeline's rate limit keys on kind and provider, so an undeclared caller would exhaust the site's budget with nothing recording which plugin did it. **The role floor is a property of the browser endpoint only** — a backend call happens in `register` or on a timer and has no caller to have a role. `usedBy: "anonymous"` is legal and almost always wrong; a metered provider behind an anonymous floor is an open spending endpoint.
- **Failure vocabulary**, each with its own status and stable problem type, because a caller that cannot tell them apart cannot act on any of them: `external-no-provider` (409), `external-provider-misconfigured` (409), `external-busy` (503), `external-rate-limited` (429), `external-timeout` (504), `external-provider-failed` (502).

---

## 17. Notifications

Three things need to tell a user something and none of them can: a display name reverted by an admin (§8.6.1) changes silently and reads as a break-in; an admin has no way to warn someone short of removing them; and a plugin that finishes a long-running thing a user took part in — a bingo resolving, the case this is written for — can only hope they come back and look. One inbox serves all three.

**In-app only.** The emails in `LinkedIdentity` were collected to establish identity (§8.2), and sending to them is a *different purpose* — with opt-in, bounce handling, deliverability and an unsubscribe path behind it. Nothing here presumes it never happens; it is simply not this.

```
Notification (id, user_id, source, kind, payload JSONB, created_at, read_at)
             source: system | admin | plugin:<id>
```

- **The user is the addressee, so the host is the sender.** A notification is written by core on behalf of a source, never handed to a delivery mechanism a plugin controls. Rate limits, caps and retention are therefore host properties and there is nowhere for a plugin to hold them.
- **`system` messages are fixed kinds, not text.** The revert notice (§8.6.1) names a kind and the shell translates it. An admin who cannot type the name must not be able to type the explanation either, or the restraint in §8.6.1 is one message away from being undone.
- **`admin` messages are free text**, because a warning that cannot say what it is about is not a warning. They are attributable and logged like role changes (§8.5), and read state is meaningful for them in a way it is not elsewhere: "they were told" is the point.

### 17.1 What a plugin may do
```ts
interface NotifyClient { send(userIds: string[], msg: NotifyMessage): Promise<string[]>; }
type NotifyMessage = { text: Record<string, string>; link?: string };   // locale → finished sentence
```
`Notifier notifier()` is the backend twin (§7.4) and is where nearly all real use lives — the thing worth announcing usually finishes on a timer, not in someone's browser. **It is `notifier()` in Java and `ctx.notify` in TypeScript**, and the asymmetry is forced: `Object.notify()` is `final`, so no Java interface may declare that name.

**`send` answers who was actually notified**, not `void`. The eligibility rule below guarantees partial sends — an erased account (§12.8) is the ordinary case — and a write whose partial failure is invisible degrades in silence: a plugin working from a stale participant list would look exactly like one working perfectly.

This is the **first plugin surface that writes into another user's experience**. Everything else a plugin touches is its own scope or the current visitor's (§7.6). Unbounded, it is a spam cannon pointed at the whole user list, so:

- **A plugin may only notify users it already holds `USER`-scope data for.** Host-enforced against the same partitions the cross-user read spans (§7.4) — whether or not the plugin declared `data.readsAllUsers` — needing no new concept: bingo may write to its participants because participants have rows, and no plugin can reach a user who never touched it. The rule survives the surface it was written for — a comments plugin later notifies a thread's participants, who are exactly the users it stores rows for.
- **Rate limits are the host's**, per plugin per recipient per window plus a ceiling across all recipients, both capped by the operator over what the manifest asked for. A limit a plugin enforces is a limit a plugin can drop.
- **Text is third-party and user-visible.** A plugin sends **one finished sentence per locale**, and the shell picks when it draws the bell. A single rendered string would freeze the language at send time, which breaks §12.7 on the surface where it is most obviously wrong: a notification is written on a timer and read days later by someone whose shell may have changed language since.
  - *Not a translation key*, which is what this section first specified — nothing can resolve one. A plugin's catalogs ship inside its **frontend bundle** (§12.7) and load when its Web Component mounts; the bell is shell chrome and renders on pages where that never happens. There is no plugin-scoped catalog endpoint and no manifest field naming one, so a key would reach a reader as the literal string. A key is the better design and may yet arrive — it needs a plugin catalog surface first, which is a larger piece of work than the notification it would serve.
  - The map must carry `en`: §12.7 makes English the one language a site cannot switch off, so it is the only safe terminal fallback. Requiring the *site's* default instead would refuse a perfectly good plugin that does not happen to ship that language. The honest cost is that the set of languages is fixed when a notification is sent, so one added later cannot appear in a message already written.
  - Rendered as text, never HTML, with length caps.
- **`link` is host-validated and internal** — a `ctx.links`-shaped target or a subpath under `/p/<pluginId>/` (§6.4). A notification is chrome the site is speaking through, and a plugin that can point it off-site is a plugin that can phish the site's own users with the site's own voice.

### 17.2 Lifecycle
Notifications are user data. They are erased with the account (§12.8) rather than left keyed to a UUID nobody can resolve, a plugin's notifications go when the plugin's data does, read ones are purged after a retention period and unread ones are capped per user — an inbox nobody empties is not a feature, and an unbounded one is a table that only grows.

---

## 18. Site access & the edge

A site is not always meant to be seen. Before a launch it should show a countdown, during planned work a holding page, and while core restarts — which every plugin install requires (§7.8) — something better than a proxy's 502. Staff need the real site through all of it. Two components share the job, and the split is the design: **core owns the policy, the edge enforces and presents it.**

- **Core decides who may see the site** (the guardrail of §10: the host decides access). The policy is a row in core's database, edited on an admin page, read by anyone over a public endpoint.
- **`mosaicast-edge` is a reverse proxy in front of core** that applies the policy to every request and renders the pages a visitor sees instead of the site. It exists as a separate process because its most important page is the one shown **while core is not running**, which nothing inside core can serve.
- **Without an edge the policy has no effect** on what visitors see, except where core enforces a part of it itself (sign-ups, below). The admin page says so plainly whenever no edge has reported in, instead of letting an admin believe a site is closed that is not.

### 18.1 The policy
```
SiteAccess (state, opens_at, ends_at, pass_floor, staff_login_visible, signups_while_closed,
            edge_counting, revision, updated_at)
        state: open | scheduled | maintenance | closed
```
- **`open`** — everyone is proxied through.
- **`scheduled`** — closed until `opens_at`, open from that instant on. The edge flips on its own clock at that moment and does not wait for a poll, so a launch happens on the second even if core is slow to answer. A `scheduled` policy whose `opens_at` has passed **is** open; core reports both the stored and the effective state so nobody has to compute it twice.
- **`maintenance`** — closed, with an optional `ends_at` that is **shown, never acted on**. Planned work overruns; a site that reopened itself on an estimate would reopen mid-migration. An admin ends maintenance.
- **`closed`** — closed with no end in sight: the sudden off switch.
- **No row means `open`.** An existing install that upgrades must not find itself closed.
- **`pass_floor`** is the lowest role that is let through while not open: `podcaster` (default) or `admin`. ADMIN always passes — an admin locked out of their own site by their own setting cannot fix it.
- **Timing rules**, so the countdown a visitor is watching never lies: a `scheduled` `opens_at` must lie at least 60 s ahead when saved. The one exception is **open now**, which sets `scheduled` with `opens_at` = now + 12 s, so the last ten seconds still play rather than the page jumping. Postponing is an ordinary save of a later time.
- **`revision`** increments on every write and is the `ETag` of the public read, so a poll that changed nothing is a `304`.
- **`edge_counting`** (default on) switches the edge's anonymous counting (§18.5, §19) on or off, independently of core's own (§19). It is a site-access setting rather than a counting one because the edge is what it governs and the edge already reads this policy.

### 18.2 What core exposes
- **`GET /api/site/access`** — public: `state`, `effectiveState`, `opensAt`, `endsAt`, `passFloor`, `staffLogin` (`visible` + the configured providers as `{id, path}`), `edgeCounting`, `now`, `revision`. Public because nothing in it is secret: the countdown shows the date anyway, and the provider list is the login button core already renders. The edge polls it every few seconds (default 2 s) — cheap because the `ETag` is the `revision`, which covers the *stored* policy; `effectiveState` and `now` are derived and may change without it, which is why the edge works out the moment of opening itself.
- **Who someone is: `GET /api/me`, unchanged.** The edge forwards the visitor's session cookie and reads `role`. There is deliberately no signed "pass" cookie: §8.5 rejected stateless tokens so that a ban or a demotion takes effect at once, and a pass the edge could verify offline would be exactly such a token. The edge may cache an answer briefly (≤ 30 s), which bounds how long a demoted podcaster keeps seeing a closed site.
- **Look and legal text, unchanged:** `GET /api/site` (name, generated light/dark tokens, mode policy), `/branding/*` (logo, favicon), `GET /api/legal` and `GET /api/legal/{slug}`. The edge copies what it needs while core is up, because its pages must render while core is down.
- **The edge reports in.** A poll carrying `Mosaicast-Edge-Token` equal to `MOSAICAST_EDGE_TOKEN` is recorded — when, and the build the edge names in `Mosaicast-Edge-Build` — in memory, not the database: it is a liveness signal, not history. The token exists so that "edge connected" on the admin page cannot be produced by anyone who can send a header; with no token configured the page shows the edge as unknown. A poll without the token is still answered: the endpoint is public.

### 18.3 What core enforces itself: sign-ups
**While the effective state is not `open`, core refuses to create new accounts** (§8.3 case 3, a first login with an unknown identity), unless `signups_while_closed` is on. Existing accounts sign in, linking an identity to a signed-in account still works, and the bootstrap admin (§8.5) is always created. This is core's to enforce rather than the edge's because the account is created in core, and a hidden login path is not a closed one: anyone who finds it can complete an OAuth flow. The refusal is a login failure with its own code, so the edge can say what happened. An admin who wants a new podcaster in before launch turns the switch on for the moment it takes.

### 18.4 The edge
- **A separate process in Go** — the one component outside §2's stack, chosen for the property that justifies its existence: a ~10 MB static binary that starts in milliseconds and shares none of core's failure modes. It talks to core only over the HTTP above, so it needs neither the SDK nor any core code.
- **Request flow** while the site is not open: the edge's own paths (`/_a/*`) and the OAuth paths (`/oauth2/authorization/*`, `/login/oauth2/code/*`) always pass; a request **without** a session cookie gets the page for the current state without core being asked anything, so anonymous load costs core nothing; a request **with** one is checked against `/api/me` and proxied if the role meets `pass_floor`. Automation with a personal access token (§8.5, `Authorization: Bearer mcp_…`) is checked the same way, with only that header forwarded, and only when no session signs the request in (a live session wins, as it does in core) — and **a token never counts as ADMIN**, mirroring core's own cap on token authority, so with `pass_floor = admin` no token passes. Any error during the check shows the page — **fail closed**. While open, everything is proxied untouched, with `Host` and the forwarded headers preserved so core builds its absolute URLs (the OAuth redirect among them) correctly.
- **Pages:** `countdown` (state `scheduled`, phases wait → final → party → handoff), `maintenance`, `closed`, `unavailable` (core does not answer — the edge decides this one itself, from the proxy's own failures), plus `/_a/login` (staff sign-in, always reachable, linked from the pages only when `staff_login_visible`) and `/_a/legal/{slug}` (the site's legal pages, from the copy). While the site is not open, a `GET`/`HEAD` of core's own legal URL `/legal/{slug}` (§12.6) redirects there, so a bookmark, a search result or a link inside the legal text still reaches the imprint rather than a countdown. The **edge owns the HTTP semantics**: maintenance, closed and unavailable answer `503` with `Retry-After` so search engines keep the site's index, and non-HTML requests get a plain `503` whatever the page looks like.
- **Themes** change how these pages look and nothing about how they behave. A theme may provide any page, restyle the edge's default for it, or provide nothing; whatever it leaves out falls back to the edge's default, which is drawn from the site's own tokens and logo and so looks like the site with no theme at all. The clock, the state machine and the handoff belong to the edge's runtime, not to the theme: they are where a launch page can go wrong, and every theme reimplementing them is every theme free to reintroduce the same bugs. Themes are **self-contained** (no external requests, no fetches from core at runtime) because the page they are most needed for is the one shown while core is down. The theme contract is the edge repo's to document.
- **The edge trusts its own clock** for the moment of opening, and tells the browser the time: a visitor's device clock is not evidence of anything.

### 18.5 What the edge reports
The edge sees the visitors core never does while the site is not open, so it reports what it counted (§19) to core, which stores and shows it.

- **`POST /api/site/edge/report`**, `Mosaicast-Edge-Token` required (404 when core has no token configured, 401 for a wrong one) and `Mosaicast-Edge-Build`. Body, JSON, at most 16 KB:
  ```
  { "waiting": 1234, "waitingCapped": false, "policyRevision": 7,
    "hours": [ { "start": "2026-10-18T09:00:00Z",
                 "views": { "countdown": 5231, "maintenance": 0, "closed": 0,
                            "unavailable": 12, "login": 3, "legal": 40 } } ] }
  ```
- **`waiting`** is the number of edge pages currently held open, smoothed as the maximum over the last 30 s and clamped to the edge's cap; `waitingCapped` says the cap was reached in that window. Connections, not people: two tabs are two. `policyRevision` is the revision the edge enforced when it sampled, and core books the peak to that revision rather than its own current one, because the report lags by up to a minute.
- **`hours`** are the **completed** UTC hours the edge was running and that core has not acknowledged, zeros included and every kind present, oldest first, at most 48 per report. "No hour" therefore means no data and an hour of zeros means nobody came. The edge never sends a partial hour: it keeps the running one across restarts. **Views** are rendered edge pages (GET, status 200 or 503), counted by page kind — never by path, which is attacker-chosen and would make the counters unbounded.
- **Core answers `200 { "ackThrough": "<UTC hour>" }`**, inclusive and no later than the newest hour in that report; the edge drops only the hours it sent in that request up to it. Core stores hours with **SET** semantics, so a resend after a lost reply is harmless. It answers 400 only for malformed input (bad JSON, a negative or non-integer count, a start that is not a UTC hour, more than 48 hours or 16 KB). **An unknown page kind is ignored** and the hour still acknowledged, and a known kind left out counts as 0, so an edge released ahead of core never wedges.
- **Cadence:** every 30 s and once on shutdown. 401/404 are retried every ten minutes, so an edge in front of an older core is harmless.
- **Invariant:** the edge keeps unacknowledged hours for at most 7 days, which must stay below the age at which core folds hourly counts into daily ones (§19, 30 days), so no resend lands on an hour that no longer exists. Neither is raised without the other.
- **`edge_counting` off** (§18.1): the edge counts and keeps nothing, deletes what it had, and sends no report; with no policy known yet it counts nothing either. A report that still arrives is acknowledged and nothing stored. Toggling it never wakes the edge's held pages. The countdown's live "waiting" display is a theme feature, not a statistic, and is unaffected.

---

## 19. Anonymous counting
A podcast wants to know whether anyone listens. Mosaicast answers that with **counts, never people**, under a rule that is the whole design: **nothing is stored on or read from a visitor's device, and nothing about a visitor reaches storage.**

- **No identifiers at all:** no cookie, no local or session storage, no ETag or cache tricks, no fingerprint, and no hash of IP and User-Agent either — the "anonymous" visitor id other analytics derive from them is still an identifier. Consequently there are **no unique visitors, sessions, bounce rates or returning visitors**, and there never will be; the admin page says so rather than leaving a gap that looks like a missing feature.
- **What core counts** (shell beacons): `view` per route subject (home, feed, episode, plugin page, about, legal page, search), `play` per episode (first play per page load) and `progress` milestones 25/50/75/100 % (first crossing per page load). A subject must exist; anything else is dropped silently, so invented slugs cannot grow a table and the endpoint does not reveal which exist. The beacon carries only kind, subject and milestone, is sent **without credentials**, and is not sent at all while an admin or podcaster is signed in, so staff are excluded without being identified.
- **What the edge counts** (§18.5): views per edge page kind and the waiting gauge.
- **Transient, never stored:** core uses the client address only in memory, for a rate limit that bounds inflation; both sides compare the User-Agent, at the moment of counting, against one frozen crawler list (below) so link previews and bots do not count. Neither value is written, hashed or kept per visitor. Counts are **approximate by design**: reloads count, and a script-running bot may.
- **Global Privacy Control** is honoured on both sides: the shell sends nothing while `navigator.globalPrivacyControl` is set, and the edge neither counts a page request with `Sec-GPC: 1` nor reports such a held page in its gauge (the page's own live display still includes it).
- **Storage:** counters per UTC hour and subject, written additively (several core instances simply add up); hourly rows older than 30 days are folded into daily rows, which are kept. **Only the web:** plays in podcast apps go from the feed to the audio host and never reach Mosaicast.
- **Switches:** core's counting (`site_config`, default on) and the edge's (`edge_counting`, §18.1) are independent.
- **Who sees it:** ADMIN and PODCASTER, on an admin page; switching core's counting is ADMIN only.
- **Crawler list** — lower-cased User-Agent substring match; an empty or missing User-Agent counts as a crawler. Frozen: a change is a spec change, and core and the edge each pin it by a test against this block.
  ```
  bot crawl spider slurp preview fetch scan
  facebookexternalhit facebookcatalog meta-externalagent whatsapp embedly iframely vkshare pinterest
  google-pagerenderer google-read-aloud mastodon bluesky snapchat
  headlesschrome lighthouse pagespeed uptime pingdom statuscake monitor
  curl/ wget/ python- go-http-client java/ okhttp node-fetch axios httpclient libwww mosaicast-
  ```
- **No consent banner follows from this** in the reading this design is built on — nothing is stored on or read from the device, and nothing personal is stored — but operators remain responsible for their own privacy notice; the README gives a sentence they can adapt. *(Not legal advice.)*
