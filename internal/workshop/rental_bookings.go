package workshop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/money"
)

// Rental bookings (#192). A booking is a unit for a renter for a period, with
// the profile version and price lines it was agreed at frozen beside it.
// Its time on the unit, cleaning buffer included, is a block in the
// availability ledger written in the same transaction: the database refuses
// two rentals of a unit at once and a rental over a job in the workshop,
// whatever the code in front of it does. What happens to a booking is a row;
// its state is the last of them.

// A booking's states.
const (
	RentalHeld        = "held"
	RentalHoldExpired = "hold_expired"
	RentalConfirmed   = "confirmed"
	RentalCheckedOut  = "checked_out"
	RentalReturned    = "returned"
	RentalCancelled   = "cancelled"
	RentalNoShow      = "no_show"
)

// rentalState is the state its events say, as SQL over a booking b and its
// block k: the last event that is a state, a hold run out read as such.
const rentalState = `
	(SELECT CASE WHEN e.kind = 'held' AND (k.status = 'expired' OR (k.status = 'held' AND k.expires_at <= clock_timestamp()))
	             THEN 'hold_expired' ELSE e.kind END
	   FROM rental_booking_events e
	  WHERE e.booking_id = b.id AND e.kind IN ('held', 'confirmed', 'checked_out', 'returned', 'cancelled', 'no_show')
	  ORDER BY e.seq DESC LIMIT 1)`

// RentalBooking is a booking as the rental desk reads it.
type RentalBooking struct {
	ID         string
	Number     int
	UnitID     string
	UnitLabel  string
	ResourceID string
	RenterID   string
	RenterName string
	Starts     time.Time
	Ends       time.Time
	State      string
	HeldUntil  *time.Time
	BlockID    string
	ProfileID  string
	PlanID     string
	IncludedKm *int
	ExtraKm    *int64
	Lines      []RentalBookingLine
	Events     []RentalBookingEvent
	Totals     money.Totals
	// The unit is out with somebody else past its return: this booking
	// may not get it in time.
	AtRisk bool
}

// Overdue reports whether the unit is out past the booking's end.
func (b RentalBooking) Overdue(now time.Time) bool {
	return b.State == RentalCheckedOut && now.After(b.Ends)
}

// Open reports whether the booking still holds its unit ahead: held,
// confirmed or out.
func (b RentalBooking) Open() bool {
	return b.State == RentalHeld || b.State == RentalConfirmed || b.State == RentalCheckedOut
}

// Reference is what a person calls it.
func (b RentalBooking) Reference() string { return fmt.Sprintf("R-%d", b.Number) }

// RentalBookingLine is a frozen price line.
type RentalBookingLine struct {
	Position       int
	Kind           string
	Unit           string
	Description    string
	QuantityMilli  int64
	UnitPriceMinor int64
	VATRateBp      int
	VATCategory    string
	Unconfirmed    bool
	PlanID         string
	SeasonID       string
	ExtraID        string
	// A price a person changed: what it was priced at, why and by whom.
	PricedMinor    *int64
	OverrideReason string
	OverrideBy     string
}

// Net is the line's amount before VAT.
func (l RentalBookingLine) Net() int64 { return money.LineNet(l.QuantityMilli, l.UnitPriceMinor) }

// RentalBookingEvent is one thing that happened to a booking.
type RentalBookingEvent struct {
	Kind      string
	At        time.Time
	By        string
	Reason    string
	OldStarts *time.Time
	OldEnds   *time.Time
	NewStarts *time.Time
	NewEnds   *time.Time
}

// PriceOverride is a person's own price for a quoted line, by its position
// (from 1), with why.
type PriceOverride struct {
	Position       int
	UnitPriceMinor int64
	// The price including VAT, as a person agreed it: given, it is the
	// override, made net at the line's own VAT rate.
	UnitGrossMinor *int64
	Reason         string
}

// NewRentalBooking is a rental to book. HoldUntil, set, makes it a hold --
// waiting for a deposit or the customer's answer -- that runs out then
// unless confirmed. Key is the request's own: sent again, from a phone that
// lost its answer, it finds the booking it made.
type NewRentalBooking struct {
	UnitID    string
	RenterID  string
	From, To  time.Time
	PlanID    string
	Extras    []ExtraChoice
	HoldUntil *time.Time
	Key       string
	Overrides []PriceOverride
}

