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
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/auth"
	"github.com/RedaEkengren/FreeSMS/internal/config"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/testsupport"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/RedaEkengren/FreeSMS/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// emptyServer is a fresh installation: migrated, and with no shop at all.
func emptyServer(t *testing.T) (*httptest.Server, *pgxpool.Pool) {
	t.Helper()
	ts, pool, _ := emptyInstallation(t)
	return ts, pool
}

// emptyInstallation is emptyServer with the Server itself, for what runs
// beside the handlers.
func emptyInstallation(t *testing.T) (*httptest.Server, *pgxpool.Pool, *Server) {
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
	return ts, pool, srv
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
	if loc := resp.Header.Get("Location"); loc != "/start" {
		t.Errorf("Location = %q, want /start", loc)
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

// A configured installation refuses setup without doing any password work.
//
// Setup is open to anybody who can reach the server, and it used to spend
// 64 MiB of argon2 on every request before noticing there was already a shop.
// Counted rather than timed: the number of argon2 computations the process has
// run must not move.
func TestSetupOnAConfiguredInstallationDoesNoPasswordWork(t *testing.T) {
	ts, _ := testServer(t)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	form := url.Values{
		"shop_name": {"Someone Else's Garage"}, "owner_name": {"Mallory"},
		"email": {"mallory@example.test"}, "password": {"a perfectly long password"},
	}

	before := auth.Checks()
	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/setup", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", ts.URL)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
			t.Errorf("setup on a configured installation = %d to %q, want 303 to /login",
				resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	if ran := auth.Checks() - before; ran != 0 {
		t.Errorf("five refused setup requests ran %d password computations; want none", ran)
	}
}

// Setup tells the background work there is now a shop. The retention sweep
// used to be handed the shop once, at start-up -- empty on a new
// installation -- and never learned of the one setup made.
func TestSetupWakesWhatWasWaitingForAShop(t *testing.T) {
	ts, pool, srv := emptyInstallation(t)

	select {
	case <-srv.ShopSet():
		t.Fatal("an empty installation announced a shop before setup")
	default:
	}
	if srv.Shop() != "" {
		t.Fatalf("Shop() = %q before setup", srv.Shop())
	}

	resp := submitSetup(t, ts, noRedirects(), "a reasonable workshop password")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("setup answered %d", resp.StatusCode)
	}

	select {
	case <-srv.ShopSet():
	case <-time.After(2 * time.Second):
		t.Fatal("setup made a shop and did not say so")
	}
	var made string
	if err := pool.QueryRow(context.Background(), `SELECT id::text FROM shops`).Scan(&made); err != nil {
		t.Fatalf("read shop: %v", err)
	}
	if srv.Shop() != made {
		t.Errorf("Shop() = %q, want the shop setup made, %q", srv.Shop(), made)
	}
}

// A shop known at start-up is not announced: the sweep runs once when it
// starts anyway, and a stale signal would run it twice.
func TestAShopKnownAtStartIsNotNews(t *testing.T) {
	_, pool := testServer(t)
	srv, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)),
		&config.Config{BaseURL: "http://localhost", DefaultLocale: "en", AttachmentsDir: t.TempDir()}, shopA)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	select {
	case <-srv.ShopSet():
		t.Error("a shop known at start-up was announced as new")
	default:
	}
}

// A new owner, with nothing but this page: the list says what is missing, each
// step is done through the forms the list links to, nothing else is blocked
// along the way, and at the end it says the workshop is ready.
func TestAFirstSetupCanBeDoneFromTheChecklistAlone(t *testing.T) {
	ts, _ := emptyServer(t)
	client := noRedirects()
	resp := submitSetup(t, ts, client, "a long enough password")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("setup answered %d", resp.StatusCode)
	}
	get := func(path string) (int, string) {
		r, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}
	post := func(path string, v url.Values) int {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(v.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", ts.URL)
		r, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}

	status, body := get("/start")
	if status != http.StatusOK {
		t.Fatalf("/start answered %d", status)
	}
	for _, want := range []string{"Getting started", "Your company details", "company number", "to do", "Who owns the backups", "Start clean", "No messaging provider is set up", "href=\"/shop\""} {
		if !strings.Contains(body, want) {
			t.Errorf("the first view does not say %q", want)
		}
	}
	if strings.Contains(body, "Ready for the first customer") {
		t.Fatal("a workshop with nothing said is ready")
	}
	// Work is not in the way: the board and the new-job form answer.
	for _, path := range []string{"/", "/jobs/new", "/board"} {
		if status, _ := get(path); status != http.StatusOK && status != http.StatusSeeOther {
			t.Errorf("%s answered %d with the setup unfinished", path, status)
		}
	}

	// The company details through the form the list links to.
	if st := post("/shop", url.Values{"name": {"Verkstaden"}, "address_line1": {"Gatan 1"}, "postal_code": {"411 38"}, "city": {"Göteborg"},
		"org_number": {"556677-8899"}, "vat_number": {"SE556677889901"}, "payment_reference": {"Bankgiro 123-4567"}, "payment_terms_days": {"30"}}); st != http.StatusSeeOther {
		t.Fatalf("saving the details answered %d", st)
	}
	step := func(name, state, note string) {
		t.Helper()
		if st := post("/start/step/"+name, url.Values{"state": {state}, "note": {note}}); st != http.StatusSeeOther {
			t.Fatalf("step %s answered %d", name, st)
		}
	}
	// A step about a person is refused without one.
	if st := post("/start/step/backups", url.Values{"state": {"done"}}); st != http.StatusBadRequest {
		t.Errorf("backups with nobody named answered %d", st)
	}
	step("operating_model", "done", "solo")
	for _, s := range []string{"locale", "invoice"} {
		step(s, "done", "")
	}
	step("data", "skipped", "")
	step("final_check", "skipped", "")
	step("accountant", "done", "Rut, 031-555 01 02")
	step("backups", "done", "Olle, 070-111 22 33")
	_, mid := get("/start")
	if strings.Contains(mid, "Ready for the first customer") {
		t.Error("ready with the support contact still missing")
	}
	step("support", "done", "Hosten, 08-123 45")
	_, body = get("/start")
	if !strings.Contains(body, "Ready for the first customer") {
		t.Errorf("not ready after every step: %s", body)
	}

	// The invoice to look at is an example and takes nothing.
	status, inv := get("/start/invoice")
	if status != http.StatusOK || !strings.Contains(inv, "Gatan 1") || !strings.Contains(inv, "556677-8899") || !strings.Contains(inv, "No invoice has been made") {
		t.Errorf("the invoice example answered %d: %s", status, inv)
	}
	// Help is there for the owner, in three parts.
	for _, p := range []string{"/help", "/help/reception", "/help/workshop", "/help/owner"} {
		if status, h := get(p); status != http.StatusOK || !strings.Contains(h, "Help") {
			t.Errorf("%s answered %d", p, status)
		}
	}
}
