// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

// Package stats counts, never people (ARCHITECTURE §19): nothing here knows who a visitor is.
package stats

import (
	"context"
	"sync"
	"time"
)

// Gauge smooths a level that drops for a moment when everyone it counts is let go at once: a policy
// change answers every held long-poll together, and they come back a few milliseconds later. Its
// value is the highest of the recent samples and the level right now, so it follows a rise at once
// and a fall only after the window.
type Gauge struct {
	read  func() int64
	limit int64 // the level cannot honestly exceed this; at it, Value says capped

	mu   sync.Mutex
	ring []int64
	next int
}

// NewGauge keeps window samples of read; limit clamps the value (read may pass it for a moment).
func NewGauge(read func() int64, limit int64, window int) *Gauge {
	return &Gauge{read: read, limit: limit, ring: make([]int64, window)}
}

// Sample records the level now, replacing the oldest sample.
func (g *Gauge) Sample() {
	v := g.read()
	g.mu.Lock()
	g.ring[g.next] = v
	g.next = (g.next + 1) % len(g.ring)
	g.mu.Unlock()
}

// Value is the smoothed level, clamped to the limit; capped is true when it reached the limit, which
// means the true number may be higher.
func (g *Gauge) Value() (n int64, capped bool) {
	n = g.read()
	g.mu.Lock()
	for _, v := range g.ring {
		n = max(n, v)
	}
	g.mu.Unlock()
	if n >= g.limit {
		return g.limit, true
	}
	return max(n, 0), false
}

// Run samples every interval until ctx ends.
func (g *Gauge) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.Sample()
		}
	}
}
