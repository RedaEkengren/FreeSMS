package workshop

import (
	"context"
	"errors"
	"fmt"
	"sort"
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
	{"sublet", 3011, "Försäljning, främmande arbete"},
	{"consumables", 3590, "Övriga fakturerade kostnader"},
	{"invoice_fee", 3540, "Faktureringsavgifter"},
	{"cash", 1910, "Kassa"},
	{"bank", 1930, "Företagskonto"},
	{"vat_25", 2611, "Utgående moms 25%"},
	{"vat_12", 2621, "Utgående moms 12%"},
	{"vat_6", 2631, "Utgående moms 6%"},
	{"rounding", 3740, "Öres- och kronutjämning"},
}

// vatAccounts maps a VAT rate, in basis points, to the purpose whose account
// receives the output VAT at that rate. These are the Swedish rates. A line at
// any other rate is refused rather than guessed at: posting 10 per cent VAT to
// the 25 per cent account would balance and still be wrong in the VAT return.
var vatAccounts = map[int]string{
	2500: "vat_25",
	1200: "vat_12",
	600:  "vat_6",
}

// salesAccounts maps a line's kind to the purpose whose account receives its
// net. One lump on 3010 was what the bookkeeper used to get: a figure with
// the work, the parts, the subcontracted work, the consumables and the fees
// in it, which could not be reconciled against anything.
var salesAccounts = map[string]string{
	"labour":      "sales",
	"part":        "sales",
	"sublet":      "sublet",
	"consumables": "consumables",
	"fee":         "invoice_fee",
}

