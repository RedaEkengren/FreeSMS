package ops_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/ops"
	"github.com/RedaEkengren/FreeSMS/internal/storage"
	"github.com/RedaEkengren/FreeSMS/internal/testsupport"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/RedaEkengren/FreeSMS/migrations"
)

const (
	shopID    = "11111111-1111-1111-1111-111111111111"
	userID    = "33333333-3333-3333-3333-333333333333"
	vehicleID = "55555555-5555-5555-5555-555555555555"
)

// workshopOnly is what an ordinary shop runs: the workshop and nothing else,
// as every shop did before there were modules.
var workshopOnly = []access.Module{access.Workshop}

func advisor() access.Scope {
	return access.Scope{ShopID: shopID, UserID: userID, Role: access.Of(access.RoleServiceAdvisor), Modules: workshopOnly}
}

// A workshop with two invoices, a credit, a payment and its reversal, and a
// photograph that is on disk.
func workshopWithBooks(t *testing.T) (*pgxpool.Pool, *storage.Store, string) {
	t.Helper()
	ctx := context.Background()
	pool := testsupport.ClonedPool(t, func(ctx context.Context, p *pgxpool.Pool) error {
		return database.Migrate(ctx, p, migrations.FS)
	})
	err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		for _, s := range []string{
			`INSERT INTO shops (id, name) VALUES ('` + shopID + `', 'Verkstaden')`,
			`INSERT INTO people (id, shop_id, display_name) VALUES ('22222222-2222-2222-2222-222222222222','` + shopID + `','An Advisor')`,
			`INSERT INTO users (id, shop_id, person_id, role, password_hash) VALUES ('` + userID + `','` + shopID + `','22222222-2222-2222-2222-222222222222','service_advisor','x')`,
			`INSERT INTO vehicles (id, shop_id, make, model) VALUES ('` + vehicleID + `','` + shopID + `','Volvo','V70')`,
			`INSERT INTO people (id, shop_id, display_name) VALUES ('22222222-2222-2222-2222-222222222223','` + shopID + `','A Customer')`,
			`INSERT INTO customers (id, shop_id, kind, person_id) VALUES ('44444444-4444-4444-4444-444444444444','` + shopID + `','private','22222222-2222-2222-2222-222222222223')`,
			`INSERT INTO vehicle_ownership (shop_id, vehicle_id, customer_id) VALUES ('` + shopID + `','` + vehicleID + `','44444444-4444-4444-4444-444444444444')`,
			`INSERT INTO vehicle_registrations (shop_id, vehicle_id, registration, normalised) VALUES ('` + shopID + `','` + vehicleID + `','ABC 12D','ABC12D')`,
		} {
			if _, err := tx.Exec(ctx, s); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var first workshop.Invoice
	for i := 0; i < 2; i++ {
		job, err := workshop.TakeIn(ctx, pool, advisor(), "ABC 12D", "Grinding", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := workshop.AddLine(ctx, pool, advisor(), job, workshop.NewLine{Kind: "labour", Description: "Brakes", QuantityMilli: 2000, UnitPriceMinor: 80000, VATRateBasis: 2500}); err != nil {
			t.Fatal(err)
		}
		for _, s := range []workshop.State{workshop.StateEstimated, workshop.StateAwaitingApproval} {
			if err := workshop.SetState(ctx, pool, advisor(), job, s); err != nil {
				t.Fatal(err)
			}
		}
		q, err := workshop.PresentQuote(ctx, pool, advisor(), job)
		if err != nil {
			t.Fatal(err)
		}
		if err := workshop.RecordQuoteAnswer(ctx, pool, advisor(), job, q.ID, workshop.QuoteAnswer{Decision: "approved", Via: "counter", AgreedGrossMinor: q.GrossMinor}); err != nil {
			t.Fatal(err)
		}
		for _, s := range []workshop.State{workshop.StateApproved, workshop.StateInProgress, workshop.StateReady} {
			if err := workshop.SetState(ctx, pool, advisor(), job, s); err != nil {
				t.Fatal(err)
			}
		}
		inv, err := workshop.Issue(ctx, pool, advisor(), job)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = inv
		}
	}
	if _, err := workshop.IssueCredit(ctx, pool, advisor(), first.ID, workshop.CreditRequest{Lines: []workshop.CreditLine{{Position: 1}}, Reason: "Wrong car", Key: "k1"}); err != nil {
		t.Fatal(err)
	}
	// A payment on the second invoice, and then taken back.
	var second string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM invoices WHERE number = 2`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	paid, err := workshop.RecordPayment(ctx, pool, advisor(), second, workshop.NewPayment{Method: "bank", PaidOn: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.ReversePayment(ctx, pool, advisor(), paid.ID, "Registrerad av misstag"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "attachments")
	store, err := storage.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return pool, store, dir
}

func tinyJPEG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for x := 0; x < 8; x++ {
		for y := 0; y < 8; y++ {
			img.Set(x, y, color.RGBA{uint8(x * 30), uint8(y * 30), 90, 255})
		}
	}
	var b bytes.Buffer
	_ = jpeg.Encode(&b, img, nil)
	return b.Bytes()
}

func find(t *testing.T, rep ops.Report, name string) ops.Check {
	t.Helper()
	for _, s := range rep.Shops {
		for _, c := range s.Checks {
			if c.Name == name {
				return c
			}
		}
	}
	t.Fatalf("no check %q in %s", name, rep.Text())
	return ops.Check{}
}

func TestAHealthyWorkshopPassesEveryCheck(t *testing.T) {
	pool, store, _ := workshopWithBooks(t)
	rep, err := ops.Verify(context.Background(), pool, store)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed() {
		t.Fatalf("a healthy workshop failed:\n%s", rep.Text())
	}
	if c := find(t, rep, "invoice numbers"); !strings.Contains(c.Detail, "A 1-3") {
		t.Errorf("invoice numbers = %+v, want an unbroken A 1-3 (two invoices and a credit)", c)
	}
	if len(rep.Shops) != 1 || rep.Shops[0].NewestWrite.IsZero() || time.Since(rep.Shops[0].NewestWrite) > time.Hour {
		t.Errorf("the newest write is %v", rep.Shops[0].NewestWrite)
	}
	if rep.Migrations < 50 || !strings.Contains(rep.Text(), "newest invoice") {
		t.Errorf("report:\n%s", rep.Text())
	}
}

// Each thing a restore can get wrong is noticed.
func TestWhatABrokenRestoreLooksLikeIsNoticed(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct{ check, breakIt string }{
		"a gap in the invoice numbers":            {"invoice numbers", `UPDATE invoices SET number = 9 WHERE number = 3`},
		"a total that is not its lines":           {"invoice totals", `UPDATE invoices SET net_minor = net_minor + 1, gross_minor = gross_minor + 1 WHERE number = 2`},
		"a credit for more than the invoice":      {"credit notes", `UPDATE invoices SET net_minor = net_minor * 2, vat_minor = vat_minor * 2, gross_minor = gross_minor * 2 WHERE credit_of_id IS NOT NULL`},
		"a payment taken back on another invoice": {"payment reversals", `UPDATE invoice_payments SET invoice_id = (SELECT id FROM invoices WHERE number = 1) WHERE reverses_id IS NOT NULL`},
		"a job invoiced and no invoice": {"jobs and invoices", `UPDATE work_orders SET state = 'invoiced' WHERE id = (SELECT w.id FROM work_orders w WHERE NOT EXISTS (SELECT 1 FROM invoices i WHERE i.work_order_id = w.id) LIMIT 1);
			INSERT INTO work_orders (shop_id, number, vehicle_id, customer_id, state) SELECT shop_id, 99, vehicle_id, customer_id, 'invoiced' FROM work_orders LIMIT 1`},
		"an event about something not there": {"audit events", `INSERT INTO audit_events (shop_id, subject_kind, subject_id, event)
			SELECT id, 'invoice', '99999999-0000-0000-0000-000000000000', 'payment.recorded' FROM shops LIMIT 1`},
		"a photograph that did not come back": {"photographs", `INSERT INTO attachments (shop_id, work_order_id, storage_key, content_type, byte_size)
			SELECT shop_id, id, 'abcdef0123456789abcdef0123456789', 'image/jpeg', 100 FROM work_orders LIMIT 1`},
	} {
		t.Run(name, func(t *testing.T) {
			pool, store, _ := workshopWithBooks(t)
			// Done as the database's owner, around the triggers that stop the
			// application doing it: this is what a damaged restore looks like,
			// not something the application can produce.
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			for _, table := range []string{"invoices", "invoice_payments", "work_orders", "attachments", "invoice_series"} {
				if _, err := tx.Exec(ctx, `ALTER TABLE `+table+` DISABLE TRIGGER USER`); err != nil {
					t.Fatalf("disable triggers on %s: %v", table, err)
				}
			}
			if _, err := tx.Exec(ctx, tc.breakIt); err != nil {
				t.Fatalf("damage the data: %v", err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			rep, err := ops.Verify(ctx, pool, store)
			if err != nil {
				t.Fatal(err)
			}
			if c := find(t, rep, tc.check); c.Level != ops.Fail {
				t.Errorf("%s = %+v, want a failure\n%s", tc.check, c, rep.Text())
			}
			if !rep.Failed() {
				t.Error("a failing check did not fail the report")
			}
		})
	}
}

func TestAPhotographThatIsThereAndTheOneThatIsTruncated(t *testing.T) {
	ctx := context.Background()
	pool, store, dir := workshopWithBooks(t)
	saved, err := store.SavePhoto(bytes.NewReader(tinyJPEG()))
	if err != nil {
		t.Fatal(err)
	}
	err = database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO attachments (shop_id, work_order_id, storage_key, content_type, byte_size)
			SELECT shop_id, id, $1, 'image/jpeg', $2 FROM work_orders LIMIT 1`, saved.Key, saved.ByteSize)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	rep, _ := ops.Verify(ctx, pool, store)
	if c := find(t, rep, "photographs"); c.Level != ops.Pass || !strings.Contains(c.Detail, "1 on disk") {
		t.Errorf("a stored photograph = %+v", c)
	}
	// Cut short: the bytes that came back are not the bytes that went in.
	matches, _ := filepath.Glob(filepath.Join(dir, "*", saved.Key+"*"))
	if len(matches) == 0 {
		t.Fatalf("no stored file for %s under %s", saved.Key, dir)
	}
	if err := os.Truncate(matches[0], 5); err != nil {
		t.Fatal(err)
	}
	rep, _ = ops.Verify(ctx, pool, store)
	if c := find(t, rep, "photographs"); c.Level != ops.Fail {
		t.Errorf("a truncated photograph = %+v", c)
	}
}

