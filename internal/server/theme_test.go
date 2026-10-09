package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Light or dark is the person's: written into their pages by the server, not
// into a colleague's, and not into a page with nobody signed in.
func TestLightOrDarkIsThePersonsOwn(t *testing.T) {
	ts, _ := testServer(t)
	tech, desk := signIn(t, ts, techEmail), signIn(t, ts, advisorEmail)
	theme := func(c *http.Client, path string) string {
		t.Helper()
		_, body := page(t, c, ts.URL+path)
		_, tag, _ := strings.Cut(body, "<html")
		html, _, _ := strings.Cut(tag, ">")
		if _, v, ok := strings.Cut(html, `data-theme="`); ok {
			return v[:strings.Index(v, `"`)]
		}
		return ""
	}
	if got := theme(tech, "/"); got != "" {
		t.Errorf("before choosing, the page says %q rather than following the device", got)
	}
	if status := postStatus(t, tech, ts.URL+"/account/theme", url.Values{"theme": {"dark"}}); status != http.StatusSeeOther {
		t.Fatalf("choosing dark answered %d", status)
	}
	if got := theme(tech, "/"); got != "dark" {
		t.Errorf("chose dark, and the page says %q", got)
	}
	if got := theme(desk, "/board"); got != "" {
		t.Errorf("a colleague's page says %q", got)
	}
	if got := theme(&http.Client{}, "/login"); got != "" {
		t.Errorf("the sign-in page says %q", got)
	}
	if status := postStatus(t, tech, ts.URL+"/account/theme", url.Values{"theme": {"purple"}}); status == http.StatusSeeOther {
		t.Error("an unknown theme was taken")
	}
	postStatus(t, tech, ts.URL+"/account/theme", url.Values{"theme": {""}})
	if got := theme(tech, "/"); got != "" {
		t.Errorf("back to the device, and the page still says %q", got)
	}
}
