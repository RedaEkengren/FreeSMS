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

// Which forms may be held offline is an allowlist in the templates. The queue
// used to hold every POST form, sign-in and setup included, and stored the
// whole body -- password and all -- in plain text in localStorage.
//
// The browser half of this was checked by hand: an offline sign-in stores
// nothing, old credential-bearing entries are purged without being sent, and a
// line added in the pit is still held. That check cannot run in CI, so the
// rule it rests on is pinned here instead.
func TestNoCredentialFormCanBeHeldOffline(t *testing.T) {
	entries, err := Templates.ReadDir("templates")
	if err != nil {
		t.Fatalf("read templates: %v", err)
	}
	formRe := regexp.MustCompile(`(?s)<form\b([^>]*)>(.*?)</form>`)

	// Money documents and one-time links need somebody watching the result.
	// The customer's decision on a shared link has nobody signed in to send
	// it as, and the queue sends nothing without a person.
	mustBeOnline := regexp.MustCompile(`action="/(login|setup|logout|shop)"|/invoice"|/credit"|/share"|/revoke"|/customer"|action="/i/`)

	var offline int
	for _, e := range entries {
		raw, err := Templates.ReadFile("templates/" + e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, m := range formRe.FindAllStringSubmatch(string(raw), -1) {
			tag, inner := m[1], m[2]
			if !strings.Contains(tag, "data-offline") {
				continue
			}
			offline++
			if strings.Contains(inner, `type="password"`) {
				t.Errorf("%s: a form with a password field is marked data-offline", e.Name())
			}
			// The queue holds a urlencoded body. A file in one is the text
			// "[object File]", so a form carrying a photograph is never held.
			if strings.Contains(inner, `type="file"`) || strings.Contains(tag, "multipart") {
				t.Errorf("%s: a form carrying a file is marked data-offline", e.Name())
			}
			if mustBeOnline.MatchString(tag) {
				t.Errorf("%s: %s is marked data-offline and must not be", e.Name(), strings.TrimSpace(tag))
			}
		}
	}
	// The allowlist being empty would pass every check above and quietly
	// switch the offline queue off.
	if offline < 5 {
		t.Errorf("only %d forms are marked data-offline; the queue would be all but disabled", offline)
	}

	keep, err := Static.ReadFile("static/keep.js")
	if err != nil {
		t.Fatalf("read keep.js: %v", err)
	}
	if !strings.Contains(string(keep), `hasAttribute("data-offline")`) {
		t.Error("keep.js no longer checks data-offline; it may be holding every form again")
	}
}

// On a shared counter browser, the next person to open a form used to be
// handed the last person's half-typed draft, and queued work with nobody's
// name on it was sent as whoever happened to be signed in.
//
// Checked by hand with two real users in one browser: drafts did not cross,
// one user's queued line was not sent while the other was signed in and was
// sent once when its owner returned, and an ownerless item was never sent.
// These are the rules that rests on.
func TestBrowserStorageBelongsToOnePerson(t *testing.T) {
	keep, err := Static.ReadFile("static/keep.js")
	if err != nil {
		t.Fatalf("read keep.js: %v", err)
	}
	src := string(keep)
	for rule, want := range map[string]string{
		"drafts are keyed by the person":            `DRAFT + currentUser() + "." + name`,
		"no drafts on a page with nobody signed in": `if (!name || !currentUser()) return;`,
		"queued work is sent only as its owner":     `it.user && it.user === me`,
		"signing out forgets this person's drafts":  `addEventListener("submit", forgetMyDrafts)`,
		"nothing is sent from an anonymous page":    `if (!currentUser() || needsSignIn)`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("%s: keep.js no longer contains %q", rule, want)
		}
	}

	layout, err := Templates.ReadFile("templates/layout.html")
	if err != nil {
		t.Fatalf("read layout: %v", err)
	}
	if !strings.Contains(string(layout), `{{if .Session.Scope.UserID}}<meta name="freesms-user"`) {
		t.Error("the layout no longer says who is signed in only when somebody is")
	}
}
