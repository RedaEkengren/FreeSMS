package database

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/RedaEkengren/RedaSMS/internal/access"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const otherShopID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

// seedSecondShop adds a second shop with one work order of its own, so that
// isolation can be tested against real rows rather than an empty table -- an
// empty table would pass every one of these tests for the wrong reason.
func seedSecondShop(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedAs(t, pool, otherShopID, []string{
		`INSERT INTO shops (id, name) VALUES ('` + otherShopID + `', 'Other Verkstad')`,
		`INSERT INTO people (id, shop_id, display_name) VALUES
		 ('bbbbbbbb-0000-0000-0000-000000000001','` + otherShopID + `','Other Person')`,
		`INSERT INTO customers (id, shop_id, kind, person_id) VALUES
		 ('bbbbbbbb-0000-0000-0000-000000000002','` + otherShopID + `','private','bbbbbbbb-0000-0000-0000-000000000001')`,
		`INSERT INTO vehicles (id, shop_id, make, model) VALUES
		 ('bbbbbbbb-0000-0000-0000-000000000003','` + otherShopID + `','Toyota','Hilux')`,
		`INSERT INTO work_orders (id, shop_id, number, vehicle_id, customer_id) VALUES
		 ('bbbbbbbb-0000-0000-0000-000000000004','` + otherShopID + `',1,
		  'bbbbbbbb-0000-0000-0000-000000000003','bbbbbbbb-0000-0000-0000-000000000002')`,
	})
}

// The test that matters most: it goes around the Go layer entirely.
//
// A query with no WHERE clause at all, run as the application role with shop A
// in scope, must still not see shop B's rows. If this fails, nothing built on
// top of it is protecting anything, however careful the code above looks.
func TestRawQueryCannotReachAnotherShop(t *testing.T) {
	pool := testPool(t)
	seed(t, pool)
	seedSecondShop(t, pool)
	ctx := context.Background()

	scope := access.Scope{ShopID: shopID, UserID: "33333333-3333-3333-3333-333333333333", Role: access.RoleOwner}

	var total, others int
	err := InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM work_orders`).Scan(&total); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM work_orders WHERE shop_id = $1`, otherShopID).Scan(&others)
	})
	if err != nil {
		t.Fatalf("InScope: %v", err)
	}
	if total != 1 {
		t.Errorf("an unfiltered count saw %d work orders, want only this shop's 1", total)
	}
	if others != 0 {
		t.Errorf("asking for the other shop's rows by id returned %d, want 0", others)
	}
}

// A request whose scope was never established must see nothing, rather than
// everything. This is the property that makes a forgotten predicate safe.
func TestWithoutScopeNothingIsVisible(t *testing.T) {
	pool := testPool(t)
	seed(t, pool)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SET LOCAL ROLE redasms_app`); err != nil {
		t.Fatalf("assume role: %v", err)
	}
	// Deliberately no app.current_shop.
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM vehicles`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("a query with no scope saw %d vehicles, want 0 -- row level security is not failing closed", count)
	}
}

// Writing into another shop must be refused as firmly as reading from it.
func TestCannotWriteIntoAnotherShop(t *testing.T) {
	pool := testPool(t)
	seed(t, pool)
	seedSecondShop(t, pool)
	ctx := context.Background()

	scope := access.Scope{ShopID: shopID, UserID: "33333333-3333-3333-3333-333333333333", Role: access.RoleOwner}
	err := InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO vehicles (shop_id, make, model) VALUES ($1, 'Planted', 'Row')`, otherShopID)
		return err
	})
	if err == nil {
		t.Fatal("a row was inserted into another shop")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "policy") {
		t.Errorf("insert failed with %v; expected a row level security policy violation", err)
	}
}

// The scope must be rejected before it reaches the database, so that the
// mistake reads as an error rather than as a shop with no data.
func TestInScopeRefusesAnEmptyScope(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	err := InScope(ctx, pool, access.Scope{Role: access.RoleOwner}, func(context.Context, pgx.Tx) error {
		t.Error("the callback ran despite there being no shop in scope")
		return nil
	})
	if !errors.Is(err, access.ErrNoScope) {
		t.Fatalf("InScope() = %v, want ErrNoScope", err)
	}
}

func TestInScopeRefusesAnUnknownRole(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	err := InScope(ctx, pool, access.Scope{ShopID: shopID, Role: "superuser"}, func(context.Context, pgx.Tx) error {
		t.Error("the callback ran with a role that does not exist")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "role") {
		t.Fatalf("InScope() = %v, want a complaint about the role", err)
	}
}

// A table added later without a policy is invisible until someone notices, and
// what they notice is a leak. This fails the build instead.
func TestEveryTenantTableHasForcedRowLevelSecurity(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	rows, err := pool.Query(ctx, `
		SELECT c.relname,
		       c.relrowsecurity,
		       c.relforcerowsecurity,
		       (SELECT count(*) FROM pg_policy p WHERE p.polrelid = c.oid) AS policies
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public'
		  AND c.relkind = 'r'
		  AND c.relname <> 'schema_migrations'
		ORDER BY c.relname`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		var enabled, forced bool
		var policies int
		if err := rows.Scan(&name, &enabled, &forced, &policies); err != nil {
			t.Fatalf("scan: %v", err)
		}
		switch {
		case !enabled:
			t.Errorf("%s does not have row level security enabled", name)
		case !forced:
			t.Errorf("%s has row level security but not FORCE, so the owner bypasses it", name)
		case policies == 0:
			t.Errorf("%s has row level security enabled and no policy, so it denies everything", name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
}

// The technician role is the one that must not be handed customer personal
// data. This pins the decision so a later convenience cannot quietly widen it.
func TestTechnicianDoesNotSeeCustomerPersonalData(t *testing.T) {
	if access.RoleTechnician.SeesCustomerPersonalData() {
		t.Error("technicians were granted customer personal data")
	}
	if access.RoleParts.SeesCustomerPersonalData() {
		t.Error("the parts role was granted customer personal data")
	}
	for _, r := range []access.Role{access.RoleOwner, access.RoleServiceAdvisor, access.RoleAdmin} {
		if !r.SeesCustomerPersonalData() {
			t.Errorf("%s cannot see customer personal data but needs to", r)
		}
	}
}
