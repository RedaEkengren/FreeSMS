package workshop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// The rental register (#200): who had which unit when, as the authority
// that granted the permit or the police may ask to see it. It is read from
// the bookings, their drivers and licence checks, their handovers and
// their agreements -- never a copy of them -- for a period, a unit or a
// registration. Only whoever runs the shop reads it, every read leaves an
// audit event saying who, when, why and with which filter, and it stays
// readable with Rental switched off: the duty to show it outlives the
// module. What it must contain and how long it is kept are not confirmed
// (gate G2, #221): it holds what the issue lists, and its retention is the
// shop's own decision, shown as missing until made.

var (
	errRegisterPurpose = fmt.Errorf("%w: say why the register is read, in 300 characters or fewer", ErrInvalid)
	errRegisterPeriod  = fmt.Errorf("%w: a register is read over a period, from a day to a later one", ErrInvalid)
	errRegisterWhen    = fmt.Errorf("%w: give the registration and the moment", ErrInvalid)
	errRetention       = fmt.Errorf("%w: keep the register between 1 and 240 months, and say on what basis", ErrInvalid)
)

// RegisterFilter is what part of the register to read: a period, and
// within it one unit or one registration if given.
type RegisterFilter struct {
	From, To     time.Time
	UnitID       string
	Registration string
}

// RegisterEntry is one rental as the register shows it.
type RegisterEntry struct {
	BookingID string
	Reference string
	Unit      string
	// The plate the vehicle carried when the rental began; empty for a
	// unit that is no vehicle.
	Registration string
	Renter       string
	State        string
	// Booked.
	Starts, Ends time.Time
	// Actual: when it went out and came back. Out and not back is open.
	Out, In *time.Time
	Drivers []RentalDriver
	// The newest agreement made for it, if any.
	AgreementID    string
	AgreementTitle string
	AgreementAt    *time.Time
}

// Open reports whether the unit is out on it now.
func (e RegisterEntry) Open() bool { return e.Out != nil && e.In == nil }

const registerColumns = `
	b.id::text, 'R-' || b.number, u.fleet_number || coalesce(' · ' || r.name, ''),
	coalesce((SELECT g.registration FROM vehicle_registrations g
	          WHERE g.vehicle_id = r.vehicle_id AND g.valid_from <= coalesce(ho.at, b.starts_at)
	            AND (g.valid_to IS NULL OR g.valid_to > coalesce(ho.at, b.starts_at))
	          ORDER BY g.valid_from DESC LIMIT 1), ''),
	` + rentalPartyNameSQL + `,
	` + rentalState + `, b.starts_at, b.ends_at, ho.at, hi.at,
	coalesce(ag.id::text, ''), coalesce(ag.title, ''), ag.created_at`

// registerOut is the time a rental was out, from check-out to check-in
// or still, both ends counted: a check-in at the check-out's moment, or
// recorded before it by a clock set wrong, is still a rental that went out.
const registerOut = `tstzrange(least(ho.at, coalesce(hi.at, 'infinity')), greatest(ho.at, coalesce(hi.at, 'infinity')), '[]')`

const registerFrom = `
	FROM rental_bookings b
	JOIN resource_blocks k ON k.id = b.block_id
	JOIN rental_units u ON u.id = b.unit_id
	JOIN resources r ON r.id = u.resource_id
	JOIN parties pa ON pa.id = b.renter_party_id
	LEFT JOIN customers c ON c.id = pa.customer_id
	LEFT JOIN people cp ON cp.id = c.person_id
	LEFT JOIN people pp ON pp.id = pa.person_id
	LEFT JOIN rental_handovers ho ON ho.booking_id = b.id AND ho.kind = 'check_out'
	LEFT JOIN rental_handovers hi ON hi.booking_id = b.id AND hi.kind = 'check_in'
	LEFT JOIN LATERAL (SELECT d.id, d.title, d.created_at FROM document_records d
	                    WHERE d.kind = 'rental_agreement' AND d.subject_id = b.id
	                    ORDER BY d.created_at DESC LIMIT 1) ag ON true`

