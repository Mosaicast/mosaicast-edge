// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mosaicast/mosaicast-edge/internal/core"
	"github.com/Mosaicast/mosaicast-edge/internal/gate"
	"github.com/Mosaicast/mosaicast-edge/internal/pages"
	"github.com/Mosaicast/mosaicast-edge/internal/policy"
	"github.com/Mosaicast/mosaicast-edge/internal/sitecopy"
)

var t0 = time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeCore is just enough of core's HTTP API (§18.2) for the edge.
type fakeCore struct {
	mu       sync.Mutex
	access   string // body of /api/site/access; "" = 404
	revision int
	roles    map[string]string // session cookie -> role
	meCalls  atomic.Int64
	meStatus int // non-zero: /api/me answers this
	down     atomic.Bool
	lastReq  *http.Request
	srv      *httptest.Server
}

func newFakeCore(t *testing.T) *fakeCore {
	fc := &fakeCore{roles: map[string]string{}}
	fc.srv = httptest.NewServer(http.HandlerFunc(fc.serve))
	t.Cleanup(fc.srv.Close)
	return fc
}

func (fc *fakeCore) setAccess(body string) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.revision++
	fc.access = body
}

func (fc *fakeCore) serve(w http.ResponseWriter, r *http.Request) {
	if fc.down.Load() {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
		return
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	switch {
	case r.URL.Path == "/api/site/access":
		if fc.access == "" {
			http.NotFound(w, r)
			return
		}
		etag := fmt.Sprintf(`"%d"`, fc.revision)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		_, _ = io.WriteString(w, fc.access)
	case r.URL.Path == "/api/me":
		fc.meCalls.Add(1)
		if fc.meStatus != 0 {
			w.WriteHeader(fc.meStatus)
			return
		}
		// core: a signed-in session wins, the bearer token is read only when the session is not (§8.5)
		c, err := r.Cookie(core.SessionCookie)
		role, ok := fc.roles[cookieValue(c, err)]
		if bearer, isBearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); !ok && isBearer {
			role, ok = fc.roles[bearer]
		}
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprintf(w, `{"id":"x","role":%q}`, role)
	case r.URL.Path == "/api/site":
		_, _ = io.WriteString(w, `{"name":"Example Cast","modePolicy":"system","defaultLocale":"de",
			"theme":{"light":{"bg":"#ffffff","text":"#111111","accent":"#cc9720","accentText":"#8a6512"},
			         "dark":{"bg":"#111111","text":"#eeeeee","accent":"#cc9720","accentText":"#e0b04a"}},
			"branding":{"logo":"/branding/logo","favicon":"/branding/favicon","darkLogo":null}}`)
	case r.URL.Path == "/branding/logo" || r.URL.Path == "/branding/favicon":
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = io.WriteString(w, `<svg xmlns="http://www.w3.org/2000/svg"/>`)
	case r.URL.Path == "/api/legal":
		_, _ = io.WriteString(w, `[{"slug":"impressum","title":"Impressum","role":"imprint"},{"slug":"datenschutz","title":"Datenschutz","role":"privacy"},{"slug":"faq","title":"FAQ","role":null}]`)
	case strings.HasPrefix(r.URL.Path, "/api/legal/"):
		slug := strings.TrimPrefix(r.URL.Path, "/api/legal/")
		_, _ = fmt.Fprintf(w, `{"slug":%q,"title":"Title of %s","role":null,"html":"<p>Body of %s</p>"}`, slug, slug, slug)
	default:
		fc.lastReq = r
		w.Header().Set("X-Upstream", "core")
		_, _ = io.WriteString(w, "mosaicast core")
	}
}

func cookieValue(c *http.Cookie, err error) string {
	if err != nil {
		return ""
	}
	return c.Value
}

// session makes a cookie value shaped like core's (base64 of a UUID).
func session(n int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%08x-0000-4000-8000-%012x", n, n)))
}

type edge struct {
	gate   *gate.Gate
	store  *policy.Store
	poller *core.Poller
	keeper *sitecopy.Keeper
	clock  *clock
	core   *fakeCore
}

func newEdge(t *testing.T, fc *fakeCore, c *clock, cacheDir string, themeDir ...string) *edge {
	t.Helper()
	return newEdgeWith(t, fc, c, cacheDir, nil, themeDir...)
}

// newEdgeWith is newEdge with a hook to change the gate's options.
func newEdgeWith(t *testing.T, fc *fakeCore, c *clock, cacheDir string, tune func(*gate.Options), themeDir ...string) *edge {
	t.Helper()
	u, _ := url.Parse(fc.srv.URL)
	dir := ""
	if len(themeDir) > 0 {
		dir = themeDir[0]
	}
	def, ext, err := loadThemes(dir)
	if err != nil {
		t.Fatal(err)
	}
	client := core.NewClient(u, "tok", "test")
	store := policy.NewStore(c.now, cacheDir)
	keeper := sitecopy.NewKeeper(client, edgeLocales, c.now, cacheDir)
	roles := &core.Roles{Client: client, TTL: 30 * time.Second, NegativeTTL: 10 * time.Second, Max: 100,
		Timeout: 2 * time.Second, PerClient: 10, InFlight: 32, Now: c.now}
	r := pages.New(pages.Options{Default: def, Theme: ext, Site: keeper, View: store.View, Now: c.now, Location: time.UTC})
	o := gate.Options{Upstream: u, Store: store, Roles: roles, Pages: r, Now: c.now, PollHold: 200 * time.Millisecond}
	if tune != nil {
		tune(&o)
	}
	g := gate.New(o)
	e := &edge{gate: g, store: store, keeper: keeper, clock: c, core: fc,
		poller: &core.Poller{Client: client, Store: store, Interval: time.Second}}
	e.poll()
	keeper.Refresh(context.Background())
	return e
}

