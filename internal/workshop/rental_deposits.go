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
)

// A rental's deposit (#196). The money -- received, given back, applied to
// a claim -- is the payment journal's: a deposit of purpose "rental"
// against the booking, recorded with RecordDepositPayment and given back
// with RefundPayment, which keep what is given back within what came in.
// What moves no money is kept here as events: what is asked and how it is
// to come, and a hold on the customer's card taken in the shop's own
// terminal, which is no money at all until the shop takes it. Nothing is
// processed: no card terminal or Swish is called, and a deposit is never
// income -- what the shop still holds is owed back to whoever paid it.
//
// It is applied only to a damage case of the same booking whose amount is
// settled, the customer told, and the customer either accepted it or let
// the time to object pass; a disputed case only once whoever runs the shop
// has set it against the deposit (#197). Never more than the case settled.

// RentalDepositMethods are the ways a rental's deposit is asked to come.
var RentalDepositMethods = []string{"card_hold", "cash", "card", "bank", "swish", "other"}

// DepositHold is a hold on the customer's card: no money moved.
type DepositHold struct {
	AmountMinor int64
	Reference   string
	ValidUntil  time.Time
	At          time.Time
}

// RentalDepositEvent is one thing done to a rental's deposit that moved
// no money.
type RentalDepositEvent struct {
	Kind        string
	AmountMinor *int64
	Method      string
	Reference   string
	ValidUntil  *time.Time
	Reason      string
	At          time.Time
	By          string
}

// RentalDeposit is a booking's deposit as the rental desk reads it. ID is
// empty when none has been asked for.
type RentalDeposit struct {
	ID          string
	BookingID   string
	DepositID   string
	PayerID     string
	PayerName   string
	BookingEnds time.Time
	// What was asked last, and how it is to come.
	RequiredMinor int64
	Method        string
	// The hold on the card standing now, if any.
	Hold    *DepositHold
	Events  []RentalDepositEvent
	Balance DepositBalance
}

// HoldEndsTooSoon reports whether the card's hold runs out before the
// rental is due back: the shop takes the deposit another way.
func (d RentalDeposit) HoldEndsTooSoon() bool {
	return d.Hold != nil && d.Hold.ValidUntil.Before(d.BookingEnds)
}

// OwedBack is what the shop holds of it still: received, less applied and
// given back. It is the customer's.
func (d RentalDeposit) OwedBack() int64 { return d.Balance.Held() }

// Short is what is asked and neither held on the card nor received.
func (d RentalDeposit) Short() int64 {
	have := d.Balance.Received - d.Balance.Refunded
	if d.Hold != nil {
		have += d.Hold.AmountMinor
	}
	if have >= d.RequiredMinor {
		return 0
	}
	return d.RequiredMinor - have
}

var (
	errDepositMethod   = fmt.Errorf("%w: say how much is asked, nothing or more, and how it is to come", ErrInvalid)
	errDepositNone     = fmt.Errorf("%w: no deposit has been asked for on this rental", ErrInvalid)
	errDepositPayer    = fmt.Errorf("%w: who pays the deposit is set when it is first asked for", ErrInvalid)
	errHoldDetails     = fmt.Errorf("%w: a hold needs its amount, the terminal's reference and until when it lasts, later than now", ErrInvalid)
	errHoldStanding    = fmt.Errorf("%w: a hold already stands; release it first", ErrInvalid)
	errNoHold          = fmt.Errorf("%w: there is no hold to release", ErrInvalid)
	errReleaseReason   = fmt.Errorf("%w: say why the hold is released, in 300 characters or fewer", ErrInvalid)
	errClaimObjection  = fmt.Errorf("%w: the customer has neither accepted the claim nor let the time to object pass", ErrInvalid)
	errClaimDisputed   = fmt.Errorf("%w: the customer disputes the claim; only once whoever runs the shop has set it against the deposit", ErrInvalid)
	errClaimShort      = fmt.Errorf("%w: that is more of the deposit than the damage case settled", ErrInvalid)
	errClaimNotSettled = fmt.Errorf("%w: a deposit is applied only to a claim whose amount is settled and the customer told", ErrInvalid)
)

