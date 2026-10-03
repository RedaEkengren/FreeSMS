package server

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/web"
)

// A page names each static file by its content, and the server keeps the
// named version forever and revalidates anything else.
//
// A fixed /static/keep.js cached for an hour meant a phone ran the old script
// against a new server for up to that hour after an update. Found verifying
// #56: the fixed keep.js was being served, and the page ran the old one from
// cache, which looked exactly like the fix not working.
func TestStaticFilesAreNamedByTheirContent(t *testing.T) {
	ts, _ := testServer(t)

	raw, err := web.Static.ReadFile("static/keep.js")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	want := "/static/keep.js?v=" + hex.EncodeToString(sum[:])[:12]

	resp, err := http.Get(ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(page), `src="`+want+`"`) {
		t.Fatalf("the sign-in page does not load keep.js as %s", want)
	}

	fetch := func(path, etag string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	current := fetch(want, "")
	if cc := current.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") || !strings.Contains(cc, "max-age=31536000") {
		t.Errorf("the current version: Cache-Control %q, want a year and immutable", cc)
	}
	for _, path := range []string{"/static/keep.js", "/static/keep.js?v=000000000000"} {
		resp := fetch(path, "")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s answered %d; an old page must still get the file", path, resp.StatusCode)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s: Cache-Control %q, want no-cache -- this is the URL an old page holds", path, cc)
		}
	}
	// Revalidating something unchanged costs a 304, not the file.
	etag := current.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag, so revalidation downloads the whole file every time")
	}
	if resp := fetch("/static/keep.js", etag); resp.StatusCode != http.StatusNotModified {
		t.Errorf("revalidating an unchanged file answered %d, want 304", resp.StatusCode)
	}
}
