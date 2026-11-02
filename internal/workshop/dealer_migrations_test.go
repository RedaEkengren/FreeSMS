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
