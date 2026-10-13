// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

package stats

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newViews(c *clock, on *atomic.Bool, dir string) *Views {
	return NewViews(ViewsOptions{Now: c.now, On: on.Load, Dir: dir})
}

func TestCrawlerListIsTheSpecs(t *testing.T) {
	doc, err := os.ReadFile("../../docs/ARCHITECTURE.md")
	if err != nil {
		t.Fatal(err)
	}
	_, after, ok := strings.Cut(string(doc), "**Crawler list**")
	if !ok {
		t.Fatal("§19 has no crawler list")
	}
	_, block, _ := strings.Cut(after, "```\n")
	block, _, _ = strings.Cut(block, "```")
	if strings.Join(strings.Fields(block), " ") != strings.Join(crawlers, " ") {
		t.Errorf("the crawler list differs from §19:\nspec: %s\nedge: %s", strings.Join(strings.Fields(block), " "), strings.Join(crawlers, " "))
	}
}

func TestIsCrawler(t *testing.T) {
	for ua, want := range map[string]bool{
		"":   true,
		"  ": true,
		"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)": true,
		"facebookexternalhit/1.1 Facebot Twitterbot/1.0":                           true,
		"WhatsApp/2.23.20.0": true,
		"curl/8.5.0":         true,
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/129.0.0.0 Safari/537.36": true,
		"mosaicast-edge/abc": true,
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36":                         false,
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1": false,
		"Mozilla/5.0 (X11; Linux x86_64; rv:131.0) Gecko/20100101 Firefox/131.0":                                                                  false,
	} {
		if IsCrawler(ua) != want {
			t.Errorf("%q: crawler=%v, want %v", ua, !want, want)
		}
	}
}

func TestViewsCompleteHoursAndAck(t *testing.T) {
	c := &clock{t: t0.Add(10 * time.Minute)}
	var on atomic.Bool
	on.Store(true)
	v := newViews(c, &on, "")
	v.Count(Countdown)
	v.Count(Countdown)
	v.Count(Legal)
	if len(v.Pending(48)) != 0 {
		t.Fatal("the running hour is never sent")
	}
	c.add(time.Hour)
	v.Tick() // an hour with views ended
	c.add(time.Hour)
	v.Tick() // an hour with none ended: sent as zeros
	p := v.Pending(48)
	if len(p) != 2 || !p[0].Start.Equal(t0) || !p[1].Start.Equal(t0.Add(time.Hour)) {
		t.Fatalf("pending: %+v", p)
	}
	if p[0].Views["countdown"] != 2 || p[0].Views["legal"] != 1 || p[0].Views["login"] != 0 || len(p[0].Views) != int(numKinds) {
		t.Errorf("first hour: %+v", p[0].Views)
	}
	if p[1].Views["countdown"] != 0 || len(p[1].Views) != int(numKinds) {
		t.Errorf("an empty hour carries every kind as 0: %+v", p[1].Views)
	}
	if got := v.Pending(1); len(got) != 1 || !got[0].Start.Equal(t0) {
		t.Errorf("at most n, oldest first: %+v", got)
	}

	// core acknowledges through the second hour, but only the first was in that report
	v.Ack(p[:1], t0.Add(time.Hour))
	if p := v.Pending(48); len(p) != 1 || !p[0].Start.Equal(t0.Add(time.Hour)) {
		t.Errorf("only sent hours are dropped: %+v", p)
	}
	v.Ack(v.Pending(48), t0) // an ack older than the hour sent drops nothing
	if len(v.Pending(48)) != 1 {
		t.Error("an hour after ackThrough stays")
	}
}

func TestViewsSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: t0.Add(10 * time.Minute)}
	var on atomic.Bool
	on.Store(true)
	v := newViews(c, &on, dir)
	v.Count(Countdown)
	v.Tick()

	c.add(20 * time.Minute) // restarted within the hour: it carries on
	v = newViews(c, &on, dir)
	v.Count(Countdown)
	v.Tick()

	c.add(3 * time.Hour) // down past the end of the hour: it is completed, the hours missed are not invented
	v = newViews(c, &on, dir)
	p := v.Pending(48)
	if len(p) != 1 || !p[0].Start.Equal(t0) || p[0].Views["countdown"] != 2 {
		t.Fatalf("after restarts: %+v", p)
	}
	v.Tick()
	v = newViews(c, &on, dir)
	if len(v.Pending(48)) != 1 {
		t.Error("pending hours survive until acknowledged")
	}
}

func TestViewsOffCountsNothingAndDeletes(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "stats.json")
	c := &clock{t: t0}
	var on atomic.Bool
	v := newViews(c, &on, dir)
	v.Count(Countdown)
	v.Tick()
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("off from the start: no file")
	}

	on.Store(true)
	v.Count(Countdown)
	c.add(time.Hour)
	v.Count(Countdown)
	v.Tick()
	if _, err := os.Stat(file); err != nil || len(v.Pending(48)) != 1 {
		t.Fatal("on: counted and kept")
	}

	on.Store(false)
	v.Count(Countdown)
	v.Tick()
	if _, err := os.Stat(file); !os.IsNotExist(err) || len(v.Pending(48)) != 0 {
		t.Fatal("off: everything dropped, the file deleted")
	}

	on.Store(true) // back on: fresh, nothing from before comes back
	c.add(time.Hour)
	v.Count(Legal)
	c.add(time.Hour)
	v.Tick()
	if p := v.Pending(48); len(p) != 1 || p[0].Views["legal"] != 1 || p[0].Views["countdown"] != 0 {
		t.Errorf("after off and on: %+v", p)
	}
}

func TestViewsKeepAtMostAWeek(t *testing.T) {
	c := &clock{t: t0}
	var on atomic.Bool
	on.Store(true)
	v := newViews(c, &on, "")
	for range MaxPending + 5 {
		v.Tick()
		c.add(time.Hour)
	}
	v.Tick()
	p := v.Pending(1000)
	if len(p) != MaxPending || !p[0].Start.Equal(t0.Add(5*time.Hour)) {
		t.Errorf("kept %d, oldest %v", len(p), p[0].Start)
	}
}

func TestKindOf(t *testing.T) {
	if k, ok := KindOf("legal"); !ok || k != Legal {
		t.Error("legal")
	}
	if _, ok := KindOf("open"); ok {
		t.Error("open is not an edge page")
	}
}
