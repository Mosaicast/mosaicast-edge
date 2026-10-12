// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

package theme

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Assets serves a theme's files under content-hashed names only (/_a/t/3fa9c2e1b7d04a11.webp). Nothing
// is reachable under its real name, there are no listings, and the names say nothing about what a file
// shows. A changed file gets a new name, so every response can be cached forever.
type Assets struct {
	prefix string            // "/_a/t/" or "/_a/s/"
	names  map[string]string // "img/wall.webp" -> "3fa9c2e1b7d04a11.webp"
	files  map[string]*asset // hashed name -> content
}

type asset struct {
	body  []byte
	gz    []byte // set for text types worth compressing
	ctype string
	src   string // the real path, for keep-lists
}

// assetTypes is the whitelist of what a theme may ship, with the type each is served as.
var assetTypes = map[string]string{
	".css": "text/css; charset=utf-8", ".js": "text/javascript; charset=utf-8",
	".woff2": "font/woff2", ".woff": "font/woff", ".ttf": "font/ttf", ".otf": "font/otf",
	".webp": "image/webp", ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".avif": "image/avif", ".svg": "image/svg+xml", ".ico": "image/x-icon",
	".mp3": "audio/mpeg", ".ogg": "audio/ogg", ".opus": "audio/ogg", ".webm": "video/webm", ".mp4": "video/mp4",
}

// Limits on what a theme may ship. The edge holds every asset in memory, and a launch page has no
// business shipping print-resolution artwork.
const (
	MaxAssetBytes = 4 << 20
	MaxThemeBytes = 24 << 20
	MaxAssets     = 400
)

var cssURL = regexp.MustCompile(`url\(\s*(['"]?)([^'")]*)(['"]?)\s*\)`)

// loadAssets reads every file under fsys (a theme's assets/ folder), hashes it and rewrites the url()s
// in its stylesheets to hashed names. An unknown reference fails the load: it would 404 at runtime.
func loadAssets(fsys fs.FS, prefix string) (*Assets, error) {
	raw := map[string][]byte{}
	total := 0
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasPrefix(path.Base(p), ".") {
			return nil // .DS_Store, editor droppings
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("assets/%s: not a regular file (symlinks are not followed)", p)
		}
		if _, ok := assetTypes[strings.ToLower(path.Ext(p))]; !ok {
			return fmt.Errorf("assets/%s: file type %q is not allowed in a theme", p, path.Ext(p))
		}
		body, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		if len(body) > MaxAssetBytes {
			return fmt.Errorf("assets/%s: %d bytes, more than the %d a theme asset may have", p, len(body), MaxAssetBytes)
		}
		total += len(body)
		raw[p] = body
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxAssets {
		return nil, fmt.Errorf("assets: %d files, more than the %d a theme may have", len(raw), MaxAssets)
	}
	if total > MaxThemeBytes {
		return nil, fmt.Errorf("assets: %d bytes in total, more than the %d a theme may have", total, MaxThemeBytes)
	}

	a := &Assets{prefix: prefix, names: map[string]string{}, files: map[string]*asset{}}
	add := func(p string, body []byte) {
		sum := sha256.Sum256(body)
		ext := strings.ToLower(path.Ext(p))
		name := hex.EncodeToString(sum[:8]) + ext
		a.names[p] = name
		a.files[name] = &asset{body: body, ctype: assetTypes[ext], src: p}
	}

	// Stylesheets point at fonts and images by relative path: hash everything else first, then
	// rewrite those references to the hashed names and hash the result. The rewritten URLs are absolute
	// paths: a url() inside a custom property may be resolved against whichever stylesheet uses the
	// variable (the edge's own, under another prefix), so a bare name would not be safe.
	var sheets []string
	for p, body := range raw {
		if path.Ext(p) == ".css" {
			sheets = append(sheets, p)
			continue
		}
		add(p, body)
	}
	sort.Strings(sheets)
	for _, p := range sheets {
		css := string(raw[p])
		if strings.Contains(css, "@import") {
			return nil, fmt.Errorf("assets/%s: @import is not supported; list each stylesheet in theme.json", p)
		}
		var bad error
		css = cssURL.ReplaceAllStringFunc(css, func(m string) string {
			ref := cssURL.FindStringSubmatch(m)[2]
			if strings.HasPrefix(ref, "data:") || strings.HasPrefix(ref, "#") {
				return m
			}
			if strings.Contains(ref, "://") || strings.HasPrefix(ref, "//") || strings.HasPrefix(ref, "/") {
				bad = fmt.Errorf("assets/%s: url(%s) leaves the theme; themes are self-contained", p, ref)
				return m
			}
			clean := strings.SplitN(strings.SplitN(ref, "?", 2)[0], "#", 2)[0]
			target := path.Join(path.Dir(p), clean)
			name, ok := a.names[target]
			if !ok {
				bad = fmt.Errorf("assets/%s references unknown asset %q", p, ref)
				return m
			}
			if i := strings.IndexByte(ref, '#'); i >= 0 {
				name += ref[i:] // keep SVG fragment identifiers
			}
			return "url(" + prefix + name + ")"
		})
		if bad != nil {
			return nil, bad
		}
		add(p, []byte(css))
	}

	for _, f := range a.files {
		if strings.HasPrefix(f.ctype, "text/") || strings.HasPrefix(f.ctype, "image/svg") {
			var buf bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			_, _ = zw.Write(f.body)
			_ = zw.Close()
			if buf.Len() < len(f.body) {
				f.gz = buf.Bytes()
			}
		}
	}
	return a, nil
}

// URL is the public path of a file, by its path under assets/.
func (a *Assets) URL(p string) (string, error) {
	name, ok := a.names[p]
	if !ok {
		return "", fmt.Errorf("unknown asset %q", p)
	}
	return a.prefix + name, nil
}

// Has reports whether p is an asset of this theme.
func (a *Assets) Has(p string) bool { _, ok := a.names[p]; return ok }

// Prefix is where these assets are served.
func (a *Assets) Prefix() string { return a.prefix }

// Source is the real path of a hashed name ("" if unknown).
func (a *Assets) Source(name string) string {
	if f, ok := a.files[name]; ok {
		return f.src
	}
	return ""
}

// Serve writes the asset with the hashed name, or 404.
func (a *Assets) Serve(w http.ResponseWriter, r *http.Request, name string, public bool) {
	f, ok := a.files[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	// Other sites may not embed these files and search engines may not index them, except what is
	// meant to be fetched from elsewhere (the share image for link previews, the favicon).
	h.Set("X-Robots-Tag", "noindex, noimageindex")
	if public {
		h.Set("Cross-Origin-Resource-Policy", "cross-origin")
	} else {
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
	}
	if strings.HasPrefix(f.ctype, "image/svg") {
		// an SVG opened on its own is a document: no script, no plugins
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src data:; sandbox")
	}
	h.Set("Content-Type", f.ctype)
	if f.gz != nil {
		h.Set("Vary", "Accept-Encoding")
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") && r.Header.Get("Range") == "" {
			h.Set("Content-Encoding", "gzip")
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(f.gz))
			return
		}
	}
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(f.body))
}

// Count is the number of assets.
func (a *Assets) Count() int { return len(a.files) }
