package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/RedaEkengren/RedaSMS/internal/config"
	"github.com/RedaEkengren/RedaSMS/internal/database"
	"github.com/RedaEkengren/RedaSMS/internal/testsupport"
	"github.com/RedaEkengren/RedaSMS/internal/workshop"
	"github.com/RedaEkengren/RedaSMS/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// emptyServer is a fresh installation: migrated, and with no shop at all.
func emptyServer(t *testing.T) (*httptest.Server, *pgxpool.Pool) {
	t.Helper()
	pool := testsupport.FreshPool(t)
	if err := database.Migrate(context.Background(), pool, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := &config.Config{
		BaseURL:       "http://localhost",
		DefaultLocale: "en",
		Release:       "test",
		// Photographs go to a directory that disappears with the test.
		AttachmentsDir: t.TempDir(),
	}
	srv, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	handler, err := srv.routes()
	if err != nil {
		t.Fatalf("routes: %v", err)
	}
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts, pool
}

func noRedirects() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// The whole point of the issue: an empty database must serve something, not
// exit and leave a container restarting in a loop.
func TestFreshInstallationSendsYouToSetup(t *testing.T) {
	ts, _ := emptyServer(t)
	client := noRedirects()

	for _, path := range []string{"/", "/login", "/board"} {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/setup" {
			t.Errorf("%s gave %d -> %q, want 303 -> /setup",
				path, resp.StatusCode, resp.Header.Get("Location"))
		}
	}

	resp, err := client.Get(ts.URL + "/setup")
	if err != nil {
		t.Fatalf("get /setup: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/setup gave %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "Set up this workshop") {
		t.Error("/setup did not render the setup form")
	}
}

// Health must answer before setup, or a container is unhealthy for as long as
// it takes somebody to fill in a form.
func TestHealthAnswersBeforeSetup(t *testing.T) {
	ts, _ := emptyServer(t)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz gave %d during setup, want 200", resp.StatusCode)
	}
}

func submitSetup(t *testing.T, ts *httptest.Server, client *http.Client, password string) *http.Response {
	t.Helper()
	form := url.Values{
		"shop_name":  {"Verkstaden"},
		"owner_name": {"An Owner"},
		"email":      {"owner@example.test"},
		"password":   {password},
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/setup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", ts.URL)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post /setup: %v", err)
	}
	return resp
}

// Setting up signs the owner in: they typed the password ten seconds ago.
func TestSetupCreatesTheShopAndSignsTheOwnerIn(t *testing.T) {
	ts, _ := emptyServer(t)
	client := noRedirects()

	resp := submitSetup(t, ts, client, "a long enough password")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303\n%s", resp.StatusCode, body)
	}
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want /", loc)
	}

	// The owner's home is the counter board, and it is reachable without
	// signing in again.
	home, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	defer home.Body.Close()
	if home.StatusCode != http.StatusOK {
		t.Errorf("/ gave %d after setup, want 200", home.StatusCode)
	}
}

// Once there is a shop the page must stop working, or it is an open
// account-creation endpoint on a live installation.
func TestSetupStopsWorkingOnceThereIsAShop(t *testing.T) {
	ts, pool := emptyServer(t)

	resp := submitSetup(t, ts, noRedirects(), "a long enough password")
	resp.Body.Close()

	second := noRedirects()
	form, err := second.Get(ts.URL + "/setup")
	if err != nil {
		t.Fatalf("get /setup: %v", err)
	}
	form.Body.Close()
	if form.StatusCode != http.StatusSeeOther || form.Header.Get("Location") != "/login" {
		t.Errorf("/setup gave %d -> %q after setup, want 303 -> /login",
			form.StatusCode, form.Header.Get("Location"))
	}

	post := submitSetup(t, ts, second, "another long password")
	post.Body.Close()
	if post.Header.Get("Location") != "/login" {
		t.Errorf("a second setup submission gave %q, want a redirect to /login",
			post.Header.Get("Location"))
	}

	var shops int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM shops`).Scan(&shops); err != nil {
		t.Fatalf("count shops: %v", err)
	}
	if shops != 1 {
		t.Errorf("there are %d shops, want 1", shops)
	}
}

// Two people opening the page at the same moment must not produce two shops.
func TestConcurrentSetupCreatesOneShop(t *testing.T) {
	ts, pool := emptyServer(t)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := submitSetupQuiet(ts, "a long enough password")
			if resp != nil {
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()

	var shops, users int
	ctx := context.Background()
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM shops`).Scan(&shops); err != nil {
		t.Fatalf("count shops: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if shops != 1 {
		t.Errorf("four simultaneous submissions produced %d shops, want 1", shops)
	}
	if users != 1 {
		t.Errorf("four simultaneous submissions produced %d owners, want 1", users)
	}
}

func submitSetupQuiet(ts *httptest.Server, password string) *http.Response {
	form := url.Values{
		"shop_name":  {"Verkstaden"},
		"owner_name": {"An Owner"},
		"email":      {"owner@example.test"},
		"password":   {password},
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/setup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", ts.URL)
	client := noRedirects()
	resp, _ := client.Do(req)
	return resp
}

// A password the setup page accepts must be one the sign-in page will accept
// afterwards, and a short one must be refused here rather than later.
func TestSetupRefusesAShortPassword(t *testing.T) {
	ts, pool := emptyServer(t)

	resp := submitSetup(t, ts, noRedirects(), strings.Repeat("a", workshop.MinPasswordLength-1))
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(string(body), "characters") {
		t.Errorf("the error does not say what is wrong:\n%s", body)
	}

	var shops int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM shops`).Scan(&shops); err != nil {
		t.Fatalf("count shops: %v", err)
	}
	if shops != 0 {
		t.Errorf("a refused setup left %d shops behind", shops)
	}
}
