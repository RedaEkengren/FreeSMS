package workshop_test

import (
	"context"
	"io/fs"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/testsupport"
	"github.com/RedaEkengren/FreeSMS/migrations"
)

// migrationsBefore is the migrations of the release before the one whose
// file holds marker: found by what it does, never by its number, which a
// merge may change.
func migrationsBefore(t *testing.T, marker string) fs.FS {
	t.Helper()
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	at := -1
	for _, name := range names {
		body, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), marker) {
			at, _ = strconv.Atoi(name[:4])
			break
		}
	}
	if at < 0 {
		t.Fatalf("no migration holds %q", marker)
	}
	out := fstest.MapFS{}
	for _, name := range names {
		if n, err := strconv.Atoi(name[:4]); err == nil && n < at {
			body, _ := fs.ReadFile(migrations.FS, name)
			out[name] = &fstest.MapFile{Data: body}
		}
	}
	return out
}

// A payout for something else that named a purchase, written by the
// release before payouts were held to their purpose -- through Core's
// CreatePayout, which never refused it -- does not stop the deploy. Its
// subject is taken off, and what it named is kept in its note.
func TestAPayoutNamingWhatItIsNotForDoesNotStopTheDeploy(t *testing.T) {
	pool := testsupport.FreshPool(t)
	ctx := context.Background()
	if err := database.Migrate(ctx, pool, migrationsBefore(t, "payouts_subject_is_its_purpose")); err != nil {
		t.Fatalf("migrate to the release before: %v", err)
	}
	const shop, person, user, party, payout, purchase = "11111111-1111-1111-1111-111111111111",
		"22222222-2222-2222-2222-222222222222", "33333333-3333-3333-3333-333333333333",
		"44444444-4444-4444-4444-444444444444", "55555555-5555-5555-5555-555555555555",
		"66666666-6666-6666-6666-666666666666"
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, stmt := range []string{
		`SET LOCAL app.current_shop = '` + shop + `'`,
		`INSERT INTO shops (id, name) VALUES ('` + shop + `', 'Verkstaden')`,
		`INSERT INTO people (id, shop_id, display_name) VALUES ('` + person + `', '` + shop + `', 'En Person')`,
		`INSERT INTO users (id, shop_id, person_id, role, password_hash) VALUES ('` + user + `', '` + shop + `', '` + person + `', 'owner', 'x')`,
		`INSERT INTO parties (id, shop_id, kind, person_id) VALUES ('` + party + `', '` + shop + `', 'private', '` + person + `')`,
		`INSERT INTO payouts (id, shop_id, party_id, purpose, subject_kind, subject_id, note, created_by)
		 VALUES ('` + payout + `', '` + shop + `', '` + party + `', 'other', 'purchase', '` + purchase + `', 'Förskott', '` + user + `')`,
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("the deploy stopped on a payout written by the release before: %v", err)
	}
	var kind, note string
	if err := pool.QueryRow(ctx, `SELECT coalesce(subject_kind, ''), note FROM payouts WHERE id = $1`, payout).Scan(&kind, &note); err != nil {
		t.Fatal(err)
	}
	if kind != "" || !strings.Contains(note, purchase) || !strings.Contains(note, "Förskott") {
		t.Errorf("the payout after the deploy: subject %q, note %q; want no subject, and what it named and said kept in the note", kind, note)
	}
	var valid bool
	if err := pool.QueryRow(ctx, `SELECT convalidated FROM pg_constraint WHERE conname = 'payouts_subject_is_its_purpose'`).Scan(&valid); err != nil || !valid {
		t.Errorf("the constraint is not validated (%v)", err)
	}
}

// A shop with its own template under the shipped template's name -- and
// the shipped one besides -- migrates, and only the oldest of each kind is
// marked as the shipped one.
func TestTheShippedTemplateBackfillMarksOnePerKind(t *testing.T) {
	pool := testsupport.FreshPool(t)
	ctx := context.Background()
	if err := database.Migrate(ctx, pool, migrationsBefore(t, "document_templates_shipped_idx")); err != nil {
		t.Fatalf("migrate to the release before: %v", err)
	}
	const shop = "11111111-1111-1111-1111-111111111111"
	for _, q := range []string{
		`INSERT INTO shops (id, name) VALUES ('` + shop + `', 'Verkstaden')`,
		`INSERT INTO document_templates (id, shop_id, kind, name, created_at) VALUES
		 ('aaaaaaaa-0000-0000-0000-000000000002', '` + shop + `', 'sales_contract', 'Köpeavtal (utkast, behöver juridisk granskning)', now()),
		 ('ffffffff-0000-0000-0000-000000000001', '` + shop + `', 'sales_contract', 'Köpeavtal (utkast, behöver juridisk granskning)', now() - interval '1 day'),
		 ('bbbbbbbb-0000-0000-0000-000000000003', '` + shop + `', 'sales_contract', 'Sales contract (draft, needs legal review)', now())`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := database.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("the deploy stopped on templates with the shipped name: %v", err)
	}
	var marked []string
	rows, err := pool.Query(ctx, `SELECT id::text FROM document_templates WHERE shipped IS NOT NULL ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		marked = append(marked, id)
	}
	if len(marked) != 1 || marked[0] != "ffffffff-0000-0000-0000-000000000001" {
		t.Errorf("marked as shipped: %v; want the oldest alone", marked)
	}
}
