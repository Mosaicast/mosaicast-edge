// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

// Package web embeds the edge's default theme: every page, in the site's own look, plus the runtime.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:default
var files embed.FS

// Default is the default theme's root folder (theme.json, templates/, texts/, assets/).
func Default() fs.FS {
	sub, err := fs.Sub(files, "default")
	if err != nil {
		panic(err)
	}
	return sub
}
