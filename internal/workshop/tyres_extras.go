package workshop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
)

var (
	errTyreExtra            = fmt.Errorf("%w: an extra has a name of 1 to 100 characters, not used by another, charged per tyre or per set, with a price and a VAT rate", ErrInvalid)
	errTyreExtraCount       = fmt.Errorf("%w: an extra is added for from 1 to 100 tyres or sets", ErrInvalid)
	errStorageFeeNotAnExtra = fmt.Errorf("%w: a storage fee is charged for a period on its own page, not added as an extra", ErrInvalid)
	errTyreExtraRetired     = fmt.Errorf("%w: that extra is no longer offered", ErrInvalid)
)

// TyreExtra is one thing the shop sells with a tyre change, at its price now.
type TyreExtra struct {
	ID             string
	Name           string
	Per            string // "tyre" or "set"
	LineKind       string // "labour", "part" or "fee"
	UnitPriceMinor int64
	VATRateBasis   int
	Active         bool
	// "season" or "year" for a storage fee, which is charged by the period
	// and not added to a job like the others; empty otherwise.
	StoragePeriod string
}

// NewTyreExtra is what the shop types in to offer one.
type NewTyreExtra struct {
	Name           string
	Per            string
	LineKind       string
	UnitPriceMinor int64
	VATRateBasis   int
	StoragePeriod  string
}

func (n NewTyreExtra) valid() bool {
	name := strings.TrimSpace(n.Name)
	return utf8.RuneCountInString(name) >= 1 && utf8.RuneCountInString(name) <= 100 &&
		(n.Per == "tyre" || n.Per == "set") &&
		(n.LineKind == "labour" || n.LineKind == "part" || n.LineKind == "fee") &&
		(n.StoragePeriod == "" || n.StoragePeriod == "season" || n.StoragePeriod == "year") &&
		n.UnitPriceMinor >= 0 && n.UnitPriceMinor <= 100_000_000 && n.VATRateBasis >= 0 && n.VATRateBasis <= 10000
}

// CreateTyreExtra adds something to the shop's price list for a tyre change.
// Whoever runs the shop: it is the shop's prices.
func CreateTyreExtra(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, n NewTyreExtra) (string, error) {
	if !scope.Role.RunsTheShop() {
		return "", access.ErrForbidden
	}
	if !n.valid() {
		return "", errTyreExtra
	}
	var id string
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO tyre_extras (shop_id, name, per, line_kind, storage_period, created_by) VALUES ($1, $2, $3, $4, NULLIF($6, ''), NULLIF($5, '')::uuid)
			ON CONFLICT (shop_id, name) DO NOTHING RETURNING id`,
			scope.ShopID, strings.TrimSpace(n.Name), n.Per, n.LineKind, scope.UserID, n.StoragePeriod).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return errTyreExtra
		}
		if err != nil {
			return err
		}
		return extraEventTx(ctx, tx, scope, id, "priced", &n.UnitPriceMinor, &n.VATRateBasis)
	})
	return id, err
}

// PriceTyreExtra gives an extra a new price. Lines already on jobs keep the
// price they were added at. Whoever runs the shop.
func PriceTyreExtra(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string, priceMinor int64, vatBasis int) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return ErrNotFound
	}
	if priceMinor < 0 || priceMinor > 100_000_000 || vatBasis < 0 || vatBasis > 10000 {
		return errTyreExtra
	}
	return inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := extraExistsTx(ctx, tx, id); err != nil {
			return err
		}
		return extraEventTx(ctx, tx, scope, id, "priced", &priceMinor, &vatBasis)
	})
}

// SetTyreExtraInUse offers an extra or stops offering it. Whoever runs the shop.
func SetTyreExtraInUse(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string, use bool) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return ErrNotFound
	}
	kind := "retired"
	if use {
		kind = "reopened"
	}
	return inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := extraExistsTx(ctx, tx, id); err != nil {
			return err
		}
		return extraEventTx(ctx, tx, scope, id, kind, nil, nil)
	})
}

func extraExistsTx(ctx context.Context, tx pgx.Tx, id string) error {
	var one int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM tyre_extras WHERE id = $1`, id).Scan(&one); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else {
		return err
	}
}

func extraEventTx(ctx context.Context, tx pgx.Tx, scope access.Scope, id, kind string, price *int64, vat *int) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO tyre_extra_events (shop_id, extra_id, kind, unit_price_minor, vat_rate_basis, by_user)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, '')::uuid)`, scope.ShopID, id, kind, price, vat, scope.UserID)
	return err
}

// tyreExtrasSQL reads every extra with its newest price and whether it is
// offered: both come from the newest matching event.
const tyreExtrasSQL = `
	SELECT x.id::text, x.name, x.per, x.line_kind,
	       p.unit_price_minor, p.vat_rate_basis,
	       coalesce((SELECT e.kind FROM tyre_extra_events e WHERE e.extra_id = x.id AND e.kind IN ('retired', 'reopened') ORDER BY e.seq DESC LIMIT 1), 'reopened') = 'reopened',
	       coalesce(x.storage_period, '')
	FROM tyre_extras x
	JOIN LATERAL (SELECT unit_price_minor, vat_rate_basis FROM tyre_extra_events e WHERE e.extra_id = x.id AND e.kind = 'priced' ORDER BY e.seq DESC LIMIT 1) p ON true`

// TyreExtras lists the price list, those offered first. Pricing is a
// conversation with the customer, so the front desk's and the owner's.
func TyreExtras(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]TyreExtra, error) {
	if !scope.Role.SeesCustomerPersonalData() && !scope.Role.RunsTheShop() {
		return nil, access.ErrForbidden
	}
	var out []TyreExtra
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, tyreExtrasSQL+` ORDER BY 7 DESC, x.name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var x TyreExtra
			if err := rows.Scan(&x.ID, &x.Name, &x.Per, &x.LineKind, &x.UnitPriceMinor, &x.VATRateBasis, &x.Active, &x.StoragePeriod); err != nil {
				return err
			}
			out = append(out, x)
		}
		return rows.Err()
	})
	return out, err
}

// AddTyreExtraToJob puts an extra on a job as a line at today's price, for a
// number of tyres or sets. The line is the job's from then on: a later price
// change does not reach it. Added after the customer said yes it is left
// unapproved, as any line is.
func AddTyreExtraToJob(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID, extraID string, count int) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(jobID) || !looksLikeUUID(extraID) {
		return ErrNotFound
	}
	if count < 1 || count > 100 {
		return errTyreExtraCount
	}
	return inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var x TyreExtra
		err := tx.QueryRow(ctx, tyreExtrasSQL+` WHERE x.id = $1`, extraID).
			Scan(&x.ID, &x.Name, &x.Per, &x.LineKind, &x.UnitPriceMinor, &x.VATRateBasis, &x.Active, &x.StoragePeriod)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !x.Active {
			return errTyreExtraRetired
		}
		if x.StoragePeriod != "" {
			return errStorageFeeNotAnExtra
		}
		line := NewLine{
			Kind: x.LineKind, Description: x.Name, QuantityMilli: int64(count) * 1000,
			UnitPriceMinor: x.UnitPriceMinor, VATRateBasis: x.VATRateBasis,
		}
		if err := checkLine(&line); err != nil {
			return err
		}
		return addLineTx(ctx, tx, scope, jobID, line, lineSource{})
	})
}
