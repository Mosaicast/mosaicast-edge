// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

package themesrc

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

type entry struct {
	name string
	typ  byte
	data string
	link string
	pax  map[string]string
}

func tgz(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Typeflag: typ, Mode: 0o644, Size: int64(len(e.data)), Linkname: e.link, PAXRecords: e.pax}
		if typ != tar.TypeReg {
			h.Size = 0
		}
		if e.pax != nil {
			h.Format = tar.FormatPAX
		}
		if typ == tar.TypeXGlobalHeader {
			h = &tar.Header{Typeflag: typ, PAXRecords: e.pax, Format: tar.FormatPAX}
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			_, _ = tw.Write([]byte(e.data))
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

const manifest = `{"schema":1,"name":"T","locales":["en"],"pages":{"closed":{"mode":"restyle"}}}`

func TestUnpackGood(t *testing.T) {
	for name, body := range map[string][]byte{
		"flat":      tgz(t, entry{name: "theme.json", data: manifest}, entry{name: "texts/en.json", data: "{}"}),
		"wrapped":   tgz(t, entry{name: "wrap/", typ: tar.TypeDir}, entry{name: "wrap/theme.json", data: manifest}, entry{name: "wrap/texts/en.json", data: "{}"}),
		"dot-slash": tgz(t, entry{name: "./theme.json", data: manifest}, entry{name: "./texts/en.json", data: "{}"}),
		"hidden":    tgz(t, entry{name: "theme.json", data: manifest}, entry{name: ".DS_Store", data: "x"}, entry{name: "texts/en.json", data: "{}"}),
	} {
		fsys, err := Unpack(body)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if b, err := fs.ReadFile(fsys, "texts/en.json"); err != nil || string(b) != "{}" {
			t.Errorf("%s: texts/en.json: %q %v", name, b, err)
		}
		if _, err := fs.Stat(fsys, ".DS_Store"); err == nil {
			t.Errorf("%s: hidden files are left out", name)
		}
	}
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	w, _ := zw.Create("theme/theme.json")
	_, _ = w.Write([]byte(manifest))
	_ = zw.Close()
	if fsys, err := Unpack(zbuf.Bytes()); err != nil {
		t.Errorf("zip: %v", err)
	} else if _, err := fs.Stat(fsys, "theme.json"); err != nil {
		t.Error("zip: wrapper folder not stripped")
	}
}

func TestUnpackRefuses(t *testing.T) {
	ok := entry{name: "theme.json", data: manifest}
	big := strings.Repeat("x", 4<<20+1)
	cases := map[string][]byte{
		"symlink":        tgz(t, ok, entry{name: "texts/en.json", typ: tar.TypeSymlink, link: "/etc/passwd"}),
		"hardlink":       tgz(t, ok, entry{name: "texts/en.json", typ: tar.TypeLink, link: "theme.json"}),
		"device":         tgz(t, ok, entry{name: "dev", typ: tar.TypeChar}),
		"fifo":           tgz(t, ok, entry{name: "pipe", typ: tar.TypeFifo}),
		"hidden link":    tgz(t, ok, entry{name: ".x", typ: tar.TypeSymlink, link: "/"}),
		"global header":  tgz(t, entry{name: "pax_global_header", typ: tar.TypeXGlobalHeader, pax: map[string]string{"comment": "x"}}, ok),
		"odd pax":        tgz(t, ok, entry{name: "a.txt", data: "a", pax: map[string]string{"comment": "unexpected"}}),
		"dotdot":         tgz(t, ok, entry{name: "../evil", data: "x"}),
		"inner dotdot":   tgz(t, ok, entry{name: "texts/../../evil", data: "x"}),
		"absolute":       tgz(t, ok, entry{name: "/etc/evil", data: "x"}),
		"backslash":      tgz(t, ok, entry{name: `assets\evil`, data: "x"}),
		"duplicate":      tgz(t, ok, entry{name: "./theme.json", data: manifest}),
		"file too big":   tgz(t, ok, entry{name: "assets/big.webp", data: big}),
		"not an archive": []byte("hello"),
		"no theme.json":  tgz(t, entry{name: "a/x", data: "1"}, entry{name: "b/y", data: "2"}),
	}
	for name, body := range cases {
		if _, err := Unpack(body); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// a gzip bomb hidden in an entry that would be skipped still stops at the limit
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "theme.json", Typeflag: tar.TypeReg, Size: int64(len(manifest)), Mode: 0o644})
	_, _ = tw.Write([]byte(manifest))
	_ = tw.WriteHeader(&tar.Header{Name: ".hidden/huge", Typeflag: tar.TypeReg, Size: 64 << 20, Mode: 0o644})
	zero := make([]byte, 1<<20)
	for i := 0; i < 64; i++ {
		_, _ = tw.Write(zero)
	}
	_ = tw.Close()
	_ = gz.Close()
	if buf.Len() > MaxArchiveBytes {
		t.Fatalf("bomb fixture too large compressed: %d", buf.Len())
	}
	if _, err := Unpack(buf.Bytes()); err == nil || !strings.Contains(err.Error(), "unpacks to more than") {
		t.Errorf("bomb in a skipped entry: %v", err)
	}
	// zip symlink
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	w, _ := zw.Create("theme.json")
	_, _ = w.Write([]byte(manifest))
	h := &zip.FileHeader{Name: "texts/en.json"}
	h.SetMode(fs.ModeSymlink | 0o777)
	w, _ = zw.CreateHeader(h)
	_, _ = w.Write([]byte("/etc/passwd"))
	_ = zw.Close()
	if _, err := Unpack(zbuf.Bytes()); err == nil {
		t.Error("zip symlink accepted")
	}
}

