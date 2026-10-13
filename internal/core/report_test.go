// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mosaicast/mosaicast-edge/internal/stats"
)

var h0 = time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)

// reportCore is core's side of §18.5: it keeps what it was sent and answers as told.
type reportCore struct {
	mu      sync.Mutex
	got     []Report
	headers []http.Header
	status  int  // 0 = 200
	ackLess bool // acknowledge one hour fewer than sent
	hours   map[time.Time]map[string]int64
}

func (rc *reportCore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if r.URL.Path != "/api/site/edge/report" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var rep Report
	if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	rc.got = append(rc.got, rep)
	rc.headers = append(rc.headers, r.Header.Clone())
	if rc.status != 0 {
		w.WriteHeader(rc.status)
		_, _ = w.Write([]byte(`{"detail":"no"}`))
		return
	}
	if rc.hours == nil {
		rc.hours = map[time.Time]map[string]int64{}
	}
	var through *time.Time
	for i, h := range rep.Hours {
		if rc.ackLess && i == len(rep.Hours)-1 {
			break
		}
		rc.hours[h.Start] = h.Views // SET
		through = &h.Start
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ackThrough": through})
}

func (rc *reportCore) reports() []Report {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]Report(nil), rc.got...)
}

type fixture struct {
	core  *reportCore
	views *stats.Views
	rep   *Reporter
	on    *atomic.Bool
	now   *atomic.Int64 // unix ms
	dir   string
}

func newFixture(t *testing.T, token string) *fixture {
	t.Helper()
	rc := &reportCore{}
	srv := httptest.NewServer(rc)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	f := &fixture{core: rc, on: &atomic.Bool{}, now: &atomic.Int64{}, dir: t.TempDir()}
	f.on.Store(true)
	f.now.Store(h0.Add(10 * time.Minute).UnixMilli())
	now := func() time.Time { return time.UnixMilli(f.now.Load()).UTC() }
	f.views = stats.NewViews(stats.ViewsOptions{Now: now, On: f.on.Load, Dir: f.dir})
	f.rep = &Reporter{Client: NewClient(u, token, "test"), Views: f.views, On: f.on.Load, Now: now,
		Waiting: func() (int64, bool) { return 1234, false }, Revision: func() int64 { return 7 }}
	return f
}

func (f *fixture) add(d time.Duration) { f.now.Add(d.Milliseconds()) }

func TestReportSendsAndAcks(t *testing.T) {
	f := newFixture(t, "tok")
	f.views.Count(stats.Countdown)
	f.rep.Tick(context.Background())
	r := f.core.reports()
	if len(r) != 1 || r[0].Waiting != 1234 || r[0].PolicyRevision != 7 || len(r[0].Hours) != 0 || r[0].Hours == nil {
		t.Fatalf("first report: %+v", r)
	}
	if h := f.core.headers[0]; h.Get("Mosaicast-Edge-Token") != "tok" || h.Get("Mosaicast-Edge-Build") != "test" {
		t.Errorf("headers: %v", h)
	}

	f.add(time.Hour)
	f.rep.Tick(context.Background())
	r = f.core.reports()
	if len(r) != 2 || len(r[1].Hours) != 1 || !r[1].Hours[0].Start.Equal(h0) || r[1].Hours[0].Views["countdown"] != 1 {
		t.Fatalf("the completed hour: %+v", r[1])
	}
	f.rep.Tick(context.Background())
	if r = f.core.reports(); len(r[2].Hours) != 0 {
		t.Errorf("an acknowledged hour is not sent again: %+v", r[2])
	}
}

func TestReportResendsWhatWasNotAcknowledged(t *testing.T) {
	f := newFixture(t, "tok")
	f.views.Count(stats.Legal)
	f.add(time.Hour)
	f.core.status = http.StatusInternalServerError // the reply is lost
	f.rep.Tick(context.Background())
	f.core.status = http.StatusBadRequest
	f.rep.Tick(context.Background())
	f.core.status = 0
	f.add(time.Hour)
	f.core.ackLess = true // core takes the first, not the second
	f.rep.Tick(context.Background())
	f.core.ackLess = false
	f.rep.Tick(context.Background())
	r := f.core.reports()
	if len(r) != 4 || len(r[0].Hours) != 1 || len(r[1].Hours) != 1 || len(r[2].Hours) != 2 || len(r[3].Hours) != 1 ||
		!r[3].Hours[0].Start.Equal(h0.Add(time.Hour)) {
		t.Fatalf("reports: %+v", r)
	}
	if f.core.hours[h0]["legal"] != 1 || len(f.views.Pending(48)) != 0 {
		t.Errorf("stored %v, pending %v", f.core.hours, f.views.Pending(48))
	}
}

