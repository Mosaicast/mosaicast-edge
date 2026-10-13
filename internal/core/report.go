// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/Mosaicast/mosaicast-edge/internal/stats"
)

// MaxHoursPerReport is §18.5's limit on hours in one report.
const MaxHoursPerReport = 48

// Report is the body of POST /api/site/edge/report (§18.5).
type Report struct {
	Waiting        int64        `json:"waiting"`
	WaitingCapped  bool         `json:"waitingCapped"`
	PolicyRevision int64        `json:"policyRevision"`
	Hours          []stats.Hour `json:"hours"`
}

// ErrRefused is core answering 400: the report was malformed in its eyes.
var ErrRefused = errors.New("core refused the report")

// errUnauthorized is a 401: the edge token is wrong.
var errUnauthorized = errors.New("core: wrong edge token")

// Report sends one report. It returns core's ackThrough (zero when core acknowledged no hour).
func (c *Client) Report(ctx context.Context, r Report) (time.Time, error) {
	if r.Hours == nil {
		r.Hours = []stats.Hour{}
	}
	body, err := json.Marshal(r)
	if err != nil {
		return time.Time{}, err
	}
	u, err := c.base.Parse("/api/site/edge/report")
	if err != nil {
		return time.Time{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "mosaicast-edge/"+c.build)
	req.Header.Set("Mosaicast-Edge-Build", c.build)
	req.Header.Set("Mosaicast-Edge-Token", c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return time.Time{}, err
	}
	defer resp.Body.Close()
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	switch resp.StatusCode {
	case http.StatusOK:
		var a struct {
			AckThrough *time.Time `json:"ackThrough"`
		}
		if err := json.Unmarshal(reply, &a); err != nil {
			return time.Time{}, fmt.Errorf("core: report answer: %w", err)
		}
		if a.AckThrough == nil {
			return time.Time{}, nil
		}
		return *a.AckThrough, nil
	case http.StatusBadRequest:
		var p struct {
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(reply, &p)
		return time.Time{}, fmt.Errorf("%w: %s", ErrRefused, p.Detail)
	case http.StatusUnauthorized:
		return time.Time{}, errUnauthorized
	case http.StatusNotFound:
		return time.Time{}, ErrNotFound
	default:
		return time.Time{}, fmt.Errorf("core: report answered %d", resp.StatusCode)
	}
}

// Reporter sends the edge's counts to core every Every (§18.5) while counting is on and the edge has a
// token, and once more on shutdown.
type Reporter struct {
	Client   *Client
	Views    *stats.Views
	Waiting  func() (n int64, capped bool) // the gauge as core gets it
	Revision func() int64                  // the revision of the policy in force
	On       func() bool                   // policy.Store.Counting
	Every    time.Duration                 // 0 = 30 s
	Now      func() time.Time

	retryAt time.Time // after a 401/404: not before
	state   string    // the last outcome logged, so a state is logged once, not every tick
}

// retryAfter is how long a 401 or 404 waits: core has no endpoint or token yet, or a wrong one.
const retryAfter = 10 * time.Minute

// Run reports until ctx ends, then keeps what is counted and reports one last time (3 s at most).
func (r *Reporter) Run(ctx context.Context) {
	every := r.Every
	if every == 0 {
		every = 30 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			last, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			r.Tick(last)
			cancel()
			return
		case <-t.C:
			r.Tick(ctx)
		}
	}
}

// Tick keeps the counts (stats.json) and sends what is due: the gauge and up to a few batches of hours.
func (r *Reporter) Tick(ctx context.Context) {
	r.Views.Tick()
	if !r.On() || r.Client.token == "" || r.Now().Before(r.retryAt) {
		return
	}
	for range 4 { // a backlog goes out in batches, not one batch every 30 s
		hours := r.Views.Pending(MaxHoursPerReport)
		n, capped := r.Waiting()
		through, err := r.Client.Report(ctx, Report{Waiting: n, WaitingCapped: capped, PolicyRevision: r.Revision(), Hours: hours})
		r.outcome(err)
		if err != nil {
			if errors.Is(err, errUnauthorized) || errors.Is(err, ErrNotFound) {
				r.retryAt = r.Now().Add(retryAfter)
			}
			return
		}
		if len(hours) > 0 && !through.IsZero() {
			r.Views.Ack(hours, through)
			r.Views.Tick()
		}
		if len(hours) < MaxHoursPerReport {
			return
		}
	}
}

func (r *Reporter) outcome(err error) {
	state := "ok"
	switch {
	case err == nil:
	case errors.Is(err, errUnauthorized):
		state = "unauthorized"
	case errors.Is(err, ErrNotFound):
		state = "not found"
	case errors.Is(err, ErrRefused):
		state = "refused"
	default:
		state = "failed"
	}
	if state == r.state {
		return
	}
	r.state = state
	switch state {
	case "ok":
		slog.Info("reporting counts to core")
	case "unauthorized":
		slog.Error("core refused the edge token for the report; check EDGE_CORE_TOKEN, retrying every 10 min")
	case "not found":
		slog.Info("core takes no report (an older core, or no edge token configured there); retrying every 10 min")
	case "refused":
		slog.Warn("core refused the report; keeping the hours", "err", err)
	default:
		slog.Warn("report not delivered; keeping the hours", "err", err)
	}
}
