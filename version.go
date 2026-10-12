// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

package main

import (
	_ "embed"
	"strings"
)

// VERSION is the version anchor: a release's tag must equal it (.github/workflows/release.yml), so every
// binary built from a released commit, from source or from the image, reports the release's version.
//
//go:embed VERSION
var versionFile string

var version = strings.TrimSpace(versionFile)
