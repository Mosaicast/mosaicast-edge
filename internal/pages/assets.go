// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

package pages

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Mosaicast/mosaicast-edge/internal/gate"
	"github.com/Mosaicast/mosaicast-edge/internal/policy"
	"github.com/Mosaicast/mosaicast-edge/internal/sitecopy"
)

// The site copy's derived files, served under /_a/site/<hash>.<ext>: the token stylesheet and the
// branding images. Rebuilt whenever the copy changes.
type siteAssetCache struct {
	mu  sync.Mutex
	cp  *sitecopy.Copy
	set *siteAssetSet
}

type siteAssetSet struct {
	css, logo, darkLogo, favicon string // URLs ("" when absent)
	files                        map[string]siteFile
}

type siteFile struct {
	body  []byte
	ctype string
}

// fallbackTokens are used before the edge has ever seen core: neutral, readable, light and dark.
var fallbackLight = map[string]string{"bg": "#f7f7f5", "surface": "#ffffff", "text": "#1d1d1b", "text-muted": "#5c5c57",
	"accent": "#3d5a80", "accent-contrast": "#ffffff", "border": "#dcdcd6", "accent-2": "#8d5a97", "accent-text": "#34507a"}
var fallbackDark = map[string]string{"bg": "#151514", "surface": "#1f1f1d", "text": "#ececea", "text-muted": "#a6a6a0",
	"accent": "#7fa3d1", "accent-contrast": "#151514", "border": "#34342f", "accent-2": "#c79bd0", "accent-text": "#9cbbe2"}

var imageExt = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/gif": ".gif",
	"image/avif": ".avif", "image/svg+xml": ".svg", "image/x-icon": ".ico", "image/vnd.microsoft.icon": ".ico"}

func (c *siteAssetCache) get(cp *sitecopy.Copy) *siteAssetSet {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cp == cp && c.set != nil {
		return c.set
	}
	s := &siteAssetSet{files: map[string]siteFile{}}
	add := func(body []byte, ctype, ext string) string {
		sum := sha256.Sum256(body)
		name := hex.EncodeToString(sum[:8]) + ext
		s.files[name] = siteFile{body: body, ctype: ctype}
		return gate.Prefix + "site/" + name
	}
	s.css = add(tokenCSS(cp), "text/css; charset=utf-8", ".css")
	if cp.Logo != nil {
		s.logo = add(cp.Logo.Body, cp.Logo.Type, imageExt[cp.Logo.Type])
	}
	if cp.DarkLogo != nil {
		s.darkLogo = add(cp.DarkLogo.Body, cp.DarkLogo.Type, imageExt[cp.DarkLogo.Type])
	}
	if cp.Favicon != nil {
		s.favicon = add(cp.Favicon.Body, cp.Favicon.Type, imageExt[cp.Favicon.Type])
	}
	c.cp, c.set = cp, s
	return s
}

// tokenCSS writes the site's light and dark tokens as --mc-* custom properties. <html data-mode> carries
// core's mode policy: light and dark force a set, system follows the visitor's preference.
func tokenCSS(cp *sitecopy.Copy) []byte {
	light, dark := cp.Site.Light, cp.Site.Dark
	if len(light) == 0 {
		light = fallbackLight
	}
	if len(dark) == 0 {
		dark = fallbackDark
	}
	decl := func(m map[string]string, scheme string) string {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, k := range keys {
			b.WriteString("--mc-" + k + ":" + m[k] + ";")
		}
		b.WriteString("color-scheme:" + scheme + ";")
		return b.String()
	}
	var b bytes.Buffer
	b.WriteString("/* the site's tokens, copied from core (§12.3) */\n")
	b.WriteString(":root{" + decl(light, "light") + "}\n")
	b.WriteString(":root[data-mode=dark]{" + decl(dark, "dark") + "}\n")
	b.WriteString("@media (prefers-color-scheme:dark){:root[data-mode=system]{" + decl(dark, "dark") + "}}\n")
	return b.Bytes()
}

// Assets serves /_a/s/ (edge), /_a/t/ (theme), /_a/site/ (site copy) and /_a/calendar.ics.
func (rd *Renderer) Assets() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+gate.Prefix+"s/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		fav, _ := rd.Default.Assets.URL("favicon.svg")
		rd.Default.Assets.Serve(w, r, name, strings.HasSuffix(fav, "/"+name))
	})
	mux.HandleFunc("GET "+gate.Prefix+"t/{name}", func(w http.ResponseWriter, r *http.Request) {
		if rd.Theme == nil {
			http.NotFound(w, r)
			return
		}
		name := r.PathValue("name")
		if rd.expired() && !rd.Theme.Public(name) {
			http.NotFound(w, r) // the site has been open for a while; the artwork is no longer handed out
			return
		}
		rd.Theme.Assets.Serve(w, r, name, rd.Theme.Shared(name))
	})
	mux.HandleFunc("GET "+gate.Prefix+"site/{name}", func(w http.ResponseWriter, r *http.Request) {
		set := rd.siteAssets.get(rd.Site.Get())
		f, ok := set.files[r.PathValue("name")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Type", f.ctype)
		// hashed names: a new logo is a new name, so old pages keep theirs and new ones get the new one
		h.Set("Cache-Control", "public, max-age=86400")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		if f.ctype == "image/svg+xml" {
			h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:; sandbox")
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(f.body))
	})
	mux.HandleFunc("GET "+gate.Prefix+"calendar.ics", rd.serveICS)
	mux.HandleFunc(gate.Prefix, http.NotFound) // nothing else of ours exists
	return mux
}

