package server

import (
	"context"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/testsupport"
	"github.com/RedaEkengren/FreeSMS/migrations"
)

// No invoice the release before issued changes by a byte when invoices get
// a source (#146). Each row is the same tuple afterwards -- the same xmin
// and place on disk, so nothing rewrote it, and the immutability trigger
// was never asked -- with the same values in every column it had, and it
// reads as the workshop's through the new column's default.
func TestIssuedInvoicesAreUntouchedByTheirSource(t *testing.T) {
	pool := testsupport.FreshPool(t)
	ctx := context.Background()
	if err := database.Migrate(ctx, pool, migrationsUpTo(t, lastBeforeM6)); err != nil {
		t.Fatalf("migrate to %04d: %v", lastBeforeM6, err)
	}
	loadBeforeM6(t, pool)

	type row struct{ tuple, values string }
	read := func(table string) map[string]row {
		rows, err := pool.Query(ctx, `SELECT id::text, xmin::text || ' ' || ctid::text, to_jsonb(t)::text FROM `+table+` t`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]row{}
		for rows.Next() {
			var id string
			var r row
			if err := rows.Scan(&id, &r.tuple, &r.values); err != nil {
				t.Fatal(err)
			}
			out[id] = r
		}
		return out
	}
	before := map[string]map[string]row{"invoices": read("invoices"), "invoice_lines": read("invoice_lines")}
	if len(before["invoices"]) == 0 {
		t.Fatal("the release before issued no invoices to compare")
	}

	if err := database.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("migrate forward: %v", err)
	}
	for table, rows := range before {
		after := read(table)
		for id, was := range rows {
			now, ok := after[id]
			if !ok {
				t.Errorf("%s %s is gone", table, id)
				continue
			}
			if now.tuple != was.tuple {
				t.Errorf("%s %s was rewritten: %s, then %s", table, id, was.tuple, now.tuple)
			}
			// Every column it had, unchanged; the new ones are compared below.
			var same bool
			if err := pool.QueryRow(ctx, `SELECT $1::jsonb <@ $2::jsonb`, was.values, now.values).Scan(&same); err != nil || !same {
				t.Errorf("%s %s reads differently: %s, then %s (%v)", table, id, was.values, now.values, err)
			}
		}
	}
	var notWorkshop int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM invoices WHERE source <> 'workshop' OR work_order_id IS NULL`).Scan(&notWorkshop); err != nil || notWorkshop != 0 {
		t.Errorf("%d invoices from before read as something other than the workshop's (%v)", notWorkshop, err)
	}
}
