// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/Mosaicast/mosaicast-edge/internal/themesrc"
)

// packTheme validates a theme folder and writes it as theme.tgz, the archive EDGE_THEME downloads:
// the same folder always gives the same bytes (sorted entries, no times, owners or modes beyond
// read/write), so its sha256 is a pin that a rebuild reproduces. Hidden files and folders (.git,
// .github, …) stay out; licenses and the README go in, since fonts and icons travel with theirs.
func packTheme(args []string) int {
	fl := flag.NewFlagSet("pack-theme", flag.ContinueOnError)
	out := fl.String("o", themesrc.AssetName, "archive to write")
	if err := fl.Parse(args); err != nil || fl.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: mosaicast-edge pack-theme [-o theme.tgz] <theme-folder>")
		return 2
	}
	dir := fl.Arg(0)
	_, ext, err := loadThemes(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "theme %s is not valid:\n%v\n", dir, err)
		return 1
	}
	body, n, err := pack(os.DirFS(dir))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// the archive must read back as the same theme, through the same code the edge uses
	if fsys, err := themesrc.Unpack(body); err != nil {
		fmt.Fprintln(os.Stderr, "packed archive does not unpack:", err)
		return 1
	} else if _, _, err := loadThemesFS(fsys); err != nil {
		fmt.Fprintln(os.Stderr, "packed archive is not a valid theme:", err)
		return 1
	}
	if err := os.WriteFile(*out, body, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	sum := sha256.Sum256(body)
	fmt.Printf("%s: theme %q %s, %d files, %d bytes\nsha256:%s\n", *out, ext.Name, ext.Version, n, len(body), hex.EncodeToString(sum[:]))
	return 0
}

func pack(fsys fs.FS) ([]byte, int, error) {
	var files []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != "." && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		switch {
		case d.IsDir():
		case d.Type().IsRegular():
			files = append(files, p)
		default:
			return fmt.Errorf("%s is not a plain file (links are not allowed in a theme)", p)
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	sort.Strings(files)
	var buf bytes.Buffer
	gz, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression) // zero header: no name, no time
	tw := tar.NewWriter(gz)
	for _, p := range files {
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil, 0, err
		}
		h := &tar.Header{Typeflag: tar.TypeReg, Name: p, Mode: 0o644, Size: int64(len(data)), Format: tar.FormatPAX}
		if len(p) < 100 {
			h.Format = tar.FormatUSTAR
		}
		if err := tw.WriteHeader(h); err != nil {
			return nil, 0, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, 0, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, 0, err
	}
	if err := gz.Close(); err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), len(files), nil
}