func (e *edge) poll() { e.poller.Poll(context.Background()) }

func (e *edge) do(method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = "203.0.113.7:4711"
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.gate.ServeHTTP(w, r)
	return w
}

func scheduled(at time.Time) string {
	return fmt.Sprintf(`{"state":"scheduled","effectiveState":"scheduled","opensAt":%q,"endsAt":null,"passFloor":"podcaster",
		"staffLogin":{"visible":true,"providers":[{"id":"discord","path":"/oauth2/authorization/discord"}]},"revision":1}`, at.Format(time.RFC3339))
}

const html = "text/html,application/xhtml+xml"

func TestCountdownServedWithoutAskingCore(t *testing.T) {
	fc := newFakeCore(t)
	fc.setAccess(scheduled(t0.Add(90 * time.Second)))
	e := newEdge(t, fc, &clock{t: t0}, "")

	for _, p := range []string{"/", "/p/wiki/some-deep-link"} {
		w := e.do("GET", p, map[string]string{"Accept": html, "Accept-Language": "de-DE"})
		if w.Code != 200 {
			t.Fatalf("%s: got %d", p, w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, "Example Cast öffnet bald") || !strings.Contains(body, `data-page="countdown"`) {
			t.Errorf("%s: German countdown expected", p)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Error("countdown must not be cached")
		}
		// 90 s out: 00 days, 00 hours, 01 minute (singular), 30 seconds
		if !strings.Contains(body, `data-edge-unit="m">01<`) || !strings.Contains(body, `data-edge-unit-label="m">Minute<`) {
			t.Error("server-rendered digits wrong")
		}
		// legal links from the copy, only those marked imprint/privacy
		if !strings.Contains(body, `/_a/legal/impressum?lang=de`) || strings.Contains(body, "/_a/legal/faq") {
			t.Error("legal links wrong")
		}
		if !strings.Contains(body, `data-edge-login href="/_a/login?lang=de"`) {
			t.Error("staff login link expected when visible")
		}
	}
	if fc.lastReq != nil {
		t.Error("core must not see any page request while the site is not open")
	}
	if w := e.do("POST", "/api/anything", nil); w.Code != 503 || w.Header().Get("Retry-After") == "" {
		t.Errorf("POST while not open: got %d", w.Code)
	}
	if w := e.do("GET", "/api/feeds", map[string]string{"Accept": "application/json"}); w.Code != 503 || strings.Contains(w.Body.String(), "<html") {
		t.Errorf("non-HTML while not open must be a plain 503, got %d", w.Code)
	}
}

func TestFlipAtOpensAtWithoutPoll(t *testing.T) {
	fc := newFakeCore(t)
	fc.setAccess(scheduled(t0.Add(10 * time.Second)))
	c := &clock{t: t0}
	e := newEdge(t, fc, c, "")
	if w := e.do("GET", "/", map[string]string{"Accept": html}); w.Code != 200 || w.Header().Get("X-Upstream") != "" {
		t.Fatal("countdown expected before opensAt")
	}
	fc.down.Store(true) // core gone: the flip must not depend on it
	c.add(10 * time.Second)
	fc.down.Store(false)
	if w := e.do("GET", "/", map[string]string{"Accept": html}); w.Header().Get("X-Upstream") != "core" {
		t.Fatalf("site must open on the second without a poll, got %d", w.Code)
	}
}

func TestProxyKeepsHostAndForwardedHeaders(t *testing.T) {
	fc := newFakeCore(t)
	fc.setAccess(`{"state":"open","effectiveState":"open","passFloor":"podcaster","staffLogin":{"visible":false,"providers":[]},"revision":3}`)
	e := newEdge(t, fc, &clock{t: t0}, "")
	w := e.do("GET", "http://podcast.example/p/bingo?x=1", map[string]string{
		"X-Forwarded-Proto": "https", "X-Forwarded-Host": "podcast.example", "X-Forwarded-For": "198.51.100.1",
	})
	if w.Code != 200 || w.Body.String() != "mosaicast core" {
		t.Fatalf("expected proxied response, got %d %q", w.Code, w.Body.String())
	}
	got := fc.lastReq
	if got.Host != "podcast.example" || got.URL.RequestURI() != "/p/bingo?x=1" {
		t.Errorf("host/path not preserved: %s %s", got.Host, got.URL.RequestURI())
	}
	if got.Header.Get("X-Forwarded-Proto") != "https" || got.Header.Get("X-Forwarded-For") != "198.51.100.1, 203.0.113.7" {
		t.Errorf("forwarded headers wrong: proto=%q xff=%q", got.Header.Get("X-Forwarded-Proto"), got.Header.Get("X-Forwarded-For"))
	}
}

func TestWrongUpstreamNeverOpens(t *testing.T) {
	// a 404 for /api/site/access means the edge talks to the wrong service (a real core without the
	// endpoint answers 401): no policy at boot means unavailable, never open
	fc := newFakeCore(t) // access "" = 404
	e := newEdge(t, fc, &clock{t: t0}, "")
	w := e.do("GET", "/", map[string]string{"Accept": html})
	if w.Code != 503 || w.Header().Get("X-Upstream") != "" || !strings.Contains(w.Body.String(), `data-page="unavailable"`) {
		t.Fatalf("404 at boot: got %d, upstream %q", w.Code, w.Header().Get("X-Upstream"))
	}

	// and a 404 later keeps the last good policy
	fc.setAccess(scheduled(t0.Add(time.Hour)))
	e.poll()
	fc.mu.Lock()
	fc.access = ""
	fc.mu.Unlock()
	e.poll()
	if w := e.do("GET", "/", map[string]string{"Accept": html}); w.Code != 200 || !strings.Contains(w.Body.String(), `data-page="countdown"`) {
		t.Fatalf("404 after a scheduled policy must keep the countdown, got %d", w.Code)
	}
}

func TestUnavailableAtBootWithoutCache(t *testing.T) {
	fc := newFakeCore(t)
	fc.down.Store(true)
	e := newEdge(t, fc, &clock{t: t0}, "")
	w := e.do("GET", "/", map[string]string{"Accept": html})
	if w.Code != 503 || w.Header().Get("Retry-After") != "5" || !strings.Contains(w.Body.String(), `data-page="unavailable"`) {
		t.Fatalf("expected the unavailable page, got %d", w.Code)
	}
}

func TestCoreDownWhileOpenThenBack(t *testing.T) {
	fc := newFakeCore(t)
	fc.setAccess(`{"state":"open","effectiveState":"open","passFloor":"podcaster","staffLogin":{"visible":false,"providers":[]},"revision":3}`)
	c := &clock{t: t0}
	e := newEdge(t, fc, c, "")
	fc.down.Store(true)
	w := e.do("GET", "/", map[string]string{"Accept": html})
	if w.Code != 503 || !strings.Contains(w.Body.String(), `data-down="short"`) {
		t.Fatalf("expected unavailable (short), got %d", w.Code)
	}
	if w := e.do("GET", "/api/x", map[string]string{"Accept": "application/json"}); w.Code != 503 || w.Header().Get("Retry-After") != "5" {
		t.Errorf("non-HTML during outage: %d", w.Code)
	}
	c.add(2 * time.Minute)
	if w := e.do("GET", "/", map[string]string{"Accept": html}); !strings.Contains(w.Body.String(), `data-down="long"`) {
		t.Error("an outage older than a minute is 'long'")
	}
	fc.down.Store(false)
	if w := e.do("GET", "/", map[string]string{"Accept": html}); w.Header().Get("X-Upstream") != "core" {
		t.Error("core back: proxied again")
	}
}

func TestRoleCheck(t *testing.T) {
	fc := newFakeCore(t)
	fc.setAccess(scheduled(t0.Add(time.Hour)))
	fc.roles[session(1)] = "admin"
	fc.roles[session(2)] = "podcaster"
	fc.roles[session(3)] = "fan"
	e := newEdge(t, fc, &clock{t: t0}, "")

	for n, want := range map[int]bool{1: true, 2: true, 3: false, 4: false} {
		w := e.do("GET", "/", map[string]string{"Accept": html, "Cookie": core.SessionCookie + "=" + session(n) + "; other=1"})
		if passed := w.Header().Get("X-Upstream") == "core"; passed != want {
			t.Errorf("session %d: passed=%v, want %v", n, passed, want)
		}
		if n == 3 && !strings.Contains(w.Body.String(), `data-viewer="signed-in"`) {
			t.Error("a fan sees the page, marked as signed in")
		}
	}
	// only the session cookie reaches core
	if c := fc.lastReq.Header.Get("Cookie"); !strings.Contains(c, core.SessionCookie) {
		t.Errorf("proxied request lost the cookie: %q", c)
	}

	// pass floor admin: a podcaster no longer passes, an admin always does
	fc.setAccess(strings.Replace(scheduled(t0.Add(time.Hour)), `"passFloor":"podcaster"`, `"passFloor":"admin"`, 1))
	e.poll()
	e.clock.add(31 * time.Second) // past the cache
	if w := e.do("GET", "/", map[string]string{"Accept": html, "Cookie": core.SessionCookie + "=" + session(2)}); w.Header().Get("X-Upstream") != "" {
		t.Error("podcaster must not pass a floor of admin")
	}
	if w := e.do("GET", "/", map[string]string{"Accept": html, "Cookie": core.SessionCookie + "=" + session(1)}); w.Header().Get("X-Upstream") != "core" {
		t.Error("admin always passes")
	}
}

// pat makes a value shaped like core's personal access tokens.
func pat(n int) string {
	return "mcp_" + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%032d", n)))
}

func TestTokenCheck(t *testing.T) {
	fc := newFakeCore(t)
	fc.setAccess(scheduled(t0.Add(time.Hour)))
	fc.roles[pat(1)] = "admin"
	fc.roles[pat(2)] = "podcaster"
	fc.roles[pat(3)] = "fan"
	fc.roles[session(3)] = "fan"
	e := newEdge(t, fc, &clock{t: t0}, "")
	upload := func(hdr map[string]string) *httptest.ResponseRecorder {
		return e.do("POST", "/api/plugins/stats/blob", hdr)
	}
	bearer := func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

	for n, want := range []bool{1: true, 2: true, 3: false, 4: false} { // in order: token 2 is the last to pass
		if n == 0 {
			continue
		}
		w := upload(bearer(pat(n)))
		if passed := w.Header().Get("X-Upstream") == "core"; passed != want {
			t.Errorf("token %d: passed=%v, want %v", n, passed, want)
		}
		if !want && (w.Code != 503 || w.Header().Get("Retry-After") == "") {
			t.Errorf("token %d refused: plain 503 with Retry-After, got %d", n, w.Code)
		}
	}
	if got := fc.lastReq.Header.Get("Authorization"); got != "Bearer "+pat(2) {
		t.Error("a passed request reaches core with its token")
	}

	// a revoked token is remembered: sent again, it costs core nothing
	calls := fc.meCalls.Load()
	upload(bearer(pat(4)))
	if fc.meCalls.Load() != calls {
		t.Error("a revoked token is negative-cached")
	}
	// a token that core never issued is not looked up
	for _, h := range []string{"Bearer mcp_short", "Bearer " + pat(2) + "x", "Basic " + pat(2), "bearer " + pat(2)} {
		if w := upload(map[string]string{"Authorization": h}); w.Code != 503 || fc.meCalls.Load() != calls {
			t.Errorf("%.12s…: %d, %d lookups", h, w.Code, fc.meCalls.Load()-calls)
		}
	}
	// core reads the token only when the session does not sign the request in: a fan's session with a
	// podcaster's token is the fan; a dead session with it is the token
	both := bearer(pat(2))
	both["Cookie"] = core.SessionCookie + "=" + session(3)
	if w := upload(both); w.Header().Get("X-Upstream") != "" {
		t.Error("a fan's session with a podcaster's token must not pass: core would serve the fan")
	}
	both["Cookie"] = core.SessionCookie + "=" + session(9)
	if w := upload(both); w.Header().Get("X-Upstream") != "core" {
		t.Error("an unknown session with a podcaster's token passes, as in core")
	}

	// a token is a podcaster at most, whatever the account: a floor of admin keeps every token out
	fc.setAccess(strings.Replace(scheduled(t0.Add(time.Hour)), `"passFloor":"podcaster"`, `"passFloor":"admin"`, 1))
	e.poll()
	if w := upload(bearer(pat(1))); w.Header().Get("X-Upstream") != "" || w.Code != 503 {
		t.Errorf("an admin's token must not pass a floor of admin, got %d", w.Code)
	}
	// and an HTML request with a token gets the page, not marked as signed in
	w := e.do("GET", "/", map[string]string{"Accept": html, "Authorization": "Bearer " + pat(3)})
	if !strings.Contains(w.Body.String(), `data-page="countdown"`) || strings.Contains(w.Body.String(), `data-viewer="signed-in"`) {
		t.Error("a fan's token: the countdown, anonymous")
	}
}

func TestRoleCheckFailsClosed(t *testing.T) {
	fc := newFakeCore(t)
	fc.setAccess(scheduled(t0.Add(time.Hour)))
	fc.roles[session(1)] = "admin"
	e := newEdge(t, fc, &clock{t: t0}, "")
	fc.meStatus = 500
	w := e.do("GET", "/", map[string]string{"Accept": html, "Cookie": core.SessionCookie + "=" + session(1)})
	if w.Code != 200 || w.Header().Get("X-Upstream") != "" || !strings.Contains(w.Body.String(), `data-page="countdown"`) {
		t.Fatalf("an error in the role check must show the page, got %d", w.Code)
	}
	fc.meStatus = 0 // errors are not cached: recovery is immediate
	if w := e.do("GET", "/", map[string]string{"Accept": html, "Cookie": core.SessionCookie + "=" + session(1)}); w.Header().Get("X-Upstream") != "core" {
		t.Error("admin must pass once core answers again")
	}
}

func TestCircuitBreaker(t *testing.T) {
	fc := newFakeCore(t)
	fc.setAccess(scheduled(t0.Add(time.Hour)))
	e := newEdge(t, fc, &clock{t: t0}, "")
	fc.down.Store(true)
	// one missed poll is not an outage (core under load): held pages are not released, no breaker
	seq := e.store.View().Seq
	e.poll()
	if e.store.IsCoreDown() || e.store.View().Seq != seq {
		t.Fatal("a single failed poll marked core down")
	}
	e.poll() // two in a row are
	before := fc.meCalls.Load()
	w := e.do("GET", "/", map[string]string{"Accept": html, "Cookie": core.SessionCookie + "=" + session(1)})
	if w.Code != 503 || !strings.Contains(w.Body.String(), `data-page="unavailable"`) || fc.meCalls.Load() != before {
		t.Fatalf("known-down core: unavailable without asking /api/me, got %d", w.Code)
	}
	// anonymous visitors still get the countdown
	if w := e.do("GET", "/", map[string]string{"Accept": html}); w.Code != 200 {
		t.Errorf("anonymous during outage: %d", w.Code)
	}
}

func TestOAuthAlwaysPassesAndLoginErrorRedirects(t *testing.T) {
	fc := newFakeCore(t)
	fc.setAccess(scheduled(t0.Add(time.Hour)))
	e := newEdge(t, fc, &clock{t: t0}, "")
	for _, p := range []string{"/oauth2/authorization/discord", "/login/oauth2/code/discord?code=x&state=y"} {
		if w := e.do("GET", p, nil); w.Header().Get("X-Upstream") != "core" {
			t.Errorf("%s must reach core while closed", p)
		}
	}
	w := e.do("GET", "/?login_error=signups_closed", map[string]string{"Accept": html})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/_a/login?error=signups_closed" {
		t.Fatalf("login_error while closed: %d %s", w.Code, w.Header().Get("Location"))
	}
	w = e.do("GET", "/_a/login?error=signups_closed", map[string]string{"Accept": html, "Accept-Language": "en"})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Sign-ups open with the site.") {
		t.Errorf("login page should explain the error, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `href="/oauth2/authorization/discord"`) {
		t.Error("login page lists the providers")
	}
}