var (
	errBookingTimes     = fmt.Errorf("%w: a rental ends after it starts", ErrInvalid)
	errBookingHold      = fmt.Errorf("%w: a hold runs out later than now and before the rental starts", ErrInvalid)
	errBookingFleet     = fmt.Errorf("%w: the unit is not in the fleet for all of that time", ErrInvalid)
	errBookingOverdue   = fmt.Errorf("%w: the unit is out past its return on another rental; book it once it is back", ErrInvalid)
	errNotARenter       = fmt.Errorf("%w: that party is not a renter", ErrInvalid)
	errBookingKey       = fmt.Errorf("%w: the request key is too long", ErrInvalid)
	errBookingKeyOther  = fmt.Errorf("%w: that request was already used for another booking", ErrInvalid)
	errOverrideWhy      = fmt.Errorf("%w: say why the price is changed, in 300 characters or fewer", ErrInvalid)
	errOverrideLine     = fmt.Errorf("%w: there is no such line to change the price of", ErrInvalid)
	errBookingState     = fmt.Errorf("%w: the booking is not in a state that allows that", ErrInvalid)
	errHoldRanOut       = fmt.Errorf("%w: the hold has run out; book it again", ErrInvalid)
	errMoveLength       = fmt.Errorf("%w: a move keeps the rental as long as it was; extend it to make it longer", ErrInvalid)
	errExtendLater      = fmt.Errorf("%w: an extension ends later than the rental does now", ErrInvalid)
	errCancelWhy        = fmt.Errorf("%w: say why, in 300 characters or fewer", ErrInvalid)
	errNoShowBeforeTime = fmt.Errorf("%w: a renter is a no-show only once the rental was due to start", ErrInvalid)
)

func checkRentalKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if len(key) > 100 {
		return "", errBookingKey
	}
	return key, nil
}

// BookRental books a unit for a renter: priced and frozen, its block in the
// ledger, in one transaction. Refused, naming what is in the way, when the
// unit is taken then -- by another rental, a job in the workshop, a service
// -- which only an explicit replanning by whoever runs the shop moves
// (OverrideBlock). The rental desk and whoever runs the shop.
func BookRental(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, n NewRentalBooking) (string, error) {
	if !scope.Role.HandsOverRentals() {
		return "", access.ErrForbidden
	}
	if !looksLikeUUID(n.UnitID) || !looksLikeUUID(n.RenterID) || (n.PlanID != "" && !looksLikeUUID(n.PlanID)) {
		return "", ErrNotFound
	}
	switch {
	case n.From.IsZero() || !n.To.After(n.From):
		return "", errBookingTimes
	case n.HoldUntil != nil && (!n.HoldUntil.After(time.Now()) || n.HoldUntil.After(n.From)):
		return "", errBookingHold
	}
	key, err := checkRentalKey(n.Key)
	if err != nil {
		return "", err
	}
	for i, o := range n.Overrides {
		if o.UnitPriceMinor < 0 || (o.UnitGrossMinor != nil && *o.UnitGrossMinor < 0) {
			return "", errExtraPrice
		}
		if n.Overrides[i].Reason, err = checkReason(o.Reason, errOverrideWhy); err != nil {
			return "", err
		}
	}
	var id string
	err = inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if key != "" {
			found, err := rentalByKeyTx(ctx, tx, key, n.UnitID)
			if found != "" || err != nil {
				id = found
				return err
			}
		}
		// In a savepoint: the same request, sent again while the first is
		// in flight, waits on the first's block and is refused by it, or
		// by the key. Either way the first has committed by then, and its
		// booking is the answer.
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		id, err = bookRentalTx(ctx, sp, scope, n, key)
		if err == nil {
			return sp.Commit(ctx)
		}
		if rerr := sp.Rollback(ctx); rerr != nil {
			return rerr
		}
		if key != "" {
			if found, ferr := rentalByKeyTx(ctx, tx, key, n.UnitID); found != "" || ferr != nil {
				id = found
				return ferr
			}
		}
		return err
	})
	return id, err
}

