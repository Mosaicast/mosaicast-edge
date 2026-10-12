// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

// Package gate is the edge's HTTP handler: it applies the access policy to every request (§18.4),
// proxies to core what may pass, and owns the HTTP semantics of the pages shown instead.
package gate

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Mosaicast/mosaicast-edge/internal/core"
	"github.com/Mosaicast/mosaicast-edge/internal/policy"
)

// Prefix is the edge's own path space. It is answered by the edge in every state, open included, so
// a page still partying when the site opens can keep loading its assets and asking for the state.
const Prefix = "/_a/"

// Page names. They double as <html data-page> values and theme.json keys.
const (
	PageCountdown   = "countdown"
	PageMaintenance = "maintenance"
	PageClosed      = "closed"
	PageUnavailable = "unavailable"
	PageLogin       = "login"
	PageLegal       = "legal"
)

// Viewer is a signed-in visitor who did not pass (a fan while the site is closed). Nil means anonymous.
type Viewer struct {
	SignedIn bool   `json:"signedIn"`
	Role     string `json:"role,omitempty"`
}

// Request is everything a page renderer needs to know about one page view.
type Request struct {
	Page       string
	View       policy.View
	Viewer     *Viewer
	LoginError string // /_a/login?error=
	LegalSlug  string // /_a/legal/{slug}
}

// Renderer turns a page request into HTML. found is false for a legal slug the copy does not have.
type Renderer interface {
	Render(r *http.Request, req Request) (body []byte, found bool, err error)
	// Assets serves everything under /_a/ that is a file: edge and theme assets, the site copy's
	// stylesheet and images, the calendar entry. It answers 404 for anything it does not know.
	Assets() http.Handler
	// Favicon serves the site's favicon (for /favicon.ico while not open).
	Favicon(w http.ResponseWriter, r *http.Request)
	// Public lists the /_a/ paths crawlers may fetch while the site is not open: the share image (link
	// previews honour robots.txt) and the favicon. Nothing else under /_a/.
	Public() []string
}

// RoleChecker is core.Roles in production.
type RoleChecker interface {
	Role(ctx context.Context, session string, visitor *http.Request) (string, error)
}

// Gate is the edge's root handler.
type Gate struct {
	upstream *url.URL
	store    *policy.Store
	roles    RoleChecker
	pages    Renderer
	now      func() time.Time
	proxy    *httputil.ReverseProxy
	mux      *http.ServeMux

	// long-polls in flight on /_a/state; beyond maxWaiters they are answered at once
	waiters    atomic.Int64
	maxWaiters int64
	pollHold   time.Duration
}

// Options configure a gate.
type Options struct {
	Upstream   *url.URL
	Store      *policy.Store
	Roles      RoleChecker
	Pages      Renderer
	Now        func() time.Time
	Transport  http.RoundTripper // nil = a transport with short dial timeouts
	MaxWaiters int64             // concurrent long-polls; 0 = 5000
	PollHold   time.Duration     // how long a long-poll is held; 0 = 25 s (under nginx's 60 s read timeout)
}

// New builds the gate.
func New(o Options) *Gate {
	g := &Gate{upstream: o.Upstream, store: o.Store, roles: o.Roles, pages: o.Pages, now: o.Now,
		maxWaiters: o.MaxWaiters, pollHold: o.PollHold}
	if g.maxWaiters == 0 {
		g.maxWaiters = 5000
	}
	if g.pollHold == 0 {
		g.pollHold = 25 * time.Second
	}
	tr := o.Transport
	if tr == nil {
		tr = defaultTransport()
	}
	g.proxy = &httputil.ReverseProxy{
		Rewrite:        g.rewrite,
		Transport:      tr,
		FlushInterval:  -1, // pass streamed responses through as they arrive
		ModifyResponse: g.modifyResponse,
		ErrorHandler:   g.upstreamDown,
	}

	g.mux = http.NewServeMux()
	g.mux.HandleFunc("GET "+Prefix+"healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok\n"))
	})
	g.mux.HandleFunc("GET "+Prefix+"state", g.serveState)
	g.mux.HandleFunc("GET "+Prefix+"login", func(w http.ResponseWriter, r *http.Request) {
		g.servePage(w, r, Request{Page: PageLogin, LoginError: r.URL.Query().Get("error")}, http.StatusOK)
	})
	g.mux.HandleFunc("GET "+Prefix+"legal/{slug}", func(w http.ResponseWriter, r *http.Request) {
		g.servePage(w, r, Request{Page: PageLegal, LegalSlug: r.PathValue("slug")}, http.StatusOK)
	})
	g.mux.Handle(Prefix, g.pages.Assets())
	return g
}

func defaultTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.MaxIdleConnsPerHost = 64
	return t
}

func (g *Gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, Prefix):
		g.mux.ServeHTTP(w, r)
		return
	case isOAuth(r.URL.Path):
		// the sign-in flow always reaches core: staff must be able to log in to a closed site
		g.proxy.ServeHTTP(w, r)
		return
	}

	view := g.store.View()
	if view.Page == policy.Open {
		g.proxy.ServeHTTP(w, r)
		return
	}
	page := pageFor(view.Page)

	// The rest of this is "not open". Core redirects a failed login to /?login_error=<code>; the
	// redirect itself is rewritten in modifyResponse, this catches anything that still arrives.
	if code := r.URL.Query().Get("login_error"); code != "" && r.URL.Path == "/" && isRead(r) {
		http.Redirect(w, r, Prefix+"login?error="+url.QueryEscape(code), http.StatusSeeOther)
		return
	}

	// Core's own legal URL reaches the edge's copy, so a bookmark, a search result or a link inside the
	// legal text still finds the imprint rather than a countdown (§18.4). Before the role check: the
	// copy is the same text, and a staff cookie must not cost the imprint when core is down.
	if slug, ok := legalSlug(r.URL.Path); ok && isRead(r) {
		to := Prefix + "legal/" + slug
		if r.URL.RawQuery != "" {
			to += "?" + r.URL.RawQuery
		}
		h := w.Header()
		SecureHeaders(h)
		h.Set("Cache-Control", "no-store") // once the site opens, the same URL is core's again
		http.Redirect(w, r, to, http.StatusFound)
		return
	}

	var viewer *Viewer
	if c, err := r.Cookie(core.SessionCookie); err == nil && core.LooksLikeSession(c.Value) && view.Access != nil {
		if g.store.IsCoreDown() {
			// circuit breaker: core is known to be down, asking it who this is would only wait
			g.serveUnavailable(w, r)
			return
		}
		role, err := g.roles.Role(r.Context(), c.Value, r)
		switch {
		case errors.Is(err, core.ErrRateLimited) || errors.Is(err, core.ErrBusy):
			slog.Debug("role check refused; showing the page", "err", err) // fail closed, quietly: floods are expected
		case err != nil:
			slog.Warn("role check failed; showing the page", "err", err) // fail closed
		case view.Access.Passes(role):
			g.proxy.ServeHTTP(w, r)
			return
		case role != core.Anonymous:
			viewer = &Viewer{SignedIn: true, Role: role}
		}
	}

	switch {
	case r.URL.Path == "/robots.txt" && isRead(r):
		g.serveRobots(w, g.pages.Public())
	case r.URL.Path == "/favicon.ico" && isRead(r):
		g.pages.Favicon(w, r)
	case !isRead(r) || !acceptsHTML(r):
		// API calls, form posts, images, feeds: a plain 503 whatever the page looks like (§18.4)
		g.plain503(w, view)
	default:
		status := http.StatusServiceUnavailable
		if page == PageCountdown {
			status = http.StatusOK
		}
		g.servePage(w, r, Request{Page: page, View: view, Viewer: viewer}, status)
	}
}

func pageFor(state string) string {
	switch state {
	case policy.Scheduled:
		return PageCountdown
	case policy.Maintenance:
		return PageMaintenance
	case policy.Closed:
		return PageClosed
	default:
		return PageUnavailable
	}
}

