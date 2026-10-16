// Package ops answers the operator's questions: is what came back from a
// backup the workshop's books, and is the running service in a state that
// will become a problem before somebody notices.
//
// A backup that restores without an error has proved that the file was a
// dump. It has not proved that the invoices are in an unbroken series, that a
// credit still points at what it credits, that a payment reversal reverses a
// payment on the same invoice, or that the photographs a job refers to are on
// disk. Those are the things that make a restored workshop trustworthy, and
// they are checked here, from the data, with the same eyes whether the data
// came from a drill or from the night a disk died.
package ops

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// Files is where photographs are kept.
type Files interface {
	Open(key string) (io.ReadCloser, error)
}

// Level says how much a finding matters.
type Level string

const (
	Pass Level = "pass"
	// Warn is worth a person's look and not a reason to refuse the restore.
	Warn Level = "warn"
	Fail Level = "fail"
)

// Check is one question and its answer.
type Check struct {
	Name   string
	Level  Level
	Detail string
}

// Sample is the newest of something, so a person can compare it with what they
// remember, or with the last thing they saw before the failure.
type Sample struct {
	What string
	When time.Time
	Ref  string
}

// ShopReport is one workshop's.
type ShopReport struct {
	ShopID string
	Name   string
	Checks []Check
	Latest []Sample
	// The most recent write of any kind that is in the data. The distance from
	// this to the moment of the failure is the data that was lost.
	NewestWrite time.Time
}

// Report is the whole installation's.
type Report struct {
	Shops []ShopReport
	// Migrations applied, as the database records them.
	Migrations int
	At         time.Time
}

// Failed is whether any check failed.
func (r Report) Failed() bool {
	for _, s := range r.Shops {
		for _, c := range s.Checks {
			if c.Level == Fail {
				return true
			}
		}
	}
	return false
}

// Text is the report for a terminal.
func (r Report) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "verified %s; %d migrations applied\n", r.At.UTC().Format(time.RFC3339), r.Migrations)
	for _, s := range r.Shops {
		fmt.Fprintf(&b, "\n%s (%s)\n", s.Name, s.ShopID)
		for _, c := range s.Checks {
			mark := map[Level]string{Pass: "ok  ", Warn: "warn", Fail: "FAIL"}[c.Level]
			fmt.Fprintf(&b, "  %s  %s: %s\n", mark, c.Name, c.Detail)
		}
		if !s.NewestWrite.IsZero() {
			fmt.Fprintf(&b, "  newest write in the data: %s\n", s.NewestWrite.UTC().Format(time.RFC3339))
		}
		for _, l := range s.Latest {
			fmt.Fprintf(&b, "  newest %s: %s %s\n", l.What, l.Ref, l.When.UTC().Format(time.RFC3339))
		}
	}
	return b.String()
}

// Verify checks every workshop in the database.
func Verify(ctx context.Context, pool *pgxpool.Pool, files Files) (Report, error) {
	rep := Report{At: time.Now()}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&rep.Migrations); err != nil {
		return rep, fmt.Errorf("read migrations: %w", err)
	}
	rows, err := pool.Query(ctx, `SELECT id::text, name FROM shops ORDER BY created_at`)
	if err != nil {
		return rep, fmt.Errorf("list shops: %w", err)
	}
	type shop struct{ id, name string }
	var shops []shop
	for rows.Next() {
		var s shop
		if err := rows.Scan(&s.id, &s.name); err != nil {
			rows.Close()
			return rep, err
		}
		shops = append(shops, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}
	for _, s := range shops {
		sr := ShopReport{ShopID: s.id, Name: s.name}
		err := database.InShop(ctx, pool, s.id, func(ctx context.Context, tx pgx.Tx) error {
			return verifyShop(ctx, tx, files, &sr)
		})
		if err != nil {
			return rep, fmt.Errorf("verify %s: %w", s.name, err)
		}
		rep.Shops = append(rep.Shops, sr)
	}
	return rep, nil
}

