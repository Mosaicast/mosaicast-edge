// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

package core

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func sessionID(n int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%08x-0000-4000-8000-%012x", n, n)))
}

type meServer struct {
	calls  atomic.Int64
	status int
	block  chan struct{} // non-nil: hold every request until closed
	srv    *httptest.Server
}

func newMe(t *testing.T) *meServer {
	m := &meServer{status: 200}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.calls.Add(1)
		if m.block != nil {
			<-m.block
		}
		if a := r.Header.Get("Authorization"); a != "" {
			if r.Header.Get("Cookie") != "" || !LooksLikeToken(strings.TrimPrefix(a, "Bearer ")) {
				t.Errorf("a token lookup sends the token alone, got Cookie %q, Authorization %d bytes", r.Header.Get("Cookie"), len(a))
			}
		} else if r.Header.Get("Cookie") == "" || strings.Count(r.Header.Get("Cookie"), "=") != 1 {
			t.Errorf("only the session cookie may reach core, got %q", r.Header.Get("Cookie"))
		}
		if m.status != 200 {
			w.WriteHeader(m.status)
			return
		}
		_, _ = w.Write([]byte(`{"role":"podcaster"}`))
	}))
	t.Cleanup(m.srv.Close)
	return m
}

type tick struct {
	mu sync.Mutex
	t  time.Time
}

func (c *tick) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *tick) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func roles(m *meServer, c *tick) *Roles {
	u, _ := url.Parse(m.srv.URL)
	return &Roles{Client: NewClient(u, "", "test"), TTL: 30 * time.Second, NegativeTTL: 10 * time.Second, Max: 100,
		Timeout: 2 * time.Second, PerClient: 10, InFlight: 32, Now: c.now}
}

func visitor(ip string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = ip + ":1234"
	return r
}

// token makes a value shaped like core's personal access tokens.
func token(n int) string {
	return "mcp_" + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%032d", n)))
}

func TestLooksLikeToken(t *testing.T) {
	for v, want := range map[string]bool{
		token(1): true,
		"mcp_" + strings.Repeat("-_aZ09", 7) + "x": true,
		"":                                     false,
		"mcp_":                                 false,
		token(1) + "A":                         false, // too long
		token(1)[:46]:                          false, // too short
		"MCP_" + token(1)[4:]:                  false,
		"mcp_" + strings.Repeat("A", 42) + "=": false, // padded, or standard base64
		"mcp_" + strings.Repeat("A", 42) + "+": false,
		sessionID(1):                           false,
	} {
		if LooksLikeToken(v) != want {
			t.Errorf("LooksLikeToken(%q) = %v", v, !want)
		}
	}
	r := httptest.NewRequest("GET", "/", nil)
	for h, want := range map[string]string{"Bearer " + token(1): token(1), "bearer " + token(1): "", "Basic x": "", "Bearer ": ""} {
		r.Header.Set("Authorization", h)
		if got, _ := BearerToken(r); got != want {
			t.Errorf("BearerToken(%q) = %q, want %q", h, got, want)
		}
	}
}

func TestTokenLookup(t *testing.T) {
	m := newMe(t)
	c := &tick{t: time.Unix(0, 0)}
	r := roles(m, c)
	if role, err := r.TokenRole(context.Background(), "mcp_short", visitor("192.0.2.1")); role != Anonymous || err != nil || m.calls.Load() != 0 {
		t.Fatalf("a malformed token is never looked up: %q %v, %d calls", role, err, m.calls.Load())
	}
	for range 3 {
		if role, err := r.TokenRole(context.Background(), token(1), visitor("192.0.2.1")); role != "podcaster" || err != nil {
			t.Fatalf("token: %q %v", role, err)
		}
	}
	if m.calls.Load() != 1 {
		t.Errorf("a token's answer is cached: %d calls", m.calls.Load())
	}
	m.status = 401
	if role, _ := r.TokenRole(context.Background(), token(2), visitor("192.0.2.1")); role != Anonymous {
		t.Error("a revoked token is anonymous")
	}
	_, _ = r.TokenRole(context.Background(), token(2), visitor("192.0.2.1"))
	if m.calls.Load() != 2 {
		t.Errorf("a revoked token is remembered: %d calls", m.calls.Load())
	}
}

