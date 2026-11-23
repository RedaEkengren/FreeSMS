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
)

// Tyre sets (#226): a set is its own thing with a history, not a note on the
// customer. It has an owner, a season, rims, the tyres on it (each its own
// row, so one replaced keeps its history) and, as an event, the car it is on.

// The seasons a set is for.
const (
	SeasonSummer         = "summer"
	SeasonWinterStudded  = "winter_studded"
	SeasonWinterFriction = "winter_friction"
	SeasonAllSeason      = "all_season"
)

// TyreSeasons are the seasons, in the order a form offers them.
var TyreSeasons = []string{SeasonSummer, SeasonWinterStudded, SeasonWinterFriction, SeasonAllSeason}

// The rims a set has.
const (
	RimsNone  = "none"
	RimsSteel = "steel"
	RimsAlloy = "alloy"
)

// TyreRims are the kinds of rims, in the order a form offers them.
var TyreRims = []string{RimsNone, RimsSteel, RimsAlloy}

// TyrePositions are where a tyre sits, in the order a form offers them.
var TyrePositions = []string{"front_left", "front_right", "rear_left", "rear_right", "spare", "other"}

// NewTyre is one tyre of a set. DotYear and DotWeek are its date of
// manufacture, both or neither.
type NewTyre struct {
	Position  string
	Dimension string
	DotWeek   int
	DotYear   int
}

// NewTyreSet is a set being taken in. VehicleID is empty for a set brought in
// loose, or one that has outlived its car.
type NewTyreSet struct {
	CustomerID string
	VehicleID  string
	Season     string
	Brand      string
	Model      string
	Rims       string
	TPMS       bool
	Fasteners  bool
	Note       string
	Tyres      []NewTyre
}

// Tyre is one tyre as it stands on its set.
type Tyre struct {
	ID        string
	Position  string
	Dimension string
	// Zero when the date is not known.
	DotWeek    int
	DotYear    int
	ReplacesID string
}

// TyreMove is where a set has been: a car, or off any car.
type TyreMove struct {
	VehicleID    string
	Registration string
	At           time.Time
}

// TyreSet is a set as whoever reads it may see it: an owner only for the
// roles that see customers' details, a technician getting the set and the car.
type TyreSet struct {
	ID         string
	Season     string
	Brand      string
	Model      string
	Rims       string
	TPMS       bool
	Fasteners  bool
	Note       string
	CreatedAt  time.Time
	CustomerID string
	Owner      string
	VehicleID  string
	// The car it is on now, in words.
	Vehicle string
	Tyres   []Tyre
	// Where it has been, oldest first. Only on a set read alone.
	Moves []TyreMove
}

var (
	errTyreSet     = fmt.Errorf("%w: a set has a season and rims from the lists, a brand and a model of at most 100 characters each, a note of at most 500 and from one to twelve tyres", ErrInvalid)
	errTyre        = fmt.Errorf("%w: a tyre has a place on the car, a dimension of 3 to 40 characters and, when its date is known, a week from 1 to 53 and a year from 1990 to 2100", ErrInvalid)
	errTyreVehicle = fmt.Errorf("%w: a set is put on a car its owner has", ErrInvalid)
)

func checkTyre(t *NewTyre) error {
	t.Dimension = strings.TrimSpace(t.Dimension)
	if !slices.Contains(TyrePositions, t.Position) || utf8.RuneCountInString(t.Dimension) < 3 || utf8.RuneCountInString(t.Dimension) > 40 {
		return errTyre
	}
	if (t.DotWeek == 0) != (t.DotYear == 0) || (t.DotWeek != 0 && (t.DotWeek < 1 || t.DotWeek > 53 || t.DotYear < 1990 || t.DotYear > 2100)) {
		return errTyre
	}
	return nil
}

func insertTyreTx(ctx context.Context, tx pgx.Tx, scope access.Scope, setID, replaces string, t NewTyre) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO tyres (shop_id, tyre_set_id, position, dimension, dot_week, dot_year, replaces_id, created_by)
		VALUES ($1, $2, $3, $4, nullif($5, 0), nullif($6, 0), nullif($7, '')::uuid, NULLIF($8, '')::uuid)`,
		scope.ShopID, setID, t.Position, t.Dimension, t.DotWeek, t.DotYear, replaces, scope.UserID)
	return err
}

// ownsVehicleTx reports whether the customer has the car now.
func ownsVehicleTx(ctx context.Context, tx pgx.Tx, customerID, vehicleID string) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM vehicle_ownership WHERE vehicle_id = $1 AND customer_id = $2 AND owned_to IS NULL)`,
		vehicleID, customerID).Scan(&ok)
	return ok, err
}

