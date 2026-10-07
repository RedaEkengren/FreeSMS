package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Nobody could choose a language. Setup made every shop English and no form
// offered another, so a workshop running with DEFAULT_LOCALE=sv read Swedish
// on the setup page and English on every page after it, and the Swedish
// translation was reachable only by editing the database.

func postForm(t *testing.T, ts *httptest.Server, client *http.Client, path string, form url.Values) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", ts.URL)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", path, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func pageText(t *testing.T, ts *httptest.Server, client *http.Client, path string) string {
	t.Helper()
	resp, err := client.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func TestAWorkshopSetUpInSwedishIsInSwedish(t *testing.T) {
	ts, pool, srv := emptyInstallation(t)
	srv.locale = "sv"
	client := noRedirects()
	if resp := submitSetup(t, ts, client, "a long enough password"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("setup gave %d", resp.StatusCode)
	}
	if got := pageText(t, ts, client, "/"); !strings.Contains(got, "Logga ut") {
		t.Fatal("the first page after setting up in Swedish is not in Swedish")
	}

	// The door too: somebody not yet signed in reads the workshop's language.
	if got := pageText(t, ts, http.DefaultClient, "/login"); !strings.Contains(got, "Logga in") {
		t.Error("the sign-in page of a Swedish workshop is not in Swedish")
	}

	// A person can read it in another language for themselves...
	if code := postForm(t, ts, client, "/account/language", url.Values{"locale": {"en"}}); code != http.StatusSeeOther {
		t.Fatalf("choosing English gave %d", code)
	}
	if got := pageText(t, ts, client, "/"); !strings.Contains(got, "Sign out") {
		t.Error("choosing English did not change the page")
	}
	// ...and go back to the workshop's, which is null, not "sv" copied in:
	// a copy would stop following the workshop the day it changes.
	if code := postForm(t, ts, client, "/account/language", url.Values{"locale": {""}}); code != http.StatusSeeOther {
		t.Fatalf("choosing the workshop's language gave %d", code)
	}
	var mine *string
	if err := pool.QueryRow(context.Background(), `SELECT locale FROM users`).Scan(&mine); err != nil {
		t.Fatal(err)
	}
	if mine != nil {
		t.Errorf("the workshop's language was stored as %q, want null", *mine)
	}

	// The workshop changes language, and the person who never chose follows.
	shop := url.Values{"name": {"Verkstaden"}, "payment_terms_days": {"30"}, "locale": {"en"}}
	if code := postForm(t, ts, client, "/shop", shop); code != http.StatusSeeOther {
		t.Fatalf("changing the workshop's language gave %d", code)
	}
	if got := pageText(t, ts, client, "/"); !strings.Contains(got, "Sign out") {
		t.Error("the workshop changed to English and the page did not follow")
	}

	// A language that did not ship is refused from either form, and nothing
	// is stored: stored, it would fall back to English without a word.
	if code := postForm(t, ts, client, "/account/language", url.Values{"locale": {"xx"}}); code != http.StatusBadRequest {
		t.Errorf("an unknown personal language gave %d, want 400", code)
	}
	shop.Set("locale", "xx")
	if code := postForm(t, ts, client, "/shop", shop); code != http.StatusBadRequest {
		t.Errorf("an unknown workshop language gave %d, want 400", code)
	}
	var shopLocale string
	if err := pool.QueryRow(context.Background(), `SELECT locale FROM shops`).Scan(&shopLocale); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT locale FROM users`).Scan(&mine); err != nil {
		t.Fatal(err)
	}
	if shopLocale != "en" || mine != nil {
		t.Errorf("after refusals the shop is %q and the person %v, want en and null", shopLocale, mine)
	}

	// A form without the field has not asked for English.
	shop.Del("locale")
	shop.Set("payment_terms_days", "20")
	postForm(t, ts, client, "/shop", url.Values{"name": {"Verkstaden"}, "payment_terms_days": {"30"}, "locale": {"sv"}})
	postForm(t, ts, client, "/shop", shop)
	if err := pool.QueryRow(context.Background(), `SELECT locale FROM shops`).Scan(&shopLocale); err != nil {
		t.Fatal(err)
	}
	if shopLocale != "sv" {
		t.Errorf("saving the details without the language left it %q, want sv", shopLocale)
	}
}
