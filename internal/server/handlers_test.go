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
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/auth"
	"github.com/RedaEkengren/FreeSMS/internal/config"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/testsupport"
	"github.com/RedaEkengren/FreeSMS/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	shopA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	shopB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	jobB  = "bbbbbbbb-0000-0000-0000-00000000000f"

	// One row of each kind belonging to shop B, so the route sweep has a real
	// identifier to ask for on every pattern.
	personB     = "bbbbbbbb-0000-0000-0000-000000000001"
	customerB   = "bbbbbbbb-0000-0000-0000-000000000002"
	vehicleB    = "bbbbbbbb-0000-0000-0000-000000000003"
	inspectionB = "bbbbbbbb-0000-0000-0000-000000000004"
	invoiceB    = "bbbbbbbb-0000-0000-0000-000000000005"
	partB       = "bbbbbbbb-0000-0000-0000-000000000006"
	itemB       = "bbbbbbbb-0000-0000-0000-000000000007"
	photoB      = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	// A string that appears nowhere in shop A. If it reaches a response body,
	// something leaked, whatever the status code said.
	secretB = "Birgitta Hemlig"

	techEmail    = "tech@shop-a.test"
	advisorEmail = "advisor@shop-a.test"
	techPass     = "a reasonable workshop password"
)

// Customer details that must never reach a technician's screen.
const (
	customerName    = "Margareta Wikstrom"
	customerAddress = "Kungsgatan 44"
	customerPhone   = "+46701234567"

	// html/template escapes the leading plus to &#43;, so a search for
	// customerPhone in a rendered page finds nothing whether or not the number
	// is there. The digits are what a leak test has to look for.
	customerPhoneDigits = "46701234567"
)

func testServer(t *testing.T) (*httptest.Server, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	pool := testsupport.FreshPool(t)
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
		`INSERT INTO people (id, shop_id, display_name, email) VALUES
		 ('aaaaaaaa-0000-0000-0000-00000000000a','` + shopA + `','An Advisor','` + advisorEmail + `')`,
		`INSERT INTO users (id, shop_id, person_id, role, password_hash) VALUES
		 ('aaaaaaaa-0000-0000-0000-00000000000b','` + shopA + `',
		  'aaaaaaaa-0000-0000-0000-00000000000a','service_advisor','` + hash + `')`,
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

	// Shop B: one of everything shop A's users must never reach. The route
	// sweep substitutes these into every pattern that takes an identifier, so
	// the fixture has to carry a real row for each kind -- a missing row would
	// make the sweep pass because the object does not exist, which is not the
	// thing being tested.
	seedShop(t, pool, shopB, []string{
		`INSERT INTO shops (id, name) VALUES ('` + shopB + `', 'Shop B')`,
		`INSERT INTO people (id, shop_id, display_name) VALUES
		 ('` + personB + `','` + shopB + `','` + secretB + `')`,
		`INSERT INTO users (id, shop_id, person_id, role, password_hash) VALUES
		 ('bbbbbbbb-0000-0000-0000-0000000000bb','` + shopB + `','` + personB + `','technician','` + hash + `')`,
		`INSERT INTO customers (id, shop_id, kind, person_id) VALUES
		 ('` + customerB + `','` + shopB + `','private','` + personB + `')`,
		`INSERT INTO vehicles (id, shop_id, make, model) VALUES
		 ('` + vehicleB + `','` + shopB + `','Toyota','Hilux')`,
		`INSERT INTO work_orders (id, shop_id, number, vehicle_id, customer_id, complaint) VALUES
		 ('` + jobB + `','` + shopB + `',1,
		  '` + vehicleB + `','` + customerB + `','Secret complaint')`,
		`INSERT INTO inspections (id, shop_id, work_order_id, template_name, performed_by) VALUES
		 ('` + inspectionB + `','` + shopB + `','` + jobB + `','Secret check',
		  'bbbbbbbb-0000-0000-0000-0000000000bb')`,
		`INSERT INTO inspection_items (id, shop_id, inspection_id, position, label) VALUES
		 ('` + itemB + `','` + shopB + `','` + inspectionB + `',1,'Secret item')`,
		`INSERT INTO attachments (shop_id, storage_key, content_type, byte_size, inspection_item_id) VALUES
		 ('` + shopB + `','` + photoB + `','image/jpeg',1,'` + itemB + `')`,
		`INSERT INTO parts (id, shop_id, number, name) VALUES
		 ('` + partB + `','` + shopB + `','SECRET-1','Secret part')`,
		`INSERT INTO invoices
		   (id, shop_id, series, number, work_order_id, customer_name,
		    net_minor, vat_minor, gross_minor)
		 VALUES ('` + invoiceB + `','` + shopB + `','B',1,'` + jobB + `','` + secretB + `',
		         100000, 25000, 125000)`,
	})

	cfg := &config.Config{
		BaseURL:       "http://localhost",
		DefaultLocale: "en",
		Release:       "test",
		// Photographs go to a directory that disappears with the test.
		AttachmentsDir: t.TempDir(),
	}
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
func signIn(t *testing.T, ts *httptest.Server, email string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	form := url.Values{"email": {email}, "password": {techPass}}
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
	client := signIn(t, ts, techEmail)

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
	client := signIn(t, ts, techEmail)

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
		for _, secret := range []string{customerName, customerAddress, customerPhoneDigits} {
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
	client := signIn(t, ts, techEmail)

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
	client := signIn(t, ts, techEmail)

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
