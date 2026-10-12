// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

// Package sitecopy keeps the edge's copy of the site's look and legal pages (§18.2): name, light/dark
// tokens, mode policy, logo, favicon and the legal pages per locale. The edge's pages render from this
// copy, because the page they matter most for is the one shown while core is down.
package sitecopy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Mosaicast/mosaicast-edge/internal/core"
	"github.com/Mosaicast/mosaicast-edge/internal/fsutil"
)

// Copy is one consistent snapshot. The zero value (no core seen yet, nothing cached) is valid: pages
// then fall back to the edge's built-in look.
type Copy struct {
	FetchedAt time.Time            `json:"fetchedAt"`
	Site      Site                 `json:"site"`
	Logo      *Image               `json:"logo,omitempty"`
	DarkLogo  *Image               `json:"darkLogo,omitempty"`
	Favicon   *Image               `json:"favicon,omitempty"`
	Legal     map[string][]Entry   `json:"legal,omitempty"` // locale -> footer entries, in core's order
	Pages     map[string]PageCache `json:"pages,omitempty"` // locale -> slug -> page
}

// PageCache is the legal pages of one locale by slug.
type PageCache map[string]Page

// Site is the part of GET /api/site the edge uses.
type Site struct {
	Name          string            `json:"name"`
	ModePolicy    string            `json:"modePolicy"` // light | dark | system
	DefaultLocale string            `json:"defaultLocale"`
	Light         map[string]string `json:"light"` // --mc-* name (without --mc-) -> #rrggbb
	Dark          map[string]string `json:"dark"`
}

// Image is a branding image as core served it.
type Image struct {
	Type string `json:"type"`
	Body []byte `json:"body"`
}

// Entry is a legal footer link.
type Entry struct {
	Slug  string `json:"slug"`
	Title string `json:"title"`
	Role  string `json:"role,omitempty"` // imprint | privacy | terms | …
}

// Page is a rendered legal page; HTML is core's sanitized rendering of the markdown (§12.6).
type Page struct {
	Title string `json:"title"`
	Role  string `json:"role,omitempty"`
	HTML  string `json:"html"`
}

// tokenNames maps core's ThemeTokenSet fields to the shell's custom properties (§12.3).
var tokenNames = map[string]string{
	"bg": "bg", "surface": "surface", "text": "text", "textMuted": "text-muted", "accent": "accent",
	"accentContrast": "accent-contrast", "border": "border", "accent2": "accent-2", "accentText": "accent-text",
}

var (
	hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	slugRe   = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	localeRe = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)
)

var imageTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/webp": true, "image/gif": true, "image/avif": true,
	"image/svg+xml": true, "image/x-icon": true, "image/vnd.microsoft.icon": true,
}

const (
	maxImage    = 2 << 20
	maxJSON     = 256 << 10
	maxLegal    = 1 << 20
	maxPages    = 50
	maxSiteName = 200
)

// Keeper owns the copy: it refreshes it from core and persists it to the cache dir.
type Keeper struct {
	Client  *core.Client
	Locales []string // the locales the edge speaks; the site's default is added to them
	Now     func() time.Time
	file    string

	mu      sync.RWMutex
	cur     *Copy
	refresh sync.Mutex // one refresh at a time
}

// NewKeeper loads the cached copy from dir, if any.
func NewKeeper(client *core.Client, locales []string, now func() time.Time, dir string) *Keeper {
	k := &Keeper{Client: client, Locales: locales, Now: now, cur: &Copy{}}
	if dir == "" {
		return k
	}
	k.file = filepath.Join(dir, "site.json")
	body, err := os.ReadFile(k.file)
	if err == nil {
		var c Copy
		if err := json.Unmarshal(body, &c); err == nil && validSite(&c.Site) == nil {
			k.cur = &c
			slog.Info("site copy loaded from cache", "fetchedAt", c.FetchedAt.Format(time.RFC3339))
		} else {
			slog.Warn("site copy cache ignored", "file", k.file)
		}
	} else if !os.IsNotExist(err) {
		slog.Warn("site copy cache unreadable", "file", k.file, "err", err)
	}
	return k
}