// ErrUnsupportedVATRate is returned when an invoice carries a rate this
// export has no account for.
var ErrUnsupportedVATRate = errors.New("workshop: an invoice in this period has a VAT rate with no account")

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

		// Payments that arrived in the period, by the day they arrived. A
		// bookkeeper given revenue with no settlement against it has a
		// receivable that never clears.
		type pay struct {
			id, invoiceID, method, invoiceRef, customer string
			number                                      int64
			amount, rounding                            int64
			paidOn                                      time.Time
			reverses, exported                          bool
		}
		var pays []pay
		prows, err := tx.Query(ctx, `
			SELECT p.id, p.invoice_id, p.method, i.series || '-' || i.number, i.customer_name,
			       p.number, p.amount_minor, p.rounding_minor, p.paid_on,
			       p.reverses_id IS NOT NULL, p.accounting_export_id IS NOT NULL
			FROM invoice_payments p JOIN invoices i ON i.id = p.invoice_id
			WHERE p.paid_on >= $1::date AND p.paid_on < $2::date
			ORDER BY p.series, p.number
			FOR UPDATE OF p`, from.Format("2006-01-02"), to.Format("2006-01-02"))
		if err != nil {
			return fmt.Errorf("read payments: %w", err)
		}
		for prows.Next() {
			var p pay
			if err := prows.Scan(&p.id, &p.invoiceID, &p.method, &p.invoiceRef, &p.customer,
				&p.number, &p.amount, &p.rounding, &p.paidOn, &p.reverses, &p.exported); err != nil {
				prows.Close()
				return fmt.Errorf("scan payment: %w", err)
			}
			pays = append(pays, p)
		}
		prows.Close()
		if err := prows.Err(); err != nil {
			return err
		}

		if len(docs) == 0 && len(pays) == 0 {
			return ErrNothingToExport
		}
		for _, d := range docs {
			if d.exported && !again {
				return ErrAlreadyExported
			}
		}
		for _, p := range pays {
			if p.exported && !again {
				return ErrAlreadyExported
			}
		}

		// VAT per rate, from the frozen lines. The invoice header carries one
		// VAT total, and posting all of it to the 25 per cent account made a
		// verification that balanced and still put 12 and 6 per cent VAT in
		// the wrong place on the VAT return. The lines carry their own rate
		// and their own rounded amount, so summing those is both the right
		// split and exactly the figure the document printed.
		vatByDoc := map[string]map[int]int64{}
		ids := make([]string, 0, len(docs))
		for _, d := range docs {
			ids = append(ids, d.id)
		}
		vrows, err := tx.Query(ctx, `
			SELECT invoice_id, vat_rate_bp, sum(vat_minor)
			FROM invoice_lines WHERE invoice_id = ANY($1)
			GROUP BY invoice_id, vat_rate_bp`, ids)
		if err != nil {
			return fmt.Errorf("read VAT per rate: %w", err)
		}
		for vrows.Next() {
			var id string
			var rate int
			var amount int64
			if err := vrows.Scan(&id, &rate, &amount); err != nil {
				vrows.Close()
				return fmt.Errorf("scan VAT per rate: %w", err)
			}
			if vatByDoc[id] == nil {
				vatByDoc[id] = map[int]int64{}
			}
			vatByDoc[id][rate] += amount
		}
		vrows.Close()
		if err := vrows.Err(); err != nil {
			return err
		}

		// The net per kind of line, for the sales side.
		netByDoc := map[string]map[string]int64{}
		nrows, err := tx.Query(ctx, `
			SELECT invoice_id, kind, sum(net_minor)
			FROM invoice_lines WHERE invoice_id = ANY($1)
			GROUP BY invoice_id, kind`, ids)
		if err != nil {
			return fmt.Errorf("read net per kind: %w", err)
		}
		for nrows.Next() {
			var id, kind string
			var amount int64
			if err := nrows.Scan(&id, &kind, &amount); err != nil {
				nrows.Close()
				return fmt.Errorf("scan net per kind: %w", err)
			}
			purpose, ok := salesAccounts[kind]
			if !ok {
				nrows.Close()
				return fmt.Errorf("workshop: a line of kind %q has no account", kind)
			}
			if netByDoc[id] == nil {
				netByDoc[id] = map[string]int64{}
			}
			netByDoc[id][purpose] += amount
		}
		nrows.Close()
		if err := nrows.Err(); err != nil {
			return err
		}

		file.Program, file.ProgramVer = "FreeSMS", "1"
		// Dates in the file are the shop's calendar, which from is in. An
		// instant formatted as it comes from the database is UTC: an invoice
		// issued at half past midnight on the first was booked on the last
		// day of the month before, in the wrong period of the accounts.
		books := from.Location()
		file.Generated = time.Now().In(books)
		file.YearFrom = time.Date(from.Year(), 1, 1, 0, 0, 0, 0, from.Location())
		file.YearTo = time.Date(from.Year(), 12, 31, 0, 0, 0, 0, from.Location())
		file.Accounts = names

		for _, d := range docs {
			v, err := InvoiceVerification(accounts, VoucherDocument{
				Series: d.series, Number: d.number, Issued: d.issued.In(books),
				Customer: d.customer, Credit: d.credit,
				NetMinor: d.net, VATMinor: d.vat, GrossMinor: d.gross,
				NetByPurpose: netByDoc[d.id], VATByRate: vatByDoc[d.id],
			})
			if err != nil {
				return err
			}
			file.Verifications = append(file.Verifications, v)
		}

		// Each payment its own voucher in its own series, dated the day the
		// money came: into the till or the bank, out of the receivable, with
		// any öre rounded at the counter on the rounding account.
		for _, p := range pays {
			into := accounts["bank"]
			if p.method == "cash" {
				into = accounts["cash"]
			}
			entries := []sie.Entry{{Account: into, Minor: p.amount}}
			if p.rounding != 0 {
				entries = append(entries, sie.Entry{Account: accounts["rounding"], Minor: p.rounding})
			}
			entries = append(entries, sie.Entry{Account: accounts["receivable"], Minor: -(p.amount + p.rounding)})
			text := fmt.Sprintf("Betalning %s %s", p.invoiceRef, p.customer)
			if p.reverses {
				text = fmt.Sprintf("Rättelse av betalning %s %s", p.invoiceRef, p.customer)
			}
			file.Verifications = append(file.Verifications, sie.Verification{
				Series:  PaymentSeries,
				Number:  fmt.Sprintf("%d", p.number),
				Date:    p.paidOn,
				Text:    text,
				Entries: entries,
			})
		}
		count = len(docs) + len(pays)

		var exportID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO accounting_exports (shop_id, period_from, period_to, exported_by, invoice_count)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			scope.ShopID, from, to.AddDate(0, 0, -1), scope.UserID, count).Scan(&exportID); err != nil {
			return fmt.Errorf("record the export: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE invoice_payments SET accounting_export_id = $1
			WHERE paid_on >= $2::date AND paid_on < $3::date AND accounting_export_id IS NULL`,
			exportID, from.Format("2006-01-02"), to.Format("2006-01-02")); err != nil {
			return fmt.Errorf("mark payments as exported: %w", err)
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

	// Any purpose the shop has no account for yet gets the default: on the
	// first export, and for a purpose added later -- the surcharges -- in a
	// shop whose chart was seeded before it existed. An account the shop
	// changed is never touched.
	for _, d := range defaultAccounts {
		if _, ok := accounts[d.Purpose]; ok {
			continue
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_accounts (shop_id, purpose, account) VALUES ($1, $2, $3)
			 ON CONFLICT DO NOTHING`, shopID, d.Purpose, d.Account); err != nil {
			return nil, nil, fmt.Errorf("seed accounts: %w", err)
		}
		accounts[d.Purpose] = d.Account
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

// VoucherDocument is an issued invoice or credit note as the books need it:
// the header's totals, and the frozen lines summed by what they sold and by
// VAT rate.
type VoucherDocument struct {
	Series   string
	Number   int64
	Issued   time.Time // in the shop's calendar
	Customer string
	Credit   bool

	NetMinor, VATMinor, GrossMinor int64
	NetByPurpose                   map[string]int64 // "sales", "sublet", ...
	VATByRate                      map[int]int64    // basis points
}

// InvoiceVerification is the verification a document is booked as: the
// receivable, each kind of sale on its own account, and the VAT per rate.
// Pure, so that what the books receive can be worked out and checked without
// a database -- including for the scene on the project's site, which prints
// what this writes.
func InvoiceVerification(accounts map[string]int, d VoucherDocument) (sie.Verification, error) {
	text := fmt.Sprintf("Faktura %s-%d %s", d.Series, d.Number, d.Customer)
	if d.Credit {
		text = fmt.Sprintf("Kreditfaktura %s-%d %s", d.Series, d.Number, d.Customer)
	}
	// A credit note is its own verification, not a correction of the
	// original. The pairing lives in the invoice data; the accounts see two
	// entries.
	entries := []sie.Entry{{Account: accounts["receivable"], Minor: d.GrossMinor}}
	// Each kind of sale on its own account, in a fixed order so the same
	// document always produces the same file.
	var sold int64
	for _, purpose := range []string{"sales", "sublet", "consumables", "invoice_fee"} {
		if amount := d.NetByPurpose[purpose]; amount != 0 {
			entries = append(entries, sie.Entry{Account: accounts[purpose], Minor: -amount})
			sold += amount
		}
	}
	if sold != d.NetMinor {
		return sie.Verification{}, fmt.Errorf("workshop: %s-%d: the lines sum to %d net and the document says %d",
			d.Series, d.Number, sold, d.NetMinor)
	}
	// In rate order, so the same document always produces the same file, and
	// only for rates that carry VAT: a zero-rated line is in the sales figure
	// and has nothing to post here.
	rates := make([]int, 0, len(d.VATByRate))
	for rate := range d.VATByRate {
		rates = append(rates, rate)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(rates)))
	var posted int64
	for _, rate := range rates {
		amount := d.VATByRate[rate]
		if amount == 0 {
			continue
		}
		purpose, ok := vatAccounts[rate]
		if !ok {
			return sie.Verification{}, fmt.Errorf("%w: %s-%d has %d.%02d%%", ErrUnsupportedVATRate,
				d.Series, d.Number, rate/100, rate%100)
		}
		entries = append(entries, sie.Entry{Account: accounts[purpose], Minor: -amount})
		posted += amount
	}
	// The lines and the header were written by the same transaction and must
	// agree. If they ever do not, the file is not written: an accountant
	// importing an unbalanced verification is worse than no file.
	if posted != d.VATMinor {
		return sie.Verification{}, fmt.Errorf("workshop: %s-%d: VAT per line sums to %d and the document says %d",
			d.Series, d.Number, posted, d.VATMinor)
	}
	return sie.Verification{
		Series:  d.Series,
		Number:  fmt.Sprintf("%d", d.Number),
		Date:    d.Issued,
		Text:    text,
		Entries: entries,
	}, nil
}

// DefaultAccounts is the chart a shop starts with: account per purpose, and
// the accounts' names.
func DefaultAccounts() (map[string]int, map[int]string) {
	accounts, names := map[string]int{}, map[int]string{}
	for _, d := range defaultAccounts {
		accounts[d.Purpose] = d.Account
		names[d.Account] = d.Name
	}
	return accounts, names
}

// SalesPurpose is the purpose whose account receives a kind of line's net.
func SalesPurpose(kind string) (string, bool) {
	purpose, ok := salesAccounts[kind]
	return purpose, ok
}
