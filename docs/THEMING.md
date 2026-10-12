# Theming mosaicast-edge

mosaicast-edge sits in front of a [Mosaicast](https://github.com/Mosaicast/mosaicast-core) site and shows
visitors a page of its own while the site is not open: a countdown before a launch, a notice during
maintenance, a holding page while the site restarts. Out of the box those pages look like the site
itself, drawn from its colours and logo. A **theme** changes how they look, and nothing about how they
behave.

This document is everything you need to build one. You do not need to know how the edge works inside,
and you do not need a Mosaicast install: `mosaicast-edge preview` plays the site for you.

- [Pages and states](#pages-and-states)
- [Three levels: full, restyle, default](#three-levels-full-restyle-default)
- [Quick start](#quick-start)
- [A theme folder](#a-theme-folder)
- [`theme.json`](#themejson)
- [Templates](#templates)
- [Texts and languages](#texts-and-languages)
- [The runtime: data, attributes, events](#the-runtime-data-attributes-events)
- [CSS hooks](#css-hooks)
- [Accessibility](#accessibility)
- [Rules](#rules)
- [Preview, check, deploy](#preview-check-deploy)
- [The example theme](#the-example-theme)

## Pages and states

| page | shown when | HTTP |
|---|---|---|
| `countdown` | the site is scheduled to open at a set moment | 200 |
| `maintenance` | the site is down for planned work; may show an announced end | 503 |
| `closed` | the site is closed with no end in sight | 503 |
| `unavailable` | the site itself does not answer (a restart, an outage) | 503 |
| `login` | `/_a/login`, the staff sign-in, reachable in every state | 200 |
| `legal` | `/_a/legal/<slug>`, the site's imprint, privacy policy and so on | 200 / 404 |

The edge decides which page a visitor gets and with which status code. A theme only decides what the
page looks like.

**The countdown has phases.** They only ever move forward:

```
wait ──────────────▶ final ─────────▶ party ─────────▶ handoff ──▶ the site
(days … seconds)     (last 10 s)      (after zero,     (the edge confirms
                                       partySeconds)    the site is open, reloads)
```

- `wait`: days, hours, minutes and seconds to go.
- `final`: the last ten seconds, one number at a time. Always ten: when an admin hits "open now", the site
  opens twelve seconds later so that these ten still play.
- `party`: from zero, for `partySeconds` (default 15). Your celebration.
- `handoff`: the page waits until the edge reports the site open, then reloads the same address and the
  visitor is on the real site.

The moment of opening may move while a visitor waits (postponed, or brought forward with "open now"). The
runtime follows it while the page is in `wait`. From `final` on it never goes back.

**`unavailable` has two moods.** For the first minute of an outage it is "back in a moment"
(`data-down="short"`), after that "this is taking longer" (`data-down="long"`). The page reloads by
itself the moment the site answers again.

## Three levels: full, restyle, default

For each page, a theme picks one of three levels:

| level | the theme ships | you write | good for |
|---|---|---|---|
| **full** | the whole page | a template, stylesheets, scripts, images, texts | the page that is the show, usually the countdown |
| **restyle** | a new look for the edge's own page | CSS (variables or rules), images via CSS, texts | pages that should match without being rebuilt |
| **default** | nothing | nothing | pages the theme does not care about |

A page left out of `theme.json` is **default**: the edge's own page, looking like the site, untouched by
the theme. A theme may provide one page or all six.

## Quick start

```sh
cp -r examples/minimal my-theme                     # a complete small theme to start from
mosaicast-edge preview --theme my-theme             # open http://127.0.0.1:8090/_preview
# edit; every page load picks up your changes
mosaicast-edge check-theme my-theme                 # before you deploy
```

With Docker instead of a local binary:

```sh
docker run --rm -p 127.0.0.1:8090:8090 -v "$PWD/my-theme:/theme:ro" <edge-image> \
  preview --theme /theme --listen 0.0.0.0:8090
docker run --rm -v "$PWD/my-theme:/theme:ro" <edge-image> check-theme /theme
```

## A theme folder

```
my-theme/
├── theme.json          the manifest (required)
├── templates/          *.html: page templates and shared partials (full pages only)
├── texts/              en.json, de.json, …: one flat file per language
└── assets/             everything the browser loads: css, js, images, fonts, sound
```

- Only `assets/` is served, and only under content-hashed names (`/_a/t/3fa9c2e1b7d04a11.webp`). The
  real file names never reach the browser and there are no folder listings.
- Anything else in the folder (a README, a LICENSE, source files) is ignored.
- Symbolic links are refused.

## `theme.json`

```json
{
  "schema": 1,
  "name": "My theme",
  "version": "1.0.0",
  "locales": ["en", "de"],
  "texts": "texts/{lang}.json",
  "styles": ["css/theme.css"],
  "scripts": ["js/theme.js"],
  "favicon": "img/favicon.svg",
  "pages": {
    "countdown":   { "mode": "full", "template": "countdown.html" },
    "maintenance": { "mode": "restyle" },
    "closed":      { "mode": "restyle" }
  },
  "countdown": { "partySeconds": 15, "handoffSeconds": 1, "preloadSeconds": 60, "cues": { "liftoff": 3 } },
  "share": { "image": "img/share.jpg" },
  "assets": { "expireAfterOpen": "1h", "keep": ["img/share.jpg"] },
  "credits": [
    { "work": "Inter", "author": "Rasmus Andersson", "license": "OFL-1.1", "url": "https://rsms.me/inter/" }
  ]
}
```

Unknown keys are an error, so a misspelt key never silently does nothing.

| key | required | meaning |
|---|---|---|
| `schema` | yes | `1`. The edge refuses a schema it does not know. |
| `name` | yes | shown in logs and by `check-theme` |
| `version` | no | yours; shown in logs |
| `locales` | yes | the languages your texts come in. **The first one is your fallback**: a visitor whose language you do not have gets your first language, not the edge's text. |
| `texts` | no | where the text files are; `{lang}` is replaced by each locale. Default `texts/{lang}.json`; with the default, a theme may have no texts at all. |
| `styles` | no | stylesheets under `assets/`, added to **every page the theme provides or restyles**, after the edge's own |
| `scripts` | no | scripts under `assets/`, loaded (deferred) on those pages, after the runtime |

`styles` and `scripts` are for what all your pages share (fonts, colours). What only one full page needs,
link from that page's template instead, so it does not land on the others:

```html
<head>
  {{.EdgeHead}}
  <link rel="stylesheet" href="{{.Asset "countdown.css"}}">
  <script src="{{.Asset "countdown.js"}}" defer></script>
</head>
```
| `favicon` | no | an image under `assets/`; replaces the site's favicon on the theme's pages and at `/favicon.ico` while the site is not open |
| `pages.<page>.mode` | — | `full` or `restyle`; a page not listed is default |
| `pages.<page>.template` | for `full` | a file name in `templates/` |
| `countdown.partySeconds` | no | 0–120, default 15: how long `party` lasts before `handoff` |
| `countdown.handoffSeconds` | no | 0–5, default 1: the pause between "the site is open" and the reload, for a closing animation |
| `countdown.preloadSeconds` | no | 0–3600, default 60: when `img[data-edge-src]` start loading (see below) |
| `countdown.cues` | no | named moments, in seconds before opening (`"liftoff": 3`). At each, the runtime sets `data-cue-<name>` on `<html>` and fires `edge:cue`. This is how a theme gets its own beats without timing anything itself. |
| `share.image` | no | JPEG, PNG or WebP under `assets/`; becomes `og:image` for link previews. 1200 × 630 is the common size. |
| `assets.expireAfterOpen` | no | a duration (`1h`, `24h`). This long after the site has opened, the theme's assets stop being served (404), except `keep`, the share image and the favicon. For artwork that should not stay downloadable. Default: never. At least `partySeconds` plus a minute. |
| `assets.keep` | no | assets exempt from expiry |
| `credits` | no | `work` (required), `author`, `license`, `url` (https). Shown on every page the theme provides or restyles. List your fonts and artwork. |

## Templates

Full pages are Go [`html/template`](https://pkg.go.dev/html/template) files. Values are escaped for
where they land, so you cannot accidentally inject markup. All files in `templates/` are parsed
together, so a page can use partials (`{{define "header"}}…{{end}}` in one file, `{{template "header" .}}`
in another).

A complete page:

```html
<!doctype html>
<html {{.HTMLAttrs}}>
<head>
  {{.EdgeHead}}
  <title>{{.T "countdown.title"}}</title>
</head>
<body>
  <h1 data-edge-text="countdown.title">{{.T "countdown.title"}}</h1>
  <p><span data-edge-unit="d">{{.Countdown.D}}</span> days,
     <span data-edge-unit="h">{{.Countdown.H}}</span>:<span data-edge-unit="m">{{.Countdown.M}}</span>:<span data-edge-unit="s">{{.Countdown.S}}</span></p>
  <p data-edge-final></p>
  <footer>{{.EdgeLegal}} {{.EdgeLogin}} {{.EdgeLangs}} {{.EdgeCredits}}</footer>
</body>
</html>
```

The page is rendered complete on the server, digits included, so it is right before any script runs and
readable without JavaScript. The runtime then keeps it moving.

### What the edge always puts in

| call | what it renders | if your template leaves it out |
|---|---|---|
| `{{.HTMLAttrs}}` | `lang`, `data-page`, `data-mode`, `data-phase`, … on `<html>` | added to `<html>` |
| `{{.EdgeHead}}` | charset, viewport, favicon, share tags, the site's colour tokens, the runtime and its data, your `styles` and `scripts` | added at the start of `<head>` |
| `{{.EdgeLegal}}` | links to the site's imprint and privacy policy | added in a footer before `</body>` |
| `{{.EdgeLogin}}` | the staff sign-in link, when the site wants it shown | same |
| `{{.EdgeCredits}}` | your `credits` | same |
| `{{.EdgeLangs}}` | a language switcher | optional, never added |

You decide **where** these go; you cannot leave them out. The legal links are not decoration: in
Germany, for one, the imprint and privacy policy must be reachable from every page, a countdown
included. If a stylesheet hides them anyway, the runtime shows them in a bar at the bottom of the
screen. Place `{{.EdgeHead}}` first in `<head>`: it carries the charset.

### Template data

| field | type | on | meaning |
|---|---|---|---|
| `.Page` | string | all | `countdown`, `maintenance`, `closed`, `unavailable`, `login`, `legal` |
| `.Phase` | string | countdown | `wait` or `final` at render time |
| `.Lang` | string | all | the page language |
| `.Locales` | []string | all | languages the page can switch to |
| `.Site.Name` | string | all | the site's name |
| `.Site.Logo`, `.Site.DarkLogo` | string | all | URLs of the site's logos, `""` when absent |
| `.OpensAt` | time | countdown | the moment of opening (`{{.OpensAt.UTC.Format "2006-01-02T15:04:05Z"}}`) |
| `.When` | string | countdown | `.OpensAt` written out for readers without JavaScript |
| `.Countdown` | | countdown | `.Days .Hours .Minutes .Seconds .Total` (numbers), `.D .H .M .S` (two digits) |
| `.Units` | list | countdown | `{Key, Value, Label}` for d, h, m, s, labels in the page language |
| `.Calendar` | string | countdown | link to an `.ics` calendar entry for the opening |
| `.EndsAt`, `.Until` | time, string | maintenance | the announced end, if any. Shown, never acted on: the edge does not reopen the site by itself. |
| `.DownSince`, `.DownLong` | time, bool | unavailable | start of the outage; true after a minute |
| `.Viewer` | | all | nil for anonymous visitors; `{SignedIn, Role}` for someone signed in who may not see the site yet (e.g. a fan before launch) |
| `.Login.Providers` | list | login | `{ID, Name, Href}` per sign-in provider; may be empty |
| `.Login.Error`, `.Login.ErrorText` | string | login | the code of a failed sign-in and its text |
| `.LegalPage` | | legal | `{Slug, Title, Role, HTML}`, nil for an unknown slug (the page is then a 404) |
| `.Legal` | list | all | `{Title, Href, Role}`, what `.EdgeLegal` renders |
| `.Credits` | list | all | your credits |

| method | |
|---|---|
| `{{.T "key"}}` | the text for `key` in the page language (see [texts](#texts-and-languages)) |
| `{{.Tr "key" "name" value …}}` | same, with `{name}` placeholders filled |
| `{{.Plural "unit.d" 3}}` | `unit.d.one` for 1, `unit.d.other` otherwise |
| `{{.Asset "img/bg.webp"}}` | the URL of a file under `assets/` |
| `{{.PageTitle}}` | the text `<page>.title` |

An unknown asset, an unknown text key or a template error fails `check-theme` and stops the edge from
starting, so a broken theme is caught before it is live, never in front of visitors.

## Texts and languages

Text files are flat JSON objects of strings:

```json
{ "countdown.title": "Something new is coming", "my.tagline": "Stay tuned" }
```

- **Every locale file must have exactly the same keys.** `check-theme` lists any difference.
- A key you define overrides the edge's text of the same name; keys of your own are fine too.
- `{site}` in any text becomes the site's name.
- `{when}` in any text becomes the opening while one is scheduled, or the announced end during
  maintenance, as a full date in the edge's time zone (`EDGE_TIMEZONE`) and the page language, with
  the zone's name: `Sunday, 18 October 2026 · 12:00 CEST`, `Sonntag, 18. Oktober 2026 · 12:00 Uhr MESZ`.
  It follows the policy, so a postponement or "open now" never leaves a stale date behind. Use it
  instead of writing a date into a text, above all in `description` (the search and link-preview text:
  crawlers do not run JavaScript, so the configured zone is the right one there) and in `ics.summary` /
  `ics.description` (the calendar entry).
  With no date (closed, open, maintenance without an end) `{when}` is dropped and the spaces around it
  are tidied, so write the date as an addition the sentence survives without:

  | text | with a date | without |
  |---|---|---|
  | `"Our new home opens soon. {when}"` | Our new home opens soon. Sunday, 18 October 2026 · 12:00 CEST | Our new home opens soon. |
  | `"Opens on {when}."` (don't) | Opens on Sunday, 18 October 2026 · 12:00 CEST. | Opens on. |
- Lookup order for a key: your text in the page language, else your text in your first locale (if you
  do not have the page language at all), else the edge's text.
- The page language comes from `?lang=` in the address, then the browser's `Accept-Language`, then the
  site's default language, then your first locale. Full pages use your `locales`; restyled and default
  pages use the edge's (English and German).
- The edge's own keys, with English and German texts, are in
  [`web/default/texts/en.json`](../web/default/texts/en.json). Overriding them is how a restyled page
  gets its own words.

The runtime switches language without a reload when a visitor clicks an element with
`data-edge-lang="de"` (what `{{.EdgeLangs}}` renders): every `[data-edge-text]` gets the new text,
dates are rewritten, and `edge:lang` fires. Nothing is stored.

## The runtime: data, attributes, events

The edge injects one script into every page: the **runtime**. It owns everything that can make a launch
page go wrong: the clock, the phases, following changes to the opening time, and the reload into the
site. Your theme never re-implements any of it. It reacts.

**The clock.** The runtime runs on the edge's clock. It starts from the server time the page was
rendered with, corrects it against the edge every few minutes, and counts with a monotonic stopwatch.
It never reads the visitor's device clock, which may be wrong or deliberately set forward.

### Attributes on `<html>`

| attribute | values | |
|---|---|---|
| `data-page` | page name | |
| `data-phase` | `wait`, `final`, `party`, `handoff` | countdown only; changes over time |
| `data-cue-<name>` | present | from the moment a cue in `countdown.cues` has passed |
| `data-mode` | `light`, `dark`, `system` | the site's colour-mode **policy**, set by the server |
| `data-scheme` | `light`, `dark` | what that policy resolves to for this visitor, set and kept live by the runtime |
| `data-down` | `short`, `long` | unavailable only |
| `data-viewer` | `signed-in` | someone signed in who cannot see the site yet |
| `lang` | the page language | changes with the language switch |

`data-mode` is the site's policy, not the colours in use: under `system` the visitor's own preference
decides. In CSS, resolve it like this, which works before any script runs:

```css
:root { --ink: #111; --paper: #fff; }                                   /* light */
:root[data-mode="dark"] { --ink: #eee; --paper: #111; }                 /* forced dark */
@media (prefers-color-scheme: dark) {
  :root[data-mode="system"] { --ink: #eee; --paper: #111; }             /* visitor prefers dark */
}
```

`data-scheme` says the same in one attribute, but it is set by the runtime, so a stylesheet keyed on it
shows the light variant for a moment before the script runs. Use it in scripts (a canvas colour, say),
or read `edge.scheme`.

Most themes need little more than CSS on these:

```css
.final, .party { display: none; }
[data-phase="final"] .wait { display: none; }
[data-phase="final"] .final { display: block; }
[data-phase="party"] .party, [data-phase="handoff"] .party { display: block; }
[data-cue-liftoff] .rocket { animation: liftoff 3s ease-in forwards; }
```

### Elements the runtime fills

| attribute | the runtime |
|---|---|
| `data-edge-unit="d\|h\|m\|s"` | writes the two-digit value every second (`wait`) |
| `data-edge-unit-label="d\|h\|m\|s"` | writes "day"/"days" etc. in the page language |
| `data-edge-final` | writes the remaining seconds, 10 … 1 (`final`), and re-adds the class `is-tick` each second so a CSS animation restarts |
| `data-edge-split` (with one of the above) | puts each character in its own `<i>`, for fixed-width digits in a font without tabular figures |
| `data-edge-when="opensAt\|endsAt\|downSince"` | writes the instant in the visitor's own time zone and language; `data-edge-format="date\|time"` for one half |
| `data-edge-text="key"` | rewrites the text on a language switch |
| `data-edge-label="key"` | rewrites `aria-label` on a language switch (icon buttons, landmarks) |
| `data-edge-lang="de"` | becomes a language switch |
| `data-edge-src="url"` on `img`, `audio`, `video` | becomes `src` at `countdown.preloadSeconds`, so heavy artwork for the finale is not loaded days early |
| `data-edge-handoff` | a click during `party` skips to the handoff ("enter now") |
| `data-edge-calendar` | keeps its `?lang=` in step with the language switch |

The runtime marks the ticking elements (`data-edge-unit`, `data-edge-unit-label`, `data-edge-final`,
`data-edge-split`) `aria-hidden="true"` and speaks for them itself; see [Accessibility](#accessibility).

### Events

All are `CustomEvent`s dispatched on `document`, with the payload in `event.detail`. Your scripts run
before the first one fires, so you see every event from `edge:ready` on.

| event | detail | when |
|---|---|---|
| `edge:ready` | `{page, phase, lang}` | once, after the page is set up |
| `edge:phase` | `{phase, from}` | the countdown entered a phase |
| `edge:wait`, `edge:final`, `edge:party`, `edge:handoff` | `{from}` | the same, one event per phase |
| `edge:tick` | `{remaining, parts: {d, h, m, s, total}, phase}` | each second the display changes (`remaining` in ms) |
| `edge:cue` | `{name, remaining}` | a cue from `countdown.cues` passed |
| `edge:retime` | `{opensAt, from}` | the opening moved while in `wait` (unix ms) |
| `edge:lang` | `{lang, from}` | the language was switched |
| `edge:down` | `{long: true}` | unavailable: the outage passed a minute |
| `edge:reconnect` | `{}` | unavailable: the site is back, the page reloads now |
| `edge:scheme` | `{scheme, from}` | the resolved colour scheme changed (the visitor switched their system setting) |

```js
document.addEventListener('edge:party', () => confetti.burst());
document.addEventListener('edge:tick', (e) => {
  if (e.detail.phase === 'final' && e.detail.parts.total <= 3) drum.hit();
});
```

### `window.edge`

A read-only object for scripts that need a value rather than an event:

| | |
|---|---|
| `edge.now()` | the edge's time in unix ms. Use it instead of `Date.now()`. |
| `edge.remaining` | ms until opening (countdown), else `null` |
| `edge.phase`, `edge.lang`, `edge.opensAt`, `edge.scheme` | current values |
| `edge.t(key, {name: value})` | a text in the current language |
| `edge.data` | `{page, viewer, site: {name}, endsAt, downSince, locales, countdown}`; `countdown` is `{finalMs, partyMs, handoffMs}` on the countdown, else `null` |

## CSS hooks

### The site's colours

Every page carries the site's own colour tokens, light and dark, as custom properties on `:root`. They
follow the site's mode policy and, under `system`, the visitor's preference. Build on them and your
theme follows the site's colours and dark mode for free:

| property | use |
|---|---|
| `--mc-bg` | page background |
| `--mc-surface` | raised surfaces (cards) |
| `--mc-text`, `--mc-text-muted` | text, secondary text |
| `--mc-accent` | **fills only** (buttons, highlights), with `--mc-accent-contrast` for text on top of it |
| `--mc-accent-text` | the accent for **text, links and focus rings**: guaranteed readable on `--mc-bg` and `--mc-surface` |
| `--mc-border` | borders, dividers |
| `--mc-accent-2` | a second accent |

The accent is whatever the site's admin picked and may be too pale to read as text. Colour text with
`--mc-accent-text`, never with `--mc-accent`.

A theme with a palette of its own (a podcast's artwork, say) is free to ignore them.

### Restyling the edge's pages

On restyled pages your stylesheets come after the edge's. The quickest lever is its variables:

| variable | default |
|---|---|
| `--edge-font`, `--edge-font-display` | system UI font |
| `--edge-bg`, `--edge-bg-image` | `--mc-bg`, none (`url(...)` in your CSS works; it is resolved to your asset) |
| `--edge-card`, `--edge-text`, `--edge-muted`, `--edge-link`, `--edge-border` | the site's tokens |
| `--edge-button`, `--edge-button-text` | `--mc-accent`, `--mc-accent-contrast` |
| `--edge-radius`, `--edge-max` | 14px, 40rem |

```css
@font-face { font-family: Display; src: url(../fonts/display.woff2) format("woff2"); }
:root { --edge-font-display: Display, serif; --edge-bg-image: url(../img/backdrop.webp); --edge-radius: 0; }
```

For more, style the classes the default pages use: `edge-page`, `edge-page--<page>` (on `<body>`),
`edge-shell`, `edge-brand`, `edge-logo`, `edge-main`, `edge-card`, `edge-title`, `edge-lead`,
`edge-note`, `edge-when`, `edge-units`, `edge-unit`, `edge-unit-value`, `edge-unit-label`, `edge-final`,
`edge-party`, `edge-button`, `edge-error`, `edge-providers`, `edge-prose`, `edge-footer`, `edge-legal`,
`edge-login`, `edge-langs`, `edge-credits`. The default templates are in
[`web/default/templates/`](../web/default/templates/) if you want to see the structure.

## Accessibility

**Screen readers get the countdown from the runtime.** A display that changes every second must not be
read out every second, so the runtime hides the ticking elements from assistive technology and keeps
one polite live region of its own with a sentence in the page language ("Opening in 2 days, 3 hours and
5 minutes."). It speaks on load, every ten minutes while the opening is more than a day away, every
minute after that, once when the final ten seconds start, and once at zero. Your theme gets this
without doing anything; do not add `aria-live` to your own countdown markup. The sentences are edge
texts (`a11y.*`) you may override.

What remains yours:

- **Motion.** Honour `prefers-reduced-motion: reduce`: no flying, flashing or shaking for those visitors.
  A cross-fade is fine.
- **Sound.** Only after a user gesture (a click on your sound button), never on load, and with a
  visible way to turn it off again. Browsers block autoplay anyway; a page that tries looks broken.
- **Contrast.** Text over artwork needs a scrim (a gradient or a darkened band) behind it so it stays
  readable at 4.5:1 wherever the image is bright. Check the phases separately: a fire backdrop in
  `final` is brighter than a night sky in `wait`.
- **Labels.** Icon-only buttons need an `aria-label`; use `data-edge-label` so it follows the language.
- **Language.** Text in another language than the page's (a motto, a quote) gets its own `lang`.

## Rules

These hold for every theme. Most are enforced; the rest are how a theme stays out of trouble on the day
it matters.

1. **Self-contained.** Everything a page needs is in your `assets/`. No CDNs, no web fonts from
   elsewhere, no analytics, no embeds. The page that matters most, `unavailable`, is shown while the site
   is down. The pages run under a strict Content Security Policy that enforces it:
   `default-src 'self'`, no inline scripts, no inline styles, no `eval`.
2. **No inline script or style.** Not `<script>…</script>`, not `style="…"`, not `onclick`. Put them in
   files under `assets/` and list them in `styles` / `scripts` (or link them with `{{.Asset}}`).
3. **No clock logic.** Never read `Date.now()` or `new Date()` to decide anything. Never decide when a
   phase starts. Use the attributes, events and `edge.now()`. The runtime is where the timing has been
   made right once; a theme that times itself brings back the bugs it removed.
4. **No requests.** A theme gets all its data from the page. Do not `fetch` anything; the edge's own
   endpoints are not part of this contract and change without notice.
5. **The legal links and the sign-in link stay.** Place them, style them; do not hide them.
6. **Allowed files** under `assets/`: `css js woff2 woff ttf otf webp png jpg jpeg gif avif svg ico mp3
   ogg opus webm mp4`. At most 4 MB per file, 24 MB and 400 files in total. CSS `url()`s must point at
   files in `assets/` (relative paths) or be `data:` URIs; `@import` is not supported.
7. **Respect `prefers-reduced-motion`.** A launch is exciting; a flashing screen can be dangerous.
8. **Design for phones first.** Most visitors of a launch come from a link on their phone. No
   horizontal scrolling at 360 px.

`check-theme` warns (without failing) about the things it can spot but not prove wrong: scripts that
read the device clock or make requests, templates with inline script or style or links to other
origins, and a theme with assets but no credits.

### Protecting artwork

There is no way to stop a determined visitor from taking a screenshot, but a theme can make sure there
is nothing better to take:

- Ship images only as large as they are shown (twice that for sharp screens), never the originals.
- Prefer flattened raster images to vector sources.
- Watermark backdrops where a crop hides it.
- Set `assets.expireAfterOpen` so the artwork is gone once the launch is over.

The edge already serves assets only under hashed names, without listings, with
`Cross-Origin-Resource-Policy: same-origin` (other sites cannot embed them; the share image and favicon
are the exception) and `X-Robots-Tag: noindex, noimageindex`.

## Preview, check, deploy

**Preview.** `mosaicast-edge preview --theme <dir>` starts the real edge on `127.0.0.1:8090` against a
fake site held in memory. `/_preview` links every page and state:

- a countdown three days out, 75 seconds out, and 20 and 12 seconds out (watch `final`, `party` and the
  handoff happen)
- signed in as a fan
- maintenance with and without an end; closed
- unavailable
- the sign-in page with providers, without, and with an error
- a legal page
- the site's colour mode forced to light, dark or system

Light and dark otherwise follow your system setting. Add `?lang=de` to any page for another language.
Every page load reloads the theme if a file changed. A theme that does not validate shows its errors
instead of the page.

**Check.** `mosaicast-edge check-theme <dir>` loads the theme the way the edge does at startup: it
validates `theme.json`, every asset reference and every text file, and renders every page you provide in
every language and phase with sample data. It prints what is wrong (all of it at once) and exits
non-zero, or prints a summary and any warnings. Run it in your theme's CI. It also takes an archive
or any `EDGE_THEME` value below (`check-theme theme.tgz`).

### Deploying a theme

`EDGE_THEME` says where the theme comes from. The grammar is the one Mosaicast core uses for plugins:

| `EDGE_THEME` | |
|---|---|
| `/srv/theme` | a folder (mounted read-only; `EDGE_THEME_DIR` is the older name for this) |
| `/srv/theme.tgz`, `./theme.zip` | an archive on disk |
| `https://example.org/theme.tgz#sha256:<digest>` | an archive anywhere |
| `owner/repo@v1.2.0#sha256:<digest>` | the `theme.tgz` of that GitHub release |
| `https://github.com/owner/repo@v1.2.0#sha256:<digest>` | the same, spelled as the address in the browser |

- **Downloads need a pin**, `#sha256:<digest>` of the archive, unless `EDGE_THEME_ALLOW_UNVERIFIED=true`:
  a theme's scripts run on the site for every visitor, so an unpinned download is unknown code.
  **Name the tag with a pin.** `owner/repo` alone means "the latest release"; a newer release would
  then fail the pin and the edge would not start.
- **A downloaded theme is kept** in the edge's cache volume, keyed by the whole spec and pin and
  checked against the pin each time it is read. A restart takes it from there without asking the
  network, so it does not depend on GitHub being up. An archive changed on the volume is refused.
  A spec that cannot be resolved, with nothing cached, stops the edge from starting.
- **A private theme repo** needs `EDGE_THEME_TOKEN`: a *fine-grained* GitHub token for that one
  repository, **Contents: read-only**, with an expiry. The edge sends it to GitHub's API only (the file
  itself comes from storage without it) and never logs it. When it expires, the cached theme keeps
  working across restarts; deploying a new release (a new pin) needs a new token.
- **Archives** are unpacked in memory. Plain files and folders only: links, devices, absolute paths and
  paths leaving the archive are refused, and so is anything over the theme limits. One top-level
  folder (`my-theme/theme.json`) is stripped; hidden files (`.git`, `.DS_Store`) are left out.

**Pack.** `mosaicast-edge pack-theme [-o theme.tgz] <dir>` validates the folder, writes the archive the
edge reads (hidden files left out, licenses included) and prints its `sha256:`. The same folder always
packs to the same bytes, so a pin can be checked by packing again.

**Release.** [`dev/templates/release-theme.yml`](../dev/templates/release-theme.yml) is a workflow for a
theme's own repository: it runs `check-theme` on every push and pull request, and when a GitHub Release
is published it checks that the tag equals `theme.json`'s `version`, packs the theme, attaches
`theme.tgz` to the release and adds the line to put in `EDGE_THEME` to the release notes.

The edge validates the theme at startup and **refuses to start with a broken one**, so a bad theme never
replaces a working page. The log line `mosaicast-edge up` names the theme and where it came from.
Restart the edge to pick up a new version (with a download: change the tag and pin). To leave a theme
behind, unset the variable: every page falls back to the edge's own.

**Compatibility.** `schema` versions the contract in this document. Additions (a new event, a new field,
a new `theme.json` key) keep schema 1, and a theme written for schema 1 keeps working on every later edge.
The other direction is stricter: `theme.json` keys an edge does not know are an error, so a theme that
uses a key added in a newer edge fails on an older one, and says so:
`unknown key "foo" (misspelt, or this theme needs a newer mosaicast-edge than 0.1.0)`. A change that could
break existing themes gets a new schema number, and the edge says so at startup.

## The example theme

[`examples/minimal`](../examples/minimal) is a complete, small theme, validated by this repository's CI:

- a **full** countdown with its own markup, built on the site's colour tokens, a cue (`almost`) for the
  last three seconds, and a script that reacts to `edge:party`
- **restyled** maintenance and closed pages that only change a font and the corner radius
- English and German texts, one of them overriding an edge text (`maintenance.lead`)
- credits

Copy it, run the preview, and change one thing at a time.
