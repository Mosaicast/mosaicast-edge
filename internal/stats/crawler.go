// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

package stats

import "strings"

// crawlerList is ARCHITECTURE §19's frozen crawler list, verbatim (a test compares the two). Core
// applies the same list, so both sides leave out the same requests.
const crawlerList = `
bot crawl spider slurp preview fetch scan
facebookexternalhit facebookcatalog meta-externalagent whatsapp embedly iframely vkshare pinterest
google-pagerenderer google-read-aloud mastodon bluesky snapchat
headlesschrome lighthouse pagespeed uptime pingdom statuscake monitor
curl/ wget/ python- go-http-client java/ okhttp node-fetch axios httpclient libwww mosaicast-
`

var crawlers = strings.Fields(crawlerList)

// IsCrawler reports whether a User-Agent is on the list, or missing. The value is compared at the
// moment of the request and never kept.
func IsCrawler(userAgent string) bool {
	if strings.TrimSpace(userAgent) == "" {
		return true
	}
	ua := strings.ToLower(userAgent)
	for _, c := range crawlers {
		if strings.Contains(ua, c) {
			return true
		}
	}
	return false
}
