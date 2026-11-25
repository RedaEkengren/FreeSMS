package workshop

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
)

// What storing a set costs (#231). A storage fee is a tyre extra with a period.
// A set is charged once for each period: on the change job, or on one invoice
// to the customer for all their sets. The charge is a row and the database
// refuses a second for the same set and period, whichever way the first was
// made. A set taken out in the middle of a period stays charged: no refund.

var (
	errStorageNotAFee   = fmt.Errorf("%w: that is not a storage fee", ErrInvalid)
	errStorageCharged   = fmt.Errorf("%w: that set is already charged for that period", ErrInvalid)
	errStorageYear      = fmt.Errorf("%w: a year from 2000 to 2100", ErrInvalid)
	errStorageNotStored = fmt.Errorf("%w: only a set in storage is charged for storing", ErrInvalid)
	errStorageOwner     = fmt.Errorf("%w: a set is charged to the customer it belongs to", ErrInvalid)
	errStorageBilling   = fmt.Errorf("%w: charge on the job or on an invoice", ErrInvalid)
)

// storagePeriod is the period a fee covers for a set: the year, or the year and
// the kind of season the set is for.
func storagePeriod(feePeriod, setSeason string, year int) string {
	if feePeriod == "year" {
		return fmt.Sprintf("%d", year)
	}
	kind := "all_season"
	switch setSeason {
	case SeasonSummer:
		kind = "summer"
	case SeasonWinterStudded, SeasonWinterFriction:
		kind = "winter"
	}
	return fmt.Sprintf("%d-%s", year, kind)
}

// StorageBilling says how the shop would rather charge: "job" or "invoice".
func StorageBillingOf(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) (string, error) {
	if !scope.Role.SeesCustomerPersonalData() && !scope.Role.RunsTheShop() {
		return "", access.ErrForbidden
	}
	billing := "job"
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT billing FROM tyre_storage_settings ORDER BY changed_at DESC, id LIMIT 1`).Scan(&billing)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	return billing, err
}

// SetStorageBilling chooses the way the page offers first. Whoever runs the shop.
func SetStorageBilling(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, billing string) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if billing != "job" && billing != "invoice" {
		return errStorageBilling
	}
	return inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tyre_storage_settings (shop_id, billing, changed_by) VALUES ($1, $2, NULLIF($3, '')::uuid)`,
			scope.ShopID, billing, scope.UserID)
		return err
	})
}

// feeTx reads a storage fee at its price now.
func feeTx(ctx context.Context, tx pgx.Tx, extraID string) (TyreExtra, error) {
	var x TyreExtra
	err := tx.QueryRow(ctx, tyreExtrasSQL+` WHERE x.id = $1`, extraID).
		Scan(&x.ID, &x.Name, &x.Per, &x.LineKind, &x.UnitPriceMinor, &x.VATRateBasis, &x.Active, &x.StoragePeriod)
	if errors.Is(err, pgx.ErrNoRows) {
		return x, ErrNotFound
	}
	if err != nil {
		return x, err
	}
	if x.StoragePeriod == "" {
		return x, errStorageNotAFee
	}
	if !x.Active {
		return x, errTyreExtraRetired
	}
	return x, nil
}

