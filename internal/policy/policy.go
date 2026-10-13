// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

// Package policy holds the site-access policy core publishes (ARCHITECTURE §18.1) and decides, on the
// edge's own clock, which page a visitor gets.
package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The stored states core can report.
const (
	Open        = "open"
	Scheduled   = "scheduled"
	Maintenance = "maintenance"
	Closed      = "closed"
)

// Unavailable is not a policy state: it is what the edge shows when it knows no policy at all (core was
// not reachable at boot and nothing was cached) or when core does not answer a proxied request.
const Unavailable = "unavailable"

// Roles, lowest first. The pass floor is one of them.
const (
	RoleFan       = "fan"
	RolePodcaster = "podcaster"
	RoleAdmin     = "admin"
)

// Access is the body of GET /api/site/access.
type Access struct {
	State          string     `json:"state"`
	EffectiveState string     `json:"effectiveState"`
	OpensAt        *time.Time `json:"opensAt"`
	EndsAt         *time.Time `json:"endsAt"`
	PassFloor      string     `json:"passFloor"`
	StaffLogin     StaffLogin `json:"staffLogin"`
	Now            *time.Time `json:"now,omitempty"` // core's clock: informational only, never used to decide
	Revision       int64      `json:"revision"`
	EdgeCounting   *bool      `json:"edgeCounting,omitempty"` // §18.1; absent (an older core) = on
}

// Counting reports whether the edge may count (§18.5, §19).
func (a *Access) Counting() bool {
	return a.EdgeCounting == nil || *a.EdgeCounting
}

// StaffLogin says whether the pages link the staff sign-in, and where each provider's flow starts.
type StaffLogin struct {
	Visible   bool       `json:"visible"`
	Providers []Provider `json:"providers"`
}

// Provider is one OAuth provider core offers, e.g. {"discord", "/oauth2/authorization/discord"}.
type Provider struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

// Parse decodes and validates a policy. A body that does not make sense is refused, so the last good
// policy stays in force rather than a half-understood one.
func Parse(body []byte) (*Access, error) {
	var a Access
	if err := json.Unmarshal(body, &a); err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}
	switch a.State {
	case Open, Maintenance, Closed:
	case Scheduled:
		if a.OpensAt == nil {
			return nil, errors.New("policy: scheduled without opensAt")
		}
	default:
		return nil, fmt.Errorf("policy: unknown state %q", a.State)
	}
	switch a.PassFloor {
	case RolePodcaster, RoleAdmin:
	case "":
		a.PassFloor = RolePodcaster
	default:
		// a floor this edge does not know could be meant to be stricter than anything it knows
		a.PassFloor = RoleAdmin
	}
	for _, p := range a.StaffLogin.Providers {
		if p.ID == "" || !isOAuthStart(p.Path) {
			return nil, fmt.Errorf("policy: provider %q has an unexpected path %q", p.ID, p.Path)
		}
	}
	return &a, nil
}

func isOAuthStart(p string) bool {
	const prefix = "/oauth2/authorization/"
	return len(p) > len(prefix) && p[:len(prefix)] == prefix
}

// Effective is the state in force at now. A scheduled policy whose opensAt has passed is open; the edge
// decides that on its own clock, not on core's effectiveState, so a launch happens on the second even
// when core is slow to answer (§18.1).
func (a *Access) Effective(now time.Time) string {
	if a.State == Scheduled && !now.Before(*a.OpensAt) {
		return Open
	}
	return a.State
}

// Passes reports whether a signed-in role may see the site while it is not open. ADMIN always passes.
func (a *Access) Passes(role string) bool {
	if role == RoleAdmin {
		return true
	}
	r, ok := rank[role]
	return ok && r >= rank[a.PassFloor]
}

var rank = map[string]int{RoleFan: 1, RolePodcaster: 2, RoleAdmin: 3}