func TestReportBacksOffOnUnauthorizedAndNotFound(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusNotFound} {
		f := newFixture(t, "tok")
		f.core.status = status
		f.rep.Tick(context.Background())
		f.add(9 * time.Minute)
		f.rep.Tick(context.Background())
		if n := len(f.core.reports()); n != 1 {
			t.Errorf("%d: retried within 10 min (%d reports)", status, n)
		}
		f.add(2 * time.Minute)
		f.rep.Tick(context.Background())
		if n := len(f.core.reports()); n != 2 {
			t.Errorf("%d: not retried after 10 min (%d reports)", status, n)
		}
	}
}

func TestReportBatchesABacklog(t *testing.T) {
	f := newFixture(t, "tok")
	for range 100 {
		f.views.Tick()
		f.add(time.Hour)
	}
	f.rep.Tick(context.Background())
	r := f.core.reports()
	if len(r) != 3 || len(r[0].Hours) != 48 || len(r[1].Hours) != 48 || len(r[2].Hours) != 4 {
		t.Fatalf("batches: %d reports", len(r))
	}
	if !r[0].Hours[0].Start.Before(r[1].Hours[0].Start) {
		t.Error("oldest first")
	}
}

func TestReportNothingWhenOffOrWithoutToken(t *testing.T) {
	f := newFixture(t, "")
	f.views.Count(stats.Countdown)
	f.rep.Tick(context.Background())
	if len(f.core.reports()) != 0 {
		t.Error("no token: no report")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "stats.json")); err != nil {
		t.Error("no token: still counted and kept")
	}

	f = newFixture(t, "tok")
	f.views.Count(stats.Countdown)
	f.views.Tick()
	f.on.Store(false)
	f.rep.Tick(context.Background())
	if len(f.core.reports()) != 0 {
		t.Error("edgeCounting off: no report")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "stats.json")); !os.IsNotExist(err) {
		t.Error("edgeCounting off: stats.json deleted")
	}
}

func TestReportOnShutdown(t *testing.T) {
	f := newFixture(t, "tok")
	f.rep.Every = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.rep.Run(ctx); close(done) }()
	f.views.Count(stats.Countdown)
	cancel()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("the last report took longer than 3 s")
	}
	if len(f.core.reports()) != 1 {
		t.Error("one report on shutdown")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "stats.json")); err != nil {
		t.Error("the running hour is kept on shutdown")
	}
}

// Hours core keeps refusing do not hold up the gauge: after three 400s it goes alone, and the hours are
// tried again every 10 minutes.
func TestReportSendsTheGaugeAloneAfterRefusals(t *testing.T) {
	f := newFixture(t, "tok")
	f.views.Count(stats.Countdown)
	f.add(time.Hour)
	f.views.Tick()
	f.core.mu.Lock()
	f.core.status = http.StatusBadRequest
	f.core.mu.Unlock()
	for range 3 {
		f.rep.Tick(context.Background())
	}
	f.core.mu.Lock()
	f.core.status = 0
	f.core.mu.Unlock()
	f.rep.Tick(context.Background())
	r := f.core.reports()
	if len(r) != 4 || len(r[2].Hours) != 1 || len(r[3].Hours) != 0 || r[3].Waiting != 1234 {
		t.Fatalf("after three refusals: %d reports, last %+v", len(r), r[len(r)-1])
	}
	f.add(11 * time.Minute)
	f.rep.Tick(context.Background())
	if r = f.core.reports(); len(r[4].Hours) != 1 || len(f.views.Pending(48)) != 0 {
		t.Errorf("the hours are tried again after 10 min: %+v", r[4])
	}
	f.rep.Tick(context.Background())
	if r = f.core.reports(); len(r) != 6 {
		t.Error("back to normal")
	}
}
