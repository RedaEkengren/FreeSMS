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
)

// Storage places for tyre sets (#228): a place has a name and a capacity in
// sets, a set is in at most one place at a time, and where it has been is the
// rows of its placements.

var (
	errStoragePlace   = fmt.Errorf("%w: a storage place has a name of 1 to 100 characters, not used by another, and room for from 1 to 1000 sets", ErrInvalid)
	errStorageFull    = fmt.Errorf("%w: that place is full", ErrInvalid)
	errStorageRetired = fmt.Errorf("%w: that place is not in use", ErrInvalid)
	errStorageInUse   = fmt.Errorf("%w: sets are still in that place; move them first", ErrInvalid)
	errStorageState   = fmt.Errorf("%w: a place is retired while in use and reopened while retired", ErrInvalid)
	errNotStored      = fmt.Errorf("%w: that set is not in storage", ErrInvalid)
)

// StoredSet is a set in a place, as whoever reads it may see it: the owner
// only for the roles that see customers' details.
type StoredSet struct {
	SetID   string
	Season  string
	Brand   string
	Model   string
	Owner   string
	Vehicle string
	Since   time.Time
}

// StorageLocation is a place with what is in it.
type StorageLocation struct {
	ID       string
	Name     string
	Capacity int
	Active   bool
	Sets     []StoredSet
}

// Free is the room left in the place.
func (l StorageLocation) Free() int { return l.Capacity - len(l.Sets) }

// CreateStorageLocation makes a place. Whoever runs the shop.
func CreateStorageLocation(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, name string, capacity int) (string, error) {
	if !scope.Role.RunsTheShop() {
		return "", access.ErrForbidden
	}
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) < 1 || utf8.RuneCountInString(name) > 100 || capacity < 1 || capacity > 1000 {
		return "", errStoragePlace
	}
	var id string
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO storage_locations (shop_id, name, capacity, created_by) VALUES ($1, $2, $3, NULLIF($4, '')::uuid)
			ON CONFLICT (shop_id, name) DO NOTHING RETURNING id`, scope.ShopID, name, capacity, scope.UserID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return errStoragePlace
		}
		return err
	})
	return id, err
}

// locationActiveTx reads whether a place is in use, and locks it for the
// transaction so that two sets going in at once are counted one after the
// other.
func locationActiveTx(ctx context.Context, tx pgx.Tx, id string) (bool, int, error) {
	var capacity int
	if err := tx.QueryRow(ctx, `SELECT capacity FROM storage_locations WHERE id = $1`, id).Scan(&capacity); errors.Is(err, pgx.ErrNoRows) {
		return false, 0, ErrNotFound
	} else if err != nil {
		return false, 0, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "storage:"+id); err != nil {
		return false, 0, err
	}
	var last *string
	if err := tx.QueryRow(ctx, `SELECT kind FROM storage_location_events WHERE location_id = $1 ORDER BY seq DESC LIMIT 1`, id).Scan(&last); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, 0, err
	}
	return last == nil || *last == "reopened", capacity, nil
}

// SetStorageLocationInUse retires a place, which must be empty, or opens a
// retired one again. Whoever runs the shop.
func SetStorageLocationInUse(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string, inUse bool) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return ErrNotFound
	}
	return inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		active, _, err := locationActiveTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if active == inUse {
			return errStorageState
		}
		if !inUse {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM tyre_set_placements WHERE location_id = $1 AND removed_at IS NULL`, id).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return errStorageInUse
			}
		}
		kind := "reopened"
		if !inUse {
			kind = "retired"
		}
		_, err = tx.Exec(ctx, `INSERT INTO storage_location_events (shop_id, location_id, kind, by_user) VALUES ($1, $2, $3, NULLIF($4, '')::uuid)`,
			scope.ShopID, id, kind, scope.UserID)
		return err
	})
}

// PlaceTyreSet puts a set in a place, taking it out of the one it was in.
// The place must be in use and have room. Whoever works in the workshop.
func PlaceTyreSet(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, setID, locationID string) error {
	if !scope.Role.WorksInTheWorkshop() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(setID) || !looksLikeUUID(locationID) {
		return ErrNotFound
	}
	return inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return placeTx(ctx, tx, scope, setID, locationID)
	})
}