func verifyShop(ctx context.Context, tx pgx.Tx, files Files, sr *ShopReport) error {
	add := func(name string, level Level, format string, args ...any) {
		sr.Checks = append(sr.Checks, Check{Name: name, Level: level, Detail: fmt.Sprintf(format, args...)})
	}
	count := func(q string, args ...any) (int, error) {
		var n int
		if err := tx.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			return 0, fmt.Errorf("%s: %w", strings.Fields(q)[0], err)
		}
		return n, nil
	}

	// Numbers are an unbroken series. A gap in a Swedish invoice series is a
	// document that is missing or was deleted, and it is the first thing an
	// auditor looks for.
	for _, table := range []struct{ name, label string }{{"invoices", "invoice numbers"}, {"invoice_payments", "payment numbers"}} {
		rows, err := tx.Query(ctx, fmt.Sprintf(`
			SELECT t.series, count(*)::bigint, min(t.number), max(t.number), coalesce(max(s.next_number), 0)
			FROM %s t LEFT JOIN invoice_series s ON s.series = t.series AND s.shop_id = t.shop_id
			GROUP BY t.series ORDER BY t.series`, table.name))
		if err != nil {
			return fmt.Errorf("read %s: %w", table.name, err)
		}
		var problems, series []string
		for rows.Next() {
			var name string
			var n, lo, hi, next int64
			if err := rows.Scan(&name, &n, &lo, &hi, &next); err != nil {
				rows.Close()
				return err
			}
			series = append(series, fmt.Sprintf("%s 1-%d", name, hi))
			switch {
			case lo != 1 || n != hi:
				problems = append(problems, fmt.Sprintf("series %s has %d documents numbered %d to %d", name, n, lo, hi))
			case next != hi+1:
				problems = append(problems, fmt.Sprintf("series %s has used %d and says the next is %d", name, hi, next))
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		switch {
		case len(problems) > 0:
			add(table.label, Fail, "%s", strings.Join(problems, "; "))
		case len(series) == 0:
			add(table.label, Pass, "none issued")
		default:
			add(table.label, Pass, "unbroken: %s", strings.Join(series, ", "))
		}
	}

	// A document says what its lines say.
	if n, err := count(`
		SELECT count(*) FROM invoices i
		WHERE EXISTS (SELECT 1 FROM invoice_lines l WHERE l.invoice_id = i.id)
		  AND (i.net_minor <> (SELECT sum(l.net_minor) FROM invoice_lines l WHERE l.invoice_id = i.id)
		    OR i.vat_minor <> (SELECT sum(l.vat_minor) FROM invoice_lines l WHERE l.invoice_id = i.id)
		    OR i.gross_minor <> i.net_minor + i.vat_minor)`); err != nil {
		return err
	} else if n > 0 {
		add("invoice totals", Fail, "%d documents whose total is not what their lines add up to", n)
	} else {
		add("invoice totals", Pass, "every total is its lines")
	}

	// A credit takes back what was invoiced, never more.
	if n, err := count(`
		SELECT count(*) FROM invoices o
		WHERE o.credit_of_id IS NULL
		  AND o.gross_minor + coalesce((SELECT sum(c.gross_minor) FROM invoices c WHERE c.credit_of_id = o.id), 0) < 0`); err != nil {
		return err
	} else if n > 0 {
		add("credit notes", Fail, "%d invoices credited for more than they were", n)
	} else {
		add("credit notes", Pass, "no invoice is credited beyond itself")
	}

	// A reversal reverses a payment on the same invoice, for the same money.
	if n, err := count(`
		SELECT count(*) FROM invoice_payments r JOIN invoice_payments p ON p.id = r.reverses_id
		WHERE r.invoice_id <> p.invoice_id OR r.amount_minor <> -p.amount_minor`); err != nil {
		return err
	} else if n > 0 {
		add("payment reversals", Fail, "%d reversals that do not undo the payment they point at", n)
	} else {
		add("payment reversals", Pass, "every reversal undoes its payment")
	}

	// Jobs and their invoices agree.
	if n, err := count(`
		SELECT count(*) FROM work_orders w
		WHERE w.state = 'invoiced'
		  AND NOT EXISTS (SELECT 1 FROM invoices i WHERE i.work_order_id = w.id AND i.credit_of_id IS NULL)`); err != nil {
		return err
	} else if n > 0 {
		add("jobs and invoices", Fail, "%d jobs are invoiced and have no invoice", n)
	} else if m, err := count(`
		SELECT count(*) FROM invoices i JOIN work_orders w ON w.id = i.work_order_id
		WHERE i.credit_of_id IS NULL AND w.state NOT IN ('invoiced', 'closed')`); err != nil {
		return err
	} else if m > 0 {
		add("jobs and invoices", Fail, "%d invoices belong to a job that is not invoiced or closed", m)
	} else {
		add("jobs and invoices", Pass, "every invoiced job has its invoice and the other way round")
	}

	// What was charged was agreed. Not a reason to refuse a restore -- a line
	// can be put on an invoice marked as not agreed -- but a number to look at.
	if n, err := count(`
		SELECT count(*) FROM work_order_lines l JOIN work_orders w ON w.id = l.work_order_id
		WHERE w.state IN ('invoiced', 'closed') AND l.cost_bearer = 'customer'
		  AND l.withdrawn_at IS NULL AND l.declined_at IS NULL AND l.approved_at IS NULL`); err != nil {
		return err
	} else if n > 0 {
		add("approvals", Warn, "%d charged lines on finished jobs were never approved", n)
	} else {
		add("approvals", Pass, "every charged line on a finished job was approved")
	}

	// The photographs a job refers to are on disk.
	if files != nil {
		rows, err := tx.Query(ctx, `SELECT storage_key, byte_size FROM attachments ORDER BY created_at DESC LIMIT 5000`)
		if err != nil {
			return fmt.Errorf("read attachments: %w", err)
		}
		type att struct {
			key  string
			size int64
		}
		var atts []att
		for rows.Next() {
			var a att
			if err := rows.Scan(&a.key, &a.size); err != nil {
				rows.Close()
				return err
			}
			atts = append(atts, a)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		var missing []string
		for _, a := range atts {
			f, err := files.Open(a.key)
			if err != nil {
				missing = append(missing, a.key)
				continue
			}
			n, _ := io.Copy(io.Discard, f)
			f.Close()
			if n != a.size {
				missing = append(missing, a.key)
			}
		}
		sort.Strings(missing)
		switch {
		case len(missing) > 0:
			shown := missing
			if len(shown) > 5 {
				shown = shown[:5]
			}
			add("photographs", Fail, "%d of %d are not on disk, such as %s", len(missing), len(atts), strings.Join(shown, ", "))
		default:
			add("photographs", Pass, "%d on disk", len(atts))
		}
	}

	// What is the newest thing in here: the line to read against the moment
	// of the failure.
	for _, s := range []struct{ what, q string }{
		{"job", `SELECT 'job ' || number, updated_at FROM work_orders ORDER BY updated_at DESC LIMIT 1`},
		{"invoice", `SELECT series || '-' || number, issued_at FROM invoices ORDER BY issued_at DESC LIMIT 1`},
		{"payment", `SELECT series || '-' || number, recorded_at FROM invoice_payments ORDER BY recorded_at DESC LIMIT 1`},
		{"state change", `SELECT to_state, at FROM work_order_state_changes ORDER BY at DESC LIMIT 1`},
		{"message", `SELECT about, created_at FROM outbound_messages ORDER BY created_at DESC LIMIT 1`},
	} {
		var ref string
		var at time.Time
		err := tx.QueryRow(ctx, s.q).Scan(&ref, &at)
		if err != nil {
			if err == pgx.ErrNoRows {
				continue
			}
			return fmt.Errorf("newest %s: %w", s.what, err)
		}
		sr.Latest = append(sr.Latest, Sample{What: s.what, When: at, Ref: ref})
		if at.After(sr.NewestWrite) {
			sr.NewestWrite = at
		}
	}
	return nil
}
