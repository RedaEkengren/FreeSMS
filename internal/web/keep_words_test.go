package web

import (
	"regexp"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/i18n"
)

// keep.js says things at the worst possible moment -- the connection has
// gone and somebody's work is being held -- and it used to say them in
// English on a Swedish screen. Its words now come from the page, which
// renders them through the catalogue. These tests keep it that way.

var (
	jsString  = `"((?:[^"\\]|\\.)*)"`
	sayCall   = regexp.MustCompile(`say\(\s*"([\w-]+)",\s*` + jsString + `\s*\)`)
	countCall = regexp.MustCompile(`count\(\s*"([\w-]+)",\s*\w+,\s*` + jsString + `,\s*` + jsString + `\s*\)`)
	wordCall  = regexp.MustCompile(`word\(\s*[\w.]+,\s*"[\w-]+",\s*` + jsString + `\s*\)`)
)

func keepSource(t *testing.T) string {
	t.Helper()
	raw, err := Static.ReadFile("static/keep.js")
	if err != nil {
		t.Fatalf("read keep.js: %v", err)
	}
	return regexp.MustCompile(`(?m)^\s*//.*$`).ReplaceAllString(string(raw), "")
}

// A sentence written straight into keep.js is a sentence that will never be
// translated. Every one has to go through say, count or word, which read the
// page first and keep the English only as a fallback.
func TestKeepJSWritesNoMessageOfItsOwn(t *testing.T) {
	src := keepSource(t)
	for _, re := range []*regexp.Regexp{sayCall, countCall, wordCall} {
		src = re.ReplaceAllString(src, "")
	}
	// What is left is code: storage keys, selectors, header names. A
	// message is words with a space between them -- including a fragment
	// like " things are waiting", which is how the English used to be
	// glued together. The console line is for a developer and "use strict"
	// is for the interpreter.
	message := regexp.MustCompile(`"[^"\\]*(?:\\.[^"\\]*)*"`)
	words := regexp.MustCompile(`[A-Za-z]+ +[A-Za-z]+`)
	for _, code := range []string{`"[keep] could not store"`, `"use strict"`} {
		src = strings.ReplaceAll(src, code, "")
	}
	for _, m := range message.FindAllString(src, -1) {
		if !words.MatchString(m) {
			continue
		}
		t.Errorf("keep.js shows %s without reading it from the page; use say or count", m)
	}

	if len(sayCall.FindAllString(keepSource(t), -1)) < 5 || len(countCall.FindAllString(keepSource(t), -1)) < 3 {
		t.Fatal("found too few say and count calls; the patterns have stopped matching")
	}
}

// Each word keep.js asks for has to be on the page, under the key its
// English fallback is, and translated. Otherwise a Swedish page quietly
// falls back to the English, which is the bug this replaced.
func TestEveryWordKeepJSAsksForIsOnThePage(t *testing.T) {
	layout, err := Templates.ReadFile("templates/layout.html")
	if err != nil {
		t.Fatalf("read layout: %v", err)
	}
	page := string(layout)
	cats, err := i18n.Load("en", true)
	if err != nil {
		t.Fatalf("load catalogues: %v", err)
	}
	en, sv := cats.For("en"), cats.For("sv")
	unquote := func(s string) string { return strings.ReplaceAll(s, `\"`, `"`) }

	translated := func(key string, n int) {
		if got := sv.Form(key, n); strings.HasPrefix(got, "!!") || got == key {
			t.Errorf("%q has no Swedish translation", key)
		}
	}

	src := keepSource(t)
	for _, m := range sayCall.FindAllStringSubmatch(src, -1) {
		name, english := m[1], unquote(m[2])
		want := `data-` + name + `="{{.T "` + english + `"}}"`
		if !strings.Contains(page, want) {
			t.Errorf("layout.html does not carry %s", want)
		}
		translated(english, 2)
	}
	for _, m := range countCall.FindAllStringSubmatch(src, -1) {
		name, one, other := m[1], unquote(m[2]), unquote(m[3])
		for _, want := range []string{
			`data-` + name + `-one="{{.Plural "` + one + `" 1}}"`,
			`data-` + name + `-other="{{.Plural "` + one + `" 2}}"`,
		} {
			if !strings.Contains(page, want) {
				t.Errorf("layout.html does not carry %s", want)
			}
		}
		// The fallback is what an older page shows; it has to be the
		// catalogue's English, not a near miss of it.
		if got := en.Form(one, 2); got != other {
			t.Errorf("keep.js falls back to %q but the catalogue's English plural is %q", other, got)
		}
		translated(one, 1)
		translated(one, 2)
	}
}
