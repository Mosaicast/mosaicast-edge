// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

// Package themesrc resolves EDGE_THEME, the place a theme comes from, into a file system the theme
// loader reads. The grammar is core's plugin spec grammar (scripts/install-plugin.sh), so an operator who
// knows one knows the other:
//
//	/srv/theme                           a folder
//	/srv/theme.tgz, ./theme.zip          an archive on disk
//	https://host/path/theme.tgz          an archive anywhere
//	owner/repo, owner/repo@v1.2.0        a GitHub release's theme.tgz (latest, or that tag)
//	https://github.com/owner/repo@v1.2.0 the same, spelled as the address copied from the browser
//
// Any of them may end in #sha256:<hex>, the digest of the archive. A download without one is refused
// unless the operator allows it in so many words: a theme's scripts run on the site's own origin, so
// fetching an unknown one is fetching unknown code for every visitor (core#186 made the same call for
// plugins).
//
// A downloaded archive is kept in the cache folder. With a pin, a restart takes it from there without
// asking the network at all, so a launch-day restart never depends on GitHub; without one, the cached
// copy is the fallback when the download fails. A spec that cannot be resolved and has nothing cached
// is an error, and the edge does not start: a site that silently runs without the theme it was given
// looks healthy and is wrong.
//
// Archives are unpacked in memory (the root file system stays read-only). Only plain files and folders
// are taken; links, absolute paths and paths leaving the archive are refused, and the theme limits are
// enforced while unpacking, before anything is held in full.
package themesrc

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing/fstest"
	"time"

	"github.com/Mosaicast/mosaicast-edge/internal/fsutil"
	"github.com/Mosaicast/mosaicast-edge/internal/theme"
)

// AssetName is the release asset a GitHub spec fetches; pack-theme writes it, the theme release
// workflow uploads it.
const AssetName = "theme.tgz"

// Limits. The compressed archive is capped as it arrives, and the decompressed stream is capped as it
// is read (skipped entries included), so neither a large download nor a gzip bomb fills memory before
// a limit trips. A theme's assets may be 24 MB in all; templates, texts and licenses get one more
// file's worth on top.
const (
	MaxArchiveBytes = 30 << 20                                  // compressed, downloaded or read
	MaxUnpackBytes  = theme.MaxThemeBytes + theme.MaxAssetBytes // every file together, unpacked
	MaxEntries      = 2000
	FetchTimeout    = 90 * time.Second // the whole download, every request in it
)

// Options configure Resolve.
type Options struct {
	CacheDir        string // where downloads are kept; "" = not kept (check-theme)
	Token           string // GitHub token for a private theme repo; never logged
	AllowUnverified bool   // download without a #sha256 pin
	Client          *http.Client
	GitHubAPI       string // https://api.github.com (tests point it elsewhere)
	GitHubWeb       string // https://github.com
	Log             *slog.Logger
}

// Source is a resolved theme.
type Source struct {
	FS   fs.FS
	Dir  string // the folder, when the theme is one (preview watches it); "" for archives
	Desc string // for the log: where it came from, with the digest
}

var (
	pinRe  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	repoRe = regexp.MustCompile(`^([A-Za-z0-9](?:[A-Za-z0-9-]{0,38}))/([A-Za-z0-9._-]{1,100})(?:@([^\s/#@]{1,128}))?$`)
)

