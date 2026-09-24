package workshop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/sie"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// defaultAccounts is the BAS chart most Swedish small businesses use, so a
// shop that has never thought about it gets a file that imports.
//
// A chart of accounts belongs to the accountant, not to the software, which is
// why these are seeded rather than hardcoded: the shop can change any of them.
var defaultAccounts = []struct {
	Purpose string
	Account int
	Name    string
}{
	{"receivable", 1510, "Kundfordringar"},
	{"sales", 3010, "Försäljning"},
	{"vat_25", 2611, "Utgående moms 25%"},
	{"vat_12", 2621, "Utgående moms 12%"},
	{"vat_6", 2631, "Utgående moms 6%"},
	{"rounding", 3740, "Öres- och kronutjämning"},
}

// ErrAlreadyExported is returned when a period has already gone to the
// accountant.
//
// An accountant who imports the same file twice produces duplicate
// verifications and a balance nobody can explain, so a second export is
// refused unless it is asked for deliberately.
var ErrAlreadyExported = errors.New("workshop: documents in this period have already been exported")

// ErrNothingToExport is returned for a period with no documents in it.
var ErrNothingToExport = errors.New("workshop: there is nothing in that period to export")

// ExportAccounting renders a period as SIE and records that it went out.
func ExportAccounting(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, from, to time.Time, again bool) ([]byte, int, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, 0, access.ErrForbidden
	}
	if !to.After(from) {
		return nil, 0, fmt.Errorf("%w: the period ends before it starts", ErrInvalid)
	}

	var file sie.File
	var count int

	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		accounts, names, err := ledgerAccounts(ctx, tx, scope.ShopID)
		if err != nil {
			return err
		}

		if err := tx.QueryRow(ctx,
			`SELECT name, coalesce(currency, 'SEK') FROM shops WHERE id = $1`, scope.ShopID).
			Scan(&file.CompanyName, new(string)); err != nil {
			return fmt.Errorf("read shop: %w", err)
		}

		// FOR UPDATE, so two people exporting the same period at once cannot
		// both decide it has not been exported yet.
		rows, err := tx.Query(ctx, `
			SELECT id, series, number, issued_at, customer_name,
			       net_minor, vat_minor, gross_minor,
			       credit_of_id IS NOT NULL, accounting_export_id IS NOT NULL
			FROM invoices
			WHERE issued_at >= $1 AND issued_at < $2
			ORDER BY series, number
			FOR UPDATE`, from, to)
		if err != nil {
			return fmt.Errorf("read invoices: %w", err)
		}

		type doc struct {
			id              string
			series          string
			number          int64
			issued          time.Time
			customer        string
			net, vat, gross int64
			credit          bool
			exported        bool
		}
		var docs []doc
		for rows.Next() {
			var d doc
			if err := rows.Scan(&d.id, &d.series, &d.number, &d.issued, &d.customer,
				&d.net, &d.vat, &d.gross, &d.credit, &d.exported); err != nil {
				rows.Close()
				return fmt.Errorf("scan invoice: %w", err)
			}
			docs = append(docs, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		if len(docs) == 0 {
			return ErrNothingToExport
		}
		for _, d := range docs {
			if d.exported && !again {
				return ErrAlreadyExported
			}
		}

		file.Program, file.ProgramVer = "FreeSMS", "1"
		file.Generated = time.Now()
		file.YearFrom = time.Date(from.Year(), 1, 1, 0, 0, 0, 0, from.Location())
		file.YearTo = time.Date(from.Year(), 12, 31, 0, 0, 0, 0, from.Location())
		file.Accounts = names

		for _, d := range docs {
			text := fmt.Sprintf("Faktura %s-%d %s", d.series, d.number, d.customer)
			if d.credit {
				text = fmt.Sprintf("Kreditfaktura %s-%d %s", d.series, d.number, d.customer)
			}
			// A credit note is its own verification, not a correction of the
			// original. The pairing lives in the invoice data; the accounts
			// see two entries.
			file.Verifications = append(file.Verifications, sie.Verification{
				Series: d.series,
				Number: fmt.Sprintf("%d", d.number),
				Date:   d.issued,
				Text:   text,
				Entries: []sie.Entry{
					{Account: accounts["receivable"], Minor: d.gross},
					{Account: accounts["sales"], Minor: -d.net},
					{Account: accounts["vat_25"], Minor: -d.vat},
				},
			})
		}
		count = len(docs)

		var exportID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO accounting_exports (shop_id, period_from, period_to, exported_by, invoice_count)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			scope.ShopID, from, to.AddDate(0, 0, -1), scope.UserID, count).Scan(&exportID); err != nil {
			return fmt.Errorf("record the export: %w", err)
		}

		// Only documents not already handed over get marked, so a deliberate
		// re-export does not rewrite which file the first one went in.
		if _, err := tx.Exec(ctx, `
			UPDATE invoices SET accounting_export_id = $1
			WHERE issued_at >= $2 AND issued_at < $3 AND accounting_export_id IS NULL`,
			exportID, from, to); err != nil {
			return fmt.Errorf("mark as exported: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}

	body, err := sie.Write(file)
	if err != nil {
		return nil, 0, err
	}
	return body, count, nil
}

// ledgerAccounts reads the shop's chart, seeding the defaults the first time.
func ledgerAccounts(ctx context.Context, tx pgx.Tx, shopID string) (map[string]int, map[int]string, error) {
	accounts := map[string]int{}
	names := map[int]string{}

	rows, err := tx.Query(ctx, `SELECT purpose, account FROM ledger_accounts`)
	if err != nil {
		return nil, nil, fmt.Errorf("read accounts: %w", err)
	}
	for rows.Next() {
		var purpose string
		var account int
		if err := rows.Scan(&purpose, &account); err != nil {
			rows.Close()
			return nil, nil, err
		}
		accounts[purpose] = account
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	if len(accounts) == 0 {
		for _, d := range defaultAccounts {
			if _, err := tx.Exec(ctx,
				`INSERT INTO ledger_accounts (shop_id, purpose, account) VALUES ($1, $2, $3)
				 ON CONFLICT DO NOTHING`, shopID, d.Purpose, d.Account); err != nil {
				return nil, nil, fmt.Errorf("seed accounts: %w", err)
			}
			accounts[d.Purpose] = d.Account
		}
	}

	for _, d := range defaultAccounts {
		if account, ok := accounts[d.Purpose]; ok {
			names[account] = d.Name
		}
	}
	return accounts, names, nil
}

// AccountingExport is a record of a file handed over.
type AccountingExport struct {
	From, To     time.Time
	ExportedAt   time.Time
	ExportedBy   string
	InvoiceCount int
}

// AccountingExports lists what has gone to the accountant.
func AccountingExports(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]AccountingExport, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	var out []AccountingExport
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT e.period_from, e.period_to, e.exported_at,
			       coalesce(p.display_name, ''), e.invoice_count
			FROM accounting_exports e
			LEFT JOIN users u  ON u.id = e.exported_by
			LEFT JOIN people p ON p.id = u.person_id
			ORDER BY e.exported_at DESC`)
		if err != nil {
			return fmt.Errorf("list exports: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var e AccountingExport
			if err := rows.Scan(&e.From, &e.To, &e.ExportedAt, &e.ExportedBy, &e.InvoiceCount); err != nil {
				return fmt.Errorf("scan export: %w", err)
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}
