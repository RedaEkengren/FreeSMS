package database

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/testsupport"
	"github.com/RedaEkengren/FreeSMS/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These exercise the schema against a real Postgres, because the guarantees
// being checked are constraints rather than Go code -- an exclusion constraint
// that is subtly wrong cannot be caught by a unit test.
//
// Skipped unless FREESMS_TEST_DATABASE_URL points at a database that may be
// wiped. CI supplies one; locally:
//
//	docker compose up -d db
//	FREESMS_TEST_DATABASE_URL=postgres://freesms:freesms@127.0.0.1:55432/freesms_test?sslmode=disable go test ./internal/database/
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testsupport.FreshPool(t)
	if err := Migrate(context.Background(), pool, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

const shopID = "11111111-1111-1111-1111-111111111111"

// seedAs runs statements in one transaction with a transaction-local scope.
//
// Transaction-local matters. A session-level set_config survives on the pooled
// connection and is handed to whatever borrows it next, so a later request
// inherits a scope it never asked for. That is the same leak the InScope
// wrapper exists to prevent, and it is easy to reintroduce in test setup --
// this helper was written with a session-level setting first, and
// TestWithoutScopeNothingIsVisible caught it.
func seedAs(t *testing.T, pool *pgxpool.Pool, shop string, stmts []string) {
	t.Helper()
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Row level security applies to the owner as well (FORCE), so a seed
	// declares its scope exactly as the application does.
	if _, err := tx.Exec(ctx, `SELECT set_config('app.current_shop', $1, true)`, shop); err != nil {
		t.Fatalf("set seed scope: %v", err)
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s); err != nil {
			t.Fatalf("seed: %v\n  %s", err, s)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
}

func seed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedAs(t, pool, shopID, []string{
		`INSERT INTO shops (id, name) VALUES ('` + shopID + `', 'Test Verkstad')`,
		`INSERT INTO people (id, shop_id, display_name) VALUES
		 ('22222222-2222-2222-2222-222222222222','` + shopID + `','A Technician')`,
		`INSERT INTO users (id, shop_id, person_id, role, password_hash) VALUES
		 ('33333333-3333-3333-3333-333333333333','` + shopID + `','22222222-2222-2222-2222-222222222222','technician','x')`,
		`INSERT INTO customers (id, shop_id, kind, person_id) VALUES
		 ('44444444-4444-4444-4444-444444444444','` + shopID + `','private','22222222-2222-2222-2222-222222222222')`,
		`INSERT INTO vehicles (id, shop_id, make, model) VALUES
		 ('55555555-5555-5555-5555-555555555555','` + shopID + `','Volvo','V70'),
		 ('66666666-6666-6666-6666-666666666666','` + shopID + `','Saab','9-5')`,
		`INSERT INTO work_orders (id, shop_id, number, vehicle_id, customer_id) VALUES
		 ('77777777-7777-7777-7777-777777777777','` + shopID + `',1,
		  '55555555-5555-5555-5555-555555555555','44444444-4444-4444-4444-444444444444')`,
	})
}

// A personalised plate moves between cars. It must be impossible for two
// vehicles to carry it at once, and possible for the second to carry it after
// the first has given it up -- which is why this is an overlap constraint and
// not a unique index.
func TestRegistrationCannotBeOnTwoVehiclesAtOnce(t *testing.T) {
	pool := testPool(t)
	seed(t, pool)
	ctx := context.Background()

	const insert = `INSERT INTO vehicle_registrations (shop_id, vehicle_id, registration, normalised)
	                VALUES ($1, $2, $3, 'ABC12D')`

	if _, err := pool.Exec(ctx, insert, shopID, "55555555-5555-5555-5555-555555555555", "ABC 12 D"); err != nil {
		t.Fatalf("first registration: %v", err)
	}
	if _, err := pool.Exec(ctx, insert, shopID, "66666666-6666-6666-6666-666666666666", "ABC-12D"); err == nil {
		t.Fatal("the same plate was accepted on a second vehicle at the same time")
	}

	if _, err := pool.Exec(ctx, `UPDATE vehicle_registrations SET valid_to = now() WHERE normalised = 'ABC12D'`); err != nil {
		t.Fatalf("close period: %v", err)
	}
	if _, err := pool.Exec(ctx, insert, shopID, "66666666-6666-6666-6666-666666666666", "ABC-12D"); err != nil {
		t.Fatalf("plate could not move to another vehicle after being given up: %v", err)
	}
}

// A vehicle with neither VIN nor plate is legitimate: an unregistered import,
// a build, an engine on a bench. Refusing it gets worked around with junk
// records, which loses the history for real.
func TestVehicleWithoutAnyIdentifierIsAllowed(t *testing.T) {
	pool := testPool(t)
	seed(t, pool)
	_, err := pool.Exec(context.Background(),
		`INSERT INTO vehicles (shop_id, label) VALUES ($1, 'Engine on bench')`, shopID)
	if err != nil {
		t.Fatalf("vehicle without VIN or registration was refused: %v", err)
	}
}

// A warranty replacement appears on the order so the history records what was
// done, at zero, with the cost falling elsewhere. A priced warranty line is a
// contradiction.
func TestWarrantyLineMustBeFree(t *testing.T) {
	pool := testPool(t)
	seed(t, pool)
	ctx := context.Background()

	const insert = `INSERT INTO work_order_lines
	 (shop_id, work_order_id, position, kind, description, quantity, unit_price_minor, vat_rate_bp, cost_bearer)
	 VALUES ($1,'77777777-7777-7777-7777-777777777777',$2,'part',$3,1,$4,2500,$5)`

	if _, err := pool.Exec(ctx, insert, shopID, 1, "Clutch", 145000, "supplier"); err == nil {
		t.Fatal("a warranty line was accepted with a price on it")
	}
	if _, err := pool.Exec(ctx, insert, shopID, 1, "Clutch (warranty)", 0, "supplier"); err != nil {
		t.Fatalf("a zero-priced warranty line was refused: %v", err)
	}
}

// Oil is not a count.
func TestQuantityCanBeFractional(t *testing.T) {
	pool := testPool(t)
	seed(t, pool)
	_, err := pool.Exec(context.Background(),
		`INSERT INTO work_order_lines
		 (shop_id, work_order_id, position, kind, description, quantity, unit_price_minor, vat_rate_bp)
		 VALUES ($1,'77777777-7777-7777-7777-777777777777',1,'part','Engine oil 5W-30',4.5,12900,2500)`, shopID)
	if err != nil {
		t.Fatalf("4.5 litres was refused: %v", err)
	}
}

// Two open clocks for one person is ambiguity that becomes a payroll dispute.
func TestOneRunningClockPerTechnician(t *testing.T) {
	pool := testPool(t)
	seed(t, pool)
	ctx := context.Background()

	const insert = `INSERT INTO time_entries (shop_id, work_order_id, user_id, started_at)
	                VALUES ($1,'77777777-7777-7777-7777-777777777777','33333333-3333-3333-3333-333333333333', now())`

	if _, err := pool.Exec(ctx, insert, shopID); err != nil {
		t.Fatalf("first clock-in: %v", err)
	}
	if _, err := pool.Exec(ctx, insert, shopID); err == nil {
		t.Fatal("a technician was allowed two running clocks at once")
	}
}

// An instrument cluster can legitimately be replaced, and a rolled-back
// odometer is something the shop wants on record rather than something the
// software refuses to hear about.
func TestDecreasingOdometerReadingIsRecorded(t *testing.T) {
	pool := testPool(t)
	seed(t, pool)
	ctx := context.Background()

	const insert = `INSERT INTO odometer_readings (shop_id, vehicle_id, km, source)
	                VALUES ($1,'55555555-5555-5555-5555-555555555555',$2,'drop_off')`
	for _, km := range []int{184000, 12000} {
		if _, err := pool.Exec(ctx, insert, shopID, km); err != nil {
			t.Fatalf("reading %d km was refused: %v", km, err)
		}
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM odometer_readings`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Errorf("stored %d readings, want 2", count)
	}
}

// Every table that holds shop data must carry shop_id. A table without it
// cannot be filtered per tenant, and that is how one shop ends up seeing
// another's work.
func TestEveryTenantTableCarriesShopID(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	rows, err := pool.Query(ctx, `
		SELECT t.tablename
		FROM pg_tables t
		WHERE t.schemaname = 'public'
		  AND t.tablename NOT IN ('schema_migrations', 'shops')
		  AND NOT EXISTS (
		      SELECT 1 FROM information_schema.columns c
		      WHERE c.table_schema = 'public'
		        AND c.table_name = t.tablename
		        AND c.column_name = 'shop_id')`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	var missing []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		missing = append(missing, name)
	}
	if len(missing) > 0 {
		t.Errorf("tables without shop_id: %s", strings.Join(missing, ", "))
	}
}

// ready_at is maintained by a trigger rather than by whoever changes the
// state, because there are already three code paths that move an order and
// each would have to remember.
func TestReadyAtIsSetAndClearedByTheTrigger(t *testing.T) {
	pool := testPool(t)
	seed(t, pool)
	ctx := context.Background()

	readyAt := func() *time.Time {
		var at *time.Time
		if err := pool.QueryRow(ctx,
			`SELECT ready_at FROM work_orders WHERE id = '77777777-7777-7777-7777-777777777777'`).
			Scan(&at); err != nil {
			t.Fatalf("read ready_at: %v", err)
		}
		return at
	}

	if at := readyAt(); at != nil {
		t.Fatalf("a new order already has ready_at = %v", at)
	}

	set := func(state string) {
		t.Helper()
		seedAs(t, pool, shopID, []string{
			`UPDATE work_orders SET state = '` + state +
				`' WHERE id = '77777777-7777-7777-7777-777777777777'`,
		})
	}

	set("ready")
	first := readyAt()
	if first == nil {
		t.Fatal("moving to ready did not set ready_at")
	}

	// A car sent back to the workshop is no longer waiting to be collected.
	// Keeping the timestamp would report it as having stood there for a week
	// when it has been on a lift all along.
	set("in_progress")
	if at := readyAt(); at != nil {
		t.Errorf("leaving ready left ready_at = %v", at)
	}
}
