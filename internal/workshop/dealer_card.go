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

// The rest of a stock car's card (#167): its damage, found and repaired;
// its keys and where each went; its service book. Each is rows, written
// while the car is in stock and read for as long as its history is kept.

// The damage map's zones and severities, the kinds of key and how a key
// moves.
var (
	DamageZones      = []string{"front", "rear", "left", "right", "roof", "interior", "underbody", "glass", "wheels"}
	DamageSeverities = []string{"cosmetic", "minor", "major"}
	KeyKinds         = []string{"key", "remote", "card", "spare", "other"}
	KeyMoves         = []string{"out", "back", "lost", "found"}
)

// Damage is one damage on the car, and its repair once repaired.
type Damage struct {
	ID          string
	Zone        string
	Description string
	Severity    string
	FoundOn     time.Time
	RepairedOn  *time.Time
	RepairNote  string
}

// Key is one key and where it is now: its newest movement.
type Key struct {
	ID     string
	Kind   string
	Label  string
	Where  string // "" (here), out, lost
	Holder string
	Since  *time.Time
}

// ServiceEntry is one entry in the service book.
type ServiceEntry struct {
	DoneOn     time.Time
	OdometerKm *int64
	DoneBy     string
	What       string
}

// VehicleCard is a car's damage, keys and service book.
type VehicleCard struct {
	Damages []Damage
	Keys    []Key
	Service []ServiceEntry
}

var (
	errDamage  = fmt.Errorf("%w: a damage is where on the car, what it is in 500 characters or fewer, how bad, and the day it was found", ErrInvalid)
	errKey     = fmt.Errorf("%w: a key is of a kind, with a label of 100 characters or fewer", ErrInvalid)
	errKeyMove = fmt.Errorf("%w: a key goes out to someone, comes back, is lost or is found", ErrInvalid)
	errService = fmt.Errorf("%w: a service entry is a day, who did it and what, and the odometer if it was read", ErrInvalid)
	errNoStock = fmt.Errorf("%w: the car's card is written while it is in stock", ErrInvalid)
)

// cardWriteTx checks the roles and the module for writing a car's card,
// and that the car is in stock now, locking its cycle.
func cardWriteTx(ctx context.Context, tx pgx.Tx, vehicleID string) error {
	var unit string
	err := tx.QueryRow(ctx, `SELECT id FROM stock_units WHERE vehicle_id = vehicle_now($1) AND closed_at IS NULL FOR UPDATE`, vehicleID).Scan(&unit)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNoStock
	}
	return err
}

// RecordDamage records a damage seen on a car in stock: what was seen,
// never a debt for anyone. For whoever buys or sells cars.
func RecordDamage(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, vehicleID, zone, description, severity string, foundOn time.Time) (string, error) {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return "", access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return "", err
	}
	description = strings.TrimSpace(description)
	if !looksLikeUUID(vehicleID) {
		return "", ErrNotFound
	}
	if !slices.Contains(DamageZones, zone) || !slices.Contains(DamageSeverities, severity) || description == "" ||
		utf8.RuneCountInString(description) > 500 || foundOn.IsZero() || foundOn.After(time.Now().Add(24*time.Hour)) {
		return "", errDamage
	}
	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := cardWriteTx(ctx, tx, vehicleID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO vehicle_damages (shop_id, vehicle_id, zone, description, severity, found_on, created_by)
			VALUES ($1, vehicle_now($2), $3, $4, $5, $6::date, NULLIF($7, '')::uuid) RETURNING id`,
			scope.ShopID, vehicleID, zone, description, severity, foundOn.Format(time.DateOnly), scope.UserID).Scan(&id)
	})
	return id, err
}

// RepairDamage records a damage repaired, once; the damage stays in the
// car's history beside its repair.
func RepairDamage(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, damageID string, on time.Time, note string) error {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	if !looksLikeUUID(damageID) {
		return ErrNotFound
	}
	note = strings.TrimSpace(note)
	if on.IsZero() || utf8.RuneCountInString(note) > 500 {
		return errDamage
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var vehicle string
		if err := tx.QueryRow(ctx, `SELECT vehicle_id FROM vehicle_damages WHERE id = $1`, damageID).Scan(&vehicle); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if err := cardWriteTx(ctx, tx, vehicle); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO vehicle_damage_repairs (shop_id, damage_id, repaired_on, note, created_by)
			VALUES ($1, $2, $3::date, nullif($4, ''), NULLIF($5, '')::uuid) ON CONFLICT (damage_id) DO NOTHING`,
			scope.ShopID, damageID, on.Format(time.DateOnly), note, scope.UserID)
		return err
	})
}

