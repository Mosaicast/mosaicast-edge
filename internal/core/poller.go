// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

package core

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/Mosaicast/mosaicast-edge/internal/policy"
)

// Poller keeps the policy store in step with core: one GET /api/site/access per interval, a 304 when
// nothing changed. OnRevision runs (outside the poll loop) whenever core reports a new revision, so the
// site copy can refresh with it.
type Poller struct {
	Client     *Client
	Store      *policy.Store
	Interval   time.Duration
	OnRevision func()

	lastRevision int64 // -1 before the first policy, so it is logged
	failures     int   // polls failed in a row
	lastErr      string
}

// Run polls until ctx ends. The first poll happens immediately.
func (p *Poller) Run(ctx context.Context) {
	p.lastRevision = -1
	t := time.NewTicker(p.Interval)
	defer t.Stop()
	for {
		p.Poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Poll does one round trip and updates the store.
func (p *Poller) Poll(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout())
	defer cancel()
	res, err := p.Client.Access(ctx, p.Store.ETag())
	if err != nil {
		p.fail(err.Error())
		// One slow answer is not an outage: core under launch load can miss a single poll. Marking it
		// down would release every held long-poll at once and show signed-in visitors "unavailable".
		// Two in a row (about 4 s) is. Proxied requests still see core's failures at once.
		if p.failures++; p.failures >= DownAfterFailures {
			p.Store.CoreDown()
		}
		return
	}
	p.failures = 0
	switch res.Status {
	case http.StatusNotModified:
		p.Store.CoreUp()
	case http.StatusOK:
		a, err := policy.Parse(res.Body)
		if err != nil {
			// core answered, so it is up; the policy it sent is not one we apply
			p.fail(err.Error())
			p.Store.CoreUp()
			return
		}
		p.Store.Set(a, res.ETag)
		if a.Revision != p.lastRevision {
			slog.Info("policy", "state", a.State, "opensAt", a.OpensAt, "revision", a.Revision)
			p.lastRevision = a.Revision
			if p.OnRevision != nil {
				go p.OnRevision()
			}
		}
	}
	if p.lastErr != "" {
		slog.Info("core reachable again")
		p.lastErr = ""
	}
}

// DownAfterFailures is how many polls in a row must fail before the edge treats core as down.
const DownAfterFailures = 2

// fail logs an error once per distinct message, not once per poll.
func (p *Poller) fail(msg string) {
	if msg != p.lastErr {
		slog.Warn("policy poll failed; keeping the last good policy", "err", msg)
		p.lastErr = msg
	}
}

func (p *Poller) timeout() time.Duration {
	if p.Interval < 2*time.Second {
		return 2 * time.Second
	}
	return p.Interval
}
