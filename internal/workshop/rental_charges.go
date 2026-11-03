package workshop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/money"
)

// What a renter owes after a return (#195): lines a person confirms, each
// with the VAT treatment its kind has in the shop's settings, frozen as it
// was confirmed. A suggestion from the return (ProposedCharge) is no line
// until then. Damage compensation comes only from a damage case of the
// booking that is settled and told (#197), at what was settled. Whether a
// kind is taxed or outside the scope of VAT is the accountant's (gate G2,
// #221): until the shop marks a setting confirmed it is shown as not.

// Return charge kinds.
const (
	ChargeCleaning        = "cleaning"
	ChargeDamage          = "damage"
	ChargeExcess          = "excess"
	ChargeCongestionTax   = "congestion_tax"
	ChargeParkingTolls    = "parking_tolls"
	ChargeAdminFee        = "admin_fee"
	ChargeCancellationFee = "cancellation_fee"
	ChargeNoShowFee       = "no_show_fee"
)

// ChargeKinds are every kind of return charge, in the order a page offers
// them.
var ChargeKinds = []string{ChargeLate, ChargeExtraKm, ChargeFuel, ChargeCleaning, ChargeDamage, ChargeExcess,
	ChargeCongestionTax, ChargeParkingTolls, ChargeAdminFee, ChargeCancellationFee, ChargeNoShowFee}

// ChargeClass is how a kind of charge is taxed: a rate with category S, or
// none with O, outside the scope of VAT. ID is empty for the built-in
// default, which is never confirmed.
type ChargeClass struct {
	ID          string
	Kind        string
	VATRateBp   int
	VATCategory string
	Confirmed   bool
}

// defaultChargeClass is a kind's treatment before the shop says: genuine
// damages and the excess outside the scope of VAT, everything else at the
// standard rate. Never confirmed.
func defaultChargeClass(kind string) ChargeClass {
	if kind == ChargeDamage || kind == ChargeExcess {
		return ChargeClass{Kind: kind, VATCategory: "O"}
	}
	return ChargeClass{Kind: kind, VATRateBp: 2500, VATCategory: "S"}
}

// RentalCharge is a confirmed charge as the rental desk reads it.
type RentalCharge struct {
	ID             string
	BookingID      string
	Kind           string
	Description    string
	QuantityMilli  int64
	UnitPriceMinor *int64
	GrossGiven     *int64
	VATRateBp      int
	VATCategory    string
	ClassConfirmed bool
	DamageCaseID   string
	Proposed       bool
	ConfirmedAt    time.Time
	ConfirmedBy    string
	// When the customer was told, and how; why it was withdrawn.
	InformedAt *time.Time
	Channel    string
	Withdrawn  string
}

// Net, VAT and Gross are the line's amounts by internal/money's rule: per
// line, rounded half away from zero. A charge given with its VAT inside
// keeps exactly that gross.
func (c RentalCharge) Net() int64 {
	if c.GrossGiven != nil {
		return *c.GrossGiven - c.VAT()
	}
	return money.LineNet(c.QuantityMilli, *c.UnitPriceMinor)
}

// VAT is the line's VAT.
func (c RentalCharge) VAT() int64 {
	if c.GrossGiven != nil {
		return money.VATInGross(*c.GrossGiven, c.VATRateBp)
	}
	return money.VAT(c.Net(), c.VATRateBp)
}

// Gross is the line with its VAT.
func (c RentalCharge) Gross() int64 { return c.Net() + c.VAT() }

// Stands reports whether the charge is still owed: not withdrawn.
func (c RentalCharge) Stands() bool { return c.Withdrawn == "" }

// NewCharge is a charge to confirm. A damage charge names its case and
// takes its amount from it; any other gives its quantity and net price.
type NewCharge struct {
	Kind           string
	Description    string
	QuantityMilli  int64
	UnitPriceMinor int64
	DamageCaseID   string
	// Confirmed from what the return suggested, rather than entered.
	Proposed bool
	Key      string
}

