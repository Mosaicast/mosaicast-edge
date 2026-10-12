// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

// Command mosaicast-edge is the reverse proxy in front of Mosaicast core: it applies the site-access
// policy core publishes and renders the pages a visitor sees instead of the site (ARCHITECTURE §18).
//
//	mosaicast-edge                     serve (configured from EDGE_* environment variables)
//	mosaicast-edge healthcheck         for the distroless image's HEALTHCHECK
//	mosaicast-edge check-theme <dir>   validate a theme before deploying it (a folder, archive or EDGE_THEME spec)
//	mosaicast-edge pack-theme <dir>    validate a theme folder and write theme.tgz, printing its sha256
//	mosaicast-edge preview [flags]     preview pages and themes locally, with a fake core
//	mosaicast-edge version             print the version and build
package main

import (
	"fmt"
	"os"

	"github.com/Mosaicast/mosaicast-edge/internal/theme"
)

func main() {
	theme.EdgeVersion = version
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		os.Exit(serve())
	case "healthcheck":
		os.Exit(healthcheck(envOr(os.Getenv, "EDGE_LISTEN", ":8080")))
	case "check-theme":
		os.Exit(checkTheme(args))
	case "pack-theme":
		os.Exit(packTheme(args))
	case "preview":
		os.Exit(preview(args))
	case "version", "--version":
		fmt.Printf("mosaicast-edge %s (build %s)\n", version, buildID())
	case "-h", "--help", "help":
		fmt.Println("usage: mosaicast-edge [serve | healthcheck | check-theme <theme> | pack-theme [-o file] <dir> | preview [--theme <dir>] [--listen addr] | version]")
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (serve, healthcheck, check-theme, pack-theme, preview, version)\n", cmd)
		os.Exit(2)
	}
}
