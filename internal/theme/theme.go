// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

// Package theme loads and validates edge themes: a theme.json manifest, templates, texts and hashed
// assets (docs/THEMING.md). The edge's own default look is a theme in the same format, embedded.
package theme

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

// SchemaVersion is the theme.json schema this edge understands.
const SchemaVersion = 1

// EdgeVersion is this edge's version, named in errors about theme.json keys it does not know. The
// command sets it from the VERSION file at startup.
var EdgeVersion = "dev"

// Pages a theme may provide, in the order THEMING.md lists them.
var Pages = []string{"countdown", "maintenance", "closed", "unavailable", "login", "legal"}

// Modes of a page entry.
const (
	ModeFull    = "full"    // the theme ships the page: its template, its assets
	ModeRestyle = "restyle" // the edge's default page with the theme's stylesheets, scripts and texts
)

// Manifest is theme.json.
type Manifest struct {
	Schema    int                 `json:"schema"`
	Name      string              `json:"name"`
	Version   string              `json:"version"`
	Locales   []string            `json:"locales"`
	Texts     string              `json:"texts"`
	Styles    []string            `json:"styles"`
	Scripts   []string            `json:"scripts"`
	Favicon   string              `json:"favicon"`
	Pages     map[string]PageSpec `json:"pages"`
	Countdown Countdown           `json:"countdown"`
	Share     Share               `json:"share"`
	Assets    AssetPolicy         `json:"assets"`
	Credits   []Credit            `json:"credits"`
}

// PageSpec says how a theme provides one page.
type PageSpec struct {
	Mode     string `json:"mode"`
	Template string `json:"template"`
}

// Countdown tunes the countdown's runtime. The final phase is fixed at ten seconds: it is what core's
// "open now" (now + 12 s) is built around.
type Countdown struct {
	PartySeconds   *int               `json:"partySeconds"`
	HandoffSeconds *float64           `json:"handoffSeconds"`
	PreloadSeconds *int               `json:"preloadSeconds"`
	Cues           map[string]float64 `json:"cues"`
}

// Share is the link-preview card.
type Share struct {
	Image string `json:"image"`
}

// AssetPolicy protects a theme's artwork.
type AssetPolicy struct {
	ExpireAfterOpen string   `json:"expireAfterOpen"`
	Keep            []string `json:"keep"`
}

// Credit is one attribution, shown on every page the theme provides.
type Credit struct {
	Work    string `json:"work"`
	Author  string `json:"author"`
	License string `json:"license"`
	URL     string `json:"url"`
}

// Theme is a loaded, validated theme.
type Theme struct {
	Manifest
	Assets    *Assets
	Templates *template.Template
	Texts     map[string]map[string]string // locale -> key -> text

	PartySeconds    int
	HandoffSeconds  float64
	PreloadSeconds  int
	ExpireAfterOpen time.Duration // 0 = never
	keep            map[string]bool
}

var (
	localeRe = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)
	cueRe    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	keyRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// Load reads a theme from fsys (the theme's root folder). assetPrefix is where its assets are served.
