package database

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// widen_text_check (0093) adds values to a CHECK that lists a text
// column's allowed values, so that two branches each adding their own --
// car sales and rental, merged in either order -- keep each other's. It
// reads the list in either form Postgres prints it in, adds only what is
// missing, and refuses a constraint it cannot read rather than write a list
// that allows everything.

// allowed is what the probe's constraint allows now, read back from the
// catalogue in the form it is written in after a widening.
func allowed(t *testing.T, pool *pgxpool.Pool, table, con string) []string {
	t.Helper()
	var def string
	if err := pool.QueryRow(context.Background(), `
		SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = $1::regclass AND conname = $2`, table, con).Scan(&def); err != nil {
		t.Fatalf("read %s: %v", con, err)
	}
	var out []string
	for _, part := range strings.Split(def, "'") {
		if part != "" && !strings.ContainsAny(part, " ():,[]") {
			out = append(out, part)
		}
	}
	slices.Sort(out)
	return out
}

func widen(pool *pgxpool.Pool, table, con, col string, add ...string) error {
	_, err := pool.Exec(context.Background(), `SELECT widen_text_check($1::regclass, $2, $3, $4)`, table, con, col, add)
	return err
}

func refused(pool *pgxpool.Pool, table, value string) bool {
	_, err := pool.Exec(context.Background(), `INSERT INTO `+table+` (k) VALUES ($1)`, value)
	return err != nil
}

func TestACheckIsWidenedWhateverFormItIsPrintedIn(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	for _, stmt := range []string{
		// As a migration writes a list: printed as 'a'::text, ...
		`CREATE TABLE probe_list (k text CONSTRAINT probe_list_k_check CHECK (k IN ('a', 'b')))`,
		// As an earlier widening wrote one: printed as '{a,b}'::text[].
		`CREATE TABLE probe_array (k text CONSTRAINT probe_array_k_check CHECK (k = ANY ('{a,b}'::text[])))`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"probe_list", "probe_array"} {
		con := table + "_k_check"
		if err := widen(pool, table, con, "k", "c"); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if got := allowed(t, pool, table, con); !slices.Equal(got, []string{"a", "b", "c"}) {
			t.Errorf("%s allows %v, want a b c", table, got)
		}
		// Twice is once.
		if err := widen(pool, table, con, "k", "c", "a"); err != nil {
			t.Fatal(err)
		}
		if got := allowed(t, pool, table, con); !slices.Equal(got, []string{"a", "b", "c"}) {
			t.Errorf("%s widened twice allows %v", table, got)
		}
		for v, want := range map[string]bool{"a": false, "c": false, "d": true, "": true} {
			if got := refused(pool, table, v); got != want {
				t.Errorf("%s: %q refused = %v, want %v", table, v, got, want)
			}
		}
	}
}

// A value with an apostrophe in it is read as itself in either form, so it
// is neither added again nor written back doubled: o'clock stays o'clock
// however many times the list is widened.
func TestAValueWithAnApostropheIsReadAsItself(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	for _, stmt := range []string{
		`CREATE TABLE probe_quote_list (k text CONSTRAINT probe_quote_list_k_check CHECK (k IN ('a', 'o''clock')))`,
		`CREATE TABLE probe_quote_array (k text CONSTRAINT probe_quote_array_k_check CHECK (k = ANY ('{a,o''clock}'::text[])))`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"probe_quote_list", "probe_quote_array"} {
		con := table + "_k_check"
		if _, err := pool.Exec(ctx, `INSERT INTO `+table+` (k) VALUES ('o''clock')`); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := widen(pool, table, con, "k", "o'clock", "b"); err != nil {
				t.Fatalf("%s: %v", table, err)
			}
		}
		var def string
		if err := pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1`, con).Scan(&def); err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(def, "clock"); n != 1 || !strings.Contains(def, "'o''clock'") {
			t.Errorf("%s reads %s; want o'clock in it once, as itself", table, def)
		}
		for v, want := range map[string]bool{"o'clock": false, "o''clock": true, "b": false} {
			if got := refused(pool, table, v); got != want {
				t.Errorf("%s: %q refused = %v, want %v", table, v, got, want)
			}
		}
	}
}

// Two branches adding their own values, in either order, end with both.
func TestTwoWideningsInEitherOrderKeepBoth(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	for _, table := range []string{"probe_one", "probe_two"} {
		if _, err := pool.Exec(ctx, `CREATE TABLE `+table+` (k text CONSTRAINT `+table+`_k_check CHECK (k IN ('core')))`); err != nil {
			t.Fatal(err)
		}
	}
	if err := widen(pool, "probe_one", "probe_one_k_check", "k", "stock_unit", "purchase"); err != nil {
		t.Fatal(err)
	}
	if err := widen(pool, "probe_one", "probe_one_k_check", "k", "rental_booking"); err != nil {
		t.Fatal(err)
	}
	if err := widen(pool, "probe_two", "probe_two_k_check", "k", "rental_booking"); err != nil {
		t.Fatal(err)
	}
	if err := widen(pool, "probe_two", "probe_two_k_check", "k", "stock_unit", "purchase"); err != nil {
		t.Fatal(err)
	}
	want := []string{"core", "purchase", "rental_booking", "stock_unit"}
	for _, table := range []string{"probe_one", "probe_two"} {
		if got := allowed(t, pool, table, table+"_k_check"); !slices.Equal(got, want) {
			t.Errorf("%s allows %v, want %v", table, got, want)
		}
	}
}

// A constraint it cannot read is refused, never rewritten to allow
// everything: none there, one on another column, one with no list.
func TestACheckThatCannotBeReadIsNotWidened(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE TABLE probe_odd (k text CONSTRAINT probe_odd_k_check CHECK (length(k) > 2), j text)`); err != nil {
		t.Fatal(err)
	}
	for name, try := range map[string][3]string{
		"no list":       {"probe_odd", "probe_odd_k_check", "k"},
		"no constraint": {"probe_odd", "probe_odd_missing", "k"},
		"other column":  {"probe_odd", "probe_odd_k_check", "j"},
	} {
		if err := widen(pool, try[0], try[1], try[2], "x"); err == nil {
			t.Errorf("%s: widened", name)
		}
	}
	if !refused(pool, "probe_odd", "ab") {
		t.Error("the unreadable constraint was changed")
	}
}
