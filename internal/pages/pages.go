// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

// Package pages renders the edge's pages from the default theme and an optional external theme, and
// injects the runtime into each of them (docs/THEMING.md).
package pages

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Mosaicast/mosaicast-edge/internal/gate"
	"github.com/Mosaicast/mosaicast-edge/internal/policy"
	"github.com/Mosaicast/mosaicast-edge/internal/sitecopy"
	"github.com/Mosaicast/mosaicast-edge/internal/theme"
)

// FinalSeconds is the length of the countdown's final phase. Core's "open now" schedules the opening
// twelve seconds ahead so that these ten still play (§18.1).
const FinalSeconds = 10

// DownLongAfter is when an outage stops being "back in a moment" and becomes "taking longer".
const DownLongAfter = 60 * time.Second

// SiteSource is sitecopy.Keeper in production.
type SiteSource interface{ Get() *sitecopy.Copy }

// Options configure a Renderer.
type Options struct {
	Default   *theme.Theme // the embedded default theme, providing every page in full
	Theme     *theme.Theme // the operator's theme, or nil
	Site      SiteSource
	View      func() policy.View
	Now       func() time.Time
	Location  *time.Location // for dates rendered without JavaScript
	PublicURL string         // absolute origin for share tags and the calendar; "" = from the request
	// Strict makes a missing text key an error instead of the key itself. check-theme and startup
	// validation render strictly; live pages never fail over a text.
	Strict bool
}

// Renderer implements gate.Renderer.
type Renderer struct {
	Options
	siteAssets *siteAssetCache
}

// New builds a renderer.
func New(o Options) *Renderer {
	return &Renderer{Options: o, siteAssets: &siteAssetCache{}}
}

var _ gate.Renderer = (*Renderer)(nil)

// layer is one source of texts, consulted in order.
type layer map[string]string

// plan is how one page view is put together.
type plan struct {
	tmpl       *theme.Theme // whose templates render the page
	entry      string       // template name
	ext        *theme.Theme // the operator theme when it touches this page (full or restyle), else nil
	mode       string       // "", restyle, full
	candidates []string     // languages the page can be shown in
}

func (rd *Renderer) plan(page string) plan {
	switch rd.Theme.Mode(page) {
	case theme.ModeFull:
		return plan{tmpl: rd.Theme, entry: rd.Theme.Pages[page].Template, ext: rd.Theme, mode: theme.ModeFull, candidates: rd.Theme.Locales}
	case theme.ModeRestyle:
		return plan{tmpl: rd.Default, entry: rd.Default.Pages[page].Template, ext: rd.Theme, mode: theme.ModeRestyle, candidates: rd.Default.Locales}
	default:
		return plan{tmpl: rd.Default, entry: rd.Default.Pages[page].Template, candidates: rd.Default.Locales}
	}
}

// layers returns the text layers for lang: the theme's (full or restyle), then the edge's.
func (rd *Renderer) layers(p plan, lang string) []layer {
	var ls []layer
	if p.ext != nil && len(p.ext.Texts) > 0 {
		if tx, ok := p.ext.Texts[lang]; ok {
			ls = append(ls, tx)
		} else {
			// a theme that does not speak lang falls back to its own first language, not to edge text
			ls = append(ls, p.ext.Texts[p.ext.Locales[0]])
		}
	}
	if tx, ok := rd.Default.Texts[lang]; ok {
		ls = append(ls, tx)
	}
	return append(ls, rd.Default.Texts["en"])
}

// Render renders one page view.
func (rd *Renderer) Render(r *http.Request, req gate.Request) ([]byte, bool, error) {
	cp := rd.Site.Get()
	p := rd.plan(req.Page)
	lang := pickLang(r, p.candidates, cp.Site.DefaultLocale)
	d := rd.data(r, req, p, lang, cp)
	found := true
	if req.Page == gate.PageLegal && d.LegalPage == nil {
		found = false
	}
	body, err := rd.execute(p, d)
	return body, found, err
}

func (rd *Renderer) execute(p plan, d *Data) ([]byte, error) {
	var buf bytes.Buffer
	if err := p.tmpl.Templates.ExecuteTemplate(&buf, p.entry, d); err != nil {
		return nil, err
	}
	return rd.enforce(buf.Bytes(), d)
}

var (
	htmlOpen = regexp.MustCompile(`(?i)<html\b`)
	headOpen = regexp.MustCompile(`(?i)<head\b[^>]*>`)
	bodyEnd  = regexp.MustCompile(`(?i)</body\s*>`)
)