func (g *Gate) servePage(w http.ResponseWriter, r *http.Request, req Request, status int) {
	if req.View.Seq == 0 && req.View.Page == "" {
		req.View = g.store.View()
	}
	body, found, err := g.pages.Render(r, req)
	if err != nil {
		slog.Error("page render failed", "page", req.Page, "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	if !found {
		status = http.StatusNotFound
	}
	h := w.Header()
	SecureHeaders(h)
	h.Set("Content-Type", "text/html; charset=utf-8")
	// never cached: the moment the site opens, every reload must reach the edge and get the site
	h.Set("Cache-Control", "no-store")
	h.Set("Vary", "Accept-Language, Cookie")
	if status == http.StatusServiceUnavailable {
		h.Set("Retry-After", strconv.Itoa(g.retryAfter(req.Page, req.View)))
	}
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func (g *Gate) plain503(w http.ResponseWriter, view policy.View) {
	h := w.Header()
	SecureHeaders(h)
	h.Set("Cache-Control", "no-store")
	h.Set("Retry-After", strconv.Itoa(g.retryAfter(pageFor(view.Page), view)))
	http.Error(w, "Service unavailable.", http.StatusServiceUnavailable)
}

func (g *Gate) serveUnavailable(w http.ResponseWriter, r *http.Request) {
	if !isRead(r) || !acceptsHTML(r) {
		h := w.Header()
		SecureHeaders(h)
		h.Set("Cache-Control", "no-store")
		h.Set("Retry-After", "5")
		http.Error(w, "Service unavailable.", http.StatusServiceUnavailable)
		return
	}
	g.servePage(w, r, Request{Page: PageUnavailable, View: g.store.View()}, http.StatusServiceUnavailable)
}

// retryAfter tells crawlers and clients when to come back, in seconds.
func (g *Gate) retryAfter(page string, v policy.View) int {
	clamp := func(d time.Duration, lo, hi int) int {
		s := int(d.Seconds()) + 1
		return max(lo, min(hi, s))
	}
	switch {
	case page == PageCountdown && v.Access != nil && v.Access.OpensAt != nil:
		return clamp(v.Access.OpensAt.Sub(g.now()), 1, 3600)
	case page == PageMaintenance && v.Access != nil && v.Access.EndsAt != nil && v.Access.EndsAt.After(g.now()):
		return clamp(v.Access.EndsAt.Sub(g.now()), 60, 3600)
	case page == PageMaintenance:
		return 300
	case page == PageClosed:
		return 3600
	default:
		return 5
	}
}

func (g *Gate) serveRobots(w http.ResponseWriter, allow []string) {
	h := w.Header()
	SecureHeaders(h)
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	var b strings.Builder
	b.WriteString("User-agent: *\n")
	for _, p := range allow { // the more specific Allow lines first, before the Disallow they carve out of
		b.WriteString("Allow: " + p + "\n")
	}
	b.WriteString("Disallow: " + Prefix + "\nAllow: /\n")
	_, _ = w.Write([]byte(b.String()))
}

// stateReply is /_a/state: what a page on screen needs to follow the policy live.
type stateReply struct {
	Now       int64  `json:"now"` // unix ms, the edge's clock
	Page      string `json:"page"`
	OpensAt   *int64 `json:"opensAt"`
	EndsAt    *int64 `json:"endsAt"`
	CoreUp    bool   `json:"coreUp"`
	DownSince *int64 `json:"downSince"`
	Rev       uint64 `json:"rev"`
}

// serveState answers at once, or with ?rev=N holds the request until the state differs from N
// (a long-poll), at most pollHold. Beyond maxWaiters concurrent holds it answers at once and the
// page falls back to polling on a timer.
func (g *Gate) serveState(w http.ResponseWriter, r *http.Request) {
	v := g.store.View()
	if raw := r.URL.Query().Get("rev"); raw != "" {
		if rev, err := strconv.ParseUint(raw, 10, 64); err == nil && rev == v.Seq {
			if g.waiters.Add(1) <= g.maxWaiters {
				ctx, cancel := context.WithTimeout(r.Context(), g.pollHold)
				v = g.store.Wait(ctx, rev)
				cancel()
			}
			g.waiters.Add(-1)
		}
	}
	reply := stateReply{Now: g.now().UnixMilli(), Page: pageFor(v.Page), CoreUp: v.CoreUp, Rev: v.Seq}
	if v.Page == policy.Open {
		reply.Page = policy.Open
	}
	if v.Access != nil {
		reply.OpensAt = ms(v.Access.OpensAt)
		reply.EndsAt = ms(v.Access.EndsAt)
	}
	reply.DownSince = ms(v.DownSince)
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	_ = json.NewEncoder(w).Encode(reply)
}

func ms(t *time.Time) *int64 {
	if t == nil {
		return nil
	}
	v := t.UnixMilli()
	return &v
}

// rewrite forwards a request to core exactly as the TLS terminator handed it to us. Core builds its
// absolute URLs (the OAuth redirect among them) from Host and X-Forwarded-*, so both must survive the
// extra hop; the edge only appends itself to the X-Forwarded-For chain.
func (g *Gate) rewrite(pr *httputil.ProxyRequest) {
	pr.SetURL(g.upstream)
	pr.Out.Host = pr.In.Host

	for _, h := range []string{"X-Forwarded-Proto", "X-Forwarded-Host", "X-Forwarded-Port", "X-Real-Ip", "Forwarded"} {
		if v := pr.In.Header.Values(h); len(v) > 0 {
			pr.Out.Header[h] = v
		}
	}
	if ip, _, err := net.SplitHostPort(pr.In.RemoteAddr); err == nil {
		if prior := strings.Join(pr.In.Header.Values("X-Forwarded-For"), ", "); prior != "" {
			pr.Out.Header.Set("X-Forwarded-For", prior+", "+ip)
		} else {
			pr.Out.Header.Set("X-Forwarded-For", ip)
		}
	}
	if pr.Out.Header.Get("X-Forwarded-Proto") == "" {
		proto := "http"
		if pr.In.TLS != nil {
			proto = "https"
		}
		pr.Out.Header.Set("X-Forwarded-Proto", proto)
	}
	if pr.Out.Header.Get("X-Forwarded-Host") == "" {
		pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)
	}
}

// modifyResponse notes that core answered, and while the site is not open turns core's redirect after
// a failed login (/?login_error=<code>) into the edge's sign-in page, so staff see why.
func (g *Gate) modifyResponse(resp *http.Response) error {
	g.store.CoreUp()
	if !isOAuth(resp.Request.URL.Path) || resp.StatusCode < 300 || resp.StatusCode > 399 {
		return nil
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || (loc.Path != "/" && loc.Path != "") {
		return nil
	}
	code := loc.Query().Get("login_error")
	if code == "" || g.store.View().Page == policy.Open {
		return nil
	}
	resp.Header.Set("Location", Prefix+"login?error="+url.QueryEscape(code))
	return nil
}

// upstreamDown answers when core does not: the unavailable page instead of a bare 502.
func (g *Gate) upstreamDown(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		return // the visitor went away; that says nothing about core
	}
	g.store.CoreDown()
	slog.Warn("core unavailable", "path", r.URL.Path, "err", err)
	g.serveUnavailable(w, r)
}

func isOAuth(p string) bool {
	return strings.HasPrefix(p, "/oauth2/authorization/") || strings.HasPrefix(p, "/login/oauth2/code/")
}

// legalSlug reports whether p is core's legal URL /legal/{slug} with a slug in §12.6's grammar:
// lowercase letters and digits in hyphen-separated runs, at most 64 characters.
func legalSlug(p string) (string, bool) {
	slug, ok := strings.CutPrefix(p, "/legal/")
	if !ok || slug == "" || len(slug) > 64 || slug[0] == '-' || slug[len(slug)-1] == '-' || strings.Contains(slug, "--") {
		return "", false
	}
	for _, c := range slug {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return "", false
		}
	}
	return slug, true
}

func isRead(r *http.Request) bool { return r.Method == http.MethodGet || r.Method == http.MethodHead }

func acceptsHTML(r *http.Request) bool {
	acc := r.Header.Get("Accept")
	return acc == "" || strings.Contains(acc, "text/html") || strings.Contains(acc, "*/*")
}

// CSP is the policy on every page the edge renders: nothing but the edge's own origin, no inline
// script or style. Themes live inside it (THEMING.md).
const CSP = "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; font-src 'self'; " +
	"connect-src 'self'; media-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// SecureHeaders sets the headers every edge-rendered response carries.
func SecureHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), interest-cohort=()")
	h.Set("Content-Security-Policy", CSP)
	h.Set("X-Frame-Options", "DENY")
}
