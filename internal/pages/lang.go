// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

package pages

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// pickLang chooses the page language from candidates: ?lang= (read, never stored), then
// Accept-Language by q-value, then the site's default language, then the first candidate.
func pickLang(r *http.Request, candidates []string, siteDefault string) string {
	has := func(l string) bool {
		for _, c := range candidates {
			if c == l {
				return true
			}
		}
		return false
	}
	if q := r.URL.Query().Get("lang"); q != "" && has(q) {
		return q
	}
	best, bestQ := "", 0.0
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		fields := strings.Split(strings.TrimSpace(part), ";")
		tag := strings.ToLower(strings.TrimSpace(fields[0]))
		if tag == "" || tag == "*" {
			continue
		}
		q := 1.0
		for _, f := range fields[1:] {
			if v, ok := strings.CutPrefix(strings.TrimSpace(f), "q="); ok {
				if p, err := strconv.ParseFloat(v, 64); err == nil {
					q = p
				}
			}
		}
		if q <= 0 || q <= bestQ {
			continue
		}
		match := ""
		if has(tag) {
			match = tag
		} else if i := strings.IndexByte(tag, '-'); i > 0 && has(tag[:i]) {
			match = tag[:i]
		}
		if match != "" {
			best, bestQ = match, q
		}
	}
	switch {
	case best != "":
		return best
	case has(siteDefault):
		return siteDefault
	default:
		return candidates[0]
	}
}

var (
	weekdays = map[string][7]string{
		"de": {"Sonntag", "Montag", "Dienstag", "Mittwoch", "Donnerstag", "Freitag", "Samstag"},
		"en": {"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"},
	}
	months = map[string][12]string{
		"de": {"Januar", "Februar", "März", "April", "Mai", "Juni", "Juli", "August", "September", "Oktober", "November", "Dezember"},
		"en": {"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"},
	}
)

// formatWhen renders an instant for readers without JavaScript, in the edge's configured time zone.
// The runtime re-renders it in the viewer's own zone with Intl.
func formatWhen(t time.Time, loc *time.Location, lang string) string {
	t = t.In(loc)
	zone, _ := t.Zone()
	switch lang {
	case "de":
		switch zone {
		case "CEST":
			zone = "MESZ"
		case "CET":
			zone = "MEZ"
		}
		return fmt.Sprintf("%s, %d. %s %d · %s Uhr %s", weekdays["de"][t.Weekday()], t.Day(), months["de"][t.Month()-1], t.Year(), t.Format("15:04"), zone)
	case "en":
		return fmt.Sprintf("%s, %d %s %d · %s %s", weekdays["en"][t.Weekday()], t.Day(), months["en"][t.Month()-1], t.Year(), t.Format("15:04"), zone)
	default:
		return t.Format("2006-01-02 15:04 MST")
	}
}