func TestLoginPageWithoutProviders(t *testing.T) {
	fc := newFakeCore(t)
	fc.setAccess(`{"state":"closed","passFloor":"podcaster","staffLogin":{"visible":true,"providers":[]},"revision":1}`)
	e := newEdge(t, fc, &clock{t: t0}, "")
	w := e.do("GET", "/_a/login", map[string]string{"Accept-Language": "en"})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "No sign-in is configured") {
		t.Fatalf("got %d", w.Code)
	}
}

func TestMaintenanceAndClosedAre503(t *testing.T) {
	fc := newFakeCore(t)
	end := t0.Add(2 * time.Hour)
	fc.setAccess(fmt.Sprintf(`{"state":"maintenance","endsAt":%q,"passFloor":"podcaster","staffLogin":{"visible":false,"providers":[]},"revision":1}`, end.Format(time.RFC3339)))
	c := &clock{t: t0}
	e := newEdge(t, fc, c, "")
	w := e.do("GET", "/", map[string]string{"Accept": html, "Accept-Language": "en"})
	if w.Code != 503 || w.Header().Get("Retry-After") != "3600" || !strings.Contains(w.Body.String(), "under maintenance") {
		t.Fatalf("maintenance: %d retry=%s", w.Code, w.Header().Get("Retry-After"))
	}
	if strings.Contains(w.Body.String(), "data-edge-login href") {
		t.Error("staff login link must be absent when not visible")
	}
	c.add(3 * time.Hour) // endsAt is shown, never acted on
	if w := e.do("GET", "/", map[string]string{"Accept": html}); w.Code != 503 {
		t.Error("maintenance must not end on its own")
	}
	fc.setAccess(`{"state":"closed","passFloor":"podcaster","staffLogin":{"visible":false,"providers":[]},"revision":2}`)
	e.poll()
	if w := e.do("GET", "/", map[string]string{"Accept": html}); w.Code != 503 || !strings.Contains(w.Body.String(), `data-page="closed"`) {
		t.Errorf("closed: %d", w.Code)
	}
}