func theArchive(t *testing.T) ([]byte, string) {
	body := tgz(t, entry{name: "theme.json", data: manifest}, entry{name: "texts/en.json", data: "{}"})
	return body, digest(body)
}

func TestLocal(t *testing.T) {
	dir := t.TempDir()
	body, sum := theArchive(t)
	file := filepath.Join(dir, "t.tgz")
	_ = os.WriteFile(file, body, 0o644)
	if src, err := Resolve(context.Background(), file+"#sha256:"+sum, Options{}); err != nil || src.Dir != "" {
		t.Fatalf("pinned local archive: %v", err)
	}
	if _, err := Resolve(context.Background(), file+"#sha256:"+strings.Repeat("0", 64), Options{}); err == nil {
		t.Error("wrong pin accepted")
	}
	if src, err := Resolve(context.Background(), dir, Options{}); err != nil || src.Dir != dir {
		t.Errorf("folder: %v", err)
	}
	if _, err := Resolve(context.Background(), dir+"#sha256:"+sum, Options{}); err == nil {
		t.Error("a pin on a folder means nothing and is refused")
	}
	if _, err := Resolve(context.Background(), "./missing/theme", Options{}); err == nil || !strings.Contains(err.Error(), "no such") {
		t.Errorf("missing path: %v", err)
	}
	if _, err := Resolve(context.Background(), "http://example.org/theme.tgz#sha256:"+sum, Options{}); err == nil {
		t.Error("plain http across a network accepted")
	}
}