// Resolve turns a spec into a theme file system.
func Resolve(ctx context.Context, spec string, o Options) (*Source, error) {
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 60 * time.Second}
	}
	if o.GitHubAPI == "" {
		o.GitHubAPI = "https://api.github.com"
	}
	if o.GitHubWeb == "" {
		o.GitHubWeb = "https://github.com"
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	spec = strings.TrimSpace(spec)
	where, pin, hasPin := strings.Cut(spec, "#sha256:")
	if hasPin && !pinRe.MatchString(pin) {
		return nil, fmt.Errorf("EDGE_THEME %q: #sha256: needs the archive's 64-character lowercase hex digest", spec)
	}
	if where == "" {
		return nil, errors.New("EDGE_THEME is empty")
	}

	// a local path wins over the owner/repo reading of the same characters ("themes/mine")
	if !strings.Contains(where, "://") {
		if st, err := os.Stat(where); err == nil {
			return local(where, st, pin)
		} else if !repoRe.MatchString(where) || strings.HasPrefix(where, ".") || strings.HasPrefix(where, "/") {
			return nil, fmt.Errorf("EDGE_THEME %q: no such folder or file", where)
		}
		return o.download(ctx, githubSpec(where), pin)
	}
	u, err := url.Parse(where)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("EDGE_THEME %q is not an address", where)
	}
	switch {
	case u.Scheme == "https" && u.Host == "github.com" && !isArchiveName(u.Path):
		rest := strings.Trim(strings.TrimSuffix(u.Path, ".git"), "/")
		if !repoRe.MatchString(rest) {
			return nil, fmt.Errorf("EDGE_THEME %q: expected https://github.com/owner/repo[@tag]", where)
		}
		return o.download(ctx, githubSpec(rest), pin)
	case u.Scheme == "https", u.Scheme == "http" && isLoopback(u.Hostname()):
		// plain http only to this machine (a local file server, the rehearsal); never across a network
		return o.download(ctx, remote{url: u.String()}, pin)
	default:
		return nil, fmt.Errorf("EDGE_THEME %q: refusing to fetch a theme over %s:// (use https://)", where, u.Scheme)
	}
}

func isArchiveName(p string) bool {
	p = strings.ToLower(p)
	return strings.HasSuffix(p, ".tgz") || strings.HasSuffix(p, ".tar.gz") || strings.HasSuffix(p, ".zip")
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func local(where string, st os.FileInfo, pin string) (*Source, error) {
	if st.IsDir() {
		if pin != "" {
			return nil, fmt.Errorf("EDGE_THEME %q is a folder: a #sha256 pin needs an archive (mosaicast-edge pack-theme)", where)
		}
		return &Source{FS: os.DirFS(where), Dir: where, Desc: "folder " + where}, nil
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("EDGE_THEME %q is neither a folder nor a file", where)
	}
	f, err := os.Open(where)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	body, err := readCapped(f, MaxArchiveBytes, where)
	if err != nil {
		return nil, err
	}
	sum := digest(body)
	if pin != "" && sum != pin {
		return nil, fmt.Errorf("EDGE_THEME %q: sha256 is %s, the pin says %s", where, sum, pin)
	}
	fsys, err := Unpack(body)
	if err != nil {
		return nil, fmt.Errorf("EDGE_THEME %q: %w", where, err)
	}
	return &Source{FS: fsys, Desc: "archive " + where + " sha256:" + sum}, nil
}

// fetcher is where a download comes from.
type fetcher interface {
	fetch(ctx context.Context, o *Options) ([]byte, error)
	String() string
}

type remote struct{ url string }

func (r remote) String() string { return r.url }
func (r remote) fetch(ctx context.Context, o *Options) ([]byte, error) {
	return o.get(ctx, r.url, nil)
}

type github struct{ owner, repo, tag string }

func githubSpec(s string) github {
	m := repoRe.FindStringSubmatch(s)
	return github{owner: m[1], repo: m[2], tag: m[3]}
}

func (g github) String() string {
	if g.tag == "" {
		return g.owner + "/" + g.repo + " (latest release)"
	}
	return g.owner + "/" + g.repo + "@" + g.tag
}

func (g github) fetch(ctx context.Context, o *Options) ([]byte, error) {
	if o.Token == "" {
		// a public repo: the release download address redirects to the file, no API call, no token
		u := o.GitHubWeb + "/" + g.owner + "/" + g.repo + "/releases/latest/download/" + AssetName
		if g.tag != "" {
			u = o.GitHubWeb + "/" + g.owner + "/" + g.repo + "/releases/download/" + url.PathEscape(g.tag) + "/" + AssetName
		}
		body, err := o.get(ctx, u, nil)
		if err != nil && strings.Contains(err.Error(), "answered 404") {
			return nil, fmt.Errorf("%w (a private repository needs EDGE_THEME_TOKEN)", err)
		}
		return body, err
	}
	// a private repo: find the asset through the API, then fetch it as a file. The API redirects to
	// storage on another host, and Go's client drops the Authorization header on that redirect.
	auth := http.Header{"Authorization": {"Bearer " + o.Token}, "X-Github-Api-Version": {"2022-11-28"}}
	rel := o.GitHubAPI + "/repos/" + g.owner + "/" + g.repo + "/releases/latest"
	if g.tag != "" {
		rel = o.GitHubAPI + "/repos/" + g.owner + "/" + g.repo + "/releases/tags/" + url.PathEscape(g.tag)
	}
	h := auth.Clone()
	h.Set("Accept", "application/vnd.github+json")
	meta, err := o.get(ctx, rel, h)
	if err != nil {
		return nil, err
	}
	var release struct {
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(meta, &release); err != nil {
		return nil, fmt.Errorf("%s: release: %w", g, err)
	}
	for _, a := range release.Assets {
		if a.Name == AssetName {
			if !strings.HasPrefix(a.URL, o.GitHubAPI+"/") {
				return nil, fmt.Errorf("%s: asset address %q is not on the API host", g, a.URL)
			}
			h := auth.Clone()
			h.Set("Accept", "application/octet-stream")
			return o.get(ctx, a.URL, h)
		}
	}
	return nil, fmt.Errorf("%s: the release has no %s (publish one with the theme release workflow)", g, AssetName)
}

func (o *Options) get(ctx context.Context, u string, h http.Header) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range h {
		req.Header[k] = v
	}
	req.Header.Set("User-Agent", "mosaicast-edge/"+theme.EdgeVersion)
	resp, err := o.Client.Do(req)
	if err != nil {
		return nil, redact(err, o.Token)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %d", stripQuery(u), resp.StatusCode)
	}
	return readCapped(resp.Body, MaxArchiveBytes, stripQuery(u))
}

// stripQuery keeps signed storage addresses out of the log.
func stripQuery(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		return u[:i]
	}
	return u
}

