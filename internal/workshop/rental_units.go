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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
)

// Rental units and fleet membership (#190). A unit is a resource -- a
// registered vehicle whose history is the workshop's history of it, or a
// thing with no registration at all -- with the profile it is rented under.
// When it was in the fleet is a row per period; there is no status column:
// what a unit is doing now is read from its memberships and the
// availability ledger (UnitStatus).

// How a unit joins the fleet and how it leaves.
var (
	FleetJoinedHow = []string{"bought", "from_stock", "leased_in", "already_owned", "other"}
	FleetLeftHow   = []string{"sold", "to_stock", "lease_returned", "scrapped", "other"}
)

// What a unit is doing now, derived.
const (
	UnitOutOfFleet = "out_of_fleet"
	UnitAvailable  = "available"
	UnitRented     = "rented"
	UnitWorkshop   = "at_workshop"
	UnitBlocked    = "blocked"
)

// FleetMembership is one period a unit was in the fleet.
type FleetMembership struct {
	ID         string
	JoinedAt   time.Time
	JoinedHow  string
	JoinedBy   string
	JoinedNote string
	LeftAt     *time.Time
	LeftHow    string
	LeftBy     string
	LeftNote   string
}

// Covers reports whether the membership stands for all of from..to (to
// zero: open-ended).
func (m FleetMembership) Covers(from, to time.Time) bool {
	if from.Before(m.JoinedAt) {
		return false
	}
	if m.LeftAt == nil {
		return true
	}
	return !to.IsZero() && !to.After(*m.LeftAt)
}

// RentalUnit is a unit as the rental desk sees it. Nothing here says who
// has rented it.
type RentalUnit struct {
	ID          string
	ResourceID  string
	FleetNumber string
	Category    string
	ProfileID   string
	ProfileName string
	ProfileBase string
	Shipped     bool
	// The vehicle, if it is one, and the plate it carries now.
	VehicleID    string
	Registration string
	Make, Model  string
	// The resource's own name, for a unit that is not a vehicle.
	Name        string
	Status      string
	Memberships []FleetMembership
}

// InFleet is the membership standing now, if any.
func (u RentalUnit) InFleet() *FleetMembership {
	for i := range u.Memberships {
		if u.Memberships[i].LeftAt == nil {
			return &u.Memberships[i]
		}
	}
	return nil
}

// Label is what a person calls the unit: its fleet number and plate or
// name.
func (u RentalUnit) Label() string {
	if u.Registration != "" {
		return u.FleetNumber + " · " + u.Registration
	}
	if u.Name != "" && u.Name != u.FleetNumber {
		return u.FleetNumber + " · " + u.Name
	}
	return u.FleetNumber
}

// NewRentalUnit is a unit to add: by registration for a vehicle, or by name
// for a thing that has none.
type NewRentalUnit struct {
	Registration string
	Name         string
	ProfileID    string
	FleetNumber  string
	Category     string
}

var (
	errUnitWhat        = fmt.Errorf("%w: give the registration of a vehicle, or a name for something that has none", ErrInvalid)
	errUnitFleetNumber = fmt.Errorf("%w: give the unit a fleet number, in 40 characters or fewer", ErrInvalid)
	errUnitCategory    = fmt.Errorf("%w: give the unit a category its prices are set by, in 60 characters or fewer", ErrInvalid)
	errUnitNumberUsed  = fmt.Errorf("%w: another unit has that fleet number", ErrInvalid)
	errUnitTwice       = fmt.Errorf("%w: that is already a rental unit", ErrInvalid)
	errUnitRetired     = fmt.Errorf("%w: that has been taken out of use", ErrInvalid)
	errJoinedHow       = fmt.Errorf("%w: say how it joins the fleet", ErrInvalid)
	errLeftHow         = fmt.Errorf("%w: say how it leaves the fleet", ErrInvalid)
	errInFleetAlready  = fmt.Errorf("%w: it is in the fleet then already", ErrInvalid)
	errLeftBeforeJoin  = fmt.Errorf("%w: it leaves the fleet after it joined", ErrInvalid)
	errLeftAlready     = fmt.Errorf("%w: that period in the fleet has already ended", ErrInvalid)
	errWhenJoined      = fmt.Errorf("%w: say when it joins the fleet", ErrInvalid)
)

// FutureBookings is a refusal to end a membership while rentals are booked
// after it: each is listed, so the desk can move or cancel them first.
type FutureBookings struct {
	Blocks []Block
}