func TestDownloadPinnedCacheAndTamper(t *testing.T) {
	body, sum := theArchive(t)
	var hits atomic.Int64
	up := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !up {
			http.Error(w, "down", 503)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	cache := t.TempDir()
	o := Options{CacheDir: cache}
	spec := srv.URL + "/theme.tgz"

	if _, err := Resolve(context.Background(), spec, o); err == nil || !strings.Contains(err.Error(), "no #sha256 pin") {
		t.Fatalf("unpinned download: %v", err)
	}
	if _, err := Resolve(context.Background(), spec+"#sha256:"+strings.Repeat("a", 64), o); err == nil {
		t.Fatal("wrong pin accepted")
	}
	src, err := Resolve(context.Background(), spec+"#sha256:"+sum, o)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(src.Desc, "cached") {
		t.Error("first start must download")
	}
	// a restart with the server gone: from the cache, without asking
	up = false
	before := hits.Load()
	src, err = Resolve(context.Background(), spec+"#sha256:"+sum, o)
	if err != nil || !strings.Contains(src.Desc, "cached") || hits.Load() != before {
		t.Fatalf("cached restart: %v %q, %d requests", err, src.Desc, hits.Load()-before)
	}
	// a different spec with the same pin does not reuse that cache entry
	if _, err := Resolve(context.Background(), srv.URL+"/other.tgz#sha256:"+sum, o); err == nil {
		t.Error("another spec served from the first one's cache")
	}
	// the cache is a volume: an archive changed there is refused, not loaded and not fetched over
	files, _ := filepath.Glob(filepath.Join(cache, "themes", "*.archive"))
	if len(files) != 1 {
		t.Fatalf("cache files: %v", files)
	}
	tampered := tgz(t, entry{name: "theme.json", data: manifest}, entry{name: "texts/en.json", data: `{"x":"evil"}`})
	_ = os.WriteFile(files[0], tampered, 0o600)
	up = true
	if _, err := Resolve(context.Background(), spec+"#sha256:"+sum, o); err == nil || !strings.Contains(err.Error(), "changed on the volume") {
		t.Errorf("tampered cache: %v", err)
	}
}

func TestDownloadUnpinnedFallsBackToVerifiedCache(t *testing.T) {
	body, _ := theArchive(t)
	up := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up {
			http.Error(w, "down", 503)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	var logs bytes.Buffer
	o := Options{CacheDir: t.TempDir(), AllowUnverified: true, Log: slog.New(slog.NewTextHandler(&logs, nil))}
	spec := srv.URL + "/theme.tgz"
	if _, err := Resolve(context.Background(), spec, o); err != nil {
		t.Fatal(err)
	}
	up = false
	if src, err := Resolve(context.Background(), spec, o); err != nil || !strings.Contains(src.Desc, "download failed") {
		t.Fatalf("fallback: %v", err)
	}
	files, _ := filepath.Glob(filepath.Join(o.CacheDir, "themes", "*.archive"))
	_ = os.WriteFile(files[0], []byte("garbage"), 0o600)
	if _, err := Resolve(context.Background(), spec, o); err == nil {
		t.Error("tampered unpinned cache used")
	}
}

func TestGitHubPrivateTokenStaysOnTheAPIHost(t *testing.T) {
	body, sum := theArchive(t)
	const token = "github_pat_SECRET123"
	var leaked atomic.Bool
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
		_, _ = w.Write(body)
	}))
	defer storage.Close()
	// a different host name for the same machine, so the redirect crosses hosts
	storageURL := strings.Replace(storage.URL, "127.0.0.1", "localhost", 1)

	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "no", 401)
			return
		}
		switch r.URL.Path {
		case "/repos/acme/theme/releases/tags/v1.0.0":
			_, _ = io.WriteString(w, `{"assets":[{"name":"other.zip","url":"`+api.URL+`/x"},{"name":"theme.tgz","url":"`+api.URL+`/repos/acme/theme/releases/assets/7"}]}`)
		case "/repos/acme/theme/releases/assets/7":
			if r.Header.Get("Accept") != "application/octet-stream" {
				http.Error(w, "json", 400)
				return
			}
			http.Redirect(w, r, storageURL+"/signed?X-Amz-Signature=abc", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	var logs bytes.Buffer
	o := Options{CacheDir: t.TempDir(), Token: token, GitHubAPI: api.URL, Log: slog.New(slog.NewTextHandler(&logs, nil))}
	src, err := Resolve(context.Background(), "acme/theme@v1.0.0#sha256:"+sum, o)
	if err != nil {
		t.Fatal(err)
	}
	if leaked.Load() {
		t.Error("Authorization reached the storage host")
	}
	if !strings.Contains(src.Desc, "acme/theme@v1.0.0") || strings.Contains(src.Desc, token) {
		t.Errorf("desc %q", src.Desc)
	}
	// errors name the spec, never the token
	_, err = Resolve(context.Background(), "acme/theme@v9.9.9#sha256:"+sum, o)
	if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "acme/theme@v9.9.9") {
		t.Errorf("error: %v", err)
	}
	if strings.Contains(logs.String(), token) {
		t.Error("token in the log")
	}
}

func TestGitHubPublicNeedsNoToken(t *testing.T) {
	body, sum := theArchive(t)
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/acme/theme/releases/download/v1.0.0/theme.tgz" || r.Header.Get("Authorization") != "" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer web.Close()
	o := Options{GitHubWeb: web.URL}
	if _, err := Resolve(context.Background(), "https://github.com/acme/theme@v1.0.0#sha256:"+sum, o); err != nil {
		t.Fatal(err)
	}
	_, err := Resolve(context.Background(), "acme/private@v1.0.0#sha256:"+sum, o)
	if err == nil || !strings.Contains(err.Error(), "EDGE_THEME_TOKEN") {
		t.Errorf("a 404 should hint at the token: %v", err)
	}
}
