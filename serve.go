// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Mosaicast/mosaicast-edge/internal/core"
	"github.com/Mosaicast/mosaicast-edge/internal/gate"
	"github.com/Mosaicast/mosaicast-edge/internal/pages"
	"github.com/Mosaicast/mosaicast-edge/internal/policy"
	"github.com/Mosaicast/mosaicast-edge/internal/sitecopy"
	"github.com/Mosaicast/mosaicast-edge/internal/stats"
	"github.com/Mosaicast/mosaicast-edge/internal/theme"
	"github.com/Mosaicast/mosaicast-edge/internal/themesrc"
	"github.com/Mosaicast/mosaicast-edge/web"
)

// edgeLocales are the languages the edge's own texts come in.
var edgeLocales = []string{"en", "de"}

func serve() int {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(log)

	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		log.Error("invalid configuration", "err", err)
		return 2
	}
	if cfg.cacheDir != "" {
		if err := os.MkdirAll(cfg.cacheDir, 0o700); err != nil {
			log.Warn("cache dir not usable; policy and site copy are kept in memory only", "dir", cfg.cacheDir, "err", err)
			cfg.cacheDir = ""
		}
	} else {
		log.Warn("EDGE_CACHE_DIR not set: the policy is not kept across restarts")
	}
	var def, ext *theme.Theme
	themeFrom := "the edge's own look"
	if cfg.theme == "" {
		def, ext, err = loadThemes("")
	} else {
		var src *themesrc.Source
		src, err = themesrc.Resolve(context.Background(), cfg.theme, themesrc.Options{CacheDir: cfg.cacheDir,
			Token: cfg.themeToken, AllowUnverified: cfg.themeAnyPin, Log: log})
		if err == nil {
			themeFrom = src.Desc
			def, ext, err = loadThemesFS(src.FS)
		}
	}
	if err != nil {
		log.Error("theme failed to load; fix it or unset EDGE_THEME", "err", err)
		return 1
	}

	build := buildID()
	client := core.NewClient(cfg.upstream, cfg.coreToken, build)
	store := policy.NewStore(time.Now, cfg.cacheDir)
	keeper := sitecopy.NewKeeper(client, edgeLocales, time.Now, cfg.cacheDir)
	roles := &core.Roles{Client: client, TTL: 30 * time.Second, NegativeTTL: 10 * time.Second, Max: 10000,
		Timeout: 2 * time.Second, PerClient: 10, InFlight: 32, Now: time.Now}
	renderer := pages.New(pages.Options{Default: def, Theme: ext, Site: keeper, View: store.View, Now: time.Now,
		Location: cfg.location, PublicURL: cfg.publicURL})
	views := stats.NewViews(stats.ViewsOptions{Now: time.Now, On: store.Counting, Dir: cfg.cacheDir})
	g := gate.New(gate.Options{Upstream: cfg.upstream, Store: store, Roles: roles, Pages: renderer, Now: time.Now,
		MaxWaiters: cfg.longPollMax, Views: views})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	poller := &core.Poller{Client: client, Store: store, Interval: cfg.poll,
		OnRevision: func() { keeper.Refresh(ctx) }}
	go poller.Run(ctx)
	go keeper.Run(ctx, cfg.siteRefresh)
	go g.Run(ctx)
	reporter := &core.Reporter{Client: client, Views: views, On: store.Counting, Now: time.Now,
		Waiting: func() (int64, bool) { w := g.Reported(); return w.N, w.Capped },
		Revision: func() int64 {
			if a := store.View().Access; a != nil {
				return a.Revision
			}
			return 0
		}}
	reported := make(chan struct{})
	go func() { reporter.Run(ctx); close(reported) }()

	srv := &http.Server{
		Addr:              cfg.listen,
		Handler:           g,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// no WriteTimeout: proxied responses may stream
	}
	themeName := "default"
	if ext != nil {
		themeName = ext.Name + " " + ext.Version
	}
	log.Info("mosaicast-edge up", "version", version, "build", build, "listen", cfg.listen, "upstream", cfg.upstream.String(),
		"theme", themeName, "themeFrom", themeFrom, "poll", cfg.poll.String(), "cache", cfg.cacheDir, "tz", cfg.location.String(),
		"token", cfg.coreToken != "")

	go func() {
		<-ctx.Done()
		g.Stop()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	ln, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		log.Error("listen failed", "err", err)
		return 1
	}
	if err := srv.Serve(limitListen(ln, cfg.maxConns)); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server stopped", "err", err)
		return 1
	}
	<-reported // the counts are kept and the last report sent (3 s at most)
	return 0
}

// loadThemes loads the embedded default theme and the operator's theme (if dir is set) and dry-renders
// every page they provide. A broken theme stops the edge from starting: better no deploy than a
// countdown that fails at the worst moment.
func loadThemes(dir string) (def, ext *theme.Theme, err error) {
	if dir == "" {
		return loadThemesFS(nil)
	}
	return loadThemesFS(os.DirFS(dir))
}

// loadThemesFS is loadThemes for a theme from any file system (a folder, or an archive in memory);
// nil = no theme.
func loadThemesFS(fsys fs.FS) (def, ext *theme.Theme, err error) {
	def, err = theme.Load(web.Default(), gate.Prefix+"s/")
	if err != nil {
		return nil, nil, fmt.Errorf("default theme: %w", err)
	}
	if err := pages.Validate(def, nil, time.Now()); err != nil {
		return nil, nil, fmt.Errorf("default theme: %w", err)
	}
	if fsys == nil {
		return def, nil, nil
	}
	ext, err = theme.Load(fsys, gate.Prefix+"t/")
	if err != nil {
		return nil, nil, err
	}
	if err := pages.Validate(def, ext, time.Now()); err != nil {
		return nil, nil, err
	}
	return def, ext, nil
}

// buildID is a short hash of the running binary: the same build reports the same ID everywhere, and
// core's admin page shows it (Mosaicast-Edge-Build).
func buildID() string {
	exe, err := os.Executable()
	if err == nil {
		if f, err := os.Open(exe); err == nil {
			defer f.Close()
			h := sha256.New()
			if _, err := io.Copy(h, f); err == nil {
				return hex.EncodeToString(h.Sum(nil))[:12]
			}
		}
	}
	return "unknown"
}

func healthcheck(listen string) int {
	host := listen
	if strings.HasPrefix(host, ":") {
		host = "127.0.0.1" + host
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + host + gate.Prefix + "healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

// checkTheme validates a theme the way the edge does at startup, for use before deploying: a folder,
// an archive, or any EDGE_THEME spec (downloads are not cached here, and need no pin).
func checkTheme(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: mosaicast-edge check-theme <folder | theme.tgz | EDGE_THEME spec>")
		return 2
	}
	src, err := themesrc.Resolve(context.Background(), args[0], themesrc.Options{Token: os.Getenv("EDGE_THEME_TOKEN"), AllowUnverified: true})
	var ext *theme.Theme
	if err == nil {
		_, ext, err = loadThemesFS(src.FS)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "theme %s is not valid:\n%v\n", args[0], err)
		return 1
	}
	for _, w := range theme.Lint(src.FS, ext) {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	var provided []string
	for _, p := range theme.Pages {
		if m := ext.Mode(p); m != "" {
			provided = append(provided, p+" ("+m+")")
		}
	}
	fmt.Printf("theme %q %s is valid: %d assets, locales %s, pages %s\n", ext.Name, ext.Version,
		ext.Assets.Count(), strings.Join(ext.Locales, ", "), strings.Join(provided, ", "))
	return 0
}