// Get returns the current copy. Callers must not modify it.
func (k *Keeper) Get() *Copy {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.cur
}

// Run refreshes now and then every interval until ctx ends.
func (k *Keeper) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		k.Refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Refresh fetches a new copy. Sections that fail keep their previous version, so a flaky core never
// replaces a good copy with a worse one.
func (k *Keeper) Refresh(ctx context.Context) {
	k.refresh.Lock()
	defer k.refresh.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	prev := k.Get()
	next := *prev
	var errs []error

	site, err := k.fetchSite(ctx)
	if err != nil {
		errs = append(errs, err)
	} else {
		next.Site = site.Site
		next.Logo, next.DarkLogo, next.Favicon = site.Logo, site.DarkLogo, site.Favicon
		next.FetchedAt = k.Now()
	}

	legal, pages, err := k.fetchLegal(ctx, next.Site.DefaultLocale)
	if err != nil {
		errs = append(errs, err)
	} else {
		next.Legal, next.Pages = legal, pages
	}

	if len(errs) == 2 {
		slog.Warn("site copy not refreshed; keeping the previous one", "err", errors.Join(errs...))
		return
	}
	if len(errs) > 0 {
		slog.Warn("site copy partly refreshed", "err", errors.Join(errs...))
	}
	k.mu.Lock()
	k.cur = &next
	k.mu.Unlock()
	k.persist(&next)
}