// registerTx reads entries and their drivers, and records the read.
func registerTx(ctx context.Context, tx pgx.Tx, scope access.Scope, where string, args []any, purpose string, detail map[string]any) ([]RegisterEntry, error) {
	rows, err := tx.Query(ctx, `SELECT `+registerColumns+registerFrom+` WHERE `+where+` ORDER BY coalesce(ho.at, b.starts_at), b.number`, args...)
	if err != nil {
		return nil, fmt.Errorf("read the register: %w", err)
	}
	var out []RegisterEntry
	for rows.Next() {
		var e RegisterEntry
		if err := rows.Scan(&e.BookingID, &e.Reference, &e.Unit, &e.Registration, &e.Renter, &e.State, &e.Starts, &e.Ends,
			&e.Out, &e.In, &e.AgreementID, &e.AgreementTitle, &e.AgreementAt); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := make([]string, len(out))
	for i, e := range out {
		ids[i] = e.BookingID
	}
	drivers, err := rentalDriversOfTx(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Drivers = drivers[out[i].BookingID]
	}
	detail["entries"] = len(out)
	if err := auditTx(ctx, tx, scope, scope.ShopID, "rental_register.read", "", "", purpose, detail); err != nil {
		return nil, err
	}
	return out, nil
}

func checkPurpose(purpose string) (string, error) {
	purpose = strings.TrimSpace(purpose)
	if purpose == "" || utf8.RuneCountInString(purpose) > 300 {
		return "", errRegisterPurpose
	}
	return purpose, nil
}

// RentalRegister reads the register over a period -- every rental that
// was out, from its check-out to its check-in or still, at some time in
// it; one booked and never handed over had nobody -- for one unit or one
// registration if the filter names it. Whoever runs the shop, with Rental on or off; the
// read is recorded with its purpose.
func RentalRegister(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, f RegisterFilter, purpose string) ([]RegisterEntry, error) {
	if !scope.Role.RunsTheShop() {
		return nil, access.ErrForbidden
	}
	purpose, err := checkPurpose(purpose)
	if err != nil {
		return nil, err
	}
	if f.From.IsZero() || !f.To.After(f.From) {
		return nil, errRegisterPeriod
	}
	if f.UnitID != "" && !looksLikeUUID(f.UnitID) {
		return nil, ErrNotFound
	}
	reg := NormaliseRegistration(f.Registration)
	var out []RegisterEntry
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = registerTx(ctx, tx, scope, `
			ho.at IS NOT NULL AND `+registerOut+` && tstzrange($1, $2)
			AND ($3 = '' OR b.unit_id::text = $3)
			AND ($4 = '' OR EXISTS (SELECT 1 FROM vehicle_registrations g WHERE g.vehicle_id = r.vehicle_id AND g.normalised = $4
			                         AND tstzrange(g.valid_from, g.valid_to) && `+registerOut+`))`,
			[]any{f.From, f.To, f.UnitID, reg}, purpose,
			map[string]any{"from": f.From, "to": f.To, "unit_id": f.UnitID, "registration": reg})
		return err
	})
	return out, err
}

// WhoHadRegistration answers who had the vehicle carrying a registration
// at a moment: the rental whose actual time -- from check-out to check-in,
// or still out -- holds it, under the plate the vehicle carried then.
// Nil when none did. Recorded as a read of the register.
func WhoHadRegistration(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, registration string, at time.Time, purpose string) (*RegisterEntry, error) {
	if !scope.Role.RunsTheShop() {
		return nil, access.ErrForbidden
	}
	purpose, err := checkPurpose(purpose)
	if err != nil {
		return nil, err
	}
	reg := NormaliseRegistration(registration)
	if reg == "" || at.IsZero() {
		return nil, errRegisterWhen
	}
	var out []RegisterEntry
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = registerTx(ctx, tx, scope, `
			ho.at <= $2 AND (hi.at IS NULL OR hi.at > $2)
			AND EXISTS (SELECT 1 FROM vehicle_registrations g WHERE g.vehicle_id = r.vehicle_id AND g.normalised = $1
			            AND g.valid_from <= $2 AND (g.valid_to IS NULL OR g.valid_to > $2))`,
			[]any{reg, at}, purpose, map[string]any{"registration": reg, "at": at})
		return err
	})
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return &out[0], nil
}

// RegisterRetention is the shop's decision on how long the register is
// kept, if it has made one.
type RegisterRetention struct {
	Months int
	Basis  string
	SetBy  string
	SetAt  time.Time
}

// RentalRegisterRetention reads the decision in force; nil while none has
// been made, which the rental settings show as missing.
func RentalRegisterRetention(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) (*RegisterRetention, error) {
	if !scope.Role.HandsOverRentals() {
		return nil, access.ErrForbidden
	}
	var out *RegisterRetention
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var r RegisterRetention
		err := tx.QueryRow(ctx, `
			SELECT x.months, x.basis, coalesce(p.display_name, ''), x.set_at
			FROM rental_register_retention x LEFT JOIN users us ON us.id = x.set_by LEFT JOIN people p ON p.id = us.person_id
			ORDER BY x.set_at DESC LIMIT 1`).Scan(&r.Months, &r.Basis, &r.SetBy, &r.SetAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read the register's retention: %w", err)
		}
		out = &r
		return nil
	})
	return out, err
}

// SetRentalRegisterRetention records how long the register is kept and
// why. Whoever runs the shop.
func SetRentalRegisterRetention(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, months int, basis string) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	basis = strings.TrimSpace(basis)
	if months < 1 || months > 240 || basis == "" || utf8.RuneCountInString(basis) > 300 {
		return errRetention
	}
	return inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO rental_register_retention (shop_id, months, basis, set_by) VALUES ($1, $2, $3, NULLIF($4, '')::uuid)`,
			scope.ShopID, months, basis, scope.UserID)
		return err
	})
}
