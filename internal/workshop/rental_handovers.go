package workshop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
)

// Check-out and check-in (#194). At handover and at return a person walks
// round the unit with a phone and records it on the checklist of the
// profile the rental was booked under: a protocol (#147), subject
// check_out or check_in, of the booking. Finishing it is the handover --
// one transaction freezes the protocol, writes the odometer reading and
// the handover's own row, and moves the booking out or back. At return
// each checkpoint is shown beside what it was at handover; a finding a
// person confirms as new damage becomes a damage case, observed, and
// nothing more: no charge, no deposit movement, no debt.

// The two ends of a rental.
const (
	HandoverOut = SubjectCheckOut
	HandoverIn  = SubjectCheckIn
)

var (
	errHandoverKind      = fmt.Errorf("%w: a handover is a check-out or a check-in", ErrInvalid)
	errHandoverNoList    = fmt.Errorf("%w: the unit's profile has no checklist for this; choose one in the rental settings first", ErrInvalid)
	errCheckOutState     = fmt.Errorf("%w: a booking is checked out once it is confirmed, and only once", ErrInvalid)
	errCheckInState      = fmt.Errorf("%w: a booking is checked in once it is out, and only once", ErrInvalid)
	errUnitStillOut      = fmt.Errorf("%w: the unit is still out on another rental; check that in first", ErrInvalid)
	errHandoverOdometer  = fmt.Errorf("%w: read the odometer, in whole kilometres", ErrInvalid)
	errHandoverFuel      = fmt.Errorf("%w: the fuel or charge is read in eighths, from empty to full", ErrInvalid)
	errHandoverAccepted  = fmt.Errorf("%w: the renter accepts the protocol only when there", ErrInvalid)
	errHandoverNotOurs   = fmt.Errorf("%w: that checkpoint is not on this protocol", ErrInvalid)
	errHandoverNotNew    = fmt.Errorf("%w: only a finding new since the check-out becomes a damage case", ErrInvalid)
	errHandoverAbandoned = fmt.Errorf("%w: that protocol was left; start the handover again", ErrInvalid)
)

// HandoverGapsError is a check-out refused while the booking lacks
// something (#193): each is listed.
type HandoverGapsError struct{ Gaps []HandoverGap }

func (e *HandoverGapsError) Error() string {
	return "something is still missing before the handover; the booking lists it"
}

// Unwrap makes it an ErrInvalid, as every refusal is.
func (e *HandoverGapsError) Unwrap() error { return ErrInvalid }

// handoverBooking is what a handover needs to know about its booking.
type handoverBooking struct {
	lockedRental
	vehicle      *string
	templateID   string
	includedKm   *int
	extraKmMinor *int64
}

func handoverBookingTx(ctx context.Context, tx pgx.Tx, bookingID, kind string) (handoverBooking, error) {
	var h handoverBooking
	b, err := lockRentalTx(ctx, tx, bookingID)
	if err != nil {
		return h, err
	}
	h.lockedRental = b
	// The checklist of the profile version the rental was booked under,
	// or of the profile as it is now if that one named none.
	err = tx.QueryRow(ctx, `
		SELECT r.vehicle_id::text, b.included_km, b.extra_km_minor,
		       coalesce(CASE $2 WHEN 'check_out' THEN v.check_out_template_id ELSE v.check_in_template_id END::text,
		                (SELECT CASE $2 WHEN 'check_out' THEN l.check_out_template_id ELSE l.check_in_template_id END::text
		                   FROM rental_profile_versions l WHERE l.profile_id = v.profile_id ORDER BY l.version DESC LIMIT 1), '')
		FROM rental_bookings b
		JOIN rental_units u ON u.id = b.unit_id
		JOIN resources r ON r.id = u.resource_id
		JOIN rental_profile_versions v ON v.id = b.profile_version_id
		WHERE b.id = $1`, bookingID, kind).Scan(&h.vehicle, &h.includedKm, &h.extraKmMinor, &h.templateID)
	if err != nil {
		return h, fmt.Errorf("read booking: %w", err)
	}
	return h, nil
}