func (k *Keeper) fetchSite(ctx context.Context) (*Copy, error) {
	body, _, err := k.Client.Fetch(ctx, "/api/site", maxJSON)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Name          string `json:"name"`
		ModePolicy    string `json:"modePolicy"`
		DefaultLocale string `json:"defaultLocale"`
		Theme         struct {
			Light map[string]string `json:"light"`
			Dark  map[string]string `json:"dark"`
		} `json:"theme"`
		Branding struct {
			Logo     string `json:"logo"`
			Favicon  string `json:"favicon"`
			DarkLogo string `json:"darkLogo"`
		} `json:"branding"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("/api/site: %w", err)
	}
	c := &Copy{Site: Site{
		Name:          raw.Name,
		ModePolicy:    raw.ModePolicy,
		DefaultLocale: raw.DefaultLocale,
		Light:         tokens(raw.Theme.Light),
		Dark:          tokens(raw.Theme.Dark),
	}}
	if err := validSite(&c.Site); err != nil {
		return nil, fmt.Errorf("/api/site: %w", err)
	}
	if c.Logo, err = k.fetchImage(ctx, raw.Branding.Logo); err != nil {
		return nil, err
	}
	if c.Favicon, err = k.fetchImage(ctx, raw.Branding.Favicon); err != nil {
		return nil, err
	}
	if c.DarkLogo, err = k.fetchImage(ctx, raw.Branding.DarkLogo); err != nil {
		return nil, err
	}
	return c, nil
}

// fetchImage loads a branding image. Only core's own /branding/ paths are followed, never a URL the
// payload names elsewhere.
func (k *Keeper) fetchImage(ctx context.Context, ref string) (*Image, error) {
	if ref == "" {
		return nil, nil
	}
	if !strings.HasPrefix(ref, "/branding/") || strings.Contains(ref, "..") {
		return nil, fmt.Errorf("branding path %q is not under /branding/", ref)
	}
	body, ctype, err := k.Client.Fetch(ctx, ref, maxImage)
	if errors.Is(err, core.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	mt, _, _ := mime.ParseMediaType(ctype)
	if !imageTypes[mt] {
		return nil, fmt.Errorf("%s: content type %q is not an image type the edge serves", ref, ctype)
	}
	return &Image{Type: mt, Body: body}, nil
}

func (k *Keeper) fetchLegal(ctx context.Context, siteDefault string) (map[string][]Entry, map[string]PageCache, error) {
	locales := append([]string(nil), k.Locales...)
	if siteDefault != "" && !contains(locales, siteDefault) {
		locales = append(locales, siteDefault)
	}
	legal := map[string][]Entry{}
	pages := map[string]PageCache{}
	for _, loc := range locales {
		body, _, err := k.Client.Fetch(ctx, "/api/legal?locale="+url.QueryEscape(loc), maxJSON)
		if err != nil {
			return nil, nil, fmt.Errorf("/api/legal: %w", err)
		}
		var entries []Entry
		if err := json.Unmarshal(body, &entries); err != nil {
			return nil, nil, fmt.Errorf("/api/legal: %w", err)
		}
		if len(entries) > maxPages {
			entries = entries[:maxPages]
		}
		pc := PageCache{}
		kept := entries[:0]
		for _, e := range entries {
			if !slugRe.MatchString(e.Slug) || len(e.Slug) > 64 {
				continue // a slug no link can reach; core refuses these, so this is belt and braces
			}
			body, _, err := k.Client.Fetch(ctx, "/api/legal/"+e.Slug+"?locale="+url.QueryEscape(loc), maxLegal)
			if err != nil {
				return nil, nil, fmt.Errorf("/api/legal/%s: %w", e.Slug, err)
			}
			var p Page
			if err := json.Unmarshal(body, &p); err != nil {
				return nil, nil, fmt.Errorf("/api/legal/%s: %w", e.Slug, err)
			}
			pc[e.Slug] = p
			kept = append(kept, e)
		}
		legal[loc], pages[loc] = kept, pc
	}
	return legal, pages, nil
}

func (k *Keeper) persist(c *Copy) {
	if k.file == "" {
		return
	}
	body, err := json.Marshal(c)
	if err == nil {
		err = fsutil.WriteAtomic(k.file, body)
	}
	if err != nil {
		slog.Warn("site copy cache not written", "file", k.file, "err", err)
	}
}

// tokens keeps the known token names, under their --mc-* names, and only well-formed colours: these
// values end up in a stylesheet.
func tokens(in map[string]string) map[string]string {
	out := map[string]string{}
	for field, name := range tokenNames {
		if v, ok := in[field]; ok && hexColor.MatchString(v) {
			out[name] = strings.ToLower(v)
		}
	}
	return out
}

func validSite(s *Site) error {
	switch s.ModePolicy {
	case "light", "dark", "system":
	case "":
		s.ModePolicy = "system"
	default:
		return fmt.Errorf("unknown mode policy %q", s.ModePolicy)
	}
	if len(s.Name) > maxSiteName {
		return errors.New("site name too long")
	}
	if s.DefaultLocale != "" && !localeRe.MatchString(s.DefaultLocale) {
		return fmt.Errorf("default locale %q is not a language tag", s.DefaultLocale)
	}
	for _, m := range []map[string]string{s.Light, s.Dark} {
		for k, v := range m {
			if !hexColor.MatchString(v) {
				delete(m, k)
			}
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Legal returns the footer entries for lang, falling back to the site default and then to any locale.
func (c *Copy) LegalFor(lang string) (string, []Entry) {
	for _, l := range []string{lang, c.Site.DefaultLocale, "en"} {
		if e, ok := c.Legal[l]; ok {
			return l, e
		}
	}
	for l, e := range c.Legal {
		return l, e
	}
	return lang, nil
}

// Page returns a legal page in lang, with the same fallback; ok is false when no locale has it.
func (c *Copy) Page(slug, lang string) (Page, bool) {
	for _, l := range []string{lang, c.Site.DefaultLocale, "en"} {
		if p, ok := c.Pages[l][slug]; ok {
			return p, true
		}
	}
	for _, pc := range c.Pages {
		if p, ok := pc[slug]; ok {
			return p, true
		}
	}
	return Page{}, false
}
