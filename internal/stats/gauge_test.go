// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

package stats

import (
	"sync/atomic"
	"testing"
)

func TestGaugeFollowsRisesAtOnceAndFallsAfterTheWindow(t *testing.T) {
	var level atomic.Int64
	g := NewGauge(level.Load, 100, 3)
	if n, capped := g.Value(); n != 0 || capped {
		t.Fatalf("empty: %d %v", n, capped)
	}
	level.Store(40)
	if n, _ := g.Value(); n != 40 {
		t.Fatalf("a rise shows at once: %d", n)
	}
	g.Sample()
	level.Store(0) // a policy change let everyone go; they are about to come back
	if n, _ := g.Value(); n != 40 {
		t.Fatalf("the release dip must not show: %d", n)
	}
	level.Store(38)
	g.Sample()
	g.Sample()
	if n, _ := g.Value(); n != 40 {
		t.Fatalf("the peak stays for the window: %d", n)
	}
	g.Sample() // the sample of 40 leaves the window
	if n, _ := g.Value(); n != 38 {
		t.Fatalf("after the window: %d", n)
	}
}

func TestGaugeClampsAtTheLimit(t *testing.T) {
	var level atomic.Int64
	g := NewGauge(level.Load, 5, 2)
	level.Store(4)
	if n, capped := g.Value(); n != 4 || capped {
		t.Fatalf("under the limit: %d %v", n, capped)
	}
	level.Store(6) // a long-poll beyond the limit counts itself for a moment before it is turned away
	if n, capped := g.Value(); n != 5 || !capped {
		t.Fatalf("over the limit: %d %v", n, capped)
	}
	level.Store(5)
	if n, capped := g.Value(); n != 5 || !capped {
		t.Fatalf("at the limit: %d %v", n, capped)
	}
	level.Store(-1)
	g = NewGauge(level.Load, 5, 2)
	if n, _ := g.Value(); n != 0 {
		t.Fatalf("never negative: %d", n)
	}
}