// StartHandover starts the protocol of a check-out or a check-in, or
// carries on with the one under way: a second tap, or a phone replaying
// the request, finds it. A check-out is refused while the booking lacks
// anything it needs before handover, and while the unit is still out on
// another rental. The rental desk and whoever runs the shop.
func StartHandover(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, bookingID, kind string) (string, error) {
	if !scope.Role.HandsOverRentals() {
		return "", access.ErrForbidden
	}
	if kind != HandoverOut && kind != HandoverIn {
		return "", errHandoverKind
	}
	if !looksLikeUUID(bookingID) {
		return "", ErrNotFound
	}
	var id string
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		h, err := handoverBookingTx(ctx, tx, bookingID, kind)
		if err != nil {
			return err
		}
		if err := handoverMayTx(ctx, tx, h, kind); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `
			SELECT id::text FROM inspections
			WHERE `+subjectColumns[kind]+` = $1 AND subject = $2 AND completed_at IS NULL AND abandoned_at IS NULL`,
			bookingID, kind).Scan(&id)
		if err == nil || !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if h.templateID == "" {
			return errHandoverNoList
		}
		var name string
		if err := tx.QueryRow(ctx, `SELECT name FROM inspection_templates WHERE id = $1 AND active AND $2 = ANY(subjects)`,
			h.templateID, kind).Scan(&name); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errHandoverNoList
			}
			return fmt.Errorf("read template: %w", err)
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO inspections (shop_id, subject, `+subjectColumns[kind]+`, vehicle_id, resource_id, template_id, template_name, performed_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
			scope.ShopID, kind, bookingID, h.vehicle, h.resource, h.templateID, name, scope.UserID).Scan(&id); err != nil {
			return fmt.Errorf("start the protocol: %w", err)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO inspection_items (shop_id, inspection_id, position, label, item_key)
			SELECT $1, $2, ti.position, ti.label, ti.item_key
			FROM inspection_template_items ti WHERE ti.template_id = $3 ORDER BY ti.position`,
			scope.ShopID, id, h.templateID)
		if err != nil {
			return fmt.Errorf("copy the checklist: %w", err)
		}
		return nil
	})
	return id, err
}

// handoverMayTx refuses a handover the booking is not ready for.
func handoverMayTx(ctx context.Context, tx pgx.Tx, h handoverBooking, kind string) error {
	if kind == HandoverIn {
		if h.state != RentalCheckedOut {
			return errCheckInState
		}
		return nil
	}
	if h.state != RentalConfirmed {
		return errCheckOutState
	}
	gaps, err := rentalBookingGapsTx(ctx, tx, h.id)
	if err != nil {
		return err
	}
	if len(gaps) > 0 {
		return &HandoverGapsError{Gaps: gaps}
	}
	var out bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM rental_bookings b JOIN resource_blocks k ON k.id = b.block_id
		               WHERE b.unit_id = $1 AND b.id <> $2 AND `+rentalState+` = 'checked_out')`, h.unit, h.id).Scan(&out); err != nil {
		return fmt.Errorf("read the unit: %w", err)
	}
	if out {
		return errUnitStillOut
	}
	return nil
}

// HandoverReading is what is read at the handover besides the checklist.
type HandoverReading struct {
	// Whole kilometres; nil for a unit with no odometer.
	OdometerKm *int64
	// Eighths of full; nil for a unit with no tank or battery.
	FuelEighths *int
	// The renter was there. A return with nobody there -- the key in the
	// box -- says so.
	RenterPresent bool
	// The renter saw the protocol and accepted it.
	RenterAccepted bool
	Note           string
	// At check-in, the checkpoints a person confirmed as new damage, by
	// item id. Each becomes a damage case, observed.
	Damage []string
	Key    string
}