// rentalDepositTx reads a booking's deposit, locked when lock. ID is empty
// when none was asked for.
func rentalDepositTx(ctx context.Context, tx pgx.Tx, bookingID string, lock bool) (RentalDeposit, error) {
	d := RentalDeposit{BookingID: bookingID}
	if lock {
		// Everything done to a booking's deposit, one at a time.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('rental_deposit:' || $1::text, 0))`, bookingID); err != nil {
			return d, fmt.Errorf("lock the deposit: %w", err)
		}
	}
	err := tx.QueryRow(ctx, `
		SELECT b.ends_at, coalesce(rd.id::text, ''), coalesce(rd.deposit_id::text, ''), coalesce(dp.party_id::text, ''), `+partyNameSQL+`
		FROM rental_bookings b
		LEFT JOIN rental_deposits rd ON rd.booking_id = b.id
		LEFT JOIN deposits dp ON dp.id = rd.deposit_id
		LEFT JOIN parties pa ON pa.id = dp.party_id
		LEFT JOIN customers c ON c.id = pa.customer_id
		LEFT JOIN people cp ON cp.id = c.person_id LEFT JOIN people pp ON pp.id = pa.person_id
		WHERE b.id = $1`, bookingID).Scan(&d.BookingEnds, &d.ID, &d.DepositID, &d.PayerID, &d.PayerName)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, fmt.Errorf("read the deposit: %w", err)
	}
	if d.ID == "" {
		return d, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT e.kind, e.amount_minor, coalesce(e.method, ''), coalesce(e.reference, ''), e.valid_until, coalesce(e.reason, ''),
		       e.at, coalesce(p.display_name, '')
		FROM rental_deposit_events e LEFT JOIN users u ON u.id = e.by_user LEFT JOIN people p ON p.id = u.person_id
		WHERE e.rental_deposit_id = $1 ORDER BY e.seq`, d.ID)
	if err != nil {
		return d, fmt.Errorf("read the deposit's events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e RentalDepositEvent
		if err := rows.Scan(&e.Kind, &e.AmountMinor, &e.Method, &e.Reference, &e.ValidUntil, &e.Reason, &e.At, &e.By); err != nil {
			return d, err
		}
		d.Events = append(d.Events, e)
		switch e.Kind {
		case "required":
			d.RequiredMinor, d.Method = *e.AmountMinor, e.Method
		case "held":
			d.Hold = &DepositHold{AmountMinor: *e.AmountMinor, Reference: e.Reference, ValidUntil: *e.ValidUntil, At: e.At}
		case "hold_released":
			d.Hold = nil
		}
	}
	if err := rows.Err(); err != nil {
		return d, err
	}
	d.Balance, err = depositTx(ctx, tx, d.DepositID, lock)
	return d, err
}

// RequireRentalDeposit asks a deposit of the rental: how much, and how it
// is to come. The first time it opens the deposit in the journal, paid by
// payerID -- the renter when empty, or a company paying for them; asked
// again, the amount or the way changes and the old ask stays as it was.
// For the rental desk and the front desk.
func RequireRentalDeposit(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, bookingID, payerID string, amountMinor int64, method string) error {
	if !scope.Role.HandsOverRentals() && !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(bookingID) || (payerID != "" && !looksLikeUUID(payerID)) {
		return ErrNotFound
	}
	if amountMinor < 0 || amountMinor > 10_000_000_000 || !slices.Contains(RentalDepositMethods, method) {
		return errDepositMethod
	}
	return inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		d, err := rentalDepositTx(ctx, tx, bookingID, true)
		if err != nil {
			return err
		}
		if d.ID == "" {
			if payerID == "" {
				if err := tx.QueryRow(ctx, `SELECT renter_party_id::text FROM rental_bookings WHERE id = $1`, bookingID).Scan(&payerID); err != nil {
					return fmt.Errorf("read the renter: %w", err)
				}
			}
			if err := partySeenTx(ctx, tx, scope, payerID); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `
				INSERT INTO deposits (shop_id, party_id, purpose, subject_kind, subject_id, created_by)
				VALUES ($1, party_now($2), 'rental', 'rental_booking', $3, $4) RETURNING id::text`,
				scope.ShopID, payerID, bookingID, scope.UserID).Scan(&d.DepositID); err != nil {
				return fmt.Errorf("open the deposit: %w", err)
			}
			if err := tx.QueryRow(ctx, `
				INSERT INTO rental_deposits (shop_id, booking_id, deposit_id, created_by) VALUES ($1, $2, $3, NULLIF($4, '')::uuid) RETURNING id::text`,
				scope.ShopID, bookingID, d.DepositID, scope.UserID).Scan(&d.ID); err != nil {
				return fmt.Errorf("open the rental's deposit: %w", err)
			}
		} else if payerID != "" && !strings.EqualFold(payerID, d.PayerID) {
			var same bool
			if err := tx.QueryRow(ctx, `SELECT party_now($1) = $2::uuid`, payerID, d.PayerID).Scan(&same); err != nil {
				return fmt.Errorf("read the payer: %w", err)
			}
			if !same {
				return errDepositPayer
			}
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO rental_deposit_events (shop_id, rental_deposit_id, kind, amount_minor, method, by_user)
			VALUES ($1, $2, 'required', $3, $4, NULLIF($5, '')::uuid)`, scope.ShopID, d.ID, amountMinor, method, scope.UserID)
		return err
	})
}