// placeTx puts a set in a place within a transaction.
func placeTx(ctx context.Context, tx pgx.Tx, scope access.Scope, setID, locationID string) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tyre_sets WHERE id = $1)`, setID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	active, capacity, err := locationActiveTx(ctx, tx, locationID)
	if err != nil {
		return err
	}
	if !active {
		return errStorageRetired
	}
	// The set's own place, if it has one.
	var from, fromName *string
	if err := tx.QueryRow(ctx, `SELECT p.location_id::text, l.name FROM tyre_set_placements p JOIN storage_locations l ON l.id = p.location_id
		WHERE p.tyre_set_id = $1 AND p.removed_at IS NULL`, setID).Scan(&from, &fromName); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if from != nil && *from == locationID {
		return nil
	}
	var used int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM tyre_set_placements WHERE location_id = $1 AND removed_at IS NULL`, locationID).Scan(&used); err != nil {
		return err
	}
	if used >= capacity {
		return errStorageFull
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	if from != nil {
		if _, err := tx.Exec(ctx, `UPDATE tyre_set_placements SET removed_at = $2, removed_by = NULLIF($3, '')::uuid WHERE tyre_set_id = $1 AND removed_at IS NULL`,
			setID, now, scope.UserID); err != nil {
			return fmt.Errorf("close the earlier place: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO tyre_set_placements (shop_id, tyre_set_id, location_id, placed_at, placed_by) VALUES ($1, $2, $3, $4, NULLIF($5, '')::uuid)`,
		scope.ShopID, setID, locationID, now, scope.UserID); err != nil {
		return fmt.Errorf("place the set: %w", err)
	}
	var to string
	if err := tx.QueryRow(ctx, `SELECT name FROM storage_locations WHERE id = $1`, locationID).Scan(&to); err != nil {
		return err
	}
	fromText := ""
	if fromName != nil {
		fromText = *fromName
	}
	return auditTx(ctx, tx, scope, setID, "tyre_set.placed", fromText, to, "", map[string]any{"location_id": locationID})
}

// TakeTyreSetOutOfStorage records a set leaving its place. Whoever works in the
// workshop.
func TakeTyreSetOutOfStorage(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, setID string) error {
	if !scope.Role.WorksInTheWorkshop() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(setID) {
		return ErrNotFound
	}
	return inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return takeOutTx(ctx, tx, scope, setID)
	})
}