// Every kind of thing an audit event may be about is one the audit check
// can find (#151). The list is read from the database's own CHECK, so a
// module that widens it -- as Dealer and Rental did -- fails here until
// verify.go has a branch for it and this test a row: otherwise a healthy
// restore of a shop running that module is reported as broken.
func TestEveryAuditSubjectKindIsOneVerifyCanFind(t *testing.T) {
	pool, store, _ := workshopWithBooks(t)
	ctx := context.Background()
	var def string
	if err := pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'audit_events_subject_kind_check'`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, m := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(def, -1) {
		kinds = append(kinds, m[1])
	}
	if len(kinds) < 10 {
		t.Fatalf("read %v from %s", kinds, def)
	}
	// One row of each, by the fewest columns its table will take: the
	// check asks only whether the subject is there. Foreign keys and
	// triggers are off on this connection, so nothing else need exist.
	const id = "99999999-9999-9999-9999-9999999999"
	subjects := map[string]string{
		"shop":           `SELECT '` + shopID + `'::uuid`,
		"user":           `SELECT '` + userID + `'::uuid`,
		"vehicle":        `SELECT '` + vehicleID + `'::uuid`,
		"customer":       `SELECT id FROM customers LIMIT 1`,
		"party":          `SELECT id FROM parties LIMIT 1`,
		"invoice":        `SELECT id FROM invoices LIMIT 1`,
		"payment":        `SELECT id FROM invoice_payments LIMIT 1`,
		"work_order":     `SELECT id FROM work_orders LIMIT 1`,
		"resource_block": `INSERT INTO resource_blocks (id, shop_id, resource_id, kind, period, status) VALUES ('` + id + `01', '` + shopID + `', gen_random_uuid(), 'workshop', tstzrange(now(), NULL), 'confirmed') RETURNING id`,
		"document": `INSERT INTO document_records (id, shop_id, created_by, kind, subject_kind, subject_id, template_version_id, title, content_sha256, body, data)
		             VALUES ('` + id + `02', '` + shopID + `', '` + userID + `', 'sales_contract', 'sale', gen_random_uuid(), gen_random_uuid(), 'x', sha256('x'), 'x', '{}') RETURNING id`,
		"identity_record": `INSERT INTO identity_records (id, shop_id, party_id, kind, purpose, legal_basis, recorded_by, expires_at, verification_ref)
		             VALUES ('` + id + `03', '` + shopID + `', gen_random_uuid(), 'licence_check', 'rental', 'contract', '` + userID + `', now() + interval '1 day', 'x') RETURNING id`,
		"stock_unit": `INSERT INTO stock_units (id, shop_id, vehicle_id, resource_id, reason) VALUES ('` + id + `04', '` + shopID + `', '` + vehicleID + `', gen_random_uuid(), 'purchase') RETURNING id`,
		"purchase": `INSERT INTO vehicle_purchases (id, shop_id, stock_unit_id, seller_party_id, purchased_on, tax_purchase_price_minor)
		             VALUES ('` + id + `05', '` + shopID + `', gen_random_uuid(), gen_random_uuid(), current_date, 0) RETURNING id`,
		"rental_booking": `INSERT INTO rental_bookings (id, shop_id, block_id, number, unit_id, renter_party_id, starts_at, ends_at, profile_version_id)
		             VALUES ('` + id + `06', '` + shopID + `', gen_random_uuid(), 1, gen_random_uuid(), gen_random_uuid(), now(), now() + interval '1 day', gen_random_uuid()) RETURNING id`,
		"test_drive": `INSERT INTO test_drives (id, shop_id, stock_unit_id, driver_party_id, block_id, starts_at, ends_at)
		             VALUES ('` + id + `07', '` + shopID + `', gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), now(), now() + interval '1 hour') RETURNING id`,
		"vehicle_sale": `INSERT INTO vehicle_sales (id, shop_id, stock_unit_id, salesperson_id)
		             VALUES ('` + id + `08', '` + shopID + `', gen_random_uuid(), '` + userID + `') RETURNING id`,
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(ctx, `RESET session_replication_role`) }()
	for _, kind := range kinds {
		q, ok := subjects[kind]
		if !ok {
			t.Errorf("audit events may be about a %q, and this test does not know one: add it here and to verify.go's audit check", kind)
			continue
		}
		var subject string
		if err := conn.QueryRow(ctx, q).Scan(&subject); err != nil {
			t.Fatalf("a %s: %v", kind, err)
		}
		if _, err := conn.Exec(ctx, `INSERT INTO audit_events (shop_id, subject_kind, subject_id, event) VALUES ($1, $2, $3, 'test.seen')`,
			shopID, kind, subject); err != nil {
			t.Fatalf("an event about a %s: %v", kind, err)
		}
	}
	rep, err := ops.Verify(ctx, pool, store)
	if err != nil {
		t.Fatal(err)
	}
	if c := find(t, rep, "audit events"); c.Level != ops.Pass {
		t.Errorf("one event about each kind of subject there is: %+v", c)
	}
}
