package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/RedaEkengren/RedaSMS/internal/auth"
	"github.com/RedaEkengren/RedaSMS/internal/config"
	"github.com/RedaEkengren/RedaSMS/internal/database"
	"github.com/RedaEkengren/RedaSMS/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	shopA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	shopB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	jobB  = "bbbbbbbb-0000-0000-0000-00000000000f"

	techEmail = "tech@shop-a.test"
	techPass  = "a reasonable workshop password"
)

// Customer details that must never reach a technician's screen.
const (
	customerName    = "Margareta Wikstrom"
	customerAddress = "Kungsgatan 44"
	customerPhone   = "+46701234567"
)

func testServer(t *testing.T) (*httptest.Server, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("REDASMS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("REDASMS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// See internal/database: test packages run in parallel and each resets the
	// one database they share.
	lockConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire lock connection: %v", err)
	}
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock($1)`, int64(991_147_003)); err != nil {
		t.Fatalf("take test lock: %v", err)
	}
	t.Cleanup(func() {
		_, _ = lockConn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, int64(991_147_003))
		lockConn.Release()
	})

	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := database.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	hash, err := auth.HashPassword(techPass)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	// Shop A: a technician, a customer with real personal details, a vehicle
	// and a job.
	seedShop(t, pool, shopA, []string{
		`INSERT INTO shops (id, name) VALUES ('` + shopA + `', 'Shop A')`,
		`INSERT INTO people (id, shop_id, display_name, email) VALUES
		 ('aaaaaaaa-0000-0000-0000-000000000001','` + shopA + `','A Technician','` + techEmail + `')`,
		`INSERT INTO users (id, shop_id, person_id, role, password_hash) VALUES
		 ('aaaaaaaa-0000-0000-0000-000000000002','` + shopA + `',
		  'aaaaaaaa-0000-0000-0000-000000000001','technician','` + hash + `')`,
		`INSERT INTO people (id, shop_id, display_name, phone) VALUES
		 ('aaaaaaaa-0000-0000-0000-000000000003','` + shopA + `','` + customerName + `','` + customerPhone + `')`,
		`INSERT INTO customers (id, shop_id, kind, person_id, address_line1) VALUES
		 ('aaaaaaaa-0000-0000-0000-000000000004','` + shopA + `','private',
		  'aaaaaaaa-0000-0000-0000-000000000003','` + customerAddress + `')`,
		`INSERT INTO vehicles (id, shop_id, make, model) VALUES
		 ('aaaaaaaa-0000-0000-0000-000000000005','` + shopA + `','Volvo','V70')`,
		`INSERT INTO work_orders (id, shop_id, number, vehicle_id, customer_id, complaint) VALUES
		 ('aaaaaaaa-0000-0000-0000-00000000000f','` + shopA + `',1,
		  'aaaaaaaa-0000-0000-0000-000000000005','aaaaaaaa-0000-0000-0000-000000000004',
		  'Grinding from the front')`,
	})

	// Shop B: a job that shop A's technician must never reach.
	seedShop(t, pool, shopB, []string{
		`INSERT INTO shops (id, name) VALUES ('` + shopB + `', 'Shop B')`,
		`INSERT INTO people (id, shop_id, display_name) VALUES
		 ('bbbbbbbb-0000-0000-0000-000000000001','` + shopB + `','B Person')`,
		`INSERT INTO customers (id, shop_id, kind, person_id) VALUES
		 ('bbbbbbbb-0000-0000-0000-000000000002','` + shopB + `','private','bbbbbbbb-0000-0000-0000-000000000001')`,
		`INSERT INTO vehicles (id, shop_id, make, model) VALUES
		 ('bbbbbbbb-0000-0000-0000-000000000003','` + shopB + `','Toyota','Hilux')`,
		`INSERT INTO work_orders (id, shop_id, number, vehicle_id, customer_id, complaint) VALUES
		 ('` + jobB + `','` + shopB + `',1,
		  'bbbbbbbb-0000-0000-0000-000000000003','bbbbbbbb-0000-0000-0000-000000000002','Secret complaint')`,
	})

	cfg := &config.Config{BaseURL: "http://localhost", DefaultLocale: "en", Release: "test"}
	srv, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), cfg, shopA)
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

func seedShop(t *testing.T, pool *pgxpool.Pool, shop string, stmts []string) {
	t.Helper()
	ctx := context.Background()
	err := database.InShop(ctx, pool, shop, func(ctx context.Context, tx pgx.Tx) error {
		for _, s := range stmts {
			if _, err := tx.Exec(ctx, s); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed %s: %v", shop, err)
	}
}

// signIn returns a client carrying a session for shop A's technician.
func signIn(t *testing.T, ts *httptest.Server) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	form := url.Values{"email": {techEmail}, "password": {techPass}}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", ts.URL)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("sign in: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("sign in returned %d, want 303\n%s", resp.StatusCode, body)
	}
	return client
}

// The acceptance criterion from the permissions issue, as a test.
func TestTechnicianGets404ForAnotherShopsJob(t *testing.T) {
	ts, _ := testServer(t)
	client := signIn(t, ts)

	resp, err := client.Get(ts.URL + "/jobs/" + jobB)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	if strings.Contains(string(body), "Secret complaint") {
		t.Error("another shop's work order leaked into the response body")
	}
}

// The other half of the criterion: a technician's screens carry the vehicle
// and the work, and no customer personal data at all.
func TestTechnicianScreensCarryNoCustomerPersonalData(t *testing.T) {
	ts, _ := testServer(t)
	client := signIn(t, ts)

	for _, path := range []string{"/", "/jobs/aaaaaaaa-0000-0000-0000-00000000000f"} {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s returned %d, want 200", path, resp.StatusCode)
		}
		for _, secret := range []string{customerName, customerAddress, customerPhone} {
			if strings.Contains(string(body), secret) {
				t.Errorf("%s contains customer personal data: %q", path, secret)
			}
		}
	}
}

func TestUnauthenticatedRequestsGoToTheSignInPage(t *testing.T) {
	ts, _ := testServer(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want /login", loc)
	}
}

// A form on another site must not be able to act with a workshop's session.
func TestCrossOriginWriteIsRefused(t *testing.T) {
	ts, _ := testServer(t)
	client := signIn(t, ts)

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/jobs/aaaaaaaa-0000-0000-0000-00000000000f/clock-in", nil)
	req.Header.Set("Origin", "https://evil.example")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestSignOutEndsTheSession(t *testing.T) {
	ts, _ := testServer(t)
	client := signIn(t, ts)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/logout", nil)
	req.Header.Set("Origin", ts.URL)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	resp.Body.Close()

	resp, err = client.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("after signing out, / returned %d, want a redirect to the sign-in page", resp.StatusCode)
	}
}