var (
	errChargeKind      = fmt.Errorf("%w: that is not a kind of charge", ErrInvalid)
	errChargeLine      = fmt.Errorf("%w: a charge says what it is, of more than nothing, at a price of nothing or more", ErrInvalid)
	errChargeWhen      = fmt.Errorf("%w: return charges come after the return, a cancellation fee after a cancellation and a no-show fee after a no-show", ErrInvalid)
	errChargeDamage    = fmt.Errorf("%w: damage is charged from a damage case of this rental, settled and told, once", ErrInvalid)
	errChargeClass     = fmt.Errorf("%w: a charge is taxed at a rate above nothing, category S, or outside the scope of VAT, category O at none", ErrInvalid)
	errChargeTold      = fmt.Errorf("%w: the customer has been told of that charge already", ErrInvalid)
	errChargeWithdrawn = fmt.Errorf("%w: that charge has been withdrawn", ErrInvalid)
	errWithdrawReason  = fmt.Errorf("%w: say why the charge is withdrawn, in 300 characters or fewer", ErrInvalid)
	errChargeChannel   = fmt.Errorf("%w: say how the customer was told", ErrInvalid)
)

// chargeClassTx is a kind's treatment now: the shop's newest setting, or
// the default.
func chargeClassTx(ctx context.Context, tx pgx.Tx, kind string) (ChargeClass, error) {
	c := ChargeClass{Kind: kind}
	err := tx.QueryRow(ctx, `SELECT id::text, vat_rate_bp, vat_category, confirmed FROM rental_charge_classes
		WHERE kind = $1 ORDER BY seq DESC LIMIT 1`, kind).Scan(&c.ID, &c.VATRateBp, &c.VATCategory, &c.Confirmed)
	if errors.Is(err, pgx.ErrNoRows) {
		return defaultChargeClass(kind), nil
	}
	return c, err
}

// ChargeClasses is how each kind of charge is taxed now. The rental desk
// and whoever runs the shop; reading is Core's.
func ChargeClasses(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]ChargeClass, error) {
	if !scope.Role.HandsOverRentals() {
		return nil, access.ErrForbidden
	}
	var out []ChargeClass
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		for _, k := range ChargeKinds {
			c, err := chargeClassTx(ctx, tx, k)
			if err != nil {
				return fmt.Errorf("read how %s is taxed: %w", k, err)
			}
			out = append(out, c)
		}
		return nil
	})
	return out, err
}

