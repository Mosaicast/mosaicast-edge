// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

package core

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// SessionCookie is core's session cookie (§8.5). It is the only cookie the edge ever forwards to
// /api/me, and it is never logged.
const SessionCookie = "MOSAICAST_SESSION"

// Anonymous is the role of a session core does not know (401), or of a cookie not worth asking about.
const Anonymous = ""

// Errors a role lookup can end in. Every one of them means "not passed" to the caller: fail closed.
var (
	ErrRateLimited = errors.New("role check: too many lookups from this client")
	ErrBusy        = errors.New("role check: too many lookups in flight")
)

// Roles answers "who is this session?" via GET /api/me (§18.2) and caches the answer briefly. There is
// deliberately no offline-verifiable pass: a ban or demotion takes effect at most TTL later.
//
// Anonymous visitors must cost core nothing, and a made-up cookie must not become a way around that.
// So before core is asked: the cookie must look like a session id core could have issued, the client
// must be within its lookup budget, and only so many lookups may be in flight at once. A 401 is
// remembered briefly, so one bad cookie sent over and over costs a single lookup.
type Roles struct {
	Client      *Client
	TTL         time.Duration // a known answer, ≤ 30 s
	NegativeTTL time.Duration // a 401
	Max         int           // cached sessions; bounded so random cookies cannot grow memory
	Timeout     time.Duration // per /api/me call
	PerClient   int           // cache-miss lookups per client IP per minute
	InFlight    int           // concurrent /api/me calls
	Now         func() time.Time

	mu      sync.Mutex
	order   *list.List // front = most recently used
	items   map[[32]byte]*list.Element
	buckets map[string]*bucket
	sem     chan struct{}
	once    sync.Once
}

type roleEntry struct {
	key     [32]byte
	role    string
	expires time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

const maxBuckets = 20000

// forwarded are the visitor's headers core may need to treat the request like the visitor's own.
var forwarded = []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "X-Forwarded-Port", "X-Real-Ip", "Forwarded"}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// LooksLikeSession reports whether v has the shape of a Spring Session cookie as core issues it: the
// base64 of a lowercase UUID, 48 characters (DefaultCookieSerializer with base64 on). Anything else was
// not issued by core. This shape is contract: core's config/SessionConfig.java sets the serializer and
// SiteAccessIntegrationTest pins the shape, so a change there fails core's build before it reaches here.
func LooksLikeSession(v string) bool {
	if len(v) != 48 {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(v)
	}
	return err == nil && uuidRe.Match(raw)
}

func (r *Roles) init() {
	r.once.Do(func() {
		r.items = map[[32]byte]*list.Element{}
		r.order = list.New()
		r.buckets = map[string]*bucket{}
		r.sem = make(chan struct{}, max(1, r.InFlight))
	})
}

// Role returns the session's role, or Anonymous. Any error must be treated as "not passed" by the
// caller (fail closed). Errors other than a 401 are not cached, so recovery is immediate.
func (r *Roles) Role(ctx context.Context, session string, visitor *http.Request) (string, error) {
	r.init()
	if !LooksLikeSession(session) {
		return Anonymous, nil // core never issued this; asking would only cost core
	}
	key := sha256.Sum256([]byte(session))
	if role, ok := r.cached(key); ok {
		return role, nil
	}
	if !r.allow(ClientIP(visitor)) {
		return "", ErrRateLimited
	}
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	default:
		return "", ErrBusy
	}

	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	h := http.Header{}
	h.Set("Accept", "application/json")
	// built by hand, never copied from the visitor: only the session cookie leaves the edge
	h.Set("Cookie", (&http.Cookie{Name: SessionCookie, Value: session}).String())
	for _, k := range forwarded {
		if v := visitor.Header.Values(k); len(v) > 0 {
			h[k] = v
		}
	}
	status, _, body, err := r.Client.get(ctx, "/api/me", h, 64<<10)
	if err != nil {
		return "", errors.New("role check: /api/me unreachable") // no detail: never risk echoing the request
	}
	switch status {
	case http.StatusOK:
		var me struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(body, &me); err != nil || me.Role == "" {
			return "", errors.New("role check: /api/me answered without a role")
		}
		r.store(key, me.Role, r.TTL)
		return me.Role, nil
	case http.StatusUnauthorized:
		r.store(key, Anonymous, r.NegativeTTL)
		return Anonymous, nil
	default:
		return "", fmt.Errorf("role check: /api/me answered %d", status)
	}
}

func (r *Roles) cached(key [32]byte) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	el, ok := r.items[key]
	if !ok {
		return "", false
	}
	e := el.Value.(*roleEntry)
	if !r.Now().Before(e.expires) {
		r.order.Remove(el)
		delete(r.items, key)
		return "", false
	}
	r.order.MoveToFront(el)
	return e.role, true
}

func (r *Roles) store(key [32]byte, role string, ttl time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	exp := r.Now().Add(ttl)
	if el, ok := r.items[key]; ok {
		e := el.Value.(*roleEntry)
		e.role, e.expires = role, exp
		r.order.MoveToFront(el)
		return
	}
	r.items[key] = r.order.PushFront(&roleEntry{key: key, role: role, expires: exp})
	for r.order.Len() > r.Max {
		oldest := r.order.Back()
		r.order.Remove(oldest)
		delete(r.items, oldest.Value.(*roleEntry).key)
	}
}

// allow spends one lookup from the client's budget: a token bucket of PerClient per minute.
func (r *Roles) allow(ip string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.Now()
	rate := float64(r.PerClient) / 60 // tokens per second
	b, ok := r.buckets[ip]
	if !ok {
		if len(r.buckets) >= maxBuckets {
			for k, v := range r.buckets {
				if now.Sub(v.last) > time.Minute {
					delete(r.buckets, k)
				}
			}
			if len(r.buckets) >= maxBuckets {
				return false // that many distinct clients in a minute: protect core, fail closed
			}
		}
		b = &bucket{tokens: float64(r.PerClient), last: now}
		r.buckets[ip] = b
	}
	b.tokens = min(float64(r.PerClient), b.tokens+now.Sub(b.last).Seconds()*rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Len is the number of cached sessions (tests).
func (r *Roles) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.items)
}

// ClientIP is the visitor's address. Behind a proxy on loopback or a private network (the TLS
// terminator), that is the last X-Forwarded-For entry, the one the proxy itself appended; a visitor
// talking to the edge directly cannot choose it.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer == nil || !(peer.IsLoopback() || peer.IsPrivate()) {
		return host
	}
	xff := strings.Join(r.Header.Values("X-Forwarded-For"), ",")
	if xff == "" {
		return host
	}
	parts := strings.Split(xff, ",")
	if ip := net.ParseIP(strings.TrimSpace(parts[len(parts)-1])); ip != nil {
		return ip.String()
	}
	return host
}