func TestLooksLikeSession(t *testing.T) {
	for v, want := range map[string]bool{
		sessionID(1):            true,
		"":                      false,
		"abc":                   false,
		strings.Repeat("A", 48): false, // right length, not a UUID inside
		base64.StdEncoding.EncodeToString([]byte("00000001-0000-4000-8000-00000000000G")): false,
		base64.StdEncoding.EncodeToString([]byte("0000000A-0000-4000-8000-000000000001")): false, // uppercase
		sessionID(1) + "=": false,
	} {
		if got := LooksLikeSession(v); got != want {
			t.Errorf("LooksLikeSession(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestShapeCheckNeverAsksCore(t *testing.T) {
	m := newMe(t)
	r := roles(m, &tick{t: time.Unix(0, 0)})
	for _, v := range []string{"random", strings.Repeat("x", 48), "' OR 1=1 --"} {
		role, err := r.Role(context.Background(), v, visitor("203.0.113.1"))
		if err != nil || role != Anonymous {
			t.Errorf("%q: role=%q err=%v", v, role, err)
		}
	}
	if m.calls.Load() != 0 {
		t.Fatalf("core was asked %d times about cookies it never issued", m.calls.Load())
	}
}

func TestPerClientLimit(t *testing.T) {
	m := newMe(t)
	c := &tick{t: time.Unix(0, 0)}
	r := roles(m, c)
	// 10 distinct well-shaped cookies from one client get looked up, the 11th is refused
	for i := 0; i < 10; i++ {
		if _, err := r.Role(context.Background(), sessionID(i), visitor("203.0.113.1")); err != nil {
			t.Fatalf("lookup %d: %v", i, err)
		}
	}
	if _, err := r.Role(context.Background(), sessionID(99), visitor("203.0.113.1")); err != ErrRateLimited {
		t.Fatalf("11th lookup in a minute: %v, want ErrRateLimited", err)
	}
	// cached answers cost nothing and stay available
	if role, err := r.Role(context.Background(), sessionID(3), visitor("203.0.113.1")); err != nil || role != "podcaster" {
		t.Errorf("cached lookup: %q %v", role, err)
	}
	// another client has its own budget
	if _, err := r.Role(context.Background(), sessionID(100), visitor("203.0.113.2")); err != nil {
		t.Errorf("other client: %v", err)
	}
	// the budget refills over time
	c.add(6 * time.Second)
	if _, err := r.Role(context.Background(), sessionID(101), visitor("203.0.113.1")); err != nil {
		t.Errorf("after refill: %v", err)
	}
	if m.calls.Load() != 12 {
		t.Errorf("core calls = %d, want 12", m.calls.Load())
	}
}

func TestGlobalInFlightCap(t *testing.T) {
	m := newMe(t)
	m.block = make(chan struct{})
	r := roles(m, &tick{t: time.Unix(0, 0)})
	r.InFlight = 2
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = r.Role(context.Background(), sessionID(i), visitor(fmt.Sprintf("203.0.113.%d", i+1)))
		}(i)
	}
	for m.calls.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	if _, err := r.Role(context.Background(), sessionID(9), visitor("203.0.113.9")); err != ErrBusy {
		t.Errorf("third concurrent lookup: %v, want ErrBusy", err)
	}
	close(m.block)
	wg.Wait()
	if _, err := r.Role(context.Background(), sessionID(9), visitor("203.0.113.9")); err != nil {
		t.Errorf("after the others finished: %v", err)
	}
}

func TestNegativeCache(t *testing.T) {
	m := newMe(t)
	m.status = 401
	c := &tick{t: time.Unix(0, 0)}
	r := roles(m, c)
	for i := 0; i < 5; i++ {
		role, err := r.Role(context.Background(), sessionID(1), visitor("203.0.113.1"))
		if err != nil || role != Anonymous {
			t.Fatalf("401: role=%q err=%v", role, err)
		}
	}
	if m.calls.Load() != 1 {
		t.Fatalf("one bad cookie sent 5 times cost %d lookups, want 1", m.calls.Load())
	}
	c.add(11 * time.Second)
	_, _ = r.Role(context.Background(), sessionID(1), visitor("203.0.113.1"))
	if m.calls.Load() != 2 {
		t.Errorf("negative cache must expire after ~10 s")
	}
}

func TestErrorsAreNotCached(t *testing.T) {
	m := newMe(t)
	m.status = 503
	r := roles(m, &tick{t: time.Unix(0, 0)})
	if _, err := r.Role(context.Background(), sessionID(1), visitor("203.0.113.1")); err == nil {
		t.Fatal("503 must be an error")
	}
	m.status = 200
	if role, err := r.Role(context.Background(), sessionID(1), visitor("203.0.113.1")); err != nil || role != "podcaster" {
		t.Errorf("recovery: %q %v", role, err)
	}
}

func TestCacheIsBounded(t *testing.T) {
	m := newMe(t)
	r := roles(m, &tick{t: time.Unix(0, 0)})
	r.Max, r.PerClient = 5, 1000
	for i := 0; i < 20; i++ {
		_, _ = r.Role(context.Background(), sessionID(i), visitor("203.0.113.1"))
	}
	if r.Len() != 5 {
		t.Errorf("cache holds %d entries, want 5", r.Len())
	}
}

func TestClientIP(t *testing.T) {
	cases := []struct{ remote, xff, want string }{
		{"203.0.113.5:1", "", "203.0.113.5"},
		{"203.0.113.5:1", "198.51.100.1", "203.0.113.5"},           // public peer: its own address, XFF ignored
		{"127.0.0.1:1", "198.51.100.1", "198.51.100.1"},            // behind the TLS terminator on loopback
		{"172.18.0.1:1", "10.0.0.1, 198.51.100.9", "198.51.100.9"}, // rightmost = what the proxy appended
		{"172.18.0.1:1", "garbage", "172.18.0.1"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := ClientIP(r); got != c.want {
			t.Errorf("ClientIP(%s, %q) = %s, want %s", c.remote, c.xff, got, c.want)
		}
	}
}
