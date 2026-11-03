package server

import (
	"context"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/testsupport"
	"github.com/RedaEkengren/FreeSMS/migrations"
)

// No invoice the release before issued changes by a byte when invoices get
// a source (#146), and no inspection or checklist when inspections get a
// subject (#147). Each row is the same tuple afterwards -- the same xmin
// and place on disk, so nothing rewrote it, and the immutability trigger
// was never asked -- with the same values in every column it had, and it
// reads as the workshop's through the new column's default.
func TestIssuedInvoicesAndInspectionsAreUntouchedByTheirSourceAndSubject(t *testing.T) {
	pool := testsupport.FreshPool(t)
	ctx := context.Background()
	if err := database.Migrate(ctx, pool, migrationsUpTo(t, lastBeforeM6)); err != nil {
		t.Fatalf("migrate to %04d: %v", lastBeforeM6, err)
	}
	loadBeforeM6(t, pool)
	// The fixture has no inspection; one is written as the release before
	// wrote them, into its schema: a checklist, and a job's check from it.
	if _, err := pool.Exec(ctx, `
		INSERT INTO inspection_templates (id, shop_id, name) VALUES
		  ('99999999-0000-0000-0000-000000000001', '11111111-1111-1111-1111-111111111111', 'Vårservice');
		INSERT INTO inspection_template_items (shop_id, template_id, position, label) VALUES
		  ('11111111-1111-1111-1111-111111111111', '99999999-0000-0000-0000-000000000001', 1, 'Bromsar');
		INSERT INTO inspections (id, shop_id, work_order_id, template_id, template_name, performed_by, completed_at) VALUES
		  ('99999999-0000-0000-0000-000000000002', '11111111-1111-1111-1111-111111111111',
		   '77777777-7777-7777-7777-777777777777', '99999999-0000-0000-0000-000000000001', 'Vårservice',
		   '33333333-3333-3333-3333-333333333334', now());
		INSERT INTO inspection_items (shop_id, inspection_id, position, label, status) VALUES
		  ('11111111-1111-1111-1111-111111111111', '99999999-0000-0000-0000-000000000002', 1, 'Bromsar', 'fail');`); err != nil {
		t.Fatalf("an inspection as the release before wrote one: %v", err)
	}

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
	before := map[string]map[string]row{}
	for _, table := range []string{"invoices", "invoice_lines", "inspections", "inspection_items",
		"inspection_templates", "inspection_template_items"} {
		before[table] = read(table)
		if len(before[table]) == 0 {
			t.Fatalf("the release before wrote no %s to compare", table)
		}
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
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM invoices WHERE source <> 'workshop' OR work_order_id IS NULL)
		     + (SELECT count(*) FROM inspections WHERE subject <> 'work_order' OR work_order_id IS NULL)
		     + (SELECT count(*) FROM inspection_templates WHERE subjects <> '{work_order}')`).Scan(&notWorkshop); err != nil || notWorkshop != 0 {
		t.Errorf("%d invoices, inspections or checklists from before read as something other than the workshop's (%v)", notWorkshop, err)
	}
}
