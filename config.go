// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

package main

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // the distroless image has no zoneinfo; dates rendered without JavaScript need EDGE_TIMEZONE
)

type config struct {
	upstream    *url.URL
	listen      string
	coreToken   string        // Mosaicast-Edge-Token; empty = core shows the edge as "unknown"
	cacheDir    string        // last good policy + site copy; empty = memory only
	theme       string        // EDGE_THEME (or EDGE_THEME_DIR): folder, archive, URL or owner/repo[@tag], #sha256 pin; empty = the edge's default look
	themeToken  string        // EDGE_THEME_TOKEN: GitHub token for a private theme repo; never logged
	themeAnyPin bool          // EDGE_THEME_ALLOW_UNVERIFIED: download a theme without a #sha256 pin
	publicURL   string        // absolute origin for share tags and the calendar; empty = from the request
	poll        time.Duration // policy poll interval
	siteRefresh time.Duration // site copy refresh interval (and on every policy revision)
	longPollMax int64         // concurrent /_a/state long-polls; beyond, answered at once
	maxConns    int           // open connections; beyond, new ones wait to be accepted
	location    *time.Location
}

func loadConfig(getenv func(string) string) (config, error) {
	var c config
	var errs []error

	up := strings.TrimSpace(getenv("EDGE_UPSTREAM"))
	if up == "" {
		errs = append(errs, errors.New("EDGE_UPSTREAM is required (core's address, e.g. http://app:8080)"))
	} else if u, err := url.Parse(up); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, fmt.Errorf("EDGE_UPSTREAM %q must be an absolute http(s) URL", up))
	} else {
		c.upstream = u
	}

	c.listen = envOr(getenv, "EDGE_LISTEN", ":8080")
	c.coreToken = strings.TrimSpace(getenv("EDGE_CORE_TOKEN"))
	c.cacheDir = strings.TrimSpace(getenv("EDGE_CACHE_DIR"))
	c.theme = strings.TrimSpace(getenv("EDGE_THEME"))
	if dir := strings.TrimSpace(getenv("EDGE_THEME_DIR")); dir != "" {
		// EDGE_THEME_DIR is the older name; one theme, so the two must not disagree
		if c.theme != "" && c.theme != dir {
			errs = append(errs, fmt.Errorf("EDGE_THEME %q and EDGE_THEME_DIR %q are both set; set one (EDGE_THEME_DIR is the older name)", c.theme, dir))
		}
		c.theme = dir
		if !strings.HasPrefix(dir, "/") && !strings.HasPrefix(dir, ".") {
			c.theme = "./" + dir // always a path, never read as owner/repo
		}
	}
	c.themeToken = strings.TrimSpace(getenv("EDGE_THEME_TOKEN"))
	switch v := strings.ToLower(strings.TrimSpace(getenv("EDGE_THEME_ALLOW_UNVERIFIED"))); v {
	case "", "false", "0", "no":
	case "true", "1", "yes":
		c.themeAnyPin = true
	default:
		errs = append(errs, fmt.Errorf("EDGE_THEME_ALLOW_UNVERIFIED %q must be true or false", v))
	}

	c.publicURL = strings.TrimRight(strings.TrimSpace(getenv("EDGE_PUBLIC_URL")), "/")
	if c.publicURL != "" {
		if u, err := url.Parse(c.publicURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" {
			errs = append(errs, fmt.Errorf("EDGE_PUBLIC_URL %q must be an origin like https://example.org", c.publicURL))
		}
	}

	var err error
	if c.poll, err = time.ParseDuration(envOr(getenv, "EDGE_POLL_INTERVAL", "2s")); err != nil || c.poll < 500*time.Millisecond || c.poll > time.Minute {
		errs = append(errs, errors.New("EDGE_POLL_INTERVAL must be a duration between 500ms and 1m (default 2s)"))
	}
	if c.siteRefresh, err = time.ParseDuration(envOr(getenv, "EDGE_SITE_REFRESH", "10m")); err != nil || c.siteRefresh < 10*time.Second {
		errs = append(errs, errors.New("EDGE_SITE_REFRESH must be a duration of at least 10s (default 10m)"))
	}

	if c.longPollMax, err = strconv.ParseInt(envOr(getenv, "EDGE_LONGPOLL_MAX", "2500"), 10, 64); err != nil || c.longPollMax < 1 || c.longPollMax > 100000 {
		errs = append(errs, errors.New("EDGE_LONGPOLL_MAX must be a whole number between 1 and 100000 (default 2500)"))
	}
	if c.maxConns, err = strconv.Atoi(envOr(getenv, "EDGE_MAX_CONNS", "3500")); err != nil || c.maxConns < 16 || c.maxConns > 200000 {
		errs = append(errs, errors.New("EDGE_MAX_CONNS must be a whole number between 16 and 200000 (default 3500)"))
	} else if int64(c.maxConns) <= c.longPollMax {
		errs = append(errs, errors.New("EDGE_MAX_CONNS must be larger than EDGE_LONGPOLL_MAX, or held pages would leave no room for anyone else"))
	}

	tz := envOr(getenv, "EDGE_TIMEZONE", "UTC")
	if c.location, err = time.LoadLocation(tz); err != nil {
		errs = append(errs, fmt.Errorf("EDGE_TIMEZONE %q is not a time zone (e.g. Europe/Berlin)", tz))
	}
	return c, errors.Join(errs...)
}

func envOr(getenv func(string) string, key, def string) string {
	if v := strings.TrimSpace(getenv(key)); v != "" {
		return v
	}
	return def
}
