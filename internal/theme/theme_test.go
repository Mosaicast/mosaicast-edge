// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

package theme

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

const page = `<!doctype html><html {{.HTMLAttrs}}><head>{{.EdgeHead}}<title>x</title></head><body>{{.T "hello"}}</body></html>`

func minimal() fstest.MapFS {
	return fstest.MapFS{
		"theme.json": {Data: []byte(`{"schema":1,"name":"T","version":"1","locales":["en","de"],
			"styles":["css/t.css"],"pages":{"countdown":{"mode":"full","template":"countdown.html"},"closed":{"mode":"restyle"}},
			"credits":[{"work":"Font","license":"OFL-1.1","url":"https://example.org"}]}`)},
		"templates/countdown.html": {Data: []byte(page)},
		"texts/en.json":            {Data: []byte(`{"hello":"Hello"}`)},
		"texts/de.json":            {Data: []byte(`{"hello":"Hallo"}`)},
		"assets/css/t.css":         {Data: []byte(`body{background:url(../img/bg.webp)} @font-face{src:url("../fonts/f.woff2")}`)},
		"assets/img/bg.webp":       {Data: []byte("RIFF....WEBP")},
		"assets/fonts/f.woff2":     {Data: []byte("wOF2")},
		"README.md":                {Data: []byte("not served")},
	}
}