// SetChargeClass says how a kind of charge is taxed from now on, and
// whether the shop's adviser has confirmed it. Whoever runs the shop.
// Charges already confirmed keep the treatment they were given.
func SetChargeClass(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, c ChargeClass) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	switch {
	case !slices.Contains(ChargeKinds, c.Kind):
		return errChargeKind
	case c.VATCategory == "S" && (c.VATRateBp <= 0 || c.VATRateBp > 10000), c.VATCategory == "O" && c.VATRateBp != 0,
		c.VATCategory != "S" && c.VATCategory != "O":
		return errChargeClass
	}
	return inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO rental_charge_classes (shop_id, kind, vat_rate_bp, vat_category, confirmed, created_by)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, '')::uuid)`, scope.ShopID, c.Kind, c.VATRateBp, c.VATCategory, c.Confirmed, scope.UserID)
		return err
	})
}

// ConfirmCharge makes a charge of the booking a line it owes, frozen with
// the treatment its kind has now. For the rental desk and whoever runs the
// shop. The same key again finds the charge it made.
func ConfirmCharge(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, bookingID string, n NewCharge) (string, error) {
	if !scope.Role.HandsOverRentals() {
		return "", access.ErrForbidden
	}
	if !looksLikeUUID(bookingID) || (n.DamageCaseID != "" && !looksLikeUUID(n.DamageCaseID)) {
		return "", ErrNotFound
	}
	n.Description, n.Key = strings.TrimSpace(n.Description), strings.TrimSpace(n.Key)
	if !slices.Contains(ChargeKinds, n.Kind) {
		return "", errChargeKind
	}
	if (n.Kind == ChargeDamage) != (n.DamageCaseID != "") {
		return "", errChargeDamage
	}
	if n.Kind != ChargeDamage && (n.Description == "" || utf8.RuneCountInString(n.Description) > 200 || n.QuantityMilli <= 0 ||
		n.QuantityMilli > 100_000_000 || n.UnitPriceMinor < 0 || n.UnitPriceMinor > 10_000_000_000) {
		return "", errChargeLine
	}
	if len(n.Key) > 100 {
		return "", errChargeLine
	}
	var id string
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('rental_charges:' || $1::text, 0))`, bookingID); err != nil {
			return fmt.Errorf("lock the charges: %w", err)
		}
		if n.Key != "" {
			err := tx.QueryRow(ctx, `SELECT id::text FROM rental_charges WHERE request_key = $1 AND booking_id = $2`, n.Key, bookingID).Scan(&id)
			if err == nil {
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("read the request: %w", err)
			}
		}
		var state string
		err := tx.QueryRow(ctx, `SELECT `+rentalState+` FROM rental_bookings b JOIN resource_blocks k ON k.id = b.block_id WHERE b.id = $1`,
			bookingID).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read the booking: %w", err)
		}
		switch n.Kind {
		case ChargeCancellationFee:
			if state != RentalCancelled {
				return errChargeWhen
			}
		case ChargeNoShowFee:
			if state != RentalNoShow {
				return errChargeWhen
			}
		default:
			if state != RentalReturned {
				return errChargeWhen
			}
		}
		class, err := chargeClassTx(ctx, tx, n.Kind)
		if err != nil {
			return fmt.Errorf("read how it is taxed: %w", err)
		}
		var unit, gross *int64
		qty := n.QuantityMilli
		if n.Kind == ChargeDamage {
			c, err := damageCaseTx(ctx, tx, n.DamageCaseID, true)
			if err != nil {
				return err
			}
			var standing bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM rental_charges r WHERE r.damage_case_id = $1
				               AND NOT EXISTS (SELECT 1 FROM rental_charge_events e WHERE e.charge_id = r.id AND e.kind = 'withdrawn'))`,
				n.DamageCaseID).Scan(&standing); err != nil {
				return fmt.Errorf("read the case's charges: %w", err)
			}
			// Written off, claimed from insurance or not the renter's: not
			// owed. Disputed: only once a person decided it is owed anyway.
			out := lastOutcome(c)
			owed := out == "" || out == "invoiced" || out == "set_against_deposit"
			if !strings.EqualFold(c.BookingID, bookingID) || !c.Chargeable() || standing || !owed || (c.Disputed() && out == "") {
				return errChargeDamage
			}
			if n.Description == "" {
				n.Description = c.Description
			}
			if utf8.RuneCountInString(n.Description) > 200 {
				return errChargeLine
			}
			gross, qty = c.Settled(), 1000
		} else {
			unit = &n.UnitPriceMinor
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO rental_charges (shop_id, booking_id, kind, description, quantity_milli, unit_price_minor, gross_minor,
			                            vat_rate_bp, vat_category, class_id, class_confirmed, damage_case_id, proposed, request_key, confirmed_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULLIF($10, '')::uuid, $11, NULLIF($12, '')::uuid, $13, nullif($14, ''), NULLIF($15, '')::uuid)
			RETURNING id::text`,
			scope.ShopID, bookingID, n.Kind, n.Description, qty, unit, gross, class.VATRateBp, class.VATCategory, class.ID, class.Confirmed,
			n.DamageCaseID, n.Proposed, n.Key, scope.UserID).Scan(&id)
		return err
	})
	return id, err
}