func TestLegalPagesFromCopy(t *testing.T) {
	fc := newFakeCore(t)
	fc.setAccess(scheduled(t0.Add(time.Hour)))
	e := newEdge(t, fc, &clock{t: t0}, "")
	fc.down.Store(true) // served from the copy
	w := e.do("GET", "/_a/legal/impressum?lang=de", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "<p>Body of impressum</p>") {
		t.Fatalf("legal page: %d", w.Code)
	}
	if w := e.do("GET", "/_a/legal/nope", nil); w.Code != 404 {
		t.Errorf("unknown legal page: %d", w.Code)
	}
}

func TestCoreLegalURLReachesTheCopyWhileNotOpen(t *testing.T) {
	fc := newFakeCore(t)
	c := &clock{t: t0}
	e := newEdge(t, fc, c, "")
	for _, st := range []struct{ name, body string }{ // closed last: the checks below expect it
		{"scheduled", scheduled(t0.Add(time.Hour))},
		{"maintenance", `{"state":"maintenance","passFloor":"podcaster","staffLogin":{"visible":false,"providers":[]},"revision":2}`},
		{"closed", `{"state":"closed","passFloor":"podcaster","staffLogin":{"visible":false,"providers":[]},"revision":3}`},
	} {
		name := st.name
		fc.setAccess(st.body)
		e.poll()
		for _, m := range []string{"GET", "HEAD"} {
			w := e.do(m, "/legal/impressum?lang=en", map[string]string{"Accept": html})
			if w.Code != http.StatusFound || w.Header().Get("Location") != "/_a/legal/impressum?lang=en" {
				t.Errorf("%s %s: %d %q", name, m, w.Code, w.Header().Get("Location"))
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("%s: the redirect must not be cached, the URL is core's once the site opens", name)
			}
		}
	}
	// a staff cookie does not cost the imprint while core is down
	fc.down.Store(true)
	e.poll()
	e.poll()
	if w := e.do("GET", "/legal/impressum", map[string]string{"Accept": html, "Cookie": core.SessionCookie + "=" + session(1)}); w.Code != http.StatusFound {
		t.Errorf("core down, with a cookie: %d", w.Code)
	}
	fc.down.Store(false)
	// the redirect target: an unknown slug gets the legal 404 page
	if w := e.do("GET", "/_a/legal/nope", map[string]string{"Accept": html}); w.Code != 404 || !strings.Contains(w.Body.String(), `data-page="legal"`) {
		t.Errorf("unknown slug: %d", w.Code)
	}
	// slugs outside §12.6's grammar, sub-paths and writes get the state's page as before
	for _, p := range []string{"/legal/Impressum", "/legal/a--b", "/legal/-a", "/legal/a-", "/legal/", "/legal/a/b",
		"/legal/" + strings.Repeat("a", 65)} {
		if w := e.do("GET", p, map[string]string{"Accept": html}); w.Code != 503 || !strings.Contains(w.Body.String(), `data-page="closed"`) {
			t.Errorf("%s: %d %q", p, w.Code, w.Header().Get("Location"))
		}
	}
	if w := e.do("POST", "/legal/impressum", nil); w.Code != 503 || w.Header().Get("Location") != "" {
		t.Errorf("POST: %d", w.Code)
	}
	// open: core's own page
	fc.setAccess(`{"state":"open","passFloor":"podcaster","staffLogin":{"visible":false,"providers":[]},"revision":4}`)
	e.poll()
	if w := e.do("GET", "/legal/impressum", map[string]string{"Accept": html}); w.Header().Get("X-Upstream") != "core" {
		t.Errorf("open: proxied to core, got %d", w.Code)
	}
}