// rentalByKeyTx finds the booking a request key made, refusing a key used
// for another unit.
func rentalByKeyTx(ctx context.Context, tx pgx.Tx, key, unitID string) (string, error) {
	var id, unit string
	err := tx.QueryRow(ctx, `SELECT id::text, unit_id::text FROM rental_bookings WHERE request_key = $1`, key).Scan(&id, &unit)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the request: %w", err)
	}
	if !strings.EqualFold(unit, unitID) {
		return "", errBookingKeyOther
	}
	return id, nil
}

// rentalUnitForTx reads what a booking needs of its unit.
type rentalUnitFacts struct {
	resource       string
	profileVersion string
	buffer         time.Duration
}

func rentalUnitForTx(ctx context.Context, tx pgx.Tx, unitID string, from, to time.Time) (rentalUnitFacts, error) {
	var u rentalUnitFacts
	err := tx.QueryRow(ctx, `
		SELECT u.resource_id, v.id, v.cleaning_buffer
		FROM rental_units u
		JOIN LATERAL (SELECT id, cleaning_buffer FROM rental_profile_versions WHERE profile_id = u.profile_id ORDER BY version DESC LIMIT 1) v ON true
		WHERE u.id = $1`, unitID).Scan(&u.resource, &u.profileVersion, &u.buffer)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, ErrNotFound
	}
	if err != nil {
		return u, fmt.Errorf("read unit: %w", err)
	}
	if err := inFleetTx(ctx, tx, unitID, from, to); err != nil {
		return u, err
	}
	return u, nil
}