// HoldRentalDeposit records a hold taken on the customer's card in the
// shop's own terminal: its amount, the terminal's reference and until when
// the card issuer keeps it. No money moved, so nothing in the journal. A
// hold that runs out before the rental is due back is recorded, and the
// booking's page says so: the shop takes the deposit another way.
func HoldRentalDeposit(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, bookingID string, amountMinor int64, reference string, validUntil time.Time) error {
	if !scope.Role.HandsOverRentals() && !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(bookingID) {
		return ErrNotFound
	}
	reference = strings.TrimSpace(reference)
	if amountMinor <= 0 || amountMinor > 10_000_000_000 || reference == "" || utf8.RuneCountInString(reference) > 100 || !validUntil.After(time.Now()) {
		return errHoldDetails
	}
	return inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		d, err := rentalDepositTx(ctx, tx, bookingID, true)
		switch {
		case err != nil:
			return err
		case d.ID == "":
			return errDepositNone
		case d.Hold != nil:
			return errHoldStanding
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO rental_deposit_events (shop_id, rental_deposit_id, kind, amount_minor, reference, valid_until, by_user)
			VALUES ($1, $2, 'held', $3, $4, $5, NULLIF($6, '')::uuid)`, scope.ShopID, d.ID, amountMinor, reference, validUntil, scope.UserID)
		return err
	})
}

// ReleaseRentalDepositHold records the card's hold let go, and why.
// Whatever is switched off: a hold on a customer's card is let go after
// the module that took it is gone.
func ReleaseRentalDepositHold(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, bookingID, reason string) error {
	if !scope.Role.HandsOverRentals() && !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(bookingID) {
		return ErrNotFound
	}
	reason = strings.TrimSpace(reason)
	if reason == "" || utf8.RuneCountInString(reason) > 300 {
		return errReleaseReason
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		d, err := rentalDepositTx(ctx, tx, bookingID, true)
		switch {
		case err != nil:
			return err
		case d.Hold == nil:
			return errNoHold
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO rental_deposit_events (shop_id, rental_deposit_id, kind, reason, by_user)
			VALUES ($1, $2, 'hold_released', $3, NULLIF($4, '')::uuid)`, scope.ShopID, d.ID, reason, scope.UserID)
		return err
	})
}

