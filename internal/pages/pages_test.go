// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

package pages

import (
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Mosaicast/mosaicast-edge/internal/gate"
	"github.com/Mosaicast/mosaicast-edge/internal/policy"
	"github.com/Mosaicast/mosaicast-edge/internal/theme"
	"github.com/Mosaicast/mosaicast-edge/web"
)

var now = time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)

func defaultTheme(t *testing.T) *theme.Theme {
	t.Helper()
	def, err := theme.Load(web.Default(), "/_a/s/")
	if err != nil {
		t.Fatal(err)
	}
	return def
}

func TestDefaultTextsHaveTheSameKeys(t *testing.T) {
	def := defaultTheme(t)
	if len(def.Texts) != 2 {
		t.Fatalf("default theme must ship en and de, has %d", len(def.Texts))
	}
	if err := theme.SameKeys(def.Texts); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultThemeValidates(t *testing.T) {
	if err := Validate(defaultTheme(t), nil, now); err != nil {
		t.Fatal(err)
	}
}

func scheduledView(at time.Time, visible bool) policy.View {
	return policy.View{Page: policy.Scheduled, CoreUp: true, Access: &policy.Access{State: policy.Scheduled, OpensAt: &at,
		PassFloor: policy.RolePodcaster, StaffLogin: policy.StaffLogin{Visible: visible}}}
}

func renderer(def, ext *theme.Theme) *Renderer {
	return New(Options{Default: def, Theme: ext, Site: staticSite{fixtureCopy()}, Now: func() time.Time { return now },
		View: func() policy.View { return policy.View{} }, Location: time.UTC})
}

// a bare theme template that places nothing: the edge must put its parts in anyway
const bare = `<!doctype html><html><head><title>t</title></head><body><h1>{{.T "hello"}}</h1></body></html>`

func extTheme(t *testing.T, files fstest.MapFS) *theme.Theme {
	t.Helper()
	th, err := theme.Load(files, "/_a/t/")
	if err != nil {
		t.Fatal(err)
	}
	return th
}

func TestEnforcedParts(t *testing.T) {
	ext := extTheme(t, fstest.MapFS{
		"theme.json":       {Data: []byte(`{"schema":1,"name":"Bare","locales":["en"],"pages":{"countdown":{"mode":"full","template":"c.html"}}}`)},
		"templates/c.html": {Data: []byte(bare)},
		"texts/en.json":    {Data: []byte(`{"hello":"Hi"}`)},
	})
	if err := Validate(defaultTheme(t), ext, now); err != nil {
		t.Fatal(err)
	}
	rd := renderer(defaultTheme(t), ext)
	body, _, err := rd.Render(httptest.NewRequest("GET", "/", nil), gate.Request{Page: gate.PageCountdown, View: scheduledView(now.Add(time.Hour), true)})
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{`<html lang="en" data-page="countdown"`, `id="edge-data"`, `data-edge-legal`, `href="/_a/legal/imprint?lang=en"`, `data-edge-login href=`, `data-edge-footer`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in\n%s", want, s)
		}
	}
	if strings.Index(s, `id="edge-data"`) > strings.Index(s, "<title>") {
		t.Error("the edge's head goes first in <head> (charset within the first bytes)")
	}
}