func TestLoadMinimal(t *testing.T) {
	th, err := Load(minimal(), "/_a/t/")
	if err != nil {
		t.Fatal(err)
	}
	if th.Mode("countdown") != ModeFull || th.Mode("closed") != ModeRestyle || th.Mode("login") != "" {
		t.Error("modes")
	}
	if th.PartySeconds != 15 || th.PreloadSeconds != 60 || th.HandoffSeconds != 1 {
		t.Error("countdown defaults")
	}
	u, err := th.Assets.URL("css/t.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(th.Assets.files[strings.TrimPrefix(u, "/_a/t/")].body)
	bg, _ := th.Assets.URL("img/bg.webp")
	if !strings.Contains(css, "url("+bg+")") || strings.Contains(css, "../") {
		t.Errorf("css refs not rewritten to hashed names: %s", css)
	}
	if th.Assets.Has("README.md") {
		t.Error("only assets/ is served")
	}
}

func TestLoadRejects(t *testing.T) {
	cases := map[string]func(fstest.MapFS){
		"unknown css ref": func(m fstest.MapFS) {
			m["assets/css/t.css"] = &fstest.MapFile{Data: []byte(`a{background:url(nope.png)}`)}
		},
		"external css ref": func(m fstest.MapFS) {
			m["assets/css/t.css"] = &fstest.MapFile{Data: []byte(`a{background:url(https://cdn.example/x.png)}`)}
		},
		"css import": func(m fstest.MapFS) { m["assets/css/t.css"] = &fstest.MapFile{Data: []byte(`@import "x.css";`)} },
		"file type":  func(m fstest.MapFS) { m["assets/x.exe"] = &fstest.MapFile{Data: []byte("MZ")} },
		"too large":  func(m fstest.MapFS) { m["assets/big.png"] = &fstest.MapFile{Data: make([]byte, MaxAssetBytes+1)} },
		"locale keys": func(m fstest.MapFS) {
			m["texts/de.json"] = &fstest.MapFile{Data: []byte(`{"hello":"Hallo","extra":"x"}`)}
		},
		"missing texts": func(m fstest.MapFS) {
			delete(m, "texts/de.json")
			m["texts/en.json"] = &fstest.MapFile{Data: []byte(`{"hello":"Hello"}`)}
			setManifest(m, `"texts":"texts/{lang}.json",`)
		},
		"unknown key":      func(m fstest.MapFS) { setManifest(m, `"colour":"red",`) },
		"unknown page":     func(m fstest.MapFS) { setManifest(m, `"pages":{"home":{"mode":"restyle"}},`) },
		"full no template": func(m fstest.MapFS) { setManifest(m, `"pages":{"closed":{"mode":"full"}},`) },
		"missing template": func(m fstest.MapFS) { setManifest(m, `"pages":{"closed":{"mode":"full","template":"nope.html"}},`) },
		"style not asset":  func(m fstest.MapFS) { setManifest(m, `"styles":["css/nope.css"],`) },
		"party bounds":     func(m fstest.MapFS) { setManifest(m, `"countdown":{"partySeconds":500},`) },
		"cue name":         func(m fstest.MapFS) { setManifest(m, `"countdown":{"cues":{"Big Bang":3}},`) },
		"expire too short": func(m fstest.MapFS) { setManifest(m, `"assets":{"expireAfterOpen":"10s"},`) },
		"keep unknown":     func(m fstest.MapFS) { setManifest(m, `"assets":{"keep":["img/nope.png"]},`) },
		"credit url":       func(m fstest.MapFS) { setManifest(m, `"credits":[{"work":"x","url":"http://insecure"}],`) },
		"schema":           func(m fstest.MapFS) { setManifest(m, `"schema":2,`) },
		"bad template":     func(m fstest.MapFS) { m["templates/countdown.html"] = &fstest.MapFile{Data: []byte(`{{if}}`)} },
	}
	for name, mutate := range cases {
		m := minimal()
		mutate(m)
		if _, err := Load(m, "/_a/t/"); err == nil {
			t.Errorf("%s: expected Load to fail", name)
		}
	}
}

// setManifest overrides top-level keys of theme.json by prepending them (later duplicates lose in Go's decoder? no:
// the last one wins), so the override goes at the end instead.
func setManifest(m fstest.MapFS, fields string) {
	body := string(m["theme.json"].Data)
	body = strings.TrimSuffix(strings.TrimSpace(body), "}") + "," + strings.TrimSuffix(fields, ",") + "}"
	m["theme.json"] = &fstest.MapFile{Data: []byte(body)}
}

func TestSymlinksRefused(t *testing.T) {
	m := minimal()
	m["templates/evil.html"] = &fstest.MapFile{Data: []byte("/etc/passwd"), Mode: fs.ModeSymlink}
	if _, err := Load(m, "/_a/t/"); err == nil || !strings.Contains(err.Error(), "symbolic links") {
		t.Fatalf("a symlink anywhere in the theme must be refused: %v", err)
	}
}

func TestUnknownKeySaysWhy(t *testing.T) {
	m := minimal()
	setManifest(m, `"sparkles":true,`)
	_, err := Load(m, "/_a/t/")
	if err == nil || !strings.Contains(err.Error(), `unknown key "sparkles"`) || !strings.Contains(err.Error(), "newer mosaicast-edge") {
		t.Fatalf("got %v", err)
	}
}

func TestSameKeys(t *testing.T) {
	if err := SameKeys(map[string]map[string]string{"en": {"a": "1"}, "de": {"a": "2"}}); err != nil {
		t.Error(err)
	}
	err := SameKeys(map[string]map[string]string{"en": {"a": "1", "b": "2"}, "de": {"a": "2", "c": "3"}})
	if err == nil || !strings.Contains(err.Error(), `"b"`) || !strings.Contains(err.Error(), `"c"`) {
		t.Errorf("both directions must be reported: %v", err)
	}
}

func TestLint(t *testing.T) {
	m := minimal()
	m["assets/js/t.js"] = &fstest.MapFile{Data: []byte(`const t = Date.now(); fetch('/api')`)}
	th, err := Load(m, "/_a/t/")
	if err != nil {
		t.Fatal(err)
	}
	w := strings.Join(Lint(m, th), "\n")
	if !strings.Contains(w, "device clock") || !strings.Contains(w, "makes requests") {
		t.Errorf("lint: %s", w)
	}
}