// AddKey records a key that comes with a car in stock.
func AddKey(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, vehicleID, kind, label string) (string, error) {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return "", access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return "", err
	}
	if !looksLikeUUID(vehicleID) {
		return "", ErrNotFound
	}
	label = strings.TrimSpace(label)
	if !slices.Contains(KeyKinds, kind) || utf8.RuneCountInString(label) > 100 {
		return "", errKey
	}
	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := cardWriteTx(ctx, tx, vehicleID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO vehicle_keys (shop_id, vehicle_id, kind, label, created_by)
			VALUES ($1, vehicle_now($2), $3, nullif($4, ''), NULLIF($5, '')::uuid) RETURNING id`,
			scope.ShopID, vehicleID, kind, label, scope.UserID).Scan(&id)
	})
	return id, err
}

// MoveKey records where a key went: out to a holder -- a colleague, a
// test driver, the workshop -- back, lost or found.
func MoveKey(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, keyID, kind, holder, note string) error {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	if !looksLikeUUID(keyID) {
		return ErrNotFound
	}
	holder, note = strings.TrimSpace(holder), strings.TrimSpace(note)
	if !slices.Contains(KeyMoves, kind) || (kind == "out" && holder == "") || utf8.RuneCountInString(holder) > 200 ||
		utf8.RuneCountInString(note) > 300 {
		return errKeyMove
	}
	if kind != "out" {
		holder = ""
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var vehicle string
		if err := tx.QueryRow(ctx, `SELECT vehicle_id FROM vehicle_keys WHERE id = $1`, keyID).Scan(&vehicle); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if err := cardWriteTx(ctx, tx, vehicle); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO vehicle_key_movements (shop_id, key_id, kind, holder, note, by_user)
			VALUES ($1, $2, $3, nullif($4, ''), nullif($5, ''), NULLIF($6, '')::uuid)`, scope.ShopID, keyID, kind, holder, note, scope.UserID)
		return err
	})
}

// AddServiceEntry writes an entry in a car's service book, its odometer
// a reading of its own.
func AddServiceEntry(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, vehicleID string, doneOn time.Time, km *int64, doneBy, what string) error {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	if !looksLikeUUID(vehicleID) {
		return ErrNotFound
	}
	doneBy, what = strings.TrimSpace(doneBy), strings.TrimSpace(what)
	if doneOn.IsZero() || doneOn.After(time.Now().Add(24*time.Hour)) || doneBy == "" || what == "" ||
		utf8.RuneCountInString(doneBy) > 200 || utf8.RuneCountInString(what) > 1000 || (km != nil && (*km < 0 || *km >= 10_000_000)) {
		return errService
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := cardWriteTx(ctx, tx, vehicleID); err != nil {
			return err
		}
		var reading *string
		if km != nil {
			var id string
			if err := tx.QueryRow(ctx, `
				INSERT INTO odometer_readings (shop_id, vehicle_id, km, source, read_at, decreasing)
				VALUES ($1, vehicle_now($2), $3, 'service', ($4::date + time '12:00') AT TIME ZONE (SELECT timezone FROM shops WHERE id = $1),
				        EXISTS (SELECT 1 FROM odometer_readings WHERE vehicle_id = vehicle_now($2) AND km > $3
				                AND read_at < ($4::date + time '12:00') AT TIME ZONE (SELECT timezone FROM shops WHERE id = $1)))
				RETURNING id`, scope.ShopID, vehicleID, *km, doneOn.Format(time.DateOnly)).Scan(&id); err != nil {
				return fmt.Errorf("record the odometer: %w", err)
			}
			reading = &id
		}
		_, err := tx.Exec(ctx, `INSERT INTO vehicle_service_entries (shop_id, vehicle_id, done_on, odometer_reading_id, done_by, what, created_by)
			VALUES ($1, vehicle_now($2), $3::date, $4, $5, $6, NULLIF($7, '')::uuid)`,
			scope.ShopID, vehicleID, doneOn.Format(time.DateOnly), reading, doneBy, what, scope.UserID)
		return err
	})
}

// CardOf reads a car's damage, keys and service book, for whoever buys or
// sells cars. Reading is Core's: the car's history stays.
func CardOf(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, vehicleID string) (VehicleCard, error) {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return VehicleCard{}, access.ErrForbidden
	}
	if !looksLikeUUID(vehicleID) {
		return VehicleCard{}, ErrNotFound
	}
	var out VehicleCard
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT d.id, d.zone, d.description, d.severity, d.found_on, r.repaired_on, coalesce(r.note, '')
			FROM vehicle_damages d LEFT JOIN vehicle_damage_repairs r ON r.damage_id = d.id
			WHERE d.vehicle_id = vehicle_now($1) ORDER BY d.found_on, d.created_at`, vehicleID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var d Damage
			if err := rows.Scan(&d.ID, &d.Zone, &d.Description, &d.Severity, &d.FoundOn, &d.RepairedOn, &d.RepairNote); err != nil {
				rows.Close()
				return err
			}
			out.Damages = append(out.Damages, d)
		}
		rows.Close()
		rows, err = tx.Query(ctx, `
			SELECT k.id, k.kind, coalesce(k.label, ''),
			       coalesce(CASE m.kind WHEN 'out' THEN 'out' WHEN 'lost' THEN 'lost' END, ''), coalesce(m.holder, ''), m.at
			FROM vehicle_keys k
			LEFT JOIN LATERAL (SELECT * FROM vehicle_key_movements x WHERE x.key_id = k.id ORDER BY x.seq DESC LIMIT 1) m ON true
			WHERE k.vehicle_id = vehicle_now($1) ORDER BY k.created_at`, vehicleID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var k Key
			if err := rows.Scan(&k.ID, &k.Kind, &k.Label, &k.Where, &k.Holder, &k.Since); err != nil {
				rows.Close()
				return err
			}
			out.Keys = append(out.Keys, k)
		}
		rows.Close()
		rows, err = tx.Query(ctx, `
			SELECT s.done_on, o.km, s.done_by, s.what FROM vehicle_service_entries s
			LEFT JOIN odometer_readings o ON o.id = s.odometer_reading_id
			WHERE s.vehicle_id = vehicle_now($1) ORDER BY s.done_on, s.created_at`, vehicleID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e ServiceEntry
			if err := rows.Scan(&e.DoneOn, &e.OdometerKm, &e.DoneBy, &e.What); err != nil {
				return err
			}
			out.Service = append(out.Service, e)
		}
		return rows.Err()
	})
	return out, err
}
