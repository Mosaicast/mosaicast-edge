// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

package stats

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/Mosaicast/mosaicast-edge/internal/fsutil"
)

// Kind is an edge page kind. The set is closed: views are counted by kind, never by path, which is
// whatever a visitor typed and would make the counters unbounded (§18.5).
type Kind int

// The page kinds, as §18.5 names them.
const (
	Countdown Kind = iota
	Maintenance
	Closed
	Unavailable
	Login
	Legal
	numKinds
)

var kindNames = [numKinds]string{"countdown", "maintenance", "closed", "unavailable", "login", "legal"}

// KindOf maps a page name to its kind.
func KindOf(page string) (Kind, bool) {
	i := slices.Index(kindNames[:], page)
	return Kind(i), i >= 0
}

// Hour is one completed UTC hour of views, as §18.5 reports it: every kind present, zeros included.
type Hour struct {
	Start time.Time        `json:"start"`
	Views map[string]int64 `json:"views"`
}

// MaxPending is how many unacknowledged hours the edge keeps: 7 days, which must stay below the age at
// which core folds hourly counts into daily ones (§18.5). Raise neither without the other.
const MaxPending = 7 * 24

// Views counts rendered edge pages per kind and UTC hour. The running hour survives a restart in
// stats.json; a completed hour waits there until core acknowledges it. Nothing is counted, kept or
// written while counting is off, and turning it off deletes what was kept.
type Views struct {
	now  func() time.Time
	on   func() bool // the policy allows counting (policy.Store.Counting)
	file string      // stats.json; empty = memory only
	hour time.Duration

	mu      sync.Mutex
	start   time.Time // the running hour; zero = none yet
	counts  [numKinds]int64
	pending []Hour
	dirty   bool // differs from the file
	gone    bool // the file is known not to exist
}

// ViewsOptions configure Views.
type ViewsOptions struct {
	Now  func() time.Time
	On   func() bool
	Dir  string        // where stats.json lives; empty = memory only
	Hour time.Duration // 0 = one hour; shorter only in tests
}

type viewsFile struct {
	Version int    `json:"version"`
	Running *Hour  `json:"running,omitempty"`
	Pending []Hour `json:"pending"`
}

// NewViews loads stats.json if there is one. A running hour that has ended since is completed.
func NewViews(o ViewsOptions) *Views {
	v := &Views{now: o.Now, on: o.On, hour: o.Hour}
	if v.hour == 0 {
		v.hour = time.Hour
	}
	if o.Dir == "" {
		v.gone = true
		return v
	}
	v.file = filepath.Join(o.Dir, "stats.json")
	body, err := os.ReadFile(v.file)
	switch {
	case os.IsNotExist(err):
		v.gone = true
	case err != nil:
		slog.Warn("stats unreadable; starting from zero", "file", v.file, "err", err)
	default:
		var f viewsFile
		if err := json.Unmarshal(body, &f); err != nil || f.Version != 1 {
			slog.Warn("stats ignored; starting from zero", "file", v.file, "err", err)
			break
		}
		v.pending = f.Pending
		if r := f.Running; r != nil {
			v.start = r.Start
			for k, name := range kindNames {
				v.counts[k] = max(r.Views[name], 0)
			}
		}
		v.mu.Lock()
		v.roll()
		v.mu.Unlock()
	}
	return v
}

// Count adds one view of kind k, if counting is on.
func (v *Views) Count(k Kind) {
	if k < 0 || k >= numKinds || !v.on() {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.roll()
	v.counts[k]++
	v.dirty = true
}

// roll starts the hour now is in, completing the running one if it has ended. Caller holds mu.
func (v *Views) roll() {
	cur := v.now().UTC().Truncate(v.hour)
	if v.start.Equal(cur) {
		return
	}
	if !v.start.IsZero() && v.start.Before(cur) {
		h := Hour{Start: v.start, Views: map[string]int64{}}
		for k, name := range kindNames {
			h.Views[name] = v.counts[k]
		}
		v.pending = append(v.pending, h)
		if n := len(v.pending) - MaxPending; n > 0 {
			slog.Warn("stats: core has not taken the oldest hours; dropping them", "hours", n, "from", v.pending[0].Start)
			v.pending = slices.Delete(v.pending, 0, n)
		}
	}
	v.start, v.counts, v.dirty = cur, [numKinds]int64{}, true
}

// Tick completes an ended hour and writes stats.json if anything changed; with counting off it drops
// everything and deletes the file instead. Called every report interval and on shutdown.
func (v *Views) Tick() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.on() {
		if !v.start.IsZero() || len(v.pending) > 0 || !v.gone {
			v.start, v.counts, v.pending, v.dirty = time.Time{}, [numKinds]int64{}, nil, false
			v.remove()
		}
		return
	}
	v.roll()
	if v.dirty {
		v.save()
	}
}

// Pending returns up to n completed hours core has not acknowledged, oldest first.
func (v *Views) Pending(n int) []Hour {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.pending[:min(n, len(v.pending))])
}

// Ack drops the hours of sent that start no later than through: only those, so a clock that jumped on
// either side never costs an hour that was not sent.
func (v *Views) Ack(sent []Hour, through time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	n := len(v.pending)
	v.pending = slices.DeleteFunc(v.pending, func(p Hour) bool {
		return !p.Start.After(through) && slices.ContainsFunc(sent, func(s Hour) bool { return s.Start.Equal(p.Start) })
	})
	if len(v.pending) != n {
		v.dirty = true
	}
}

// save writes stats.json. Caller holds mu. A failure is logged; it costs only what a restart loses.
func (v *Views) save() {
	if v.file == "" {
		v.dirty = false
		return
	}
	f := viewsFile{Version: 1, Pending: v.pending}
	if !v.start.IsZero() {
		r := Hour{Start: v.start, Views: map[string]int64{}}
		for k, name := range kindNames {
			r.Views[name] = v.counts[k]
		}
		f.Running = &r
	}
	if f.Pending == nil {
		f.Pending = []Hour{}
	}
	body, _ := json.Marshal(f)
	if err := fsutil.WriteAtomic(v.file, body); err != nil {
		slog.Warn("stats not written", "file", v.file, "err", err)
		return
	}
	v.dirty, v.gone = false, false
}

// remove deletes stats.json. Caller holds mu.
func (v *Views) remove() {
	if v.file == "" {
		v.gone = true
		return
	}
	if err := os.Remove(v.file); err != nil && !os.IsNotExist(err) {
		slog.Warn("stats not deleted", "file", v.file, "err", err)
		return
	}
	v.gone = true
}