// FinishHandover finishes a check-out or check-in protocol, and with it
// the handover: in one transaction the protocol is frozen, the odometer
// reading is written (flagged, not refused, when lower than the last), the
// handover is recorded with its own time, and the booking is out or back.
// At check-in what is left of the unit's block is let go, and each
// finding confirmed as new damage becomes a damage case. Sent again -- a
// phone that lost the answer -- it finds the handover it made.
func FinishHandover(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, protocolID string, r HandoverReading) error {
	if !scope.Role.HandsOverRentals() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(protocolID) {
		return ErrNotFound
	}
	switch {
	case r.OdometerKm != nil && (*r.OdometerKm < 0 || *r.OdometerKm >= 10000000):
		return errHandoverOdometer
	case r.FuelEighths != nil && (*r.FuelEighths < 0 || *r.FuelEighths > 8):
		return errHandoverFuel
	case r.RenterAccepted && !r.RenterPresent:
		return errHandoverAccepted
	}
	note, err := checkNote(r.Note)
	if err != nil {
		return err
	}
	key, err := checkRentalKey(r.Key)
	if err != nil {
		return err
	}
	return inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var kind, bookingID string
		var completed, abandoned bool
		err := tx.QueryRow(ctx, `
			SELECT subject, coalesce(check_out_id, check_in_id)::text, completed_at IS NOT NULL, abandoned_at IS NOT NULL
			FROM inspections WHERE id = $1 AND subject IN ('check_out', 'check_in')`, protocolID).Scan(&kind, &bookingID, &completed, &abandoned)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read the protocol: %w", err)
		}
		h, err := handoverBookingTx(ctx, tx, bookingID, kind)
		if err != nil {
			return err
		}
		// The same request again finds what it did.
		var done bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM rental_handovers WHERE protocol_id = $1)`, protocolID).Scan(&done); err != nil {
			return fmt.Errorf("read the handover: %w", err)
		}
		if done {
			return nil
		}
		if abandoned {
			return errHandoverAbandoned
		}
		if err := handoverMayTx(ctx, tx, h, kind); err != nil {
			return err
		}
		if h.vehicle == nil {
			r.OdometerKm = nil
		} else if r.OdometerKm == nil {
			return errHandoverOdometer
		}
		var damage []handoverFinding
		if kind == HandoverIn {
			cmp, err := compareTx(ctx, tx, bookingID, protocolID, h.resource)
			if err != nil {
				return err
			}
			for _, item := range slices.Compact(slices.Sorted(slices.Values(r.Damage))) {
				f, ok := cmp.finding(item)
				switch {
				case !ok:
					return errHandoverNotOurs
				case !f.New:
					return errHandoverNotNew
				}
				damage = append(damage, handoverFinding{item: item, key: f.Key, label: f.Label, note: f.In.Note})
			}
		} else if len(r.Damage) > 0 {
			return errHandoverNotNew
		}
		if _, err := tx.Exec(ctx, `UPDATE inspections SET completed_at = now() WHERE id = $1`, protocolID); err != nil {
			return finished(err)
		}
		var reading *string
		if r.OdometerKm != nil {
			var id string
			if err := tx.QueryRow(ctx, `
				INSERT INTO odometer_readings (shop_id, vehicle_id, km, source, decreasing)
				VALUES ($1, $2, $3, $4, EXISTS (SELECT 1 FROM odometer_readings WHERE vehicle_id = $2 AND km > $3))
				RETURNING id`, scope.ShopID, *h.vehicle, *r.OdometerKm, kind).Scan(&id); err != nil {
				return fmt.Errorf("record the odometer: %w", err)
			}
			reading = &id
		}
		var at time.Time
		if err := tx.QueryRow(ctx, `
			INSERT INTO rental_handovers (shop_id, booking_id, kind, by_user, protocol_id, odometer_reading_id, fuel_eighths,
			                              renter_present, renter_accepted, note, request_key)
			VALUES ($1, $2, $3, NULLIF($4, '')::uuid, $5, $6, $7, $8, $9, nullif($10, ''), nullif($11, ''))
			RETURNING at`, scope.ShopID, bookingID, kind, scope.UserID, protocolID, reading, r.FuelEighths,
			r.RenterPresent, r.RenterAccepted, note, key).Scan(&at); err != nil {
			return fmt.Errorf("record the handover: %w", err)
		}
		event := "checked_out"
		if kind == HandoverIn {
			event = "returned"
			// Back early: the rest of the time is the unit's again, from
			// the return and its cleaning on. Back late, the block stays
			// as it was: the time it was out over is gone, and the next
			// rental's own block was never moved for it.
			// Back before the time it was booked from even began -- handed
			// over early and returned early -- the whole block goes.
			if until := at.Add(h.buffer); until.Before(h.ends.Add(h.buffer)) {
				if _, err := tx.Exec(ctx, `
					UPDATE resource_blocks
					SET period = CASE WHEN $2 > lower(period) THEN tstzrange(lower(period), $2) ELSE period END,
					    status = CASE WHEN $2 > lower(period) THEN status ELSE 'released' END,
					    ended_at = CASE WHEN $2 > lower(period) THEN ended_at ELSE clock_timestamp() END,
					    ended_by = CASE WHEN $2 > lower(period) THEN ended_by ELSE NULLIF($3, '')::uuid END
					WHERE id = $1`, h.block, until, scope.UserID); err != nil {
					return fmt.Errorf("let the rest of the block go: %w", err)
				}
			}
			var vehicle any
			if h.vehicle != nil {
				vehicle = *h.vehicle
			}
			for _, d := range damage {
				desc := d.label
				if d.note != "" {
					desc += ": " + d.note
				}
				if len([]rune(desc)) > 300 {
					desc = string([]rune(desc)[:300])
				}
				if _, err := tx.Exec(ctx, `
					INSERT INTO damage_cases (shop_id, resource_id, vehicle_id, booking_id, found_in, item_id, item_key, description, observed_by)
					VALUES ($1, $2, $3, $4, $5, $6, nullif($7, ''), $8, NULLIF($9, '')::uuid)`,
					scope.ShopID, h.resource, vehicle, bookingID, protocolID, d.item, d.key, desc, scope.UserID); err != nil {
					return fmt.Errorf("open the damage case: %w", err)
				}
			}
		}
		_, err = rentalEventTx(ctx, tx, scope, bookingID, event, "", nil)
		return err
	})
}

type handoverFinding struct{ item, key, label, note string }

// AbandonHandover leaves a handover's protocol unfinished, and why: it
// stays, frozen, and a new one can be started. The booking is as it was.
func AbandonHandover(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, protocolID, reason string) error {
	if !scope.Role.HandsOverRentals() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Rental); err != nil {
		return err
	}
	return AbandonProtocol(ctx, pool, scope, protocolID, reason)
}

// RentalHandover is one end of a rental as it happened.
type RentalHandover struct {
	Kind           string
	At             time.Time
	By             string
	ProtocolID     string
	OdometerKm     *int64
	Decreasing     bool
	FuelEighths    *int
	RenterPresent  bool
	RenterAccepted bool
	Note           string
}

// ComparedItem is one checkpoint at return beside what it was at
// handover: matched by its key, so two checklists with the same
// checkpoints compare even if they are not the same list.
type ComparedItem struct {
	Key   string
	Label string
	Out   *ProtocolItem
	In    ProtocolItem
	// Worse than at handover, or found where nothing was: to confirm as
	// damage or let go.
	New bool
	// An open damage case already names this checkpoint: known, not new.
	Known bool
}

// HandoverComparison is a check-in beside the check-out before it.
type HandoverComparison struct {
	Items []ComparedItem
}

func (c HandoverComparison) finding(itemID string) (ComparedItem, bool) {
	for _, it := range c.Items {
		if it.In.ID == itemID {
			return it, true
		}
	}
	return ComparedItem{}, false
}

// statusRank orders what a checkpoint can be found as, from nothing to
// fail; a note alone on an unchecked one is a finding as well.
func statusRank(it ProtocolItem) int {
	switch it.Status {
	case "attention":
		return 2
	case "fail":
		return 3
	case "pass":
		return 1
	}
	return 0
}

// DamageCase is a finding confirmed as new damage, as the desk reads it.
type DamageCase struct {
	ID          string
	ItemKey     string
	Description string
	State       string
	ObservedAt  time.Time
	BookingID   string
}

// openDamageTx is the unit's damage register: every case not yet
// repaired. (#197 adds repair; until then every case is open.)
func openDamageTx(ctx context.Context, tx pgx.Tx, resource string) ([]DamageCase, error) {
	rows, err := tx.Query(ctx, `
		SELECT id::text, coalesce(item_key, ''), description, state, observed_at, coalesce(booking_id::text, '')
		FROM damage_cases WHERE resource_id = $1 ORDER BY observed_at, id`, resource)
	if err != nil {
		return nil, fmt.Errorf("read the damage register: %w", err)
	}
	defer rows.Close()
	var out []DamageCase
	for rows.Next() {
		var d DamageCase
		if err := rows.Scan(&d.ID, &d.ItemKey, &d.Description, &d.State, &d.ObservedAt, &d.BookingID); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// compareTx puts a check-in protocol beside the booking's check-out.
func compareTx(ctx context.Context, tx pgx.Tx, bookingID, checkInID, resource string) (HandoverComparison, error) {
	var c HandoverComparison
	ins, err := readProtocols(ctx, tx, `i.id = $1`, checkInID)
	if err != nil || len(ins) == 0 {
		return c, err
	}
	var outs []Protocol
	var outID string
	err = tx.QueryRow(ctx, `SELECT protocol_id::text FROM rental_handovers WHERE booking_id = $1 AND kind = 'check_out'`, bookingID).Scan(&outID)
	switch {
	case err == nil:
		if outs, err = readProtocols(ctx, tx, `i.id = $1`, outID); err != nil {
			return c, err
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return c, fmt.Errorf("read the check-out: %w", err)
	}
	before := map[string]*ProtocolItem{}
	if len(outs) > 0 {
		for i := range outs[0].Items {
			if k := outs[0].Items[i].Key; k != "" {
				before[k] = &outs[0].Items[i]
			}
		}
	}
	known := map[string]bool{}
	open, err := openDamageTx(ctx, tx, resource)
	if err != nil {
		return c, err
	}
	for _, d := range open {
		if d.ItemKey != "" {
			known[d.ItemKey] = true
		}
	}
	for _, it := range ins[0].Items {
		ci := ComparedItem{Key: it.Key, Label: it.Label, In: it}
		if it.Key != "" {
			ci.Out = before[it.Key]
		}
		was := 0
		if ci.Out != nil {
			was = statusRank(*ci.Out)
		}
		now := statusRank(it)
		ci.Known = it.Key != "" && known[it.Key]
		ci.New = !ci.Known && now >= 2 && now > was
		c.Items = append(c.Items, ci)
	}
	return c, nil
}

// ProposedCharge is something a return suggests charging for (#195): a
// person confirms it, and until then it is no line on anything. A price
// is given where the booking agreed one.
type ProposedCharge struct {
	Kind     string
	Quantity int64
	// The agreed price of one, net; nil where the booking agreed none.
	UnitPriceMinor *int64
}

// What a return can suggest charging for.
const (
	ChargeExtraKm = "extra_km"
	ChargeFuel    = "fuel"
	ChargeLate    = "late"
)

// RentalHandoverView is a booking's handovers as the desk reads them: each
// end that has happened, the protocol under way, at check-in the
// comparison with the check-out, the unit's damage register, and what the
// return suggests charging.
type RentalHandoverView struct {
	BookingID   string
	Out, In     *RentalHandover
	UnderWay    *Protocol
	UnderWayFor string
	Comparison  *HandoverComparison
	Damage      []DamageCase
	Proposed    []ProposedCharge
	HasOdometer bool
}

// RentalHandovers reads a booking's handovers. The rental desk and whoever
// runs the shop.
func RentalHandovers(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, bookingID string) (RentalHandoverView, error) {
	if !scope.Role.HandsOverRentals() {
		return RentalHandoverView{}, access.ErrForbidden
	}
	if !looksLikeUUID(bookingID) {
		return RentalHandoverView{}, ErrNotFound
	}
	v := RentalHandoverView{BookingID: bookingID}
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		v, err = rentalHandoversTx(ctx, tx, bookingID)
		return err
	})
	return v, err
}

// RentalHandoverOfProtocol reads the handovers of the booking a check-out
// or check-in protocol is of, with the protocol.
func RentalHandoverOfProtocol(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, protocolID string) (RentalHandoverView, Protocol, error) {
	if !scope.Role.HandsOverRentals() {
		return RentalHandoverView{}, Protocol{}, access.ErrForbidden
	}
	if !looksLikeUUID(protocolID) {
		return RentalHandoverView{}, Protocol{}, ErrNotFound
	}
	var v RentalHandoverView
	var p Protocol
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var bookingID string
		err := tx.QueryRow(ctx, `SELECT coalesce(check_out_id, check_in_id)::text FROM inspections WHERE id = $1 AND subject IN ('check_out', 'check_in')`,
			protocolID).Scan(&bookingID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read the protocol: %w", err)
		}
		ps, err := readProtocols(ctx, tx, `i.id = $1`, protocolID)
		if err != nil || len(ps) == 0 {
			return err
		}
		p = ps[0]
		if v, err = rentalHandoversTx(ctx, tx, bookingID); err != nil {
			return err
		}
		if p.Subject == HandoverIn && v.Comparison == nil {
			var resource string
			if err := tx.QueryRow(ctx, `SELECT u.resource_id FROM rental_bookings b JOIN rental_units u ON u.id = b.unit_id WHERE b.id = $1`,
				bookingID).Scan(&resource); err != nil {
				return fmt.Errorf("read the unit: %w", err)
			}
			cmp, err := compareTx(ctx, tx, bookingID, protocolID, resource)
			if err != nil {
				return err
			}
			v.Comparison = &cmp
		}
		return nil
	})
	return v, p, err
}

func rentalHandoversTx(ctx context.Context, tx pgx.Tx, bookingID string) (RentalHandoverView, error) {
	v := RentalHandoverView{BookingID: bookingID}
	var resource string
	var ends time.Time
	var includedKm *int
	var extraKm *int64
	err := tx.QueryRow(ctx, `
		SELECT u.resource_id, r.vehicle_id IS NOT NULL, b.ends_at, b.included_km, b.extra_km_minor
		FROM rental_bookings b JOIN rental_units u ON u.id = b.unit_id JOIN resources r ON r.id = u.resource_id
		WHERE b.id = $1`, bookingID).Scan(&resource, &v.HasOdometer, &ends, &includedKm, &extraKm)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	}
	if err != nil {
		return v, fmt.Errorf("read booking: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT h.kind, h.at, coalesce(p.display_name, ''), h.protocol_id::text, o.km, coalesce(o.decreasing, false), h.fuel_eighths,
		       h.renter_present, h.renter_accepted, coalesce(h.note, '')
		FROM rental_handovers h
		LEFT JOIN odometer_readings o ON o.id = h.odometer_reading_id
		LEFT JOIN users us ON us.id = h.by_user LEFT JOIN people p ON p.id = us.person_id
		WHERE h.booking_id = $1`, bookingID)
	if err != nil {
		return v, fmt.Errorf("read the handovers: %w", err)
	}
	for rows.Next() {
		var h RentalHandover
		if err := rows.Scan(&h.Kind, &h.At, &h.By, &h.ProtocolID, &h.OdometerKm, &h.Decreasing, &h.FuelEighths,
			&h.RenterPresent, &h.RenterAccepted, &h.Note); err != nil {
			rows.Close()
			return v, err
		}
		if h.Kind == HandoverOut {
			v.Out = &h
		} else {
			v.In = &h
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return v, err
	}
	under, err := readProtocols(ctx, tx, `coalesce(i.check_out_id, i.check_in_id) = $1 AND i.subject IN ('check_out', 'check_in')
		AND i.completed_at IS NULL AND i.abandoned_at IS NULL`, bookingID)
	if err != nil {
		return v, err
	}
	if len(under) > 0 {
		v.UnderWay, v.UnderWayFor = &under[0], under[0].Subject
	}
	if v.Damage, err = openDamageTx(ctx, tx, resource); err != nil {
		return v, err
	}
	if v.In != nil {
		cmp, err := compareTx(ctx, tx, bookingID, v.In.ProtocolID, resource)
		if err != nil {
			return v, err
		}
		v.Comparison = &cmp
		v.Proposed = proposedCharges(v.Out, v.In, ends, includedKm, extraKm)
	}
	return v, nil
}