// CreateTyreSet takes a set in. It is on the car given, which its owner must
// have, or on none. For whoever deals with customers.
func CreateTyreSet(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, n NewTyreSet) (string, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return "", access.ErrForbidden
	}
	if !looksLikeUUID(n.CustomerID) || (n.VehicleID != "" && !looksLikeUUID(n.VehicleID)) {
		return "", ErrNotFound
	}
	n.Brand, n.Model, n.Note = strings.TrimSpace(n.Brand), strings.TrimSpace(n.Model), strings.TrimSpace(n.Note)
	if !slices.Contains(TyreSeasons, n.Season) || !slices.Contains(TyreRims, n.Rims) || utf8.RuneCountInString(n.Brand) > 100 ||
		utf8.RuneCountInString(n.Model) > 100 || utf8.RuneCountInString(n.Note) > 500 || len(n.Tyres) < 1 || len(n.Tyres) > 12 {
		return "", errTyreSet
	}
	for i := range n.Tyres {
		if err := checkTyre(&n.Tyres[i]); err != nil {
			return "", err
		}
	}
	var id string
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM customers WHERE id = $1)`, n.CustomerID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		if n.VehicleID != "" {
			ok, err := ownsVehicleTx(ctx, tx, n.CustomerID, n.VehicleID)
			if err != nil {
				return err
			}
			if !ok {
				return errTyreVehicle
			}
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO tyre_sets (shop_id, customer_id, season, brand, model, rims, tpms, fasteners, note, created_by)
			VALUES ($1, $2, $3, nullif($4, ''), nullif($5, ''), $6, $7, $8, nullif($9, ''), NULLIF($10, '')::uuid) RETURNING id`,
			scope.ShopID, n.CustomerID, n.Season, n.Brand, n.Model, n.Rims, n.TPMS, n.Fasteners, n.Note, scope.UserID).Scan(&id); err != nil {
			return fmt.Errorf("create the set: %w", err)
		}
		for _, t := range n.Tyres {
			if err := insertTyreTx(ctx, tx, scope, id, "", t); err != nil {
				return fmt.Errorf("add a tyre: %w", err)
			}
		}
		if n.VehicleID != "" {
			return putTyreSetTx(ctx, tx, scope, id, n.VehicleID)
		}
		return nil
	})
	return id, err
}

func putTyreSetTx(ctx context.Context, tx pgx.Tx, scope access.Scope, setID, vehicleID string) error {
	_, err := tx.Exec(ctx, `INSERT INTO tyre_set_vehicle_events (shop_id, tyre_set_id, vehicle_id, by_user) VALUES ($1, $2, NULLIF($3, '')::uuid, NULLIF($4, '')::uuid)`,
		scope.ShopID, setID, vehicleID, scope.UserID)
	return err
}

// ReplaceTyre puts a new tyre where one stood, the old one staying as it was
// with the new pointing at it. For whoever deals with customers.
func ReplaceTyre(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, tyreID string, n NewTyre) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(tyreID) {
		return ErrNotFound
	}
	if err := checkTyre(&n); err != nil {
		return err
	}
	return inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var set string
		var replaced bool
		if err := tx.QueryRow(ctx, `SELECT t.tyre_set_id, EXISTS (SELECT 1 FROM tyres r WHERE r.replaces_id = t.id) FROM tyres t WHERE t.id = $1`, tyreID).
			Scan(&set, &replaced); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		// The set is locked, so two replacements of one tyre are told apart.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, set); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tyres r WHERE r.replaces_id = $1)`, tyreID).Scan(&replaced); err != nil {
			return err
		}
		if replaced {
			return errTyreReplaced
		}
		return insertTyreTx(ctx, tx, scope, set, tyreID, n)
	})
}

var errTyreReplaced = fmt.Errorf("%w: that tyre has already been replaced", ErrInvalid)

// MoveTyreSet puts a set on another car of its owner, or on none with an empty
// vehicleID. The earlier place stays in its history. For whoever deals with
// customers.
func MoveTyreSet(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, setID, vehicleID string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(setID) || (vehicleID != "" && !looksLikeUUID(vehicleID)) {
		return ErrNotFound
	}
	return inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var customer string
		if err := tx.QueryRow(ctx, `SELECT customer_id FROM tyre_sets WHERE id = $1`, setID).Scan(&customer); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if vehicleID != "" {
			ok, err := ownsVehicleTx(ctx, tx, customer, vehicleID)
			if err != nil {
				return err
			}
			if !ok {
				return errTyreVehicle
			}
		}
		return putTyreSetTx(ctx, tx, scope, setID, vehicleID)
	})
}

// TyreFilter narrows the sets read.
type TyreFilter struct {
	CustomerID string
	VehicleID  string
}