func redact(err error, token string) error {
	if token == "" || !strings.Contains(err.Error(), token) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, "[token]"))
}

func readCapped(r io.Reader, limit int64, what string) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%s is larger than %d MB", what, limit>>20)
	}
	return body, nil
}

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// download resolves a remote spec, through the cache.
func (o *Options) download(ctx context.Context, f fetcher, pin string) (*Source, error) {
	if pin == "" && !o.AllowUnverified {
		return nil, fmt.Errorf("EDGE_THEME %s has no #sha256 pin: a theme's scripts run on the site, so an unpinned download is refused. "+
			"Add #sha256:<digest> (pack-theme and the theme release print it), or set EDGE_THEME_ALLOW_UNVERIFIED=true", f)
	}
	// The cache is a volume, not something to trust: it is keyed by the whole spec and pin (a changed
	// EDGE_THEME never reuses an older archive), and an archive read from it is checked against the pin,
	// or, unpinned, against the digest recorded when it was downloaded, every time.
	dir, cached := "", ""
	if o.CacheDir != "" {
		dir = filepath.Join(o.CacheDir, "themes")
		cached = filepath.Join(dir, digest([]byte(f.String() + "#sha256:" + pin))[:32]+".archive")
	}
	fromCache := func(want string) (fs.FS, string, error) {
		body, err := os.ReadFile(cached)
		if err != nil {
			return nil, "", err
		}
		if want == "" {
			side, err := os.ReadFile(cached + ".sha256")
			if err != nil {
				return nil, "", err
			}
			want = strings.TrimSpace(string(side))
		}
		if sum := digest(body); sum != want {
			return nil, "", fmt.Errorf("cached archive %s has sha256 %s, expected %s: changed on the volume", filepath.Base(cached), sum, want)
		}
		fsys, err := Unpack(body)
		return fsys, want, err
	}
	// pinned and already here: no network at all
	if pin != "" && cached != "" {
		fsys, _, err := fromCache(pin)
		switch {
		case err == nil:
			return &Source{FS: fsys, Desc: f.String() + " sha256:" + pin + " (cached)"}, nil
		case !errors.Is(err, fs.ErrNotExist):
			return nil, fmt.Errorf("EDGE_THEME %s: %w", f, err) // tampered or damaged: refuse, do not fetch over it quietly
		}
	}

	fctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	body, err := f.fetch(fctx, o)
	if err != nil {
		err = fmt.Errorf("%s: %w", f, err)
	}
	if err == nil && pin != "" && digest(body) != pin {
		err = fmt.Errorf("%s: sha256 is %s, the pin says %s", f, digest(body), pin)
	}
	if err != nil {
		if pin == "" && cached != "" {
			if fsys, sum, cerr := fromCache(""); cerr == nil {
				o.Log.Warn("theme download failed; using the copy from the last start", "theme", f.String(), "err", err)
				return &Source{FS: fsys, Desc: f.String() + " sha256:" + sum + " (cached, download failed)"}, nil
			}
		}
		return nil, fmt.Errorf("EDGE_THEME %w", err)
	}
	fsys, err := Unpack(body)
	if err != nil {
		return nil, fmt.Errorf("EDGE_THEME %s: %w", f, err)
	}
	sum := digest(body)
	if pin == "" {
		o.Log.Warn("theme downloaded without a pin", "theme", f.String(), "pin", "#sha256:"+sum)
	}
	if cached != "" {
		if err := os.MkdirAll(dir, 0o700); err == nil {
			err := fsutil.WriteAtomic(cached, body)
			if err == nil {
				err = fsutil.WriteAtomic(cached+".sha256", []byte(sum+"\n"))
			}
			if err != nil {
				o.Log.Warn("theme not cached; a restart will download it again", "err", err)
			} else {
				prune(dir, filepath.Base(cached))
			}
		}
	}
	return &Source{FS: fsys, Desc: f.String() + " sha256:" + sum}, nil
}

