// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

package policy

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/Mosaicast/mosaicast-edge/internal/fsutil"
)

// Store is the edge's view of the world: the last good policy, whether core answers, and since when
// it does not. Every change that a page on screen must react to bumps Seq and wakes the waiters, which
// is what the long-poll at /_a/state hands to the browser.
type Store struct {
	now  func() time.Time
	file string // cache file; empty = memory only

	mu        sync.Mutex
	access    *Access // nil = no policy known yet
	etag      string
	effective string     // last observed effective state, to notice the flip at opensAt
	openedAt  *time.Time // when the site last became open; theme assets may expire relative to it
	coreUp    bool
	downSince *time.Time
	seq       uint64
	changed   chan struct{} // closed and replaced on every bump
	flip      *time.Timer
}

// View is a consistent snapshot of the store.
type View struct {
	Access    *Access    // nil when no policy is known
	Page      string     // open | scheduled | maintenance | closed | unavailable
	CoreUp    bool       // core answered the last poll or proxied request
	DownSince *time.Time // start of the current outage, nil while core answers
	OpenedAt  *time.Time
	Seq       uint64
}

type cacheFile struct {
	Access   *Access    `json:"access"`
	ETag     string     `json:"etag"`
	OpenedAt *time.Time `json:"openedAt,omitempty"`
}

// NewStore loads the cached policy from dir (if any). Nothing cached and core not reachable means the
// edge shows `unavailable` until the first good poll.
func NewStore(now func() time.Time, dir string) *Store {
	s := &Store{now: now, changed: make(chan struct{})}
	if dir == "" {
		return s
	}
	s.file = filepath.Join(dir, "policy.json")
	body, err := os.ReadFile(s.file)
	switch {
	case os.IsNotExist(err):
	case err != nil:
		slog.Warn("policy cache unreadable", "file", s.file, "err", err)
	default:
		var c cacheFile
		if err := json.Unmarshal(body, &c); err != nil || c.Access == nil {
			slog.Warn("policy cache ignored", "file", s.file, "err", err)
			break
		}
		// re-validate: a cache written by an older edge must meet today's rules too
		raw, _ := json.Marshal(c.Access)
		a, err := Parse(raw)
		if err != nil {
			slog.Warn("policy cache ignored", "file", s.file, "err", err)
			break
		}
		s.access, s.etag, s.openedAt = a, c.ETag, c.OpenedAt
		s.effective = a.Effective(now())
		s.armFlip()
		slog.Info("policy loaded from cache", "state", a.State, "revision", a.Revision)
	}
	return s
}

// View returns the current snapshot. It also notices a scheduled policy that has just become open,
// so a request arriving on the second of opening is proxied even before the flip timer fires.
func (s *Store) View() View {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observe()
	return s.view()
}

func (s *Store) view() View {
	v := View{Access: s.access, CoreUp: s.coreUp, DownSince: s.downSince, OpenedAt: s.openedAt, Seq: s.seq, Page: Unavailable}
	if s.access != nil {
		v.Page = s.effective
	}
	return v
}

// Wait blocks until the snapshot differs from seq or ctx ends, and returns the snapshot then.
func (s *Store) Wait(ctx context.Context, seq uint64) View {
	s.mu.Lock()
	s.observe()
	if s.seq != seq {
		v := s.view()
		s.mu.Unlock()
		return v
	}
	ch := s.changed
	s.mu.Unlock()
	select {
	case <-ch:
	case <-ctx.Done():
	}
	return s.View()
}

// Set installs a freshly polled policy (and the ETag it came with) and records that core answered.
// It wakes the pages only for what they react to; any other change (a new revision, the staff link,
// the pass floor, edge counting) is kept and cached without making every page on screen poll again.
func (s *Store) Set(a *Access, etag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	wake := !sameForPages(s.access, a)
	changed := wake || !reflect.DeepEqual(withoutNow(s.access), withoutNow(a))
	s.access, s.etag = a, etag
	if !s.coreUp {
		s.coreUp, s.downSince = true, nil
		wake = true
	}
	if flipped := s.observe(); wake && !flipped {
		s.bump()
	}
	s.armFlip()
	if changed {
		s.persist()
	}
}

// sameForPages reports whether two policies look the same to a page on screen, which follows the
// state and its two moments (edge.js onState) and nothing else.
func sameForPages(a, b *Access) bool {
	if a == nil || b == nil {
		return a == b
	}
	same := func(x, y *time.Time) bool { return x == nil && y == nil || x != nil && y != nil && x.Equal(*y) }
	return a.State == b.State && same(a.OpensAt, b.OpensAt) && same(a.EndsAt, b.EndsAt)
}

// Counting reports whether the edge may count (§18.5): only under a known policy that allows it. With
// none yet (a first start with core unreachable) it does not.
func (s *Store) Counting() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.access != nil && s.access.Counting()
}

// ETag is what the next poll sends as If-None-Match.
func (s *Store) ETag() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.etag
}

// CoreUp records an answer from core (a 304 poll, a proxied response).
func (s *Store) CoreUp() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.coreUp {
		s.coreUp, s.downSince = true, nil
		s.bump()
	}
}

// CoreDown records that core did not answer. The outage starts at the first failure.
func (s *Store) CoreDown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.downSince == nil {
		t := s.now()
		s.downSince = &t
	}
	if s.coreUp {
		s.coreUp = false
		s.bump()
	}
}

// IsCoreDown reports whether the poller currently knows core is not answering. Used as a circuit
// breaker: no point asking /api/me for every staff request while core is down.
func (s *Store) IsCoreDown() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.downSince != nil
}

// observe recomputes the effective state and reports whether it changed. Caller holds mu.
func (s *Store) observe() bool {
	if s.access == nil {
		return false
	}
	eff := s.access.Effective(s.now())
	if eff == s.effective {
		return false
	}
	if eff == Open {
		t := s.now()
		if s.access.State == Scheduled {
			t = *s.access.OpensAt // the moment it opened, not the moment we noticed
		}
		s.openedAt = &t
	}
	s.effective = eff
	s.bump()
	s.persist()
	return true
}

// armFlip makes sure the waiters wake at opensAt without waiting for the next poll. Caller holds mu.
func (s *Store) armFlip() {
	if s.flip != nil {
		s.flip.Stop()
		s.flip = nil
	}
	if s.access == nil || s.access.State != Scheduled {
		return
	}
	d := s.access.OpensAt.Sub(s.now())
	if d < 0 {
		return
	}
	s.flip = time.AfterFunc(d, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.observe() && s.access != nil && s.access.State == Scheduled && s.now().Before(*s.access.OpensAt) {
			// timer fired a hair early against our clock: try again right after opensAt
			s.armFlip()
		}
	})
}

func (s *Store) bump() {
	s.seq++
	close(s.changed)
	s.changed = make(chan struct{})
}

// persist writes the policy atomically. Caller holds mu. Errors are logged: a read-only or full disk
// must not take the edge down, it only loses the policy across a restart.
func (s *Store) persist() {
	if s.file == "" || s.access == nil {
		return
	}
	body, err := json.Marshal(cacheFile{Access: s.access, ETag: s.etag, OpenedAt: s.openedAt})
	if err == nil {
		err = fsutil.WriteAtomic(s.file, body)
	}
	if err != nil {
		slog.Warn("policy cache not written", "file", s.file, "err", err)
	}
}

func withoutNow(a *Access) *Access {
	if a == nil {
		return nil
	}
	c := *a
	c.Now, c.EffectiveState = nil, "" // derived by core, may change without a revision; we derive our own
	return &c
}