// inFleetTx refuses a period the unit is not in the fleet for, all of it.
func inFleetTx(ctx context.Context, tx pgx.Tx, unitID string, from, to time.Time) error {
	var in bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM fleet_memberships WHERE unit_id = $1 AND tstzrange(joined_at, left_at) @> tstzrange($2, $3))`,
		unitID, from, to).Scan(&in); err != nil {
		return fmt.Errorf("read the fleet: %w", err)
	}
	if !in {
		return errBookingFleet
	}
	return nil
}

func bookRentalTx(ctx context.Context, tx pgx.Tx, scope access.Scope, n NewRentalBooking, key string) (string, error) {
	u, err := rentalUnitForTx(ctx, tx, n.UnitID, n.From, n.To)
	if err != nil {
		return "", err
	}
	// A unit out past its return on another rental has no time it is
	// known to be back by.
	var overdue bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM rental_bookings b JOIN resource_blocks k ON k.id = b.block_id
		                WHERE b.unit_id = $1 AND b.ends_at < clock_timestamp() AND `+rentalState+` = 'checked_out')`,
		n.UnitID).Scan(&overdue); err != nil {
		return "", fmt.Errorf("read the unit's rentals: %w", err)
	}
	if overdue {
		return "", errBookingOverdue
	}
	if err := partySeenTx(ctx, tx, scope, n.RenterID); err != nil {
		return "", err
	}
	var renter string
	err = tx.QueryRow(ctx, `
		SELECT p.id::text FROM parties p
		WHERE p.id = party_now($1)
		  AND EXISTS (SELECT 1 FROM party_roles r WHERE r.party_id = p.id AND r.role = 'renter' AND r.valid_to IS NULL)`,
		n.RenterID).Scan(&renter)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errNotARenter
	}
	if err != nil {
		return "", fmt.Errorf("read renter: %w", err)
	}
	quote, err := quoteTx(ctx, tx, QuoteRequest{UnitID: n.UnitID, PlanID: n.PlanID, From: n.From, To: n.To, Extras: n.Extras})
	if err != nil {
		return "", err
	}
	lines, err := overriddenLines(quote.Lines, n.Overrides)
	if err != nil {
		return "", err
	}

	var id string
	if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		return "", err
	}
	status := BlockConfirmed
	if n.HoldUntil != nil {
		status = BlockHeld
	}
	block, err := insertRentalBlockTx(ctx, tx, scope, u.resource, id, status, n.From, n.To, u.buffer, n.HoldUntil)
	if err != nil {
		return "", err
	}
	// One number at a time.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('rental_numbers:' || $1::text, 0))`, scope.ShopID); err != nil {
		return "", fmt.Errorf("lock numbers: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO rental_bookings (id, shop_id, number, unit_id, renter_party_id, starts_at, ends_at, profile_version_id,
		                             plan_id, included_km, extra_km_minor, block_id, request_key, created_by)
		VALUES ($1, $2, (SELECT coalesce(max(number), 0) + 1 FROM rental_bookings), $3, $4, $5, $6, $7,
		        $8, $9, $10, $11, nullif($12, ''), NULLIF($13, '')::uuid)`,
		id, scope.ShopID, n.UnitID, renter, n.From, n.To, u.profileVersion, quote.PlanID, quote.IncludedKm, quote.ExtraKmMinor,
		block, key, scope.UserID)
	if err != nil {
		return "", fmt.Errorf("record the booking: %w", err)
	}
	kind := "confirmed"
	if n.HoldUntil != nil {
		kind = "held"
	}
	event, err := rentalEventTx(ctx, tx, scope, id, kind, "", nil)
	if err != nil {
		return "", err
	}
	if err := insertRentalLinesTx(ctx, tx, scope, id, event, 1, lines); err != nil {
		return "", err
	}
	return id, nil
}

// overriddenLines is the quote's lines with a person's prices put on them.
type frozenLine struct {
	QuotedLine
	pricedMinor *int64
	reason      string
}

func overriddenLines(lines []QuotedLine, overrides []PriceOverride) ([]frozenLine, error) {
	out := make([]frozenLine, len(lines))
	for i, l := range lines {
		out[i] = frozenLine{QuotedLine: l}
	}
	for _, o := range overrides {
		if o.Position < 1 || o.Position > len(out) {
			return nil, errOverrideLine
		}
		l := &out[o.Position-1]
		priced, net := l.UnitPriceMinor, o.UnitPriceMinor
		if o.UnitGrossMinor != nil {
			net = money.Scale(*o.UnitGrossMinor, 10000, int64(10000+l.VATRateBp))
		}
		l.pricedMinor, l.reason, l.UnitPriceMinor = &priced, o.Reason, net
	}
	return out, nil
}

func insertRentalLinesTx(ctx context.Context, tx pgx.Tx, scope access.Scope, bookingID, eventID string, first int, lines []frozenLine) error {
	for i, l := range lines {
		var by any
		if l.reason != "" {
			by = nullableUser(scope.UserID)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO rental_booking_lines (shop_id, booking_id, position, event_id, kind, unit, description, quantity_milli,
				unit_price_minor, vat_rate_bp, vat_category, unconfirmed, plan_id, season_id, extra_id, priced_minor, override_reason, override_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, NULLIF($13, '')::uuid, NULLIF($14, '')::uuid, NULLIF($15, '')::uuid,
			        $16, nullif($17, ''), $18::uuid)`,
			scope.ShopID, bookingID, first+i, eventID, l.Kind, l.Unit, l.Description, l.QuantityMilli, l.UnitPriceMinor,
			l.VATRateBp, l.VATCategory, l.Unconfirmed, l.PlanID, l.SeasonID, l.ExtraID, l.pricedMinor, l.reason, by); err != nil {
			return fmt.Errorf("freeze the price: %w", err)
		}
		if l.reason != "" {
			if err := auditTx(ctx, tx, scope, bookingID, "rental.price_overridden", fmt.Sprint(*l.pricedMinor), fmt.Sprint(l.UnitPriceMinor),
				l.reason, map[string]any{"position": first + i, "priced_minor": *l.pricedMinor, "unit_price_minor": l.UnitPriceMinor}); err != nil {
				return err
			}
		}
	}
	return nil
}

func nullableUser(id string) any {
	if id == "" {
		return nil
	}
	return id
}