// prune removes the archives of earlier themes, so a cache folder holds one.
func prune(dir, keep string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != keep && e.Name() != keep+".sha256" && (strings.HasSuffix(e.Name(), ".archive") || strings.HasSuffix(e.Name(), ".archive.sha256")) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// Unpack reads a .tgz or .zip into memory, by its content rather than its name.
func Unpack(body []byte) (fs.FS, error) {
	files := map[string][]byte{}
	var total int64
	add := func(name string, isDir, isRegular bool, size int64, open func() (io.Reader, error)) error {
		if len(files) >= MaxEntries {
			return fmt.Errorf("archive has more than %d files", MaxEntries)
		}
		if !isRegular && !isDir {
			return fmt.Errorf("archive entry %q is not a plain file or folder (links, devices and the like are not allowed in a theme)", name)
		}
		clean, skip, err := entryName(name)
		if err != nil || skip || isDir {
			return err
		}
		if size > theme.MaxAssetBytes {
			return fmt.Errorf("archive entry %q: %d bytes, more than the %d a theme file may have", name, size, theme.MaxAssetBytes)
		}
		r, err := open()
		if err != nil {
			return err
		}
		data, err := readCapped(r, theme.MaxAssetBytes, "archive entry "+name)
		if err != nil {
			return err
		}
		if total += int64(len(data)); total > MaxUnpackBytes {
			return fmt.Errorf("archive unpacks to more than %d MB", MaxUnpackBytes>>20)
		}
		if _, dup := files[clean]; dup {
			return fmt.Errorf("archive has %q twice", clean)
		}
		files[clean] = data
		return nil
	}

	switch {
	case bytes.HasPrefix(body, []byte{0x1f, 0x8b}):
		gz, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("archive: %w", err)
		}
		gz.Multistream(false)
		// every decompressed byte counts, the ones of skipped entries and headers too
		tr := tar.NewReader(&capped{r: gz, left: MaxUnpackBytes + 4<<20})
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("archive: %w", err)
			}
			// Go's reader folds PAX and GNU long-name records into the header they belong to; what
			// is left must be a plain file or a folder (no global headers, sparse files, links, …)
			if len(h.PAXRecords) > 0 {
				for k := range h.PAXRecords {
					if k != "path" && k != "linkpath" && k != "size" && k != "mtime" && k != "atime" && k != "ctime" &&
						k != "uid" && k != "gid" && k != "uname" && k != "gname" && !strings.HasPrefix(k, "SCHILY.") && !strings.HasPrefix(k, "LIBARCHIVE.") {
						return nil, fmt.Errorf("archive entry %q: unexpected PAX record %q", h.Name, k)
					}
				}
			}
			isReg := h.Typeflag == tar.TypeReg
			if err := add(h.Name, h.Typeflag == tar.TypeDir, isReg, h.Size,
				func() (io.Reader, error) { return tr, nil }); err != nil {
				return nil, err
			}
		}
	case bytes.HasPrefix(body, []byte("PK\x03\x04")):
		zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err != nil {
			return nil, fmt.Errorf("archive: %w", err)
		}
		var declared uint64
		for _, f := range zr.File {
			if declared += f.UncompressedSize64; declared > MaxUnpackBytes {
				return nil, fmt.Errorf("archive unpacks to more than %d MB", MaxUnpackBytes>>20)
			}
		}
		for _, f := range zr.File {
			mode := f.Mode()
			if err := add(f.Name, mode.IsDir(), mode.IsRegular(), int64(f.UncompressedSize64),
				func() (io.Reader, error) { return f.Open() }); err != nil {
				return nil, err
			}
		}
	default:
		return nil, errors.New("not a .tgz or .zip archive")
	}
	if len(files) == 0 {
		return nil, errors.New("archive is empty")
	}

	// a single top-level folder (theme/theme.json, …) is the archive's wrapper, not part of the theme
	prefix := ""
	if _, atRoot := files["theme.json"]; !atRoot {
		for name := range files {
			first, _, _ := strings.Cut(name, "/")
			if prefix == "" {
				prefix = first + "/"
			}
			if !strings.HasPrefix(name, prefix) {
				prefix = ""
				break
			}
		}
	}
	m := fstest.MapFS{}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		rel := strings.TrimPrefix(name, prefix)
		if rel == "" {
			continue
		}
		m[rel] = &fstest.MapFile{Data: files[name], Mode: 0o444}
	}
	if _, ok := m["theme.json"]; !ok {
		return nil, errors.New("archive has no theme.json at its top (or in its one top-level folder)")
	}
	return m, nil
}

// entryName checks an archive path: relative, inside the archive, forward slashes. Hidden files and the
// folders macOS adds to zips are skipped, not refused.
func entryName(name string) (clean string, skip bool, err error) {
	if name == "" || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") || strings.ContainsRune(name, 0) {
		return "", false, fmt.Errorf("archive entry %q: not a relative path", name)
	}
	clean = path.Clean(strings.TrimPrefix(name, "./"))
	if clean == ".." || strings.HasPrefix(clean, "../") || !fs.ValidPath(clean) {
		return "", false, fmt.Errorf("archive entry %q leaves the archive", name)
	}
	for _, part := range strings.Split(clean, "/") {
		if strings.HasPrefix(part, ".") || part == "__MACOSX" {
			return clean, true, nil
		}
	}
	return clean, false, nil
}

// capped fails a read once more than left bytes have come through: a decompression bomb stops at the
// limit instead of filling memory.
type capped struct {
	r    io.Reader
	left int64
}

func (c *capped) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, fmt.Errorf("archive unpacks to more than %d MB", MaxUnpackBytes>>20)
	}
	if int64(len(p)) > c.left {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		return n, fmt.Errorf("archive unpacks to more than %d MB", MaxUnpackBytes>>20)
	}
	return n, err
}