func TestRestyleUsesDefaultTemplateWithThemeStyles(t *testing.T) {
	ext := extTheme(t, fstest.MapFS{
		"theme.json":    {Data: []byte(`{"schema":1,"name":"R","locales":["de"],"styles":["r.css"],"pages":{"closed":{"mode":"restyle"}}}`)},
		"texts/de.json": {Data: []byte(`{"closed.title":"Zu!"}`)},
		"assets/r.css":  {Data: []byte(`:root{--edge-radius:0}`)},
	})
	rd := renderer(defaultTheme(t), ext)
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Accept-Language", "en")
	body, _, err := rd.Render(r, gate.Request{Page: gate.PageClosed, View: policy.View{Page: policy.Closed, Access: &policy.Access{State: policy.Closed}}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	css, _ := ext.Assets.URL("r.css")
	if !strings.Contains(s, css) || !strings.Contains(s, "edge-closed") {
		t.Error("restyle = default template + theme stylesheet")
	}
	// the page is English (edge locales), the theme only speaks German: its first locale, not edge text
	if !strings.Contains(s, ">Zu!<") || !strings.Contains(s, "The site isn&#39;t available") {
		t.Errorf("text layering wrong:\n%s", s)
	}
	// pages the theme does not touch stay pure default
	body, _, _ = rd.Render(r, gate.Request{Page: gate.PageMaintenance, View: policy.View{Page: policy.Maintenance, Access: &policy.Access{State: policy.Maintenance}}})
	if strings.Contains(string(body), css) || strings.Contains(string(body), "Zu!") {
		t.Error("an untouched page must not carry the theme")
	}
}

func TestValidateCatchesTemplateErrors(t *testing.T) {
	for name, tmpl := range map[string]string{
		"unknown asset": `<html><head></head><body><img src="{{.Asset "nope.png"}}"></body></html>`,
		"unknown text":  `<html><head></head><body>{{.T "nope"}}</body></html>`,
		"no body":       `<html><head></head>`,
		"bad field":     `<html><head></head><body>{{.Nope}}</body></html>`,
	} {
		ext := extTheme(t, fstest.MapFS{
			"theme.json":       {Data: []byte(`{"schema":1,"name":"X","locales":["en"],"pages":{"closed":{"mode":"full","template":"c.html"}}}`)},
			"templates/c.html": {Data: []byte(tmpl)},
		})
		if err := Validate(defaultTheme(t), ext, now); err == nil {
			t.Errorf("%s: Validate must fail", name)
		}
	}
}

func TestPickLang(t *testing.T) {
	for _, c := range []struct{ header, query, def, want string }{
		{"", "", "de", "de"},
		{"fr-FR,fr;q=0.9", "", "de", "de"},
		{"fr-FR,fr;q=0.9", "", "fr", "en"},
		{"en-US,en;q=0.9", "", "de", "en"},
		{"de;q=0.5,en-GB;q=0.8", "", "de", "en"},
		{"en", "de", "en", "de"},
		{"en", "xx", "en", "en"},
	} {
		r := httptest.NewRequest("GET", "/?lang="+c.query, nil)
		r.Header.Set("Accept-Language", c.header)
		if got := pickLang(r, []string{"en", "de"}, c.def); got != c.want {
			t.Errorf("%+v: got %s", c, got)
		}
	}
}

func TestSplit(t *testing.T) {
	p := split(90*time.Second + 400*time.Millisecond)
	if p.M != "01" || p.S != "31" || p.Total != 91 {
		t.Errorf("%+v", p)
	}
	if p := split(-time.Second); p.Total != 0 {
		t.Error("negative is zero")
	}
}

func TestTokenCSSOnlyCarriesColours(t *testing.T) {
	cp := fixtureCopy()
	css := string(tokenCSS(cp))
	if !strings.Contains(css, "--mc-accent-text:#8a6512") || !strings.Contains(css, ":root[data-mode=dark]") {
		t.Errorf("%s", css)
	}
}

func TestWhenPlaceholderFollowsThePolicy(t *testing.T) {
	ext := extTheme(t, fstest.MapFS{
		"theme.json":    {Data: []byte(`{"schema":1,"name":"W","locales":["de","en"],"pages":{"login":{"mode":"restyle"},"countdown":{"mode":"restyle"}}}`)},
		"texts/de.json": {Data: []byte(`{"description":"Bald offen. {when}","ics.summary":"Eröffnung {when}"}`)},
		"texts/en.json": {Data: []byte(`{"description":"Opening soon. {when}","ics.summary":"Opening {when}"}`)},
	})
	berlin, _ := time.LoadLocation("Europe/Berlin")
	view := scheduledView(now.Add(time.Hour), true) // 10:00 UTC = 12:00 in Berlin, summer time
	rd := New(Options{Default: defaultTheme(t), Theme: ext, Site: staticSite{fixtureCopy()}, Now: func() time.Time { return now },
		View: func() policy.View { return view }, Location: berlin})
	desc := func(lang string) string {
		t.Helper()
		body, _, err := rd.Render(httptest.NewRequest("GET", "/_a/login?lang="+lang, nil), gate.Request{Page: gate.PageLogin, View: view})
		if err != nil {
			t.Fatal(err)
		}
		s := string(body)
		i := strings.Index(s, `<meta name="description" content="`)
		if i < 0 {
			t.Fatalf("no description in\n%s", s)
		}
		s = s[i+len(`<meta name="description" content="`):]
		return s[:strings.Index(s, `"`)]
	}
	for lang, want := range map[string]string{
		"de": "Bald offen. Sonntag, 18. Oktober 2026 · 12:00 Uhr MESZ",
		"en": "Opening soon. Sunday, 18 October 2026 · 12:00 CEST",
	} {
		if got := desc(lang); got != want {
			t.Errorf("%s: %q, want %q", lang, got, want)
		}
	}

	// postponed by eight days, into winter time: the description follows, zone name included
	later := now.Add(8 * 24 * time.Hour)
	view = scheduledView(later, true)
	if got := desc("en"); got != "Opening soon. Monday, 26 October 2026 · 10:00 CET" {
		t.Errorf("after postponing: %q", got)
	}

	// the calendar entry takes {when} as well
	w := httptest.NewRecorder()
	rd.Assets().ServeHTTP(w, httptest.NewRequest("GET", "/_a/calendar.ics?lang=de", nil))
	if !strings.Contains(w.Body.String(), `SUMMARY:Eröffnung Montag\, 26. Oktober 2026 · 10:00 Uhr MEZ`) {
		t.Errorf("calendar summary:\n%s", w.Body.String())
	}

	// maintenance: the announced end; no date at all: the placeholder goes, the sentence stays tidy
	end := now.Add(2 * time.Hour)
	view = policy.View{Page: policy.Maintenance, Access: &policy.Access{State: policy.Maintenance, EndsAt: &end}}
	if got := desc("en"); got != "Opening soon. Sunday, 18 October 2026 · 13:00 CEST" {
		t.Errorf("maintenance: %q", got)
	}
	view = policy.View{Page: policy.Closed, Access: &policy.Access{State: policy.Closed}}
	if got := desc("de"); got != "Bald offen." {
		t.Errorf("closed: %q", got)
	}
	view = policy.View{Page: policy.Open, Access: &policy.Access{State: policy.Scheduled, OpensAt: &end}}
	if got := desc("en"); got != "Opening soon." {
		t.Errorf("scheduled but already open: %q", got)
	}
}

func TestWaitingText(t *testing.T) {
	ext := extTheme(t, fstest.MapFS{
		"theme.json":       {Data: []byte(`{"schema":1,"name":"W","locales":["en","de"],"pages":{"countdown":{"mode":"full","template":"c.html"}}}`)},
		"templates/c.html": {Data: []byte(`<html {{.HTMLAttrs}}><head>{{.EdgeHead}}</head><body><p data-edge-count-text="waiting">{{.WaitingText}}</p></body></html>`)},
	})
	if err := Validate(defaultTheme(t), ext, now); err != nil {
		t.Fatal(err)
	}
	rd := renderer(defaultTheme(t), ext)
	for _, c := range []struct {
		lang string
		w    gate.Waiting
		want string
	}{
		{"en", gate.Waiting{N: 0}, ">0 are waiting<"},
		{"en", gate.Waiting{N: 1}, ">1 is waiting<"},
		{"en", gate.Waiting{N: 1234}, ">1,234 are waiting<"},
		{"de", gate.Waiting{N: 1234567}, ">1.234.567 warten<"},
		{"de", gate.Waiting{N: 1}, ">1 wartet<"},
		{"en", gate.Waiting{N: 2500, Capped: true}, ">2,500&#43; are waiting<"},
		{"en", gate.Waiting{N: 1, Capped: true}, ">1&#43; are waiting<"},
	} {
		body, _, err := rd.Render(httptest.NewRequest("GET", "/?lang="+c.lang, nil),
			gate.Request{Page: gate.PageCountdown, View: scheduledView(now.Add(time.Hour), true), Waiting: c.w})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), c.want) {
			t.Errorf("%s %+v: want %s", c.lang, c.w, c.want)
		}
	}
}