// chargeEvent records that the customer was told of a charge, or that it
// was withdrawn. Whatever is switched off: a charge is withdrawn, or the
// customer told of it, after the module is gone.
func chargeEvent(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, chargeID, kind, channel, reason string) error {
	if !looksLikeUUID(chargeID) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var booking string
		if err := tx.QueryRow(ctx, `SELECT booking_id::text FROM rental_charges WHERE id = $1`, chargeID).Scan(&booking); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("read the charge: %w", err)
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('rental_charges:' || $1::text, 0))`, booking); err != nil {
			return fmt.Errorf("lock the charges: %w", err)
		}
		var told, withdrawn bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM rental_charge_events WHERE charge_id = $1 AND kind = 'informed'),
			EXISTS (SELECT 1 FROM rental_charge_events WHERE charge_id = $1 AND kind = 'withdrawn')`, chargeID).Scan(&told, &withdrawn); err != nil {
			return fmt.Errorf("read the charge's events: %w", err)
		}
		switch {
		case withdrawn:
			return errChargeWithdrawn
		case kind == "informed" && told:
			return errChargeTold
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO rental_charge_events (shop_id, charge_id, kind, channel, reason, by_user)
			VALUES ($1, $2, $3, nullif($4, ''), nullif($5, ''), NULLIF($6, '')::uuid)`, scope.ShopID, chargeID, kind, channel, reason, scope.UserID)
		return err
	})
}

// InformCharge records that the customer was told of a charge, and how.
// A charge is invoiced only once they have been (#199).
func InformCharge(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, chargeID, channel string) error {
	if !scope.Role.HandsOverRentals() {
		return access.ErrForbidden
	}
	if !slices.Contains(DamageChannels, channel) {
		return errChargeChannel
	}
	return chargeEvent(ctx, pool, scope, chargeID, "informed", channel, "")
}

// WithdrawCharge withdraws a charge, with why: disputed and given up, or
// wrong. It stays, marked; a corrected one is a new charge.
func WithdrawCharge(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, chargeID, reason string) error {
	if !scope.Role.HandsOverRentals() {
		return access.ErrForbidden
	}
	reason = strings.TrimSpace(reason)
	if reason == "" || utf8.RuneCountInString(reason) > 300 {
		return errWithdrawReason
	}
	return chargeEvent(ctx, pool, scope, chargeID, "withdrawn", "", reason)
}

// rentalChargesTx reads a booking's charges, oldest first.
func rentalChargesTx(ctx context.Context, tx pgx.Tx, bookingID string) ([]RentalCharge, error) {
	rows, err := tx.Query(ctx, `
		SELECT r.id::text, r.booking_id::text, r.kind, r.description, r.quantity_milli, r.unit_price_minor, r.gross_minor,
		       r.vat_rate_bp, r.vat_category, r.class_confirmed, coalesce(r.damage_case_id::text, ''), r.proposed, r.confirmed_at,
		       coalesce(p.display_name, ''),
		       (SELECT e.at FROM rental_charge_events e WHERE e.charge_id = r.id AND e.kind = 'informed'),
		       coalesce((SELECT e.channel FROM rental_charge_events e WHERE e.charge_id = r.id AND e.kind = 'informed'), ''),
		       coalesce((SELECT e.reason FROM rental_charge_events e WHERE e.charge_id = r.id AND e.kind = 'withdrawn'), '')
		FROM rental_charges r LEFT JOIN users u ON u.id = r.confirmed_by LEFT JOIN people p ON p.id = u.person_id
		WHERE r.booking_id = $1 ORDER BY r.confirmed_at, r.id`, bookingID)
	if err != nil {
		return nil, fmt.Errorf("read the charges: %w", err)
	}
	defer rows.Close()
	var out []RentalCharge
	for rows.Next() {
		var c RentalCharge
		if err := rows.Scan(&c.ID, &c.BookingID, &c.Kind, &c.Description, &c.QuantityMilli, &c.UnitPriceMinor, &c.GrossGiven,
			&c.VATRateBp, &c.VATCategory, &c.ClassConfirmed, &c.DamageCaseID, &c.Proposed, &c.ConfirmedAt, &c.ConfirmedBy,
			&c.InformedAt, &c.Channel, &c.Withdrawn); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RentalCharges reads a booking's confirmed charges, withdrawn ones
// included. The rental desk and whoever runs the shop; reading is Core's.
func RentalCharges(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, bookingID string) ([]RentalCharge, error) {
	if !scope.Role.HandsOverRentals() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(bookingID) {
		return nil, ErrNotFound
	}
	var out []RentalCharge
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = rentalChargesTx(ctx, tx, bookingID)
		return err
	})
	return out, err
}