// expired reports whether theme artwork has stopped being served: the site is open and has been for
// longer than the theme's assets.expireAfterOpen.
func (rd *Renderer) expired() bool {
	if rd.Theme == nil || rd.Theme.ExpireAfterOpen == 0 {
		return false
	}
	v := rd.View()
	return v.Page == policy.Open && v.OpenedAt != nil && !rd.Now().Before(v.OpenedAt.Add(rd.Theme.ExpireAfterOpen))
}

// Public is the share image and the favicon of the theme, if it has them.
func (rd *Renderer) Public() []string {
	var out []string
	if rd.Theme == nil {
		return nil
	}
	for _, p := range []string{rd.Theme.Share.Image, rd.Theme.Favicon} {
		if p == "" {
			continue
		}
		if u, err := rd.Theme.Assets.URL(p); err == nil {
			out = append(out, u)
		}
	}
	return out
}

// Favicon serves the favicon: the theme's, the site's, or the edge's own.
func (rd *Renderer) Favicon(w http.ResponseWriter, r *http.Request) {
	if rd.Theme != nil && rd.Theme.Favicon != "" {
		u, _ := rd.Theme.Assets.URL(rd.Theme.Favicon)
		rd.Theme.Assets.Serve(w, r, strings.TrimPrefix(u, rd.Theme.Assets.Prefix()), true)
		return
	}
	set := rd.siteAssets.get(rd.Site.Get())
	if set.favicon != "" {
		f := set.files[strings.TrimPrefix(set.favicon, gate.Prefix+"site/")]
		w.Header().Set("Content-Type", f.ctype)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(f.body))
		return
	}
	u, _ := rd.Default.Assets.URL("favicon.svg")
	rd.Default.Assets.Serve(w, r, strings.TrimPrefix(u, rd.Default.Assets.Prefix()), true)
}

// serveICS hands out a one-event calendar file for the opening. Generated from the policy, so it is
// always right and nothing is stored. 404 when nothing is scheduled.
func (rd *Renderer) serveICS(w http.ResponseWriter, r *http.Request) {
	v := rd.View()
	if v.Access == nil || v.Access.State != policy.Scheduled || v.Page == policy.Open {
		http.NotFound(w, r)
		return
	}
	cp := rd.Site.Get()
	p := rd.plan(gate.PageCountdown)
	lang := pickLang(r, p.candidates, cp.Site.DefaultLocale)
	d := &Data{cp: cp, layers: rd.layers(p, lang), whenText: formatWhen(*v.Access.OpensAt, rd.Location, lang)}
	if d.siteName = cp.Site.Name; d.siteName == "" {
		d.siteName = d.text("site.fallbackName")
	}
	base := rd.baseURL(r)
	host := strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
	utc := func(x time.Time) string { return x.UTC().Format("20060102T150405Z") }
	at := *v.Access.OpensAt
	summary := d.text("ics.summary")

	lines := []string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//Mosaicast//edge//" + strings.ToUpper(lang),
		"CALSCALE:GREGORIAN",
		"METHOD:PUBLISH",
		"BEGIN:VEVENT",
		"UID:opening-" + strconv.FormatInt(at.Unix(), 10) + "@" + host,
		"DTSTAMP:" + utc(rd.Now()),
		"DTSTART:" + utc(at),
		"DTEND:" + utc(at.Add(time.Hour)),
		"SUMMARY:" + icsEscape(summary),
		"DESCRIPTION:" + icsEscape(d.text("ics.description")+"\n"+base+"/"),
		"URL:" + base + "/",
		"BEGIN:VALARM",
		"ACTION:DISPLAY",
		"TRIGGER:-PT15M",
		"DESCRIPTION:" + icsEscape(summary),
		"END:VALARM",
		"END:VEVENT",
		"END:VCALENDAR",
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(icsFold(l))
		b.WriteString("\r\n")
	}
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="opening.ics"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(b.String()))
}

func icsEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\n", `\n`, "\r", "").Replace(s)
}

// icsFold wraps content lines at 75 octets as RFC 5545 asks, without splitting a UTF-8 sequence.
func icsFold(line string) string {
	if len(line) <= 75 {
		return line
	}
	var b strings.Builder
	n := 0
	for _, r := range line {
		size := len(string(r))
		if n+size > 75 {
			b.WriteString("\r\n ")
			n = 1
		}
		b.WriteRune(r)
		n += size
	}
	return b.String()
}
