// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

// Package core talks to Mosaicast core over the public HTTP API of ARCHITECTURE §18.2 and nothing else:
// the access policy, who a session belongs to, and the look and legal pages the edge keeps a copy of.
package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Client is a small HTTP client bound to core's base URL.
type Client struct {
	base  *url.URL
	token string // Mosaicast-Edge-Token; empty = not sent
	build string // Mosaicast-Edge-Build
	http  *http.Client
}

// NewClient builds a client. Requests never follow redirects: core answering a redirect to anything
// the edge reads is an error, not something to chase.
func NewClient(base *url.URL, token, build string) *Client {
	return &Client{
		base:  base,
		token: token,
		build: build,
		http: &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// ErrNotFound is a 404 from core.
var ErrNotFound = errors.New("core: not found")

// get fetches path (with query) from core and returns status, headers and at most limit bytes of body.
func (c *Client) get(ctx context.Context, pathQuery string, hdr http.Header, limit int64) (int, http.Header, []byte, error) {
	u, err := c.base.Parse(pathQuery)
	if err != nil {
		return 0, nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, nil, nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("User-Agent", "mosaicast-edge/"+c.build)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return 0, nil, nil, err
	}
	if int64(len(body)) > limit {
		return 0, nil, nil, fmt.Errorf("core: %s answered more than %d bytes", u.Path, limit)
	}
	return resp.StatusCode, resp.Header, body, nil
}

// AccessResult is one poll of GET /api/site/access.
type AccessResult struct {
	Status int // 200 or 304
	ETag   string
	Body   []byte
}

// Access polls the policy with If-None-Match. It reports in with the edge token and build (§18.2).
func (c *Client) Access(ctx context.Context, etag string) (AccessResult, error) {
	h := http.Header{}
	h.Set("Accept", "application/json")
	h.Set("Mosaicast-Edge-Build", c.build)
	if c.token != "" {
		h.Set("Mosaicast-Edge-Token", c.token)
	}
	if etag != "" {
		h.Set("If-None-Match", etag)
	}
	status, rh, body, err := c.get(ctx, "/api/site/access", h, 64<<10)
	if err != nil {
		return AccessResult{}, err
	}
	switch status {
	case http.StatusOK, http.StatusNotModified:
		return AccessResult{Status: status, ETag: rh.Get("ETag"), Body: body}, nil
	default:
		// Anything else, a 404 included, is not a policy. A core without the endpoint answers 401
		// (non-public /api/** needs a session), so a 404 means the edge is talking to the wrong
		// upstream, and that must never open a site before its launch: keep the last good policy.
		return AccessResult{}, fmt.Errorf("core: /api/site/access answered %d", status)
	}
}

// Fetch is a plain GET for the site copy (JSON or image bytes), with a size cap.
func (c *Client) Fetch(ctx context.Context, pathQuery string, limit int64) (body []byte, contentType string, err error) {
	status, h, body, err := c.get(ctx, pathQuery, http.Header{"Accept": {"application/json, image/*"}}, limit)
	if err != nil {
		return nil, "", err
	}
	if status == http.StatusNotFound {
		return nil, "", ErrNotFound
	}
	if status != http.StatusOK {
		return nil, "", fmt.Errorf("core: %s answered %d", pathQuery, status)
	}
	return body, h.Get("Content-Type"), nil
}