// enforce puts in whatever the template left out: the page attributes on <html>, the edge's head (runtime,
// tokens, data), and the legal links, staff sign-in and credits. A theme can place these; it cannot drop
// them. Done on the server, so it holds without JavaScript.
func (rd *Renderer) enforce(out []byte, d *Data) ([]byte, error) {
	if !d.used["attrs"] {
		loc := htmlOpen.FindIndex(out)
		if loc == nil {
			return nil, errors.New("template has no <html> element")
		}
		out = splice(out, loc[1], []byte(" "+string(d.HTMLAttrs())))
	}
	if !d.used["head"] {
		loc := headOpen.FindIndex(out)
		if loc == nil {
			return nil, errors.New("template has no <head> element")
		}
		head, err := d.EdgeHead()
		if err != nil {
			return nil, err
		}
		out = splice(out, loc[1], []byte(head))
	}
	var foot strings.Builder
	for _, part := range []struct {
		key string
		f   func() (template.HTML, error)
	}{{"legal", d.EdgeLegal}, {"login", d.EdgeLogin}, {"credits", d.EdgeCredits}} {
		if d.used[part.key] {
			continue
		}
		h, err := part.f()
		if err != nil {
			return nil, err
		}
		foot.WriteString(string(h))
	}
	if foot.Len() > 0 {
		all := bodyEnd.FindAllIndex(out, -1)
		if all == nil {
			return nil, errors.New("template has no </body>")
		}
		at := all[len(all)-1][0]
		out = splice(out, at, []byte(`<footer class="edge-footer" data-edge-footer>`+foot.String()+`</footer>`))
	}
	return out, nil
}

func splice(b []byte, at int, ins []byte) []byte {
	out := make([]byte, 0, len(b)+len(ins))
	out = append(out, b[:at]...)
	out = append(out, ins...)
	return append(out, b[at:]...)
}