// insertRentalBlockTx puts a rental's time on its unit in the ledger: the
// rental and its cleaning buffer after. What is in the way is named; the
// database's constraint decides.
func insertRentalBlockTx(ctx context.Context, tx pgx.Tx, scope access.Scope, resource, bookingID, status string,
	from, to time.Time, buffer time.Duration, holdUntil *time.Time) (string, error) {
	var retired bool
	if err := tx.QueryRow(ctx, `SELECT retired_at IS NOT NULL FROM resources WHERE id = $1`, resource).Scan(&retired); err != nil {
		return "", fmt.Errorf("read resource: %w", err)
	}
	if retired {
		return "", errUnitRetired
	}
	period := blockRange(from, to.Add(buffer))
	if _, err := tx.Exec(ctx, `SELECT release_expired_holds($1, $2)`, resource, period); err != nil {
		return "", fmt.Errorf("release expired holds: %w", err)
	}
	if err := takenTx(ctx, tx, resource, period, ""); err != nil {
		return "", err
	}
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO resource_blocks (shop_id, resource_id, kind, status, period, buffer_after, expires_at, source_kind, source_id, created_by)
		VALUES ($1, $2, 'rental', $3, $4, $5, $6, 'rental', $7, NULLIF($8, '')::uuid) RETURNING id`,
		scope.ShopID, resource, status, period, buffer, holdUntil, bookingID, scope.UserID).Scan(&id)
	return id, blockConflict(err)
}

// takenTx refuses period on resource when anything but except is booked or
// held over it, naming its kind.
func takenTx(ctx context.Context, tx pgx.Tx, resource string, period pgtype.Range[pgtype.Timestamptz], except string) error {
	var taken string
	err := tx.QueryRow(ctx, `
		SELECT kind FROM resource_blocks
		WHERE resource_id = $1 AND status IN ('held', 'confirmed') AND period && $2 AND id::text <> $3
		ORDER BY lower(period) LIMIT 1`, resource, period, except).Scan(&taken)
	if err == nil {
		return takenBy(taken)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read what is booked: %w", err)
	}
	return nil
}

func blockRange(from, to time.Time) pgtype.Range[pgtype.Timestamptz] {
	return pgtype.Range[pgtype.Timestamptz]{
		Lower: pgtype.Timestamptz{Time: from, Valid: true}, LowerType: pgtype.Inclusive,
		Upper: pgtype.Timestamptz{Time: to, Valid: true}, UpperType: pgtype.Exclusive, Valid: true,
	}
}

// rentalEventTx records what happened to a booking.
func rentalEventTx(ctx context.Context, tx pgx.Tx, scope access.Scope, bookingID, kind, reason string, moved *[4]time.Time) (string, error) {
	var oldS, oldE, newS, newE any
	if moved != nil {
		oldS, oldE, newS, newE = moved[0], moved[1], moved[2], moved[3]
	}
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO rental_booking_events (shop_id, booking_id, kind, by_user, reason, old_starts, old_ends, new_starts, new_ends)
		VALUES ($1, $2, $3, NULLIF($4, '')::uuid, nullif($5, ''), $6, $7, $8, $9) RETURNING id`,
		scope.ShopID, bookingID, kind, scope.UserID, reason, oldS, oldE, newS, newE).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("record what happened: %w", err)
	}
	return id, nil
}

// lockedRental is a booking read for a change, its row locked.
type lockedRental struct {
	id, unit, resource, block, state string
	starts, ends                     time.Time
	buffer                           time.Duration
	blockStatus                      string
}

func lockRentalTx(ctx context.Context, tx pgx.Tx, id string) (lockedRental, error) {
	b := lockedRental{id: id}
	err := tx.QueryRow(ctx, `
		SELECT b.unit_id, k.resource_id, b.block_id, `+rentalState+`, b.starts_at, b.ends_at, k.buffer_after, k.status
		FROM rental_bookings b JOIN resource_blocks k ON k.id = b.block_id
		WHERE b.id = $1 FOR UPDATE OF b, k`, id).Scan(&b.unit, &b.resource, &b.block, &b.state, &b.starts, &b.ends, &b.buffer, &b.blockStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, ErrNotFound
	}
	if err != nil {
		return b, fmt.Errorf("read booking: %w", err)
	}
	return b, nil
}

// ConfirmRentalBooking makes a hold a booking: the deposit came, the
// customer said yes. A hold that has run out is not there to confirm.
func ConfirmRentalBooking(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) error {
	if !scope.Role.HandsOverRentals() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return ErrNotFound
	}
	return inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		b, err := lockRentalTx(ctx, tx, id)
		if err != nil {
			return err
		}
		switch b.state {
		case RentalConfirmed:
			return nil
		case RentalHoldExpired:
			return errHoldRanOut
		case RentalHeld:
		default:
			return errBookingState
		}
		if _, err := tx.Exec(ctx, `UPDATE resource_blocks SET status = 'confirmed' WHERE id = $1`, b.block); err != nil {
			return fmt.Errorf("confirm the block: %w", err)
		}
		_, err = rentalEventTx(ctx, tx, scope, id, "confirmed", "", nil)
		return err
	})
}