// Every problem found is returned at once, so a theme author fixes them in one go.
func Load(fsys fs.FS, assetPrefix string) (*Theme, error) {
	// Nothing in a theme may be a symbolic link: a link can point outside the theme folder, and what
	// the edge serves and renders must be exactly what check-theme looked at.
	var links []string
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type()&fs.ModeSymlink != 0 {
			links = append(links, p)
		}
		return nil
	})
	if len(links) > 0 {
		return nil, fmt.Errorf("symbolic links are not allowed in a theme: %s", strings.Join(links, ", "))
	}
	body, err := fs.ReadFile(fsys, "theme.json")
	if err != nil {
		return nil, fmt.Errorf("theme.json: %w", err)
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields() // a misspelt key must not silently do nothing
	if err := dec.Decode(&m); err != nil {
		if key, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
			// strict on purpose, but say why: a key a newer edge added is unknown to this one
			return nil, fmt.Errorf("theme.json: unknown key %s (misspelt, or this theme needs a newer mosaicast-edge than %s)", key, EdgeVersion)
		}
		return nil, fmt.Errorf("theme.json: %w", err)
	}

	t := &Theme{Manifest: m, keep: map[string]bool{}}
	var errs []error
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if m.Schema != SchemaVersion {
		fail("theme.json: schema is %d, this edge understands %d", m.Schema, SchemaVersion)
	}
	if strings.TrimSpace(m.Name) == "" {
		fail("theme.json: name is required")
	}
	if len(m.Locales) == 0 {
		fail("theme.json: locales must name at least one language")
	}
	seen := map[string]bool{}
	for _, l := range m.Locales {
		if !localeRe.MatchString(l) || seen[l] {
			fail("theme.json: locale %q is not a language tag or is listed twice", l)
		}
		seen[l] = true
	}

	if sub, err := fs.Sub(fsys, "assets"); err != nil {
		fail("assets: %v", err)
	} else if _, statErr := fs.Stat(fsys, "assets"); statErr != nil {
		t.Assets = &Assets{prefix: assetPrefix, names: map[string]string{}, files: map[string]*asset{}}
	} else if t.Assets, err = loadAssets(sub, assetPrefix); err != nil {
		fail("%v", err)
	}
	if t.Assets == nil {
		return nil, errors.Join(errs...)
	}

	for _, p := range m.Styles {
		if path.Ext(p) != ".css" || !t.Assets.Has(p) {
			fail("theme.json: styles: %q is not a stylesheet under assets/", p)
		}
	}
	for _, p := range m.Scripts {
		if path.Ext(p) != ".js" || !t.Assets.Has(p) {
			fail("theme.json: scripts: %q is not a script under assets/", p)
		}
	}
	if m.Favicon != "" {
		if !t.Assets.Has(m.Favicon) {
			fail("theme.json: favicon: %q is not under assets/", m.Favicon)
		}
		t.keep[m.Favicon] = true
	}
	if m.Share.Image != "" {
		switch path.Ext(m.Share.Image) {
		case ".jpg", ".jpeg", ".png", ".webp":
		default:
			fail("theme.json: share.image must be a JPEG, PNG or WebP")
		}
		if !t.Assets.Has(m.Share.Image) {
			fail("theme.json: share.image: %q is not under assets/", m.Share.Image)
		}
		t.keep[m.Share.Image] = true
	}

	// templates: every templates/*.html, parsed as one set so pages can share partials
	var tmplFiles []string
	if entries, err := fs.ReadDir(fsys, "templates"); err == nil {
		for _, e := range entries {
			if !e.IsDir() && path.Ext(e.Name()) == ".html" {
				tmplFiles = append(tmplFiles, "templates/"+e.Name())
			}
		}
	}
	sort.Strings(tmplFiles)
	if len(tmplFiles) > 0 {
		t.Templates, err = template.New("").Option("missingkey=error").ParseFS(fsys, tmplFiles...)
		if err != nil {
			fail("templates: %v", err)
		}
	}

	for name, spec := range m.Pages {
		if !contains(Pages, name) {
			fail("theme.json: pages: unknown page %q (pages are %s)", name, strings.Join(Pages, ", "))
			continue
		}
		switch spec.Mode {
		case ModeFull:
			if spec.Template == "" {
				fail("theme.json: pages.%s: mode full needs a template", name)
			} else if t.Templates == nil || t.Templates.Lookup(spec.Template) == nil {
				fail("theme.json: pages.%s: template %q is not a file in templates/", name, spec.Template)
			}
		case ModeRestyle:
			if spec.Template != "" {
				fail("theme.json: pages.%s: a restyled page has no template (use mode full)", name)
			}
		default:
			fail("theme.json: pages.%s: mode must be %q or %q", name, ModeFull, ModeRestyle)
		}
	}

	// texts
	t.Texts = map[string]map[string]string{}
	pattern := m.Texts
	if pattern == "" {
		pattern = "texts/{lang}.json"
	}
	if !strings.Contains(pattern, "{lang}") {
		fail("theme.json: texts must contain {lang}, e.g. texts/{lang}.json")
	} else {
		for _, l := range m.Locales {
			p := strings.ReplaceAll(pattern, "{lang}", l)
			body, err := fs.ReadFile(fsys, p)
			if errors.Is(err, fs.ErrNotExist) && m.Texts == "" {
				continue // a theme without texts of its own uses the edge's
			}
			if err != nil {
				fail("texts: %v", err)
				continue
			}
			var tx map[string]string
			if err := json.Unmarshal(body, &tx); err != nil {
				fail("%s: %v (a flat object of strings)", p, err)
				continue
			}
			for k := range tx {
				if !keyRe.MatchString(k) {
					fail("%s: key %q: use letters, digits, dot, dash and underscore", p, k)
				}
			}
			t.Texts[l] = tx
		}
		if err := SameKeys(t.Texts); err != nil {
			fail("texts: %v", err)
		}
	}

	// countdown
	c := m.Countdown
	t.PartySeconds = 15
	if c.PartySeconds != nil {
		if *c.PartySeconds < 0 || *c.PartySeconds > 120 {
			fail("theme.json: countdown.partySeconds must be between 0 and 120")
		}
		t.PartySeconds = *c.PartySeconds
	}
	t.HandoffSeconds = 1
	if c.HandoffSeconds != nil {
		if *c.HandoffSeconds < 0 || *c.HandoffSeconds > 5 {
			fail("theme.json: countdown.handoffSeconds must be between 0 and 5")
		}
		t.HandoffSeconds = *c.HandoffSeconds
	}
	t.PreloadSeconds = 60
	if c.PreloadSeconds != nil {
		if *c.PreloadSeconds < 0 || *c.PreloadSeconds > 3600 {
			fail("theme.json: countdown.preloadSeconds must be between 0 and 3600")
		}
		t.PreloadSeconds = *c.PreloadSeconds
	}
	for name, at := range c.Cues {
		if !cueRe.MatchString(name) {
			fail("theme.json: countdown.cues: name %q: lowercase letters, digits and dashes", name)
		}
		if at <= 0 || at > 3600 {
			fail("theme.json: countdown.cues.%s: seconds before opening, between 0 and 3600", name)
		}
	}

	// asset protection
	if raw := m.Assets.ExpireAfterOpen; raw != "" {
		d, err := time.ParseDuration(raw)
		switch {
		case err != nil || d < 0:
			fail("theme.json: assets.expireAfterOpen must be a duration like 1h or 24h")
		case d > 0 && d < time.Duration(t.PartySeconds)*time.Second+time.Minute:
			fail("theme.json: assets.expireAfterOpen must be at least the party plus a minute")
		}
		t.ExpireAfterOpen = d
	}
	for _, p := range m.Assets.Keep {
		if !t.Assets.Has(p) {
			fail("theme.json: assets.keep: %q is not under assets/", p)
		}
		t.keep[p] = true
	}

	for i, cr := range m.Credits {
		if cr.Work == "" {
			fail("theme.json: credits[%d]: work is required", i)
		}
		if cr.URL != "" && !strings.HasPrefix(cr.URL, "https://") {
			fail("theme.json: credits[%d]: url must be https://", i)
		}
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return t, nil
}

