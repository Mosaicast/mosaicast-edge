// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Mosaicast/mosaicast-edge/internal/core"
	"github.com/Mosaicast/mosaicast-edge/internal/gate"
	"github.com/Mosaicast/mosaicast-edge/internal/pages"
	"github.com/Mosaicast/mosaicast-edge/internal/policy"
	"github.com/Mosaicast/mosaicast-edge/internal/sitecopy"
)

// preview runs the real edge against a fake core held in memory, so a theme author can see every page
// and state without a Mosaicast install: `mosaicast-edge preview --theme ./my-theme`, then open
// /_preview. The theme is reloaded whenever a file in it changes. Nothing here exists in `serve`.
func preview(args []string) int {
	fl := flag.NewFlagSet("preview", flag.ContinueOnError)
	themeDir := fl.String("theme", "", "theme folder to preview (default: the edge's own look)")
	listen := fl.String("listen", "127.0.0.1:8090", "address to serve on")
	tz := fl.String("timezone", "Europe/Berlin", "time zone for dates rendered without JavaScript")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	loc, err := time.LoadLocation(*tz)
	if err != nil {
		fmt.Fprintln(os.Stderr, "preview:", err)
		return 2
	}

	fc := newPreviewCore()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "preview:", err)
		return 1
	}
	go func() { _ = http.Serve(ln, fc) }()
	upstream, _ := url.Parse("http://" + ln.Addr().String())

	p := &previewer{core: fc, upstream: upstream, themeDir: *themeDir, loc: loc}
	p.client = core.NewClient(upstream, "", "preview")
	p.store = policy.NewStore(time.Now, "")
	p.keeper = sitecopy.NewKeeper(p.client, edgeLocales, time.Now, "")
	p.poller = &core.Poller{Client: p.client, Store: p.store, Interval: time.Second}
	p.sync()
	if err := p.reload(); err != nil {
		fmt.Fprintf(os.Stderr, "theme is not valid (fix it; the preview reloads on save):\n%v\n", err)
	}
	go func() {
		for range time.Tick(time.Second) {
			p.poller.Poll(context.Background())
		}
	}()

	fmt.Printf("preview on http://%s/_preview  (theme: %s)\n", *listen, orDefault(*themeDir))
	srv := &http.Server{Addr: *listen, Handler: p, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "preview:", err)
		return 1
	}
	return 0
}

func orDefault(s string) string {
	if s == "" {
		return "the edge's default look"
	}
	return s
}

type previewer struct {
	core     *previewCore
	upstream *url.URL
	themeDir string
	loc      *time.Location
	client   *core.Client
	store    *policy.Store
	keeper   *sitecopy.Keeper
	poller   *core.Poller

	mu      sync.Mutex
	gate    *gate.Gate
	stamp   string // theme folder's file times, to notice edits
	loadErr error
	waiting gate.Waiting // the count the pages show; nobody really waits on a preview
}

// sync pulls the fake core's current state into the edge right away, instead of waiting for a poll.
func (p *previewer) sync() {
	p.poller.Poll(context.Background())
	p.keeper.Refresh(context.Background())
}

// reload rebuilds the gate with the theme as it is on disk now.
func (p *previewer) reload() error {
	def, ext, err := loadThemes(p.themeDir)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stamp = p.dirStamp()
	p.loadErr = err
	if err != nil {
		return err
	}
	roles := &core.Roles{Client: p.client, TTL: time.Second, NegativeTTL: time.Second, Max: 100,
		Timeout: 2 * time.Second, PerClient: 1000, InFlight: 8, Now: time.Now}
	r := pages.New(pages.Options{Default: def, Theme: ext, Site: p.keeper, View: p.store.View, Now: time.Now, Location: p.loc})
	p.gate = gate.New(gate.Options{Upstream: p.upstream, Store: p.store, Roles: roles, Pages: r, Now: time.Now,
		Waiting: func() gate.Waiting { p.mu.Lock(); defer p.mu.Unlock(); return p.waiting }})
	return nil
}

func (p *previewer) dirStamp() string {
	if p.themeDir == "" {
		return ""
	}
	var b strings.Builder
	_ = fs.WalkDir(os.DirFS(p.themeDir), ".", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				fmt.Fprintf(&b, "%s:%d:%d;", path, info.Size(), info.ModTime().UnixNano())
			}
		}
		return nil
	})
	return b.String()
}

func (p *previewer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/_preview") {
		p.control(w, r)
		return
	}
	if p.themeDir != "" && p.dirStamp() != p.stamp {
		if err := p.reload(); err != nil {
			fmt.Fprintf(os.Stderr, "theme is not valid:\n%v\n", err)
		} else {
			fmt.Println("theme reloaded")
		}
	}
	p.mu.Lock()
	g, err := p.gate, p.loadErr
	p.mu.Unlock()
	if err != nil || g == nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "The theme is not valid. Fix it and reload this page.\n\n%v\n", err)
		return
	}
	g.ServeHTTP(w, r)
}