func (e *FutureBookings) Error() string {
	return fmt.Sprintf("%d rentals are booked on it after that; move or cancel them first", len(e.Blocks))
}

// Unwrap makes it an ErrInvalid, as every refusal is.
func (e *FutureBookings) Unwrap() error { return ErrInvalid }

// AsFutureBookings unwraps that refusal.
func AsFutureBookings(err error) (*FutureBookings, bool) {
	var f *FutureBookings
	ok := errors.As(err, &f)
	return f, ok
}

// AddRentalUnit makes something rentable: a vehicle found by its current
// plate, or made with it if the shop has never seen it, or a thing by its
// name. It joins the fleet separately (StartFleetMembership). The rental
// desk and whoever runs the shop.
func AddRentalUnit(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, n NewRentalUnit) (string, error) {
	if !scope.Role.HandsOverRentals() {
		return "", access.ErrForbidden
	}
	n.FleetNumber, n.Category, n.Name = strings.TrimSpace(n.FleetNumber), strings.TrimSpace(n.Category), strings.TrimSpace(n.Name)
	normalised := NormaliseRegistration(n.Registration)
	switch {
	case (normalised == "") == (n.Name == ""), utf8.RuneCountInString(n.Name) > 120, utf8.RuneCountInString(n.Registration) > 20:
		return "", errUnitWhat
	case n.FleetNumber == "" || utf8.RuneCountInString(n.FleetNumber) > 40:
		return "", errUnitFleetNumber
	case n.Category == "" || utf8.RuneCountInString(n.Category) > 60:
		return "", errUnitCategory
	case !looksLikeUUID(n.ProfileID):
		return "", ErrNotFound
	}
	var id string
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var profile bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM rental_profiles WHERE id = $1)`, n.ProfileID).Scan(&profile); err != nil || !profile {
			if err == nil {
				err = ErrNotFound
			}
			return err
		}
		resource, err := unitResourceTx(ctx, tx, scope, n.Registration, normalised, n.Name)
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO rental_units (shop_id, resource_id, profile_id, fleet_number, category, created_by)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, '')::uuid) RETURNING id`,
			scope.ShopID, resource, n.ProfileID, n.FleetNumber, n.Category, scope.UserID).Scan(&id)
		switch {
		case isUniqueViolation(err, "rental_units_fleet_number_idx"):
			return errUnitNumberUsed
		case isUniqueViolation(err, "rental_units_resource_id_key"):
			return errUnitTwice
		}
		return err
	})
	return id, err
}

// unitResourceTx finds or makes the resource a unit is: the vehicle with
// the plate now -- made, plate and all, if there is none -- or an item.
func unitResourceTx(ctx context.Context, tx pgx.Tx, scope access.Scope, registration, normalised, name string) (string, error) {
	var id string
	if normalised == "" {
		err := tx.QueryRow(ctx, `INSERT INTO resources (shop_id, kind, name, created_by) VALUES ($1, 'item', $2, NULLIF($3, '')::uuid) RETURNING id`,
			scope.ShopID, name, scope.UserID).Scan(&id)
		return id, err
	}
	var vehicle string
	err := tx.QueryRow(ctx, `SELECT vehicle_id FROM vehicle_registrations WHERE normalised = $1 AND valid_to IS NULL`, normalised).Scan(&vehicle)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.QueryRow(ctx, `INSERT INTO vehicles (shop_id) VALUES ($1) RETURNING id`, scope.ShopID).Scan(&vehicle); err != nil {
			return "", fmt.Errorf("create vehicle: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO vehicle_registrations (shop_id, vehicle_id, registration, normalised) VALUES ($1, $2, $3, $4)`,
			scope.ShopID, vehicle, strings.TrimSpace(registration), normalised); err != nil {
			return "", fmt.Errorf("record registration: %w", err)
		}
	} else if err != nil {
		return "", fmt.Errorf("look up registration: %w", err)
	}
	// The vehicle's one resource, however often it is bought and sold
	// (0070). One taken out of use is not made rentable from here.
	var retired bool
	err = tx.QueryRow(ctx, `
		INSERT INTO resources (shop_id, kind, vehicle_id, created_by) VALUES ($1, 'vehicle', $2, NULLIF($3, '')::uuid)
		ON CONFLICT (vehicle_id) DO NOTHING RETURNING id, false`, scope.ShopID, vehicle, scope.UserID).Scan(&id, &retired)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `SELECT id, retired_at IS NOT NULL FROM resources WHERE vehicle_id = $1`, vehicle).Scan(&id, &retired)
	}
	if err != nil {
		return "", fmt.Errorf("the vehicle's resource: %w", err)
	}
	if retired {
		return "", errUnitRetired
	}
	return id, nil
}

