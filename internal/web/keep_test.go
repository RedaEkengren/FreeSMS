package web

import (
	"regexp"
	"strings"
	"testing"
)

// Every function keep.js calls has to be defined in keep.js.
//
// A rewrite of the offline queue once replaced a block that happened to
// contain guard() -- the function that catches a form submitted with no
// connection. The file still parsed, so a syntax check passed. In the browser
// the start-up handler threw on the first missing name and stopped, which
// silently took down the queue, the print button and the copy button with it.
// It was found by a browser and should have been found here.
//
// Not a JavaScript parser: a bare call -- a name followed by an opening
// bracket, not after a dot -- is either a builtin or one of ours, and ours
// have to exist.
func TestEveryFunctionKeepJSCallsIsDefined(t *testing.T) {
	raw, err := Static.ReadFile("static/keep.js")
	if err != nil {
		t.Fatalf("read keep.js: %v", err)
	}
	// Comments mention names in prose; strings are messages. Neither is code.
	strip := func(s string) string {
		s = regexp.MustCompile(`(?m)//.*$`).ReplaceAllString(s, "")
		return regexp.MustCompile(`"(?:[^"\\]|\\.)*"`).ReplaceAllString(s, `""`)
	}
	src := strip(string(raw))

	builtin := map[string]bool{
		// keywords followed by a bracket
		"if": true, "for": true, "while": true, "switch": true, "catch": true,
		"function": true, "return": true, "typeof": true,
		// browser and language globals
		"fetch": true, "encodeURIComponent": true, "decodeURIComponent": true,
		"String": true, "URL": true, "URLSearchParams": true, "FormData": true,
		"Event": true, "setTimeout": true, "clearTimeout": true,
	}

	defined := map[string]bool{}
	for _, m := range regexp.MustCompile(`function\s+([A-Za-z_]\w*)\s*\(`).FindAllStringSubmatch(src, -1) {
		defined[m[1]] = true
	}
	if len(defined) < 10 {
		t.Fatalf("found only %d function definitions; the pattern has stopped matching", len(defined))
	}
	seen := map[string]bool{}

	// The start-up block hands functions over by name rather than calling
	// them -- forEach.call(forms, guard), addEventListener("online", flush) --
	// and that is exactly how guard went missing without a bare call to it
	// anywhere. A name as the last argument there has to exist as well.
	// The section is found by its heading in the raw text, which is a
	// comment, and then stripped like everything else.
	if i := strings.Index(string(raw), "---- Wiring"); i >= 0 {
		for _, m := range regexp.MustCompile(`,\s*([a-z]\w*)\s*\)`).FindAllStringSubmatch(strip(string(raw)[i:]), -1) {
			name := m[1]
			if name == "true" || name == "false" || name == "null" || defined[name] || seen[name] {
				continue
			}
			seen[name] = true
			t.Errorf("keep.js hands %s to something at start-up and never defines it", name)
		}
	} else {
		t.Error("keep.js has no wiring section; this test no longer knows where start-up is")
	}

	// A bare call: not preceded by a dot or a word character.
	calls := regexp.MustCompile(`(^|[^.\w])([A-Za-z_]\w*)\s*\(`)
	for _, m := range calls.FindAllStringSubmatch(src, -1) {
		name := m[2]
		if builtin[name] || defined[name] || seen[name] {
			continue
		}
		// "function (" and "new X(" are not calls of X as one of ours.
		if strings.HasPrefix(name, "new") {
			continue
		}
		seen[name] = true
		t.Errorf("keep.js calls %s() and never defines it; the start-up handler will throw there", name)
	}
}