// baseURL is the configured public origin, or what the TLS terminator says it is.
func (rd *Renderer) baseURL(r *http.Request) string {
	if rd.PublicURL != "" {
		return rd.PublicURL
	}
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto != "https" && proto != "http" {
		proto = "http"
		if r.TLS != nil {
			proto = "https"
		}
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	if !validHost.MatchString(host) {
		host = "localhost"
	}
	return proto + "://" + host
}

var validHost = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]{1,5})?$|^\[[0-9A-Fa-f:.]+\](:[0-9]{1,5})?$`)

// Validate dry-renders every page the theme touches, in every language and phase, with a fixture
// site. Any template error, unknown asset or text key, or missing <html>/<head>/</body> is reported.
func Validate(def, ext *theme.Theme, now time.Time) error {
	opensAt := now.Add(3*24*time.Hour + 4*time.Hour + 5*time.Minute + 6*time.Second)
	endsAt := now.Add(2 * time.Hour)
	down := now.Add(-90 * time.Second)
	fx := fixtureCopy()
	var errs []error
	pagesToCheck := theme.Pages
	for _, page := range pagesToCheck {
		if ext != nil && ext.Mode(page) == "" {
			continue // the default page alone is the edge's business, tested in its own suite
		}
		rd := New(Options{Default: def, Theme: ext, Site: staticSite{fx}, Now: func() time.Time { return now },
			View: func() policy.View { return policy.View{} }, Location: time.UTC, Strict: true})
		p := rd.plan(page)
		for _, lang := range p.candidates {
			for _, v := range fixtureViews(page, now, opensAt, endsAt, down) {
				r, _ := http.NewRequest(http.MethodGet, "http://edge.test/?lang="+lang, nil)
				req := gate.Request{Page: page, View: v.view, Viewer: v.viewer, LoginError: v.loginError, LegalSlug: v.slug}
				if _, _, err := rd.Render(r, req); err != nil {
					errs = append(errs, fmt.Errorf("page %s, %s, %s: %w", page, lang, v.name, err))
				}
			}
		}
	}
	return errors.Join(errs...)
}

type staticSite struct{ c *sitecopy.Copy }

func (s staticSite) Get() *sitecopy.Copy { return s.c }

type fixtureView struct {
	name       string
	view       policy.View
	viewer     *gate.Viewer
	loginError string
	slug       string
}

func fixtureViews(page string, now, opensAt, endsAt, down time.Time) []fixtureView {
	providers := []policy.Provider{{ID: "discord", Path: "/oauth2/authorization/discord"}}
	acc := func(state string, at, end *time.Time, visible bool) *policy.Access {
		return &policy.Access{State: state, OpensAt: at, EndsAt: end, PassFloor: policy.RolePodcaster,
			StaffLogin: policy.StaffLogin{Visible: visible, Providers: providers}}
	}
	final := now.Add(7 * time.Second)
	switch page {
	case gate.PageCountdown:
		return []fixtureView{
			{name: "wait", view: policy.View{Page: policy.Scheduled, Access: acc(policy.Scheduled, &opensAt, nil, true), CoreUp: true}},
			{name: "final", view: policy.View{Page: policy.Scheduled, Access: acc(policy.Scheduled, &final, nil, false), CoreUp: true}},
			{name: "wait, fan signed in", view: policy.View{Page: policy.Scheduled, Access: acc(policy.Scheduled, &opensAt, nil, false)}, viewer: &gate.Viewer{SignedIn: true, Role: policy.RoleFan}},
		}
	case gate.PageMaintenance:
		return []fixtureView{
			{name: "with end", view: policy.View{Page: policy.Maintenance, Access: acc(policy.Maintenance, nil, &endsAt, true), CoreUp: true}},
			{name: "open-ended", view: policy.View{Page: policy.Maintenance, Access: acc(policy.Maintenance, nil, nil, false), CoreUp: true}},
		}
	case gate.PageClosed:
		return []fixtureView{
			{name: "closed", view: policy.View{Page: policy.Closed, Access: acc(policy.Closed, nil, nil, true), CoreUp: true}},
			{name: "closed, fan signed in", view: policy.View{Page: policy.Closed, Access: acc(policy.Closed, nil, nil, false)}, viewer: &gate.Viewer{SignedIn: true, Role: policy.RoleFan}},
		}
	case gate.PageUnavailable:
		long := now.Add(-10 * time.Minute)
		return []fixtureView{
			{name: "just down", view: policy.View{Page: policy.Open, Access: acc(policy.Open, nil, nil, false), DownSince: &down}},
			{name: "down a while", view: policy.View{Page: policy.Open, Access: acc(policy.Open, nil, nil, false), DownSince: &long}},
			{name: "no policy known", view: policy.View{Page: policy.Unavailable}},
		}
	case gate.PageLogin:
		v := policy.View{Page: policy.Scheduled, Access: acc(policy.Scheduled, &opensAt, nil, true), CoreUp: true}
		none := policy.View{Page: policy.Closed, Access: &policy.Access{State: policy.Closed, PassFloor: policy.RolePodcaster}}
		return []fixtureView{
			{name: "providers", view: v},
			{name: "error signups_closed", view: v, loginError: "signups_closed"},
			{name: "unknown error", view: v, loginError: "something_new"},
			{name: "no providers", view: none},
		}
	case gate.PageLegal:
		v := policy.View{Page: policy.Scheduled, Access: acc(policy.Scheduled, &opensAt, nil, true), CoreUp: true}
		return []fixtureView{{name: "imprint", view: v, slug: "imprint"}, {name: "missing", view: v, slug: "nope"}}
	}
	return nil
}

func fixtureCopy() *sitecopy.Copy {
	light := map[string]string{"bg": "#fafaf7", "surface": "#ffffff", "text": "#1c1b19", "text-muted": "#5f5d58",
		"accent": "#cc9720", "accent-contrast": "#1c1b19", "border": "#deddd8", "accent-2": "#2a6f97", "accent-text": "#8a6512"}
	dark := map[string]string{"bg": "#141311", "surface": "#1e1d1a", "text": "#ecebe6", "text-muted": "#a8a69f",
		"accent": "#cc9720", "accent-contrast": "#1c1b19", "border": "#34322e", "accent-2": "#6fb0d6", "accent-text": "#e0b04a"}
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect width="10" height="10"/></svg>`)
	pages := sitecopy.PageCache{
		"imprint": {Title: "Imprint", Role: "imprint", HTML: "<p>Example operator, Example street 1</p>"},
		"privacy": {Title: "Privacy", Role: "privacy", HTML: "<p>We keep nothing.</p>"},
	}
	entries := []sitecopy.Entry{{Slug: "imprint", Title: "Imprint", Role: "imprint"}, {Slug: "privacy", Title: "Privacy", Role: "privacy"}}
	return &sitecopy.Copy{
		Site:    sitecopy.Site{Name: "Example Podcast", ModePolicy: "system", DefaultLocale: "en", Light: light, Dark: dark},
		Logo:    &sitecopy.Image{Type: "image/svg+xml", Body: svg},
		Favicon: &sitecopy.Image{Type: "image/svg+xml", Body: svg},
		Legal:   map[string][]sitecopy.Entry{"en": entries, "de": entries},
		Pages:   map[string]sitecopy.PageCache{"en": pages, "de": pages},
	}
}