// CancelRentalBooking cancels a booking, or records the renter as a no-show
// once it was due to start. Its block is let go; the booking and what
// happened to it stay.
func CancelRentalBooking(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id, reason string, noShow bool) error {
	if !scope.Role.HandsOverRentals() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return ErrNotFound
	}
	reason, err := checkReason(reason, errCancelWhy)
	if err != nil {
		return err
	}
	return inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		b, err := lockRentalTx(ctx, tx, id)
		if err != nil {
			return err
		}
		switch {
		case b.state != RentalHeld && b.state != RentalConfirmed && b.state != RentalHoldExpired:
			return errBookingState
		case noShow && (b.state != RentalConfirmed || time.Now().Before(b.starts)):
			return errNoShowBeforeTime
		}
		if _, err := tx.Exec(ctx, `
			UPDATE resource_blocks SET status = 'released', ended_at = clock_timestamp(), ended_by = NULLIF($2, '')::uuid
			WHERE id = $1 AND status IN ('held', 'confirmed')`, b.block, scope.UserID); err != nil {
			return fmt.Errorf("let the block go: %w", err)
		}
		kind := "cancelled"
		if noShow {
			kind = "no_show"
		}
		_, err = rentalEventTx(ctx, tx, scope, id, kind, reason, nil)
		return err
	})
}

// MoveRentalBooking moves a booking not yet handed over to another period
// as long as it was, its block with it in the same transaction. Refused,
// naming what is in the way, on a conflict; a longer rental is an
// extension, priced as one.
func MoveRentalBooking(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string, from, to time.Time, reason string) error {
	if !scope.Role.HandsOverRentals() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return ErrNotFound
	}
	reason, err := checkNote(reason)
	if err != nil {
		return err
	}
	if from.IsZero() || !to.After(from) {
		return errBookingTimes
	}
	return inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		b, err := lockRentalTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if b.state != RentalHeld && b.state != RentalConfirmed {
			return errBookingState
		}
		if to.Sub(from) != b.ends.Sub(b.starts) {
			return errMoveLength
		}
		if err := inFleetTx(ctx, tx, b.unit, from, to); err != nil {
			return err
		}
		if err := reblockTx(ctx, tx, b, from, to); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE rental_bookings SET starts_at = $2, ends_at = $3 WHERE id = $1`, id, from, to); err != nil {
			return fmt.Errorf("move the booking: %w", err)
		}
		_, err = rentalEventTx(ctx, tx, scope, id, "moved", reason, &[4]time.Time{b.starts, b.ends, from, to})
		return err
	})
}

// reblockTx moves a booking's block to from..to and its buffer.
func reblockTx(ctx context.Context, tx pgx.Tx, b lockedRental, from, to time.Time) error {
	period := blockRange(from, to.Add(b.buffer))
	if _, err := tx.Exec(ctx, `SELECT release_expired_holds($1, $2)`, b.resource, period); err != nil {
		return fmt.Errorf("release expired holds: %w", err)
	}
	if err := takenTx(ctx, tx, b.resource, period, b.block); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE resource_blocks SET period = $2 WHERE id = $1`, b.block, period)
	return blockConflict(err)
}

