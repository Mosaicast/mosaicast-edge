// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

package policy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)

func TestParse(t *testing.T) {
	good := `{"state":"scheduled","opensAt":"2026-10-18T10:00:00Z","passFloor":"admin","staffLogin":{"visible":true,"providers":[{"id":"discord","path":"/oauth2/authorization/discord"}]},"revision":4}`
	a, err := Parse([]byte(good))
	if err != nil || a.PassFloor != RoleAdmin || a.Revision != 4 {
		t.Fatalf("%+v %v", a, err)
	}
	for name, body := range map[string]string{
		"unknown state":      `{"state":"party"}`,
		"scheduled, no time": `{"state":"scheduled"}`,
		"provider elsewhere": `{"state":"open","staffLogin":{"providers":[{"id":"x","path":"https://evil.example/"}]}}`,
		"not json":           `nope`,
	} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	a, _ = Parse([]byte(`{"state":"closed","passFloor":"superuser"}`))
	if a.PassFloor != RoleAdmin {
		t.Error("an unknown floor must be treated as the strictest")
	}
	a, _ = Parse([]byte(`{"state":"closed"}`))
	if a.PassFloor != RolePodcaster {
		t.Error("default floor is podcaster")
	}
}

func TestEffectiveAndPasses(t *testing.T) {
	at := t0
	a := &Access{State: Scheduled, OpensAt: &at, PassFloor: RolePodcaster}
	if a.Effective(t0.Add(-time.Millisecond)) != Scheduled || a.Effective(t0) != Open {
		t.Error("scheduled flips to open exactly at opensAt")
	}
	m := &Access{State: Maintenance}
	if m.Effective(t0.Add(1000*time.Hour)) != Maintenance {
		t.Error("maintenance never ends on its own")
	}
	for role, want := range map[string]bool{RoleAdmin: true, RolePodcaster: true, RoleFan: false, "": false, "root": false} {
		if a.Passes(role) != want {
			t.Errorf("floor podcaster, role %q: %v", role, !want)
		}
	}
	a.PassFloor = RoleAdmin
	if a.Passes(RolePodcaster) || !a.Passes(RoleAdmin) {
		t.Error("floor admin")
	}
}

func TestStoreFlipWakesWaiters(t *testing.T) {
	now := time.Now()
	s := NewStore(time.Now, "")
	at := now.Add(80 * time.Millisecond)
	s.Set(&Access{State: Scheduled, OpensAt: &at, PassFloor: RolePodcaster, Revision: 1}, `"1"`)
	v := s.View()
	if v.Page != Scheduled {
		t.Fatalf("page %s", v.Page)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	next := s.Wait(ctx, v.Seq)
	if next.Page != Open || next.OpenedAt == nil || !next.OpenedAt.Equal(at) {
		t.Fatalf("flip at opensAt must wake waiters with page open, got %+v", next)
	}
}

func TestStoreDownSince(t *testing.T) {
	clk := t0
	s := NewStore(func() time.Time { return clk }, "")
	s.CoreDown()
	clk = clk.Add(time.Minute)
	s.CoreDown()
	if v := s.View(); v.DownSince == nil || !v.DownSince.Equal(t0) || !s.IsCoreDown() {
		t.Fatalf("outage starts at the first failure: %+v", v)
	}
	s.CoreUp()
	if v := s.View(); v.DownSince != nil || !v.CoreUp {
		t.Fatal("core up clears the outage")
	}
}

func TestStoreCache(t *testing.T) {
	dir := t.TempDir()
	clk := t0
	s := NewStore(func() time.Time { return clk }, dir)
	if s.View().Page != Unavailable {
		t.Fatal("no cache, no core: unavailable")
	}
	at := t0.Add(time.Hour)
	s.Set(&Access{State: Scheduled, OpensAt: &at, PassFloor: RolePodcaster, Revision: 7}, `"7"`)
	s2 := NewStore(func() time.Time { return clk }, dir)
	if v := s2.View(); v.Page != Scheduled || v.Access.Revision != 7 || s2.ETag() != `"7"` {
		t.Fatalf("cache not restored: %+v", v)
	}
	// a cache that no longer validates is ignored rather than applied
	_ = os.WriteFile(filepath.Join(dir, "policy.json"), []byte(`{"access":{"state":"bogus"}}`), 0o600)
	if NewStore(func() time.Time { return clk }, dir).View().Page != Unavailable {
		t.Error("invalid cache must be ignored")
	}
}

func TestStoreIgnoresDerivedFields(t *testing.T) {
	s := NewStore(func() time.Time { return t0 }, "")
	s.Set(&Access{State: Closed, PassFloor: RolePodcaster, Revision: 1, EffectiveState: Closed}, `"1"`)
	seq := s.View().Seq
	n := t0.Add(time.Second)
	s.Set(&Access{State: Closed, PassFloor: RolePodcaster, Revision: 1, EffectiveState: Closed, Now: &n}, `"1"`)
	if s.View().Seq != seq {
		t.Error("core's now changing must not wake every page")
	}
}