const tyreSetColumns = `
	SELECT s.id, s.season, coalesce(s.brand, ''), coalesce(s.model, ''), s.rims, s.tpms, s.fasteners, coalesce(s.note, ''), s.created_at,
	       CASE WHEN $1 THEN s.customer_id::text ELSE '' END,
	       CASE WHEN $1 THEN coalesce(CASE WHEN c.kind = 'company' THEN c.company_name END, p.display_name, '') ELSE '' END,
	       coalesce(m.vehicle_id::text, ''), coalesce(nullif(btrim(concat_ws(' ', r.registration, v.make, v.model)), ''), '')
	FROM tyre_sets s
	JOIN customers c ON c.id = s.customer_id
	LEFT JOIN people p ON p.id = c.person_id
	LEFT JOIN LATERAL (SELECT e.vehicle_id FROM tyre_set_vehicle_events e WHERE e.tyre_set_id = s.id ORDER BY e.seq DESC LIMIT 1) m ON true
	LEFT JOIN vehicles v ON v.id = m.vehicle_id
	LEFT JOIN vehicle_registrations r ON r.vehicle_id = v.id AND r.valid_to IS NULL`

func scanTyreSet(row pgx.Row) (TyreSet, error) {
	var s TyreSet
	err := row.Scan(&s.ID, &s.Season, &s.Brand, &s.Model, &s.Rims, &s.TPMS, &s.Fasteners, &s.Note, &s.CreatedAt,
		&s.CustomerID, &s.Owner, &s.VehicleID, &s.Vehicle)
	return s, err
}

// currentTyresTx reads the tyres now on the sets, the ones not replaced.
func currentTyresTx(ctx context.Context, tx pgx.Tx, sets []TyreSet) error {
	if len(sets) == 0 {
		return nil
	}
	ids := make([]string, len(sets))
	at := map[string]int{}
	for i, s := range sets {
		ids[i], at[s.ID] = s.ID, i
	}
	rows, err := tx.Query(ctx, `
		SELECT t.tyre_set_id, t.id, t.position, t.dimension, coalesce(t.dot_week, 0), coalesce(t.dot_year, 0), coalesce(t.replaces_id::text, '')
		FROM tyres t
		WHERE t.tyre_set_id = ANY($1::uuid[]) AND NOT EXISTS (SELECT 1 FROM tyres r WHERE r.replaces_id = t.id)
		ORDER BY array_position(ARRAY['front_left','front_right','rear_left','rear_right','spare','other'], t.position), t.created_at`, ids)
	if err != nil {
		return fmt.Errorf("read the tyres: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var set string
		var t Tyre
		if err := rows.Scan(&set, &t.ID, &t.Position, &t.Dimension, &t.DotWeek, &t.DotYear, &t.ReplacesID); err != nil {
			return err
		}
		sets[at[set]].Tyres = append(sets[at[set]].Tyres, t)
	}
	return rows.Err()
}

// TyreSetsOf lists the sets, the newest first. Whoever works in the
// workshop reads them; the owner is named only for the roles that see
// customers' details.
func TyreSetsOf(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, f TyreFilter) ([]TyreSet, error) {
	if !scope.Role.WorksInTheWorkshop() {
		return nil, access.ErrForbidden
	}
	if (f.CustomerID != "" && !looksLikeUUID(f.CustomerID)) || (f.VehicleID != "" && !looksLikeUUID(f.VehicleID)) {
		return nil, ErrNotFound
	}
	var out []TyreSet
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, tyreSetColumns+`
			WHERE ($2 = '' OR s.customer_id = NULLIF($2, '')::uuid) AND ($3 = '' OR m.vehicle_id = NULLIF($3, '')::uuid)
			ORDER BY s.created_at DESC, s.id`, scope.Role.SeesCustomerPersonalData(), f.CustomerID, f.VehicleID)
		if err != nil {
			return fmt.Errorf("read the sets: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			s, err := scanTyreSet(rows)
			if err != nil {
				return err
			}
			out = append(out, s)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		return currentTyresTx(ctx, tx, out)
	})
	return out, err
}

// TyreSetByID reads one set with where it has been.
func TyreSetByID(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) (TyreSet, error) {
	if !scope.Role.WorksInTheWorkshop() {
		return TyreSet{}, access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return TyreSet{}, ErrNotFound
	}
	var out TyreSet
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		s, err := scanTyreSet(tx.QueryRow(ctx, tyreSetColumns+` WHERE s.id = $2`, scope.Role.SeesCustomerPersonalData(), id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read the set: %w", err)
		}
		sets := []TyreSet{s}
		if err := currentTyresTx(ctx, tx, sets); err != nil {
			return err
		}
		s = sets[0]
		rows, err := tx.Query(ctx, `
			SELECT coalesce(e.vehicle_id::text, ''), coalesce(nullif(btrim(concat_ws(' ', r.registration, v.make, v.model)), ''), ''), e.at
			FROM tyre_set_vehicle_events e
			LEFT JOIN vehicles v ON v.id = e.vehicle_id
			LEFT JOIN vehicle_registrations r ON r.vehicle_id = v.id AND r.valid_to IS NULL
			WHERE e.tyre_set_id = $1 ORDER BY e.seq`, id)
		if err != nil {
			return fmt.Errorf("read the moves: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var m TyreMove
			if err := rows.Scan(&m.VehicleID, &m.Registration, &m.At); err != nil {
				return err
			}
			s.Moves = append(s.Moves, m)
		}
		out = s
		return rows.Err()
	})
	return out, err
}