// ExtendRentalBooking makes a rental end later, before it is handed over or
// while it is out. The time added is priced under the plans as they are
// now, as lines of its own; the lines agreed before stay as they were.
func ExtendRentalBooking(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string, to time.Time, reason string) error {
	if !scope.Role.HandsOverRentals() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return ErrNotFound
	}
	reason, err := checkNote(reason)
	if err != nil {
		return err
	}
	return inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		b, err := lockRentalTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if !b.open() {
			return errBookingState
		}
		if !to.After(b.ends) {
			return errExtendLater
		}
		if err := inFleetTx(ctx, tx, b.unit, b.starts, to); err != nil {
			return err
		}
		var plan string
		var next int
		if err := tx.QueryRow(ctx, `SELECT coalesce(plan_id::text, ''), (SELECT coalesce(max(position), 0) + 1 FROM rental_booking_lines WHERE booking_id = $1)
			FROM rental_bookings WHERE id = $1`, id).Scan(&plan, &next); err != nil {
			return fmt.Errorf("read booking: %w", err)
		}
		quote, err := quoteTx(ctx, tx, QuoteRequest{UnitID: b.unit, PlanID: plan, From: b.ends, To: to})
		if err != nil {
			return err
		}
		if err := reblockTx(ctx, tx, b, b.starts, to); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE rental_bookings SET ends_at = $2 WHERE id = $1`, id, to); err != nil {
			return fmt.Errorf("extend the booking: %w", err)
		}
		event, err := rentalEventTx(ctx, tx, scope, id, "extended", reason, &[4]time.Time{b.starts, b.ends, b.starts, to})
		if err != nil {
			return err
		}
		lines, _ := overriddenLines(quote.Lines, nil)
		return insertRentalLinesTx(ctx, tx, scope, id, event, next, lines)
	})
}

func (b lockedRental) open() bool {
	return b.state == RentalHeld || b.state == RentalConfirmed || b.state == RentalCheckedOut
}

const rentalBookingColumns = `
	b.id, b.number, b.unit_id, u.fleet_number || coalesce(' · ' || reg.registration, ' · ' || r.name, ''), u.resource_id,
	b.renter_party_id, coalesce(CASE WHEN pa.kind = 'company' THEN coalesce(c.company_name, pa.company_name) END, cp.display_name, pp.display_name, ''),
	b.starts_at, b.ends_at, ` + rentalState + `, CASE WHEN k.status = 'held' THEN k.expires_at END, b.block_id,
	v.profile_id, coalesce(b.plan_id::text, ''), b.included_km, b.extra_km_minor,
	EXISTS (SELECT 1 FROM rental_bookings o JOIN resource_blocks ok ON ok.id = o.block_id
	         WHERE o.unit_id = b.unit_id AND o.id <> b.id AND o.ends_at < clock_timestamp() AND o.starts_at < b.starts_at
	           AND (SELECT e.kind FROM rental_booking_events e WHERE e.booking_id = o.id
	                  AND e.kind IN ('held', 'confirmed', 'checked_out', 'returned', 'cancelled', 'no_show')
	                ORDER BY e.seq DESC LIMIT 1) = 'checked_out')`

const rentalBookingFrom = `
	FROM rental_bookings b
	JOIN resource_blocks k ON k.id = b.block_id
	JOIN rental_units u ON u.id = b.unit_id
	JOIN resources r ON r.id = u.resource_id
	JOIN rental_profile_versions v ON v.id = b.profile_version_id
	LEFT JOIN vehicle_registrations reg ON reg.vehicle_id = r.vehicle_id AND reg.valid_to IS NULL
	JOIN parties pa ON pa.id = b.renter_party_id
	LEFT JOIN customers c ON c.id = pa.customer_id
	LEFT JOIN people cp ON cp.id = c.person_id
	LEFT JOIN people pp ON pp.id = pa.person_id`

func scanRentalBooking(row pgx.Row) (RentalBooking, error) {
	var b RentalBooking
	err := row.Scan(&b.ID, &b.Number, &b.UnitID, &b.UnitLabel, &b.ResourceID, &b.RenterID, &b.RenterName, &b.Starts, &b.Ends,
		&b.State, &b.HeldUntil, &b.BlockID, &b.ProfileID, &b.PlanID, &b.IncludedKm, &b.ExtraKm, &b.AtRisk)
	if b.State != RentalConfirmed && b.State != RentalHeld {
		b.AtRisk = false
	}
	return b, err
}

// RentalBookingByID reads a booking with its lines and what happened to
// it. The rental desk and whoever runs the shop.
func RentalBookingByID(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) (RentalBooking, error) {
	if !scope.Role.HandsOverRentals() {
		return RentalBooking{}, access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return RentalBooking{}, ErrNotFound
	}
	var b RentalBooking
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		b, err = scanRentalBooking(tx.QueryRow(ctx, `SELECT `+rentalBookingColumns+rentalBookingFrom+` WHERE b.id = $1`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read booking: %w", err)
		}
		if b.Lines, err = rentalLinesTx(ctx, tx, id); err != nil {
			return err
		}
		b.Totals = rentalTotals(b.Lines)
		b.Events, err = rentalEventsTx(ctx, tx, id)
		return err
	})
	return b, err
}

func rentalTotals(lines []RentalBookingLine) money.Totals {
	ml := make([]money.Line, 0, len(lines))
	for _, l := range lines {
		ml = append(ml, money.Line{QuantityMilli: l.QuantityMilli, UnitPriceMinor: l.UnitPriceMinor, VATRateBasis: l.VATRateBp, ChargedToCustomer: true})
	}
	return money.Compute(ml)
}

func rentalLinesTx(ctx context.Context, tx pgx.Tx, id string) ([]RentalBookingLine, error) {
	rows, err := tx.Query(ctx, `
		SELECT l.position, l.kind, l.unit, l.description, l.quantity_milli, l.unit_price_minor, l.vat_rate_bp, l.vat_category,
		       l.unconfirmed, coalesce(l.plan_id::text, ''), coalesce(l.season_id::text, ''), coalesce(l.extra_id::text, ''),
		       l.priced_minor, coalesce(l.override_reason, ''), coalesce(pe.display_name, '')
		FROM rental_booking_lines l
		LEFT JOIN users u ON u.id = l.override_by LEFT JOIN people pe ON pe.id = u.person_id
		WHERE l.booking_id = $1 ORDER BY l.position`, id)
	if err != nil {
		return nil, fmt.Errorf("read lines: %w", err)
	}
	defer rows.Close()
	var out []RentalBookingLine
	for rows.Next() {
		var l RentalBookingLine
		if err := rows.Scan(&l.Position, &l.Kind, &l.Unit, &l.Description, &l.QuantityMilli, &l.UnitPriceMinor, &l.VATRateBp,
			&l.VATCategory, &l.Unconfirmed, &l.PlanID, &l.SeasonID, &l.ExtraID, &l.PricedMinor, &l.OverrideReason, &l.OverrideBy); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func rentalEventsTx(ctx context.Context, tx pgx.Tx, id string) ([]RentalBookingEvent, error) {
	rows, err := tx.Query(ctx, `
		SELECT e.kind, e.at, coalesce(pe.display_name, ''), coalesce(e.reason, ''), e.old_starts, e.old_ends, e.new_starts, e.new_ends
		FROM rental_booking_events e
		LEFT JOIN users u ON u.id = e.by_user LEFT JOIN people pe ON pe.id = u.person_id
		WHERE e.booking_id = $1 ORDER BY e.seq`, id)
	if err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	defer rows.Close()
	var out []RentalBookingEvent
	for rows.Next() {
		var e RentalBookingEvent
		if err := rows.Scan(&e.Kind, &e.At, &e.By, &e.Reason, &e.OldStarts, &e.OldEnds, &e.NewStarts, &e.NewEnds); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RentalBookings lists the bookings that overlap from..to, or are out past
// their end, soonest first. The rental desk and whoever runs the shop.
func RentalBookings(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, from, to time.Time) ([]RentalBooking, error) {
	if !scope.Role.HandsOverRentals() {
		return nil, access.ErrForbidden
	}
	var out []RentalBooking
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+rentalBookingColumns+rentalBookingFrom+`
			WHERE tstzrange(b.starts_at, b.ends_at) && tstzrange($1, $2)
			   OR (b.ends_at < $1 AND `+rentalState+` = 'checked_out')
			ORDER BY b.starts_at, b.number`, from, to)
		if err != nil {
			return fmt.Errorf("read bookings: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			b, err := scanRentalBooking(rows)
			if err != nil {
				return fmt.Errorf("scan booking: %w", err)
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	return out, err
}

// Renter is a party with the renter role, as the rental desk picks one.
type Renter struct {
	ID    string
	Name  string
	Phone string
}

// Renters lists the parties that rent, by name: whom a booking can be for.
func Renters(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]Renter, error) {
	if !scope.Role.HandsOverRentals() {
		return nil, access.ErrForbidden
	}
	var out []Renter
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT pa.id, coalesce(CASE WHEN pa.kind = 'company' THEN coalesce(c.company_name, pa.company_name) END, cp.display_name, pp.display_name, ''),
			       coalesce(cp.phone, pp.phone, '')
			FROM parties pa
			LEFT JOIN customers c ON c.id = pa.customer_id
			LEFT JOIN people cp ON cp.id = c.person_id
			LEFT JOIN people pp ON pp.id = pa.person_id
			WHERE pa.merged_into IS NULL
			  AND EXISTS (SELECT 1 FROM party_roles r WHERE r.party_id = pa.id AND r.role = 'renter' AND r.valid_to IS NULL)
			ORDER BY 2, pa.id`)
		if err != nil {
			return fmt.Errorf("read renters: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r Renter
			if err := rows.Scan(&r.ID, &r.Name, &r.Phone); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}