func TestStateLongPoll(t *testing.T) {
	fc := newFakeCore(t)
	fc.setAccess(scheduled(t0.Add(time.Hour)))
	e := newEdge(t, fc, &clock{t: t0}, "")
	var first struct {
		Rev     uint64 `json:"rev"`
		Page    string `json:"page"`
		OpensAt int64  `json:"opensAt"`
	}
	_ = json.Unmarshal(e.do("GET", "/_a/state", nil).Body.Bytes(), &first)
	if first.Page != "countdown" || first.OpensAt != t0.Add(time.Hour).UnixMilli() {
		t.Fatalf("state: %+v", first)
	}
	done := make(chan []byte)
	go func() { done <- e.do("GET", fmt.Sprintf("/_a/state?rev=%d", first.Rev), nil).Body.Bytes() }()
	time.Sleep(50 * time.Millisecond)
	fc.setAccess(scheduled(t0.Add(12 * time.Second))) // "open now"
	e.poll()
	select {
	case body := <-done:
		var next struct {
			OpensAt int64 `json:"opensAt"`
		}
		_ = json.Unmarshal(body, &next)
		if next.OpensAt != t0.Add(12*time.Second).UnixMilli() {
			t.Errorf("long-poll did not deliver the new opensAt: %s", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("long-poll did not wake on a policy change")
	}
}

type stateWaiting struct {
	Rev           uint64 `json:"rev"`
	Waiting       int64  `json:"waiting"`
	WaitingCapped bool   `json:"waitingCapped"`
}

func (e *edge) state(t *testing.T, target string) stateWaiting {
	t.Helper()
	var s stateWaiting
	if err := json.Unmarshal(e.do("GET", target, nil).Body.Bytes(), &s); err != nil {
		t.Error(err) // not Fatal: called from goroutines too
	}
	return s
}

// The waiting count is the held long-polls: it reaches pages with every answer, never wakes them, and
// stops at the long-poll limit.
func TestWaitingCount(t *testing.T) {
	fc := newFakeCore(t)
	fc.setAccess(scheduled(t0.Add(time.Hour)))
	e := newEdgeWith(t, fc, &clock{t: t0}, "", func(o *gate.Options) { o.MaxWaiters = 3; o.PollHold = 5 * time.Second })
	first := e.state(t, "/_a/state")
	if first.Waiting != 0 || first.WaitingCapped {
		t.Fatalf("nobody yet: %+v", first)
	}
	hold := fmt.Sprintf("/_a/state?rev=%d", first.Rev)
	answered := make(chan stateWaiting, 4)
	for range 2 {
		go func() { answered <- e.state(t, hold) }()
	}
	deadline := time.Now().Add(2 * time.Second)
	for e.state(t, "/_a/state").Waiting != 2 {
		if time.Now().After(deadline) {
			t.Fatal("two held long-polls never showed as 2 waiting")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case s := <-answered:
		t.Fatalf("a change in the count woke a long-poll: %+v", s)
	case <-time.After(100 * time.Millisecond):
	}

	go func() { answered <- e.state(t, hold) }() // the third is held: the limit is 3
	for e.state(t, "/_a/state").Waiting != 3 {
		if time.Now().After(deadline) {
			t.Fatal("the third long-poll never showed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	over := e.state(t, hold) // the fourth is beyond the limit: answered at once, the page polls on a timer
	if over.Rev != first.Rev || over.Waiting != 3 || !over.WaitingCapped {
		t.Fatalf("beyond the limit: %+v", over)
	}
	page := e.do("GET", "/", map[string]string{"Accept": html}).Body.String()
	for _, want := range []string{`data-waiting="3" data-waiting-capped`, `"waiting":3,"waitingCapped":true`} {
		if !strings.Contains(page, want) {
			t.Errorf("countdown page lacks %s", want)
		}
	}

	fc.setAccess(scheduled(t0.Add(2 * time.Hour)))
	e.poll()
	for range 3 {
		select {
		case s := <-answered:
			if s.Rev == first.Rev {
				t.Errorf("woken without a new rev: %+v", s)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a policy change did not wake every long-poll")
		}
	}
}

// A shutdown answers the held long-polls at once instead of waiting out their hold, and holds no more.
func TestStopAnswersHeldLongPolls(t *testing.T) {
	fc := newFakeCore(t)
	fc.setAccess(scheduled(t0.Add(time.Hour)))
	e := newEdgeWith(t, fc, &clock{t: t0}, "", func(o *gate.Options) { o.PollHold = time.Minute })
	rev := e.state(t, "/_a/state").Rev
	hold := fmt.Sprintf("/_a/state?rev=%d", rev)
	answered := make(chan stateWaiting, 1)
	go func() { answered <- e.state(t, hold) }()
	deadline := time.Now().Add(2 * time.Second)
	for e.state(t, "/_a/state").Waiting != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the long-poll was never held")
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.gate.Stop()
	select {
	case s := <-answered:
		if s.Rev != rev {
			t.Errorf("answered with the same state: %+v", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not answer the held long-poll")
	}
	start := time.Now()
	e.state(t, hold)
	if time.Since(start) > time.Second {
		t.Error("a long-poll after Stop was held")
	}
}

func TestPolicyCacheSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	fc := newFakeCore(t)
	fc.setAccess(scheduled(t0.Add(time.Hour)))
	newEdge(t, fc, &clock{t: t0}, dir)
	if _, err := os.Stat(filepath.Join(dir, "policy.json")); err != nil {
		t.Fatal(err)
	}
	fc.down.Store(true)
	e := newEdge(t, fc, &clock{t: t0}, dir) // restart with core down
	w := e.do("GET", "/", map[string]string{"Accept": html})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Example Cast") {
		t.Fatalf("cached policy and site copy must apply after a restart, got %d", w.Code)
	}
}

func TestPreviewIsNotReachableInServe(t *testing.T) {
	fc := newFakeCore(t)
	fc.setAccess(scheduled(t0.Add(time.Hour)))
	e := newEdge(t, fc, &clock{t: t0}, "")
	for _, p := range []string{"/?preview=5", "/#preview-5"} {
		body := e.do("GET", p, map[string]string{"Accept": html}).Body.String()
		if strings.Contains(body, `"preview"`) {
			t.Errorf("%s: production pages carry no preview switch", p)
		}
	}
	if w := e.do("GET", "/_a/preview", nil); w.Code != 404 {
		t.Errorf("/_a/preview must not exist in serve: %d", w.Code)
	}
}

func TestAssetsAndUnknownEdgePaths(t *testing.T) {
	fc := newFakeCore(t)
	fc.setAccess(scheduled(t0.Add(time.Hour)))
	e := newEdge(t, fc, &clock{t: t0}, "")
	body := e.do("GET", "/", map[string]string{"Accept": html}).Body.String()
	i := strings.Index(body, `<script src="/_a/s/`)
	if i < 0 {
		t.Fatal("runtime script missing")
	}
	src := body[i+13 : i+13+strings.Index(body[i+13:], `"`)]
	w := e.do("GET", src, map[string]string{"Accept-Encoding": "gzip"})
	if w.Code != 200 || w.Header().Get("Content-Encoding") != "gzip" || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("runtime asset: %d %v", w.Code, w.Header())
	}
	for _, p := range []string{"/_a/s/edge.js", "/_a/s/", "/_a/t/x.css", "/_a/whatever"} {
		if w := e.do("GET", p, nil); w.Code != 404 {
			t.Errorf("%s: %d, want 404", p, w.Code)
		}
	}
	if w := e.do("GET", "/_a/healthz", nil); w.Code != 200 {
		t.Error("healthz")
	}
	if w := e.do("GET", "/_a/calendar.ics", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "DTSTART:20261018T100000Z") {
		t.Errorf("calendar: %d %s", w.Code, w.Body.String())
	}
}

func TestLoadConfig(t *testing.T) {
	env := map[string]string{"EDGE_UPSTREAM": "http://app:8080"}
	c, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.listen != ":8080" || c.poll != 2*time.Second || c.location != time.UTC {
		t.Fatalf("defaults: %+v", c)
	}
	bad := map[string]string{"EDGE_UPSTREAM": "app:8080", "EDGE_POLL_INTERVAL": "0", "EDGE_TIMEZONE": "Mars/Base", "EDGE_PUBLIC_URL": "https://x.org/path"}
	_, err = loadConfig(func(k string) string { return bad[k] })
	for _, want := range []string{"EDGE_UPSTREAM", "EDGE_POLL_INTERVAL", "EDGE_TIMEZONE", "EDGE_PUBLIC_URL"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %s: %v", want, err)
		}
	}
}

func TestThemeSettings(t *testing.T) {
	cfg := func(env map[string]string) (config, error) {
		env["EDGE_UPSTREAM"] = "http://app:8080"
		return loadConfig(func(k string) string { return env[k] })
	}
	// the older name is always a path, never read as owner/repo
	if c, err := cfg(map[string]string{"EDGE_THEME_DIR": "themes/mine"}); err != nil || c.theme != "./themes/mine" {
		t.Errorf("EDGE_THEME_DIR: %q %v", c.theme, err)
	}
	if c, err := cfg(map[string]string{"EDGE_THEME": "/theme", "EDGE_THEME_DIR": "/theme"}); err != nil || c.theme != "/theme" {
		t.Errorf("both, agreeing: %q %v", c.theme, err)
	}
	if _, err := cfg(map[string]string{"EDGE_THEME": "acme/theme@v1", "EDGE_THEME_DIR": "/theme"}); err == nil {
		t.Error("EDGE_THEME and EDGE_THEME_DIR disagreeing must be refused")
	}
	if c, err := cfg(map[string]string{"EDGE_THEME": "acme/theme@v1#sha256:00", "EDGE_THEME_TOKEN": "x", "EDGE_THEME_ALLOW_UNVERIFIED": "true"}); err != nil || !c.themeAnyPin || c.themeToken != "x" {
		t.Errorf("token/unverified: %+v %v", c, err)
	}
	if _, err := cfg(map[string]string{"EDGE_THEME_ALLOW_UNVERIFIED": "maybe"}); err == nil {
		t.Error("EDGE_THEME_ALLOW_UNVERIFIED takes true or false")
	}
}

func TestRobotsAllowsOnlyShareImageAndFavicon(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"theme.json":         `{"schema":1,"name":"S","locales":["en"],"favicon":"f.svg","share":{"image":"og.jpg"},"pages":{"closed":{"mode":"restyle"}}}`,
		"assets/og.jpg":      "jpeg",
		"assets/f.svg":       "<svg/>",
		"assets/secret.webp": "art",
	}
	for p, body := range files {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
		if err := os.WriteFile(filepath.Join(dir, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fc := newFakeCore(t)
	fc.setAccess(scheduled(t0.Add(time.Hour)))
	e := newEdge(t, fc, &clock{t: t0}, "", dir)
	body := e.do("GET", "/robots.txt", nil).Body.String()
	if strings.Count(body, "Allow: /_a/t/") != 2 || !strings.Contains(body, "Disallow: /_a/") ||
		strings.Index(body, "Allow: /_a/t/") > strings.Index(body, "Disallow: /_a/") {
		t.Fatalf("robots.txt:\n%s", body)
	}
	page := e.do("GET", "/", map[string]string{"Accept": html}).Body.String()
	if !strings.Contains(page, `property="og:image" content="http://example.com/_a/t/`) {
		t.Error("share image missing from the page")
	}
}

func TestThemeArtworkExpiresAfterOpening(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"theme.json":         `{"schema":1,"name":"S","locales":["en"],"share":{"image":"og.jpg"},"pages":{"closed":{"mode":"restyle"}},"assets":{"expireAfterOpen":"1h","keep":["k.css"]}}`,
		"assets/og.jpg":      "jpeg",
		"assets/k.css":       "a{}",
		"assets/secret.webp": "art",
	}
	for p, body := range files {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
		_ = os.WriteFile(filepath.Join(dir, p), []byte(body), 0o644)
	}
	fc := newFakeCore(t)
	fc.setAccess(scheduled(t0.Add(time.Minute)))
	c := &clock{t: t0}
	e := newEdge(t, fc, c, "", dir)
	_, ext, _ := loadThemes(dir)
	url := func(p string) string { u, _ := ext.Assets.URL(p); return u }

	for _, p := range []string{"secret.webp", "og.jpg", "k.css"} {
		if w := e.do("GET", url(p), nil); w.Code != 200 {
			t.Fatalf("%s before opening: %d", p, w.Code)
		}
	}
	c.add(time.Minute + 59*time.Minute) // open for 59 minutes: still served (pages may still be partying)
	if w := e.do("GET", url("secret.webp"), nil); w.Code != 200 {
		t.Errorf("artwork within the hour: %d", w.Code)
	}
	c.add(2 * time.Minute)
	if w := e.do("GET", url("secret.webp"), nil); w.Code != 404 {
		t.Errorf("artwork an hour after opening must be gone: %d", w.Code)
	}
	for _, p := range []string{"og.jpg", "k.css"} {
		if w := e.do("GET", url(p), nil); w.Code != 200 {
			t.Errorf("%s is kept: %d", p, w.Code)
		}
	}
	if w := e.do("GET", url("og.jpg"), nil); w.Header().Get("Cross-Origin-Resource-Policy") != "cross-origin" {
		t.Error("the share image is fetched by other sites")
	}
}

func TestLimitListenerQueuesBeyondMax(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := limitListen(inner, 2)
	defer ln.Close()
	accepted := make(chan net.Conn, 3)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	for i := 0; i < 3; i++ {
		c, err := net.Dial("tcp", inner.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
	}
	first, second := <-accepted, <-accepted
	select {
	case <-accepted:
		t.Fatal("a third connection was accepted while two were open")
	case <-time.After(100 * time.Millisecond):
	}
	first.Close()
	first.Close() // closing twice must not free two slots
	select {
	case c := <-accepted:
		c.Close()
	case <-time.After(time.Second):
		t.Fatal("the waiting connection was not accepted after one closed")
	}
	second.Close()
}

func TestConnLimitAboveLongPollLimit(t *testing.T) {
	env := map[string]string{"EDGE_UPSTREAM": "http://app:8080", "EDGE_LONGPOLL_MAX": "4000", "EDGE_MAX_CONNS": "3500"}
	if _, err := loadConfig(func(k string) string { return env[k] }); err == nil || !strings.Contains(err.Error(), "EDGE_MAX_CONNS") {
		t.Fatalf("held pages must leave room: %v", err)
	}
}