// proposedCharges is what a return suggests charging: the kilometres over
// the allowance at the agreed price, the eighths of fuel or charge short
// of what went out, and the minutes past the agreed end. A person
// confirms each (#195); none is a line until then.
func proposedCharges(out, in *RentalHandover, ends time.Time, includedKm *int, extraKm *int64) []ProposedCharge {
	var p []ProposedCharge
	if out == nil || in == nil {
		return nil
	}
	if out.OdometerKm != nil && in.OdometerKm != nil && includedKm != nil {
		if over := *in.OdometerKm - *out.OdometerKm - int64(*includedKm); over > 0 {
			p = append(p, ProposedCharge{Kind: ChargeExtraKm, Quantity: over, UnitPriceMinor: extraKm})
		}
	}
	if out.FuelEighths != nil && in.FuelEighths != nil && *in.FuelEighths < *out.FuelEighths {
		p = append(p, ProposedCharge{Kind: ChargeFuel, Quantity: int64(*out.FuelEighths - *in.FuelEighths)})
	}
	if late := in.At.Sub(ends); late >= time.Minute {
		p = append(p, ProposedCharge{Kind: ChargeLate, Quantity: int64(late / time.Minute)})
	}
	return p
}

// RentalProtocolPhoto reports whether a stored photograph is on a check-out
// or check-in protocol of the caller's shop, for the rental desk in a shop
// that may not run the workshop.
func RentalProtocolPhoto(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, key string) (bool, error) {
	if !scope.Role.HandsOverRentals() {
		return false, access.ErrForbidden
	}
	var ok bool
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM attachments a JOIN inspection_items it ON it.id = a.inspection_item_id
			               JOIN inspections i ON i.id = it.inspection_id
			               WHERE a.storage_key = $1 AND i.subject IN ('check_out', 'check_in'))`, strings.TrimSpace(key)).Scan(&ok)
	})
	return ok, err
}

// Km is the odometer reading, for a page; 0 when there is none.
func (h RentalHandover) Km() int64 {
	if h.OdometerKm == nil {
		return 0
	}
	return *h.OdometerKm
}

// Fuel is the fuel or charge in eighths, for a page; -1 when not read.
func (h RentalHandover) Fuel() int {
	if h.FuelEighths == nil {
		return -1
	}
	return *h.FuelEighths
}

// Price is the agreed price of one, net, for a page; 0 when none.
func (c ProposedCharge) Price() int64 {
	if c.UnitPriceMinor == nil {
		return 0
	}
	return *c.UnitPriceMinor
}