// previewSession is a cookie shaped like core's, which the fake core knows as the chosen viewer role.
var previewSession = base64.StdEncoding.EncodeToString([]byte("00000000-0000-4000-8000-000000000001"))

// control is /_preview: the list of states, and /_preview/set to switch to one.
func (p *previewer) control(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/_preview/set" {
		q := r.URL.Query()
		p.core.set(q, p.loc)
		if n, err := strconv.ParseInt(q.Get("waiting"), 10, 64); err == nil && n >= 0 {
			p.mu.Lock()
			p.waiting = gate.Waiting{N: n, Capped: q.Get("capped") == "1"}
			p.mu.Unlock()
		}
		if v := q.Get("viewer"); v != "" {
			if v == "none" {
				http.SetCookie(w, &http.Cookie{Name: core.SessionCookie, Value: "", Path: "/", MaxAge: -1})
			} else {
				http.SetCookie(w, &http.Cookie{Name: core.SessionCookie, Value: previewSession, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
			}
		}
		p.sync()
		if q.Get("down") == "1" {
			p.core.goDown()
			p.store.CoreDown()
		}
		to := q.Get("to")
		if to == "" || !strings.HasPrefix(to, "/") || strings.HasPrefix(to, "//") {
			to = "/"
		}
		http.Redirect(w, r, to, http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = previewIndex.Execute(w, map[string]any{"Theme": orDefault(p.themeDir), "Err": p.loadErr})
}

var previewIndex = template.Must(template.New("").Parse(`<!doctype html><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>edge preview</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:46rem;margin:2rem auto;padding:0 16px}
h2{margin-top:2rem;font-size:1.1rem}a{display:inline-block;margin:.15rem .6rem .15rem 0}code{background:#eee;padding:0 .3em}
.err{background:#fee;border:1px solid #c00;padding:.5rem 1rem;white-space:pre-wrap}</style>
<h1>mosaicast-edge preview</h1>
<p>Theme: <code>{{.Theme}}</code>. Edits to the theme are picked up on the next page load. Light and dark
follow your system setting, or force one with the mode links. A fake core plays the site; nothing leaves
this machine.</p>
{{if .Err}}<p class="err">{{.Err}}</p>{{end}}
<h2>Countdown</h2>
<a href="/_preview/set?state=scheduled&in=273906">wait (3 days)</a>
<a href="/_preview/set?state=scheduled&in=75">wait (75 s)</a>
<a href="/_preview/set?state=scheduled&in=20">launch in 20 s</a>
<a href="/_preview/set?state=scheduled&in=12">open now (12 s: final, party, handoff)</a>
<a href="/_preview/set?state=scheduled&in=3600&viewer=fan">wait, signed in as a fan</a>
<a href="/_preview/set?state=scheduled&in=3600&staff=0">wait, no staff link</a>
<h2>Other pages</h2>
<a href="/_preview/set?state=maintenance&ends=7200">maintenance (with end)</a>
<a href="/_preview/set?state=maintenance">maintenance (open-ended)</a>
<a href="/_preview/set?state=closed">closed</a>
<a href="/_preview/set?state=closed&viewer=fan">closed, signed in as a fan</a>
<a href="/_preview/set?state=open&down=1">unavailable (core down)</a>
<a href="/_preview/set?state=open">open (the site itself)</a>
<a href="/_preview/set?to=/_a/login">staff sign-in</a>
<a href="/_preview/set?to=/_a/login%3Ferror%3Dsignups_closed">sign-in with an error</a>
<a href="/_preview/set?providers=0&to=/_a/login">sign-in, no providers</a>
<a href="/_preview/set?to=/_a/legal/imprint">legal page</a>
<h2>Waiting count</h2>
<p>Pages show it with <code>data-edge-count="waiting"</code>. It reaches open pages with the next state
answer, so reload to see it at once.</p>
<a href="/_preview/set?waiting=0&to=/_preview">0</a>
<a href="/_preview/set?waiting=1&to=/_preview">1</a>
<a href="/_preview/set?waiting=7&to=/_preview">7</a>
<a href="/_preview/set?waiting=1234&to=/_preview">1,234</a>
<a href="/_preview/set?waiting=2500&capped=1&to=/_preview">2,500+ (at the limit)</a>
<h2>Site</h2>
<a href="/_preview/set?mode=system&to=/_preview">mode: system</a>
<a href="/_preview/set?mode=light&to=/_preview">mode: light</a>
<a href="/_preview/set?mode=dark&to=/_preview">mode: dark</a>
<a href="/_preview/set?viewer=none&to=/_preview">sign out</a>
<p>Language: add <code>?lang=de</code> or <code>?lang=en</code> to any page, or use the switcher.</p>`))

// previewCore is the fake core: just the HTTP API the edge reads (ARCHITECTURE §18.2), in memory.
type previewCore struct {
	mu        sync.Mutex
	access    policy.Access
	rev       int64
	mode      string
	viewer    string // role of previewSession; "" = none
	providers bool
	down      bool
}

func newPreviewCore() *previewCore {
	at := time.Now().Add(3 * 24 * time.Hour)
	return &previewCore{mode: "system", providers: true,
		access: policy.Access{State: policy.Scheduled, OpensAt: &at, PassFloor: policy.RolePodcaster, StaffLogin: policy.StaffLogin{Visible: true}}}
}

func (c *previewCore) set(q url.Values, loc *time.Location) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := q.Get("state"); s != "" {
		c.access = policy.Access{State: s, PassFloor: policy.RolePodcaster, StaffLogin: policy.StaffLogin{Visible: q.Get("staff") != "0"}}
		if secs, err := strconv.Atoi(q.Get("in")); err == nil {
			at := time.Now().Add(time.Duration(secs) * time.Second)
			c.access.OpensAt = &at
		}
		if secs, err := strconv.Atoi(q.Get("ends")); err == nil {
			at := time.Now().Add(time.Duration(secs) * time.Second)
			c.access.EndsAt = &at
		}
		c.down = false
		c.viewer = ""
	}
	switch v := q.Get("viewer"); v {
	case "none":
		c.viewer = ""
	case "":
	default:
		c.viewer = v
	}
	if m := q.Get("mode"); m != "" {
		c.mode = m
	}
	if p := q.Get("providers"); p != "" {
		c.providers = p != "0"
	}
	c.rev++
}

func (c *previewCore) goDown() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.down = true
}

func (c *previewCore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down { // gone entirely, like a core that is restarting
		hj, ok := w.(http.Hijacker)
		if ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
				return
			}
		}
	}
	js := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case r.URL.Path == "/api/site/access":
		a := c.access
		a.Revision = c.rev
		if c.providers {
			a.StaffLogin.Providers = []policy.Provider{{ID: "discord", Path: "/oauth2/authorization/discord"}}
		}
		js(a)
	case r.URL.Path == "/api/me":
		ck, err := r.Cookie(core.SessionCookie)
		if err != nil || ck.Value != previewSession || c.viewer == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		js(map[string]string{"role": c.viewer})
	case r.URL.Path == "/api/site":
		js(map[string]any{"name": "Example Podcast", "modePolicy": c.mode, "defaultLocale": "en",
			"theme": map[string]any{
				"light": map[string]string{"bg": "#faf8f5", "surface": "#ffffff", "text": "#1f1c18", "textMuted": "#615b52", "accent": "#c2410c", "accentContrast": "#ffffff", "border": "#e2ddd5", "accent2": "#0f766e", "accentText": "#b13c0b"},
				"dark":  map[string]string{"bg": "#14120f", "surface": "#1f1c18", "text": "#efebe4", "textMuted": "#aaa398", "accent": "#c2410c", "accentContrast": "#ffffff", "border": "#36322c", "accent2": "#2dd4bf", "accentText": "#f08a5d"}},
			"branding": map[string]any{"logo": "/branding/logo", "favicon": "/branding/favicon", "darkLogo": nil}})
	case r.URL.Path == "/branding/logo" || r.URL.Path == "/branding/favicon":
		w.Header().Set("Content-Type", "image/svg+xml")
		fmt.Fprint(w, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 220 48"><rect x="2" y="6" width="36" height="36" rx="9" fill="#c2410c"/><text x="50" y="34" font-family="sans-serif" font-size="26" font-weight="700" fill="#888">Example</text></svg>`)
	case r.URL.Path == "/api/legal":
		js([]map[string]any{{"slug": "imprint", "title": "Imprint", "role": "imprint"}, {"slug": "privacy", "title": "Privacy", "role": "privacy"}})
	case strings.HasPrefix(r.URL.Path, "/api/legal/"):
		slug := strings.TrimPrefix(r.URL.Path, "/api/legal/")
		js(map[string]any{"slug": slug, "title": strings.ToUpper(slug[:1]) + slug[1:], "role": slug,
			"html": "<p>This is where the operator's " + template.HTMLEscapeString(slug) + " text appears, rendered by core.</p><h2>A heading</h2><p>And a paragraph.</p>"})
	case strings.HasPrefix(r.URL.Path, "/oauth2/"):
		http.Redirect(w, r, "/?login_error=access_denied", http.StatusFound)
	default:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><title>The site</title><body style="font:20px system-ui;padding:3rem">This is the site itself (the fake core). <a href="/_preview">Back to the preview</a>`)
	}
}
