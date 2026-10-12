// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

package pages

import (
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

// Data is what a page template sees as {{.}}. Its fields and methods are the template half of the
// theme contract (THEMING.md, "Template data"); renaming one breaks themes.
type Data struct {
	Page      string   // countdown | maintenance | closed | unavailable | login | legal
	Phase     string   // countdown only: wait | final (the runtime moves on to party and handoff)
	Lang      string   // the language this page is rendered in
	Locales   []string // the languages it can switch to
	Site      SiteData
	OpensAt   *time.Time // countdown: the moment the site opens
	EndsAt    *time.Time // maintenance: the announced end, shown and never acted on
	When      string     // OpensAt formatted for readers without JavaScript
	Until     string     // EndsAt formatted likewise
	Countdown Parts      // OpensAt minus now, server-rendered so the page is right before any script runs
	Viewer    *gate.Viewer
	DownSince *time.Time // unavailable: start of the outage
	DownLong  bool       // unavailable: the outage is older than a minute
	Login     LoginData
	LegalPage *LegalPageData // legal: the page, nil when the slug is unknown
	Legal     []Link         // the legal links every page carries
	Credits   []theme.Credit
	Calendar  string // countdown: link to a calendar entry for the opening

	rd       *Renderer
	siteName string // for {site} in texts
	whenText string // for {when} in texts: the opening (or the end of maintenance), "" when none
	plan     plan
	layers   []layer
	r        *http.Request
	cp       *sitecopy.Copy
	req      gate.Request
	used     map[string]bool
	runtime  runtimeConfig
}

// SiteData is the site as the edge's copy knows it.
type SiteData struct {
	Name     string
	Logo     string // URL, "" when the site has none
	DarkLogo string // URL, "" when the site has none
}

// Parts is a remaining duration split for display; D/H/M/S are zero-padded to two digits.
type Parts struct {
	Days, Hours, Minutes, Seconds int
	D, H, M, S                    string
	Total                         int // whole seconds
}

// LoginData is the staff sign-in page.
type LoginData struct {
	Providers []ProviderLink
	Error     string // the code core sent, "" when none
	ErrorText string // the text for it in the page language
}

// ProviderLink starts one provider's sign-in flow.
type ProviderLink struct {
	ID, Name, Href string
}

// LegalPageData is one legal page from the site copy.
type LegalPageData struct {
	Slug, Title, Role string
	HTML              template.HTML // core's sanitized rendering
}

// Link is a legal link.
type Link struct {
	Title, Href, Role string
}

var codeRe = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

func (rd *Renderer) data(r *http.Request, req gate.Request, p plan, lang string, cp *sitecopy.Copy) *Data {
	now := rd.Now()
	d := &Data{Page: req.Page, Lang: lang, Locales: p.candidates, rd: rd, plan: p, r: r, cp: cp, req: req,
		used: map[string]bool{}, Viewer: req.Viewer}
	d.layers = rd.layers(p, lang)
	sa := rd.siteAssets.get(cp)
	d.Site = SiteData{Name: cp.Site.Name, Logo: sa.logo, DarkLogo: sa.darkLogo}
	if d.Site.Name == "" {
		d.Site.Name = d.text("site.fallbackName")
	}
	d.siteName = d.Site.Name

	v := req.View
	d.whenText = rd.whenFor(v, lang)
	if v.Access != nil {
		if req.Page == gate.PageCountdown && v.Access.OpensAt != nil {
			d.OpensAt = v.Access.OpensAt
			d.When = formatWhen(*d.OpensAt, rd.Location, lang)
			d.Countdown = split(d.OpensAt.Sub(now))
			d.Phase = "wait"
			if d.OpensAt.Sub(now) <= FinalSeconds*time.Second {
				d.Phase = "final"
			}
			d.Calendar = gate.Prefix + "calendar.ics?lang=" + lang
		}
		if req.Page == gate.PageMaintenance && v.Access.EndsAt != nil {
			d.EndsAt = v.Access.EndsAt
			d.Until = formatWhen(*d.EndsAt, rd.Location, lang)
		}
	}
	if req.Page == gate.PageUnavailable {
		d.DownSince = v.DownSince
		d.DownLong = v.DownSince != nil && now.Sub(*v.DownSince) >= DownLongAfter
	}

	// staff sign-in: providers as core configured them, the error core reported
	if v.Access != nil {
		for _, pr := range v.Access.StaffLogin.Providers {
			d.Login.Providers = append(d.Login.Providers, ProviderLink{ID: pr.ID, Href: pr.Path, Name: d.providerName(pr.ID)})
		}
	}
	if code := req.LoginError; code != "" {
		if !codeRe.MatchString(code) {
			code = "failed"
		}
		d.Login.Error = code
		if t, ok := d.lookup("loginError." + code); ok {
			d.Login.ErrorText = t
		} else {
			d.Login.ErrorText = d.text("loginError.generic")
		}
	}

	_, entries := cp.LegalFor(lang)
	marked := false
	for _, e := range entries {
		if e.Role == "imprint" || e.Role == "privacy" {
			marked = true
		}
	}
	for _, e := range entries {
		if !marked || e.Role == "imprint" || e.Role == "privacy" {
			d.Legal = append(d.Legal, Link{Title: e.Title, Href: gate.Prefix + "legal/" + e.Slug + "?lang=" + lang, Role: e.Role})
		}
	}
	if req.Page == gate.PageLegal {
		if pg, ok := cp.Page(req.LegalSlug, lang); ok {
			d.LegalPage = &LegalPageData{Slug: req.LegalSlug, Title: pg.Title, Role: pg.Role, HTML: template.HTML(pg.HTML)}
		}
	}
	if p.ext != nil {
		d.Credits = p.ext.Credits
	}
	d.runtime = rd.runtimeConfig(d, sa, now)
	return d
}

func split(left time.Duration) Parts {
	if left < 0 {
		left = 0
	}
	s := int((left + time.Second - 1) / time.Second) // ceil: 0.4 s left still shows 1
	p := Parts{Days: s / 86400, Hours: s / 3600 % 24, Minutes: s / 60 % 60, Seconds: s % 60, Total: s}
	p.D, p.H, p.M, p.S = fmt.Sprintf("%02d", p.Days), fmt.Sprintf("%02d", p.Hours), fmt.Sprintf("%02d", p.Minutes), fmt.Sprintf("%02d", p.Seconds)
	return p
}

func (d *Data) providerName(id string) string {
	if t, ok := d.lookup("login.provider." + id); ok {
		return t
	}
	if id == "" {
		return id
	}
	return strings.ToUpper(id[:1]) + id[1:]
}

func (d *Data) lookup(key string) (string, bool) {
	for _, l := range d.layers {
		if t, ok := l[key]; ok {
			return fill(t, d.siteName, d.whenText), true
		}
	}
	return "", false
}

func (d *Data) text(key string) string {
	t, _ := d.lookup(key)
	return t
}

// T is the text for key in the page language: the theme's text if it has one, else the edge's.
// {site} in a text becomes the site name, {when} the opening (see fill).
func (d *Data) T(key string) (string, error) {
	if t, ok := d.lookup(key); ok {
		return t, nil
	}
	if d.rd.Strict {
		return "", fmt.Errorf("unknown text key %q", key)
	}
	return key, nil
}

// Plural picks key.one for n == 1 and key.other otherwise.
func (d *Data) Plural(key string, n int) (string, error) {
	if n == 1 {
		return d.T(key + ".one")
	}
	return d.T(key + ".other")
}

// Asset is the URL of a file under the rendering theme's assets/.
func (d *Data) Asset(p string) (string, error) {
	return d.plan.tmpl.Assets.URL(p)
}

// HTMLAttrs are the attributes the edge puts on <html>: lang, data-page, data-phase, data-mode.
func (d *Data) HTMLAttrs() template.HTMLAttr {
	d.used["attrs"] = true
	var b strings.Builder
	fmt.Fprintf(&b, `lang="%s" data-page="%s" data-mode="%s"`, template.HTMLEscapeString(d.Lang), d.Page, template.HTMLEscapeString(modeOf(d.cp)))
	if d.Phase != "" {
		fmt.Fprintf(&b, ` data-phase="%s"`, d.Phase)
	}
	if d.Page == gate.PageUnavailable {
		if d.DownLong {
			b.WriteString(` data-down="long"`)
		} else {
			b.WriteString(` data-down="short"`)
		}
	}
	if d.Viewer != nil && d.Viewer.SignedIn {
		b.WriteString(` data-viewer="signed-in"`)
	}
	return template.HTMLAttr(b.String())
}

func modeOf(cp *sitecopy.Copy) string {
	if cp.Site.ModePolicy == "" {
		return "system"
	}
	return cp.Site.ModePolicy
}

// EdgeHead is the edge's part of <head>: charset, viewport, favicon, share tags, the site's tokens,
// the base styles, the runtime and its data, and the theme's stylesheets and scripts.
func (d *Data) EdgeHead() (template.HTML, error) {
	d.used["head"] = true
	rd, p := d.rd, d.plan
	h := headData{Data: d}
	var err error
	if h.Runtime, err = rd.Default.Assets.URL("edge.js"); err != nil {
		return "", err
	}
	if h.Base, err = rd.Default.Assets.URL("base.css"); err != nil {
		return "", err
	}
	sa := rd.siteAssets.get(d.cp)
	h.Tokens = sa.css
	if p.mode != theme.ModeFull {
		if h.Default, err = rd.Default.Assets.URL("edge.css"); err != nil {
			return "", err
		}
	}
	h.Favicon = sa.favicon
	if p.ext != nil {
		if p.ext.Favicon != "" {
			h.Favicon, _ = p.ext.Assets.URL(p.ext.Favicon)
		}
		for _, s := range p.ext.Styles {
			u, _ := p.ext.Assets.URL(s)
			h.Styles = append(h.Styles, u)
		}
		for _, s := range p.ext.Scripts {
			u, _ := p.ext.Assets.URL(s)
			h.Scripts = append(h.Scripts, u)
		}
	}
	if h.Favicon == "" {
		h.Favicon, _ = rd.Default.Assets.URL("favicon.svg")
	}
	if rd.Theme != nil && rd.Theme.Share.Image != "" {
		u, _ := rd.Theme.Assets.URL(rd.Theme.Share.Image)
		h.ShareImage = rd.baseURL(d.r) + u
	}
	h.URL = rd.baseURL(d.r) + "/"
	h.Title = d.text("title")
	h.Description = d.text("description")
	h.Config = d.runtime
	return render("head", h)
}

type headData struct {
	*Data
	Runtime, Base, Tokens, Default, Favicon string
	Styles, Scripts                         []string
	ShareImage, URL, Title, Description     string
	Config                                  runtimeConfig
}

// EdgeLegal is the list of legal links (imprint, privacy). German law wants them one click away from
// any page, a countdown included.
func (d *Data) EdgeLegal() (template.HTML, error) {
	d.used["legal"] = true
	return render("legal", d)
}

// EdgeLogin is the staff sign-in link, empty unless the policy makes it visible.
func (d *Data) EdgeLogin() (template.HTML, error) {
	d.used["login"] = true
	if d.req.View.Access == nil || !d.req.View.Access.StaffLogin.Visible || d.Page == gate.PageLogin {
		return `<span hidden data-edge-login></span>`, nil
	}
	return render("login", d)
}

// EdgeCredits is the theme's attribution list, empty without a theme.
func (d *Data) EdgeCredits() (template.HTML, error) {
	d.used["credits"] = true
	return render("credits", d)
}

// EdgeLangs is a language switcher (links with ?lang=, upgraded in place by the runtime). Optional.
func (d *Data) EdgeLangs() (template.HTML, error) {
	if len(d.Locales) < 2 {
		return "", nil
	}
	return render("langs", d)
}

// runtimeConfig is the JSON the runtime reads from <script id="edge-data">. It is the data half of
// the theme contract (THEMING.md, "Runtime data").
type runtimeConfig struct {
	Page      string                       `json:"page"`
	Lang      string                       `json:"lang"`
	Locales   []string                     `json:"locales"`
	Now       int64                        `json:"now"`
	OpensAt   *int64                       `json:"opensAt"`
	EndsAt    *int64                       `json:"endsAt"`
	DownSince *int64                       `json:"downSince"`
	Rev       uint64                       `json:"rev"`
	Viewer    *gate.Viewer                 `json:"viewer"`
	Site      runtimeSite                  `json:"site"`
	Countdown *runtimeCountdown            `json:"countdown,omitempty"`
	Texts     map[string]map[string]string `json:"texts"`
}

type runtimeSite struct {
	Name string `json:"name"`
}

type runtimeCountdown struct {
	FinalMs   int64            `json:"finalMs"`
	PartyMs   int64            `json:"partyMs"`
	HandoffMs int64            `json:"handoffMs"`
	PreloadMs int64            `json:"preloadMs"`
	Cues      map[string]int64 `json:"cues"`
}

func (rd *Renderer) runtimeConfig(d *Data, sa *siteAssetSet, now time.Time) runtimeConfig {
	v := d.req.View
	c := runtimeConfig{Page: d.Page, Lang: d.Lang, Locales: d.Locales, Now: now.UnixMilli(), Rev: v.Seq,
		Viewer: d.Viewer, Site: runtimeSite{Name: d.Site.Name}, Texts: map[string]map[string]string{}}
	c.OpensAt, c.EndsAt = ms(d.OpensAt), ms(d.EndsAt)
	c.DownSince = ms(d.DownSince)
	if d.Page == gate.PageCountdown {
		t := rd.Default
		if d.plan.ext != nil {
			t = d.plan.ext
		}
		cd := &runtimeCountdown{FinalMs: FinalSeconds * 1000, PartyMs: int64(t.PartySeconds) * 1000,
			HandoffMs: int64(t.HandoffSeconds * 1000), PreloadMs: int64(t.PreloadSeconds) * 1000, Cues: map[string]int64{}}
		for name, at := range t.Countdown.Cues {
			cd.Cues[name] = int64(at * 1000)
		}
		c.Countdown = cd
	}
	// every text in every language the page can switch to, so the language toggle needs no request
	for _, l := range d.Locales {
		merged := map[string]string{}
		ls := rd.layers(d.plan, l)
		for i := len(ls) - 1; i >= 0; i-- {
			for k, t := range ls[i] {
				merged[k] = fill(t, d.siteName, rd.whenFor(v, l))
			}
		}
		c.Texts[l] = merged
	}
	return c
}

func ms(t *time.Time) *int64 {
	if t == nil {
		return nil
	}
	v := t.UnixMilli()
	return &v
}

// The edge's own fragments. Built with html/template so every value is escaped for where it lands;
// the runtime data in particular is JSON-encoded with <, > and & escaped inside the script element.
var parts = template.Must(template.New("").Parse(`
{{define "head"}}<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">` +
	`<link rel="icon" href="{{.Favicon}}">` +
	`{{if .Description}}<meta name="description" content="{{.Description}}">{{end}}` +
	`<meta property="og:type" content="website"><meta property="og:url" content="{{.URL}}">` +
	`{{if .Title}}<meta property="og:title" content="{{.Title}}">{{end}}` +
	`{{if .Description}}<meta property="og:description" content="{{.Description}}">{{end}}` +
	`{{if .ShareImage}}<meta property="og:image" content="{{.ShareImage}}"><meta name="twitter:card" content="summary_large_image">{{end}}` +
	`<link rel="stylesheet" href="{{.Tokens}}"><link rel="stylesheet" href="{{.Base}}">` +
	`{{if .Default}}<link rel="stylesheet" href="{{.Default}}">{{end}}` +
	`{{range .Styles}}<link rel="stylesheet" href="{{.}}">{{end}}` +
	`<script type="application/json" id="edge-data">{{.Config}}</script>` +
	`<script src="{{.Runtime}}" defer></script>{{range .Scripts}}<script src="{{.}}" defer></script>{{end}}{{end}}
{{define "legal"}}<nav class="edge-legal" data-edge-legal aria-label="{{.T "legal.nav"}}">{{range .Legal}}<a href="{{.Href}}" data-role="{{.Role}}">{{.Title}}</a>{{end}}</nav>{{end}}
{{define "login"}}<a class="edge-login" data-edge-login href="/_a/login?lang={{.Lang}}">{{.T "login.link"}}</a>{{end}}
{{define "credits"}}{{if .Credits}}<p class="edge-credits" data-edge-credits>{{range $i, $c := .Credits}}{{if $i}} · {{end}}` +
	`{{if $c.URL}}<a href="{{$c.URL}}" rel="noopener">{{$c.Work}}</a>{{else}}{{$c.Work}}{{end}}` +
	`{{if $c.Author}} – {{$c.Author}}{{end}}{{if $c.License}} ({{$c.License}}){{end}}{{end}}</p>` +
	`{{else}}<span hidden data-edge-credits></span>{{end}}{{end}}
{{define "langs"}}<nav class="edge-langs" data-edge-langs aria-label="{{.T "lang.nav"}}">{{range .Locales}}` +
	`<a href="?lang={{.}}" data-edge-lang="{{.}}" hreflang="{{.}}" lang="{{.}}"{{if eq . $.Lang}} aria-current="true"{{end}}>{{.}}</a>{{end}}</nav>{{end}}
`))

func render(name string, data any) (template.HTML, error) {
	var b strings.Builder
	if err := parts.ExecuteTemplate(&b, name, data); err != nil {
		return "", err
	}
	return template.HTML(b.String()), nil
}

// Tr is T with named values: {{.Tr "login.with" "provider" .Name}} fills {provider}.
func (d *Data) Tr(key string, pairs ...string) (string, error) {
	t, err := d.T(key)
	if err != nil {
		return "", err
	}
	if len(pairs)%2 != 0 {
		return "", fmt.Errorf("Tr %q: values come in name/value pairs", key)
	}
	for i := 0; i < len(pairs); i += 2 {
		t = strings.ReplaceAll(t, "{"+pairs[i]+"}", pairs[i+1])
	}
	return t, nil
}

// PageTitle is the document title: the page's own "<page>.title" text.
func (d *Data) PageTitle() (string, error) {
	return d.T(d.Page + ".title")
}

// Unit is one countdown unit for templates that loop over them.
type Unit struct {
	Key   string // d | h | m | s, as in data-edge-unit
	Value string // zero-padded
	Label string // singular or plural, in the page language
}

// Units are days, hours, minutes and seconds of the countdown, with their labels.
func (d *Data) Units() ([]Unit, error) {
	c := d.Countdown
	var out []Unit
	for _, u := range []struct {
		key string
		n   int
		v   string
	}{{"d", c.Days, c.D}, {"h", c.Hours, c.H}, {"m", c.Minutes, c.M}, {"s", c.Seconds, c.S}} {
		label, err := d.Plural("unit."+u.key, u.n)
		if err != nil {
			return nil, err
		}
		out = append(out, Unit{Key: u.key, Value: u.v, Label: label})
	}
	return out, nil
}

// whenFor is the date {when} stands for, in the edge's time zone and lang, with the zone's name: the
// opening while one is scheduled, the announced end during maintenance, else "". It does not depend on
// the page, so the share description reads the same on every page a crawler may land on.
func (rd *Renderer) whenFor(v policy.View, lang string) string {
	a := v.Access
	switch {
	case a == nil:
		return ""
	case a.State == policy.Scheduled && a.OpensAt != nil && v.Page != policy.Open:
		return formatWhen(*a.OpensAt, rd.Location, lang)
	case a.State == policy.Maintenance && a.EndsAt != nil:
		return formatWhen(*a.EndsAt, rd.Location, lang)
	}
	return ""
}

// fill puts the site name into {site} and the date into {when}. With no date, {when} is dropped and the
// spaces it leaves are tidied; a theme writes the date as an addition the sentence survives without.
func fill(t, site, when string) string {
	t = strings.ReplaceAll(t, "{site}", site)
	if !strings.Contains(t, "{when}") {
		return t
	}
	t = strings.ReplaceAll(t, "{when}", when)
	if when == "" {
		t = strings.Join(strings.Fields(t), " ")
	}
	return t
}
