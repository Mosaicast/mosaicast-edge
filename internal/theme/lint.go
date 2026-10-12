// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

package theme

import (
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
)

var (
	deviceClock = regexp.MustCompile(`Date\.now\(\)|new Date\(\s*\)|performance\.timeOrigin`)
	remoteRef   = regexp.MustCompile(`(?i)(?:src|href)\s*=\s*["']?(?:https?:)?//`)
)

// Lint reports things that are allowed but probably wrong. check-theme prints them as warnings;
// they never stop the edge from starting.
func Lint(fsys fs.FS, t *Theme) []string {
	var out []string
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		switch path.Ext(p) {
		case ".js":
			body, _ := fs.ReadFile(fsys, p)
			if deviceClock.Match(body) {
				out = append(out, fmt.Sprintf("%s reads the device clock; use window.edge.now() or the edge:tick event (the device clock is not evidence of anything)", p))
			}
			if strings.Contains(string(body), "fetch(") || strings.Contains(string(body), "XMLHttpRequest") {
				out = append(out, fmt.Sprintf("%s makes requests; themes get their data from the runtime, and the CSP allows only the edge's own origin", p))
			}
		case ".html":
			body, _ := fs.ReadFile(fsys, p)
			if remoteRef.Match(body) {
				out = append(out, fmt.Sprintf("%s links to another origin; themes are self-contained and the CSP will block it", p))
			}
			if strings.Contains(string(body), "<script>") || strings.Contains(string(body), " style=\"") {
				out = append(out, fmt.Sprintf("%s has inline script or style; the CSP blocks both, use files under assets/", p))
			}
		}
		return nil
	})
	if len(t.Credits) == 0 && t.Assets.Count() > 0 {
		out = append(out, "theme.json has no credits; list the fonts and artwork you ship and their licenses")
	}
	sort.Strings(out)
	return out
}