// StartFleetMembership puts a unit in the fleet from a moment. Refused while
// it is in the fleet then already -- the database refuses it too, for the
// unit and for its vehicle.
func StartFleetMembership(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, unitID string, from time.Time, how, note string) (string, error) {
	if !scope.Role.HandsOverRentals() {
		return "", access.ErrForbidden
	}
	if !looksLikeUUID(unitID) {
		return "", ErrNotFound
	}
	if from.IsZero() {
		return "", errWhenJoined
	}
	if !slices.Contains(FleetJoinedHow, how) {
		return "", errJoinedHow
	}
	note, err := checkNote(note)
	if err != nil {
		return "", err
	}
	var id string
	err = inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var vehicle *string
		err := tx.QueryRow(ctx, `
			SELECT r.vehicle_id::text FROM rental_units u JOIN resources r ON r.id = u.resource_id WHERE u.id = $1`, unitID).Scan(&vehicle)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read unit: %w", err)
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO fleet_memberships (shop_id, unit_id, vehicle_id, joined_at, joined_how, joined_by, joined_note)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, '')::uuid, nullif($7, '')) RETURNING id`,
			scope.ShopID, unitID, vehicle, from, how, scope.UserID, note).Scan(&id)
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23P01" {
			return errInFleetAlready
		}
		return err
	})
	return id, err
}

// EndFleetMembership ends a unit's period in the fleet at a moment. Refused,
// listing them, while rentals are booked or held on it after then; what was
// rented before stays as it was.
func EndFleetMembership(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, membershipID string, at time.Time, how, note string) error {
	if !scope.Role.HandsOverRentals() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(membershipID) {
		return ErrNotFound
	}
	if !slices.Contains(FleetLeftHow, how) {
		return errLeftHow
	}
	note, err := checkNote(note)
	if err != nil {
		return err
	}
	return inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var joined time.Time
		var left *time.Time
		var resource string
		err := tx.QueryRow(ctx, `
			SELECT m.joined_at, m.left_at, u.resource_id FROM fleet_memberships m JOIN rental_units u ON u.id = m.unit_id
			WHERE m.id = $1 FOR UPDATE OF m`, membershipID).Scan(&joined, &left, &resource)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read membership: %w", err)
		}
		// One at a time with a booking of the unit, which takes the same
		// row: whichever is second sees what the first did.
		if err := lockUnitResourceTx(ctx, tx, resource); err != nil {
			return err
		}
		switch {
		case left != nil:
			return errLeftAlready
		case at.IsZero() || !at.After(joined):
			return errLeftBeforeJoin
		}
		if _, err := tx.Exec(ctx, `SELECT release_expired_holds($1, tstzrange($2, NULL))`, resource, at); err != nil {
			return fmt.Errorf("release expired holds: %w", err)
		}
		ahead, err := rentalBlocksAfterTx(ctx, tx, resource, at)
		if err != nil {
			return err
		}
		if len(ahead) > 0 {
			return &FutureBookings{Blocks: ahead}
		}
		_, err = tx.Exec(ctx, `
			UPDATE fleet_memberships SET left_at = $2, left_how = $3, left_by = NULLIF($4, '')::uuid, left_note = nullif($5, '')
			WHERE id = $1`, membershipID, at, how, scope.UserID, note)
		return err
	})
}

// rentalBlocksAfterTx lists the rentals booked or held on a resource that
// run past at.
func rentalBlocksAfterTx(ctx context.Context, tx pgx.Tx, resource string, at time.Time) ([]Block, error) {
	rows, err := tx.Query(ctx, `
		SELECT id::text, kind, status, lower(period), upper(period), expires_at, coalesce(source_kind, ''), coalesce(source_id::text, '')
		FROM resource_blocks
		WHERE resource_id = $1 AND kind = 'rental' AND status IN ('held', 'confirmed') AND period && tstzrange($2, NULL)
		ORDER BY lower(period)`, resource, at)
	if err != nil {
		return nil, fmt.Errorf("read what is booked: %w", err)
	}
	defer rows.Close()
	var out []Block
	for rows.Next() {
		var b Block
		if err := rows.Scan(&b.ID, &b.Kind, &b.Status, &b.From, &b.To, &b.HeldUntil, &b.SourceKind, &b.SourceID); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

const unitColumns = `
	u.id, u.resource_id, u.fleet_number, u.category, p.id, p.name, p.base, p.created_by IS NULL,
	coalesce(r.vehicle_id::text, ''), coalesce(reg.registration, ''), coalesce(v.make, ''), coalesce(v.model, ''),
	coalesce(r.name, ''),
	CASE
	  WHEN NOT EXISTS (SELECT 1 FROM fleet_memberships m WHERE m.unit_id = u.id
	                    AND tstzrange(m.joined_at, m.left_at) @> clock_timestamp()) THEN 'out_of_fleet'
	  ELSE coalesce((SELECT CASE b.kind WHEN 'rental' THEN 'rented' WHEN 'workshop' THEN 'at_workshop' ELSE 'blocked' END
	                 FROM resource_blocks b
	                 WHERE b.resource_id = u.resource_id AND b.period @> clock_timestamp()
	                   AND (b.status = 'confirmed' OR (b.status = 'held' AND b.expires_at > clock_timestamp()))
	                 ORDER BY b.kind = 'rental' DESC LIMIT 1), 'available')
	END`

const unitFrom = `
	FROM rental_units u
	JOIN rental_profiles p ON p.id = u.profile_id
	JOIN resources r ON r.id = u.resource_id
	LEFT JOIN vehicles v ON v.id = r.vehicle_id
	LEFT JOIN vehicle_registrations reg ON reg.vehicle_id = r.vehicle_id AND reg.valid_to IS NULL`

func scanUnit(row pgx.Row) (RentalUnit, error) {
	var u RentalUnit
	err := row.Scan(&u.ID, &u.ResourceID, &u.FleetNumber, &u.Category, &u.ProfileID, &u.ProfileName, &u.ProfileBase, &u.Shipped,
		&u.VehicleID, &u.Registration, &u.Make, &u.Model, &u.Name, &u.Status)
	return u, err
}

// RentalUnits lists the shop's units with what each is doing now, in or out
// of the fleet. The rental desk and whoever runs the shop.
func RentalUnits(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]RentalUnit, error) {
	if !scope.Role.HandsOverRentals() {
		return nil, access.ErrForbidden
	}
	var out []RentalUnit
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+unitColumns+unitFrom+` ORDER BY lower(u.fleet_number), u.id`)
		if err != nil {
			return fmt.Errorf("read units: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			u, err := scanUnit(rows)
			if err != nil {
				return fmt.Errorf("scan unit: %w", err)
			}
			out = append(out, u)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range out {
			if out[i].Memberships, err = membershipsTx(ctx, tx, out[i].ID); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// RentalUnitByID reads one unit with every period it was in the fleet.
func RentalUnitByID(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) (RentalUnit, error) {
	if !scope.Role.HandsOverRentals() {
		return RentalUnit{}, access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return RentalUnit{}, ErrNotFound
	}
	var u RentalUnit
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		u, err = unitTx(ctx, tx, id)
		return err
	})
	return u, err
}

func unitTx(ctx context.Context, tx pgx.Tx, id string) (RentalUnit, error) {
	u, err := scanUnit(tx.QueryRow(ctx, `SELECT `+unitColumns+unitFrom+` WHERE u.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return u, ErrNotFound
	}
	if err != nil {
		return u, fmt.Errorf("read unit: %w", err)
	}
	u.Memberships, err = membershipsTx(ctx, tx, id)
	return u, err
}

func membershipsTx(ctx context.Context, tx pgx.Tx, unitID string) ([]FleetMembership, error) {
	rows, err := tx.Query(ctx, `
		SELECT m.id, m.joined_at, m.joined_how, coalesce(pj.display_name, ''), coalesce(m.joined_note, ''),
		       m.left_at, coalesce(m.left_how, ''), coalesce(pl.display_name, ''), coalesce(m.left_note, '')
		FROM fleet_memberships m
		LEFT JOIN users uj ON uj.id = m.joined_by LEFT JOIN people pj ON pj.id = uj.person_id
		LEFT JOIN users ul ON ul.id = m.left_by LEFT JOIN people pl ON pl.id = ul.person_id
		WHERE m.unit_id = $1 ORDER BY m.joined_at DESC`, unitID)
	if err != nil {
		return nil, fmt.Errorf("read memberships: %w", err)
	}
	defer rows.Close()
	var out []FleetMembership
	for rows.Next() {
		var m FleetMembership
		if err := rows.Scan(&m.ID, &m.JoinedAt, &m.JoinedHow, &m.JoinedBy, &m.JoinedNote, &m.LeftAt, &m.LeftHow, &m.LeftBy, &m.LeftNote); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