// RentalDepositOf reads a booking's deposit: what was asked, the hold, and
// the money from the journal. Reading is Core's.
func RentalDepositOf(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, bookingID string) (RentalDeposit, error) {
	if !scope.Role.HandsOverRentals() && !scope.Role.SeesCustomerPersonalData() {
		return RentalDeposit{}, access.ErrForbidden
	}
	if !looksLikeUUID(bookingID) {
		return RentalDeposit{}, ErrNotFound
	}
	var d RentalDeposit
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d, err = rentalDepositTx(ctx, tx, bookingID, false)
		return err
	})
	return d, err
}

// claimOpenTx refuses a damage case the deposit may not settle yet, and
// returns what of its settled amount is not yet taken from the deposit.
func claimOpenTx(ctx context.Context, tx pgx.Tx, c DamageCaseDetail, now time.Time) (int64, error) {
	settled := c.Settled()
	switch {
	case !c.Chargeable():
		return 0, errClaimNotSettled
	case c.Disputed():
		if lastOutcome(c) != "set_against_deposit" {
			return 0, errClaimDisputed
		}
	case c.Accepted():
	case c.RespondBy() != nil && now.After(c.RespondBy().AddDate(0, 0, 1)):
		// The whole last day to object has passed.
	default:
		return 0, errClaimObjection
	}
	var taken int64
	if err := tx.QueryRow(ctx, `
		SELECT coalesce(sum(-p.amount_minor), 0)
		FROM rental_deposit_applications a JOIN invoice_payments p ON p.id = a.deposit_payment_id
		WHERE a.damage_case_id = $1 AND NOT EXISTS (SELECT 1 FROM invoice_payments r WHERE r.reverses_id = p.id)`, c.ID).Scan(&taken); err != nil {
		return 0, fmt.Errorf("read what the case has taken: %w", err)
	}
	return *settled - taken, nil
}

// lastOutcome is the case's outcome since it was last reopened, if any.
func lastOutcome(c DamageCaseDetail) string {
	out := ""
	for _, s := range c.Steps {
		switch s.Step {
		case "outcome":
			out = s.Outcome
		case "reopened":
			out = ""
		}
	}
	return out
}

// ApplyRentalDeposit takes amountMinor of the rental's deposit for a damage
// case of the same booking, onto the invoice that charges it: no money
// moves, two rows in the journal (ApplyDeposit). Refused unless the case's
// amount is settled and the customer told, and they accepted or let the
// time to object pass -- a disputed case only once whoever runs the shop
// set it against the deposit -- and never beyond what the case settled.
// What is left of the deposit is the customer's, given back; a claim
// larger than the deposit is the invoice's to collect.
func ApplyRentalDeposit(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, bookingID, caseID, invoiceID string, amountMinor int64, key string) error {
	if !scope.Role.HandsOverRentals() && !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(bookingID) || !looksLikeUUID(caseID) || !looksLikeUUID(invoiceID) {
		return ErrNotFound
	}
	if amountMinor <= 0 {
		return fmt.Errorf("%w: say how much, above nothing", ErrInvalid)
	}
	key = strings.TrimSpace(key)
	return inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		d, err := rentalDepositTx(ctx, tx, bookingID, true)
		switch {
		case err != nil:
			return err
		case d.ID == "":
			return errDepositNone
		}
		c, err := damageCaseTx(ctx, tx, caseID, true)
		if err != nil {
			return err
		}
		if !strings.EqualFold(c.BookingID, bookingID) {
			return ErrNotFound
		}
		left, err := claimOpenTx(ctx, tx, c, time.Now())
		if err != nil {
			return err
		}
		// The same request again finds what it applied, below; the limit
		// is for a new one.
		if amountMinor > left {
			if key == "" {
				return errClaimShort
			}
			if found, err := replayedApplicationTx(ctx, tx, key, d.DepositID, invoiceID, amountMinor); err != nil || !found {
				if err == nil {
					err = errClaimShort
				}
				return err
			}
			return nil
		}
		applied, err := applyDepositTx(ctx, tx, scope, d.DepositID, invoiceID, amountMinor, key)
		if err != nil || applied == "" {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO rental_deposit_applications (shop_id, deposit_payment_id, damage_case_id) VALUES ($1, $2, $3)`,
			scope.ShopID, applied, caseID)
		return err
	})
}