// Mode returns how the theme provides page ("" = it does not).
func (t *Theme) Mode(page string) string {
	if t == nil {
		return ""
	}
	return t.Pages[page].Mode
}

// Public reports whether a hashed asset stays reachable for everyone and forever: the share image,
// the favicon and whatever assets.keep lists.
func (t *Theme) Public(name string) bool {
	return t.keep[t.Assets.Source(name)]
}

// Shared reports whether a hashed asset is the share image or favicon, which other sites fetch.
func (t *Theme) Shared(name string) bool {
	src := t.Assets.Source(name)
	return src != "" && (src == t.Share.Image || src == t.Favicon)
}

// SameKeys reports locales whose key sets differ from the first locale's (sorted for stable output).
func SameKeys(texts map[string]map[string]string) error {
	locales := make([]string, 0, len(texts))
	for l := range texts {
		locales = append(locales, l)
	}
	sort.Strings(locales)
	if len(locales) < 2 {
		return nil
	}
	var errs []error
	ref := texts[locales[0]]
	for _, l := range locales[1:] {
		for k := range ref {
			if _, ok := texts[l][k]; !ok {
				errs = append(errs, fmt.Errorf("%s lacks %q (present in %s)", l, k, locales[0]))
			}
		}
		for k := range texts[l] {
			if _, ok := ref[k]; !ok {
				errs = append(errs, fmt.Errorf("%s has %q, which %s lacks", l, k, locales[0]))
			}
		}
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
	return errors.Join(errs...)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