// feeQuantityTx is how many the fee is charged for: one for a set, or the
// tyres standing in it.
func feeQuantityTx(ctx context.Context, tx pgx.Tx, x TyreExtra, setID string) (int64, error) {
	if x.Per == "set" {
		return 1000, nil
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*)::int FROM tyres t WHERE t.tyre_set_id = $1 AND NOT EXISTS (SELECT 1 FROM tyres r WHERE r.replaces_id = t.id)`, setID).Scan(&n); err != nil {
		return 0, err
	}
	if n == 0 {
		n = 1
	}
	return int64(n) * 1000, nil
}

func feeLabel(x TyreExtra, brand, model, period string) string {
	return strings.TrimSpace(x.Name+" – "+strings.TrimSpace(brand+" "+model)) + ", " + period
}

func checkFeeYear(year int) error {
	if year < 2000 || year > 2100 {
		return errStorageYear
	}
	return nil
}

// ChargeStorageOnJob puts the storage fee for a set and a period on the change
// job as a line, once: charged already, by a job or an invoice, it is refused.
// The set must be the job customer's and in storage. Whoever deals with
// customers.
func ChargeStorageOnJob(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID, setID, extraID string, year int) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(jobID) || !looksLikeUUID(setID) || !looksLikeUUID(extraID) {
		return ErrNotFound
	}
	if err := checkFeeYear(year); err != nil {
		return err
	}
	return inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		job, err := jobTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		x, err := feeTx(ctx, tx, extraID)
		if err != nil {
			return err
		}
		var owner, season, brand, model string
		var stored bool
		if err := tx.QueryRow(ctx, `
			SELECT ts.customer_id::text, ts.season, coalesce(ts.brand, ''), coalesce(ts.model, ''),
			       EXISTS (SELECT 1 FROM tyre_set_placements pl WHERE pl.tyre_set_id = ts.id AND pl.removed_at IS NULL)
			FROM tyre_sets ts WHERE ts.id = $1`, setID).Scan(&owner, &season, &brand, &model, &stored); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if owner != job.customer {
			return errStorageOwner
		}
		if !stored {
			return errStorageNotStored
		}
		period := storagePeriod(x.StoragePeriod, season, year)
		qty, err := feeQuantityTx(ctx, tx, x, setID)
		if err != nil {
			return err
		}
		if err := chargeRowTx(ctx, tx, scope, setID, x, period, qty, jobID, ""); err != nil {
			return err
		}
		line := NewLine{Kind: x.LineKind, Description: feeLabel(x, brand, model, period), QuantityMilli: qty,
			UnitPriceMinor: x.UnitPriceMinor, VATRateBasis: x.VATRateBasis}
		if err := checkLine(&line); err != nil {
			return err
		}
		return addLineTx(ctx, tx, scope, jobID, line, lineSource{})
	})
}

func chargeRowTx(ctx context.Context, tx pgx.Tx, scope access.Scope, setID string, x TyreExtra, period string, qty int64, jobID, invoiceID string) error {
	tag, err := tx.Exec(ctx, `
		INSERT INTO tyre_storage_charges (shop_id, tyre_set_id, extra_id, period, quantity_milli, unit_price_minor, vat_rate_basis,
		                                  work_order_id, invoice_id, charged_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, '')::uuid, NULLIF($9, '')::uuid, NULLIF($10, '')::uuid)
		ON CONFLICT (tyre_set_id, period) DO NOTHING`,
		scope.ShopID, setID, x.ID, period, qty, x.UnitPriceMinor, x.VATRateBasis, jobID, invoiceID, scope.UserID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errStorageCharged
	}
	return nil
}

// StorageDue is a customer with sets in storage not yet charged for a period,
// and what they would come to.
type StorageDue struct {
	CustomerID string
	Name       string
	Sets       []string
	NetMinor   int64
}

// StorageDueFor lists the customers with sets in storage that the fee has not
// been charged for in the year, by name. Whoever deals with customers.
func StorageDueFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, extraID string, year int) ([]StorageDue, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(extraID) {
		return nil, ErrNotFound
	}
	if err := checkFeeYear(year); err != nil {
		return nil, err
	}
	var out []StorageDue
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		x, err := feeTx(ctx, tx, extraID)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT c.id::text, coalesce(CASE WHEN c.kind = 'company' THEN c.company_name END, p.display_name, ''),
			       ts.id::text, ts.season, coalesce(ts.brand, '') || CASE WHEN coalesce(ts.model, '') <> '' THEN ' ' || ts.model ELSE '' END
			FROM tyre_sets ts
			JOIN tyre_set_placements pl ON pl.tyre_set_id = ts.id AND pl.removed_at IS NULL
			JOIN customers c ON c.id = ts.customer_id LEFT JOIN people p ON p.id = c.person_id
			WHERE NOT EXISTS (SELECT 1 FROM shop_own_customers o WHERE o.customer_id = c.id)
			ORDER BY 2, c.id, ts.created_at, ts.id`)
		if err != nil {
			return err
		}
		type row struct{ cust, name, set, season, label string }
		var all []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.cust, &r.name, &r.set, &r.season, &r.label); err != nil {
				rows.Close()
				return err
			}
			all = append(all, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		byCustomer := map[string]*StorageDue{}
		var order []string
		for _, r := range all {
			var charged bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tyre_storage_charges WHERE tyre_set_id = $1 AND period = $2)`,
				r.set, storagePeriod(x.StoragePeriod, r.season, year)).Scan(&charged); err != nil {
				return err
			}
			if charged {
				continue
			}
			qty, err := feeQuantityTx(ctx, tx, x, r.set)
			if err != nil {
				return err
			}
			d := byCustomer[r.cust]
			if d == nil {
				d = &StorageDue{CustomerID: r.cust, Name: r.name}
				byCustomer[r.cust] = d
				order = append(order, r.cust)
			}
			d.Sets = append(d.Sets, r.label)
			d.NetMinor += qty * x.UnitPriceMinor / 1000
		}
		for _, id := range order {
			out = append(out, *byCustomer[id])
		}
		return nil
	})
	return out, err
}

// InvoiceStorage invoices a customer for the storage fee on every set of theirs
// in storage that has not been charged for the period: one invoice, one line
// for each set, however many sets they have. A set already charged is left
// off, and with nothing left there is nothing to invoice. The invoice goes in
// the same series as any other and is followed up as an unpaid one is.
// Whoever deals with customers.
func InvoiceStorage(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, customerID, extraID string, year int) (Invoice, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return Invoice{}, access.ErrForbidden
	}
	if !looksLikeUUID(customerID) || !looksLikeUUID(extraID) {
		return Invoice{}, ErrNotFound
	}
	if err := checkFeeYear(year); err != nil {
		return Invoice{}, err
	}
	var inv Invoice
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		// One invoicing of a customer at a time, so two clicks are told apart.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "storagefee:"+customerID); err != nil {
			return err
		}
		x, err := feeTx(ctx, tx, extraID)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT ts.id::text, ts.season, coalesce(ts.brand, ''), coalesce(ts.model, '')
			FROM tyre_sets ts
			JOIN tyre_set_placements pl ON pl.tyre_set_id = ts.id AND pl.removed_at IS NULL
			WHERE ts.customer_id = $1 AND NOT EXISTS (SELECT 1 FROM shop_own_customers o WHERE o.customer_id = ts.customer_id)
			ORDER BY ts.created_at, ts.id`, customerID)
		if err != nil {
			return err
		}
		type set struct{ id, season, brand, model string }
		var sets []set
		for rows.Next() {
			var s set
			if err := rows.Scan(&s.id, &s.season, &s.brand, &s.model); err != nil {
				rows.Close()
				return err
			}
			sets = append(sets, s)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		type todo struct {
			set    set
			period string
			qty    int64
		}
		var due []todo
		var lines []SourcedLine
		for _, s := range sets {
			period := storagePeriod(x.StoragePeriod, s.season, year)
			var charged bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tyre_storage_charges WHERE tyre_set_id = $1 AND period = $2)`, s.id, period).Scan(&charged); err != nil {
				return err
			}
			if charged {
				continue
			}
			qty, err := feeQuantityTx(ctx, tx, x, s.id)
			if err != nil {
				return err
			}
			due = append(due, todo{s, period, qty})
			lines = append(lines, SourcedLine{Kind: x.LineKind, Description: feeLabel(x, s.brand, s.model, period),
				QuantityMilli: qty, UnitPriceMinor: x.UnitPriceMinor, VATRateBasis: x.VATRateBasis})
		}
		if len(lines) == 0 {
			return ErrNothingToInvoice
		}
		inv, err = issueSourcedTx(ctx, tx, scope, sourced{source: SourceManual, customerID: customerID}, lines)
		if err != nil {
			return err
		}
		for _, d := range due {
			if err := chargeRowTx(ctx, tx, scope, d.set.id, x, d.period, d.qty, "", inv.ID); err != nil {
				return err
			}
		}
		return nil
	})
	return inv, err
}