// takeOutTx records a set leaving its place within a transaction.
func takeOutTx(ctx context.Context, tx pgx.Tx, scope access.Scope, setID string) error {
	var name string
	err := tx.QueryRow(ctx, `
		UPDATE tyre_set_placements p SET removed_at = clock_timestamp(), removed_by = NULLIF($2, '')::uuid
		FROM storage_locations l
		WHERE p.tyre_set_id = $1 AND p.removed_at IS NULL AND l.id = p.location_id RETURNING l.name`, setID, scope.UserID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tyre_sets WHERE id = $1)`, setID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		return errNotStored
	}
	if err != nil {
		return err
	}
	return auditTx(ctx, tx, scope, setID, "tyre_set.taken_out", name, "", "", nil)
}

// StorageLocations lists the places by name with the sets in each. For whoever
// works in the workshop; the owner is named only for the roles that see
// customers' details.
func StorageLocations(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]StorageLocation, error) {
	if !scope.Role.WorksInTheWorkshop() {
		return nil, access.ErrForbidden
	}
	var out []StorageLocation
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT l.id, l.name, l.capacity, coalesce(e.kind, 'reopened') = 'reopened'
			FROM storage_locations l
			LEFT JOIN LATERAL (SELECT x.kind FROM storage_location_events x WHERE x.location_id = l.id ORDER BY x.seq DESC LIMIT 1) e ON true
			ORDER BY l.name`)
		if err != nil {
			return fmt.Errorf("read the places: %w", err)
		}
		defer rows.Close()
		at := map[string]int{}
		for rows.Next() {
			var l StorageLocation
			if err := rows.Scan(&l.ID, &l.Name, &l.Capacity, &l.Active); err != nil {
				return err
			}
			at[l.ID] = len(out)
			out = append(out, l)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		sets, err := tx.Query(ctx, `
			SELECT p.location_id, s.id, s.season, coalesce(s.brand, ''), coalesce(s.model, ''),
			       CASE WHEN $1 THEN coalesce(CASE WHEN c.kind = 'company' THEN c.company_name END, pe.display_name, '') ELSE '' END,
			       coalesce(nullif(btrim(concat_ws(' ', r.registration, v.make, v.model)), ''), ''), p.placed_at
			FROM tyre_set_placements p
			JOIN tyre_sets s ON s.id = p.tyre_set_id
			JOIN customers c ON c.id = s.customer_id
			LEFT JOIN people pe ON pe.id = c.person_id
			LEFT JOIN LATERAL (SELECT e.vehicle_id FROM tyre_set_vehicle_events e WHERE e.tyre_set_id = s.id ORDER BY e.seq DESC LIMIT 1) m ON true
			LEFT JOIN vehicles v ON v.id = m.vehicle_id
			LEFT JOIN vehicle_registrations r ON r.vehicle_id = v.id AND r.valid_to IS NULL
			WHERE p.removed_at IS NULL ORDER BY p.placed_at, s.id`, scope.Role.SeesCustomerPersonalData())
		if err != nil {
			return fmt.Errorf("read the sets in storage: %w", err)
		}
		defer sets.Close()
		for sets.Next() {
			var loc string
			var s StoredSet
			if err := sets.Scan(&loc, &s.SetID, &s.Season, &s.Brand, &s.Model, &s.Owner, &s.Vehicle, &s.Since); err != nil {
				return err
			}
			out[at[loc]].Sets = append(out[at[loc]].Sets, s)
		}
		return sets.Err()
	})
	return out, err
}

// A label carries a short code the shop's scanners read as keyboard input:
// two letters for what it is and the first twelve hexadecimal digits of its
// identifier. Twelve digits of a random identifier do not clash inside one
// shop, and a code that matched two rows is refused rather than guessed.
const (
	labelSet   = "TS"
	labelPlace = "SL"
)

func labelCode(prefix, id string) string {
	h := strings.ToUpper(strings.ReplaceAll(id, "-", ""))
	if len(h) < 12 {
		return ""
	}
	return prefix + "-" + h[:12]
}

// TyreSetLabel is the code on a set's label.
func TyreSetLabel(id string) string { return labelCode(labelSet, id) }

// StorageLabel is the code on a place's label.
func StorageLabel(id string) string { return labelCode(labelPlace, id) }

// ResolveStorageLabel says what a scanned code is: "set" or "place", and its
// identifier. A code that is neither, or that matches more than one row, is
// not found. Whoever works in the workshop.
func ResolveStorageLabel(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, code string) (kind, id string, err error) {
	if !scope.Role.WorksInTheWorkshop() {
		return "", "", access.ErrForbidden
	}
	prefix, hex, ok := strings.Cut(strings.ToUpper(strings.TrimSpace(code)), "-")
	if !ok || len(hex) != 12 || strings.Trim(hex, "0123456789ABCDEF") != "" {
		return "", "", ErrNotFound
	}
	table, kind := "tyre_sets", "set"
	switch prefix {
	case labelSet:
	case labelPlace:
		table, kind = "storage_locations", "place"
	default:
		return "", "", ErrNotFound
	}
	err = inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM `+table+` WHERE upper(replace(id::text, '-', '')) LIKE $1 || '%' LIMIT 2`, hex)
		if err != nil {
			return err
		}
		defer rows.Close()
		var found []string
		for rows.Next() {
			var x string
			if err := rows.Scan(&x); err != nil {
				return err
			}
			found = append(found, x)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(found) != 1 {
			return ErrNotFound
		}
		id = found[0]
		return nil
	})
	if err != nil {
		return "", "", err
	}
	return kind, id, nil
}
