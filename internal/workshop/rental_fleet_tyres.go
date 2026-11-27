package workshop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// The fleet's tyre change (#233): every car in the fleet that is not on the
// right season's tyres, listed against a date the shop sets, and a job opened
// for each the desk picks. A job blocks its car on the availability ledger as any
// job does, so a car in for its tyres is not rented over.

var (
	errFleetSeason = fmt.Errorf("%w: summer or winter", ErrInvalid)
	errFleetUnits  = fmt.Errorf("%w: pick from 1 to 100 units", ErrInvalid)
)

// FleetTyreRow is one car of the fleet and what it stands on.
type FleetTyreRow struct {
	UnitID       string
	Label        string
	Registration string
	// The season of the set on the car now, empty when it has none recorded.
	OnSeason string
	// The shop's own sets of the right season in storage, any of which could be fitted.
	StoredRight int
	// Whether it needs a change for the season asked.
	NeedsChange bool
}

// seasonsFor are the sets that will do for a season: all-season where allowed,
// which is taken as allowed.
func seasonsFor(season string) []string {
	if season == "winter" {
		return []string{SeasonWinterStudded, SeasonWinterFriction, SeasonAllSeason}
	}
	return []string{SeasonSummer, SeasonAllSeason}
}

// FleetTyreChange lists every car in the fleet now with the season of the set it
// stands on, those that do not suit the season first. For the rental desk, in a
// shop that runs the rental and the workshop.
func FleetTyreChange(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, season string) ([]FleetTyreRow, error) {
	if !scope.Role.HandsOverRentals() {
		return nil, access.ErrForbidden
	}
	if err := scope.Require(access.Rental, access.Workshop); err != nil {
		return nil, err
	}
	if season != "summer" && season != "winter" {
		return nil, errFleetSeason
	}
	var out []FleetTyreRow
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT u.id::text, u.fleet_number || coalesce(' · ' || reg.registration, ''), coalesce(reg.registration, ''),
			       coalesce((SELECT ts.season FROM tyre_sets ts
			                 WHERE (SELECT e.vehicle_id FROM tyre_set_vehicle_events e WHERE e.tyre_set_id = ts.id ORDER BY e.seq DESC LIMIT 1) = r.vehicle_id
			                   AND NOT EXISTS (SELECT 1 FROM tyre_set_exits x WHERE x.tyre_set_id = ts.id)
			                 ORDER BY ts.created_at DESC LIMIT 1), ''),
			       (SELECT count(*)::int FROM tyre_sets ts JOIN shop_own_customers o ON o.customer_id = ts.customer_id
			        JOIN tyre_set_placements pl ON pl.tyre_set_id = ts.id AND pl.removed_at IS NULL
			        WHERE ts.season = ANY($1))
			FROM rental_units u
			JOIN resources r ON r.id = u.resource_id AND r.retired_at IS NULL AND r.vehicle_id IS NOT NULL
			JOIN fleet_memberships m ON m.unit_id = u.id AND m.left_at IS NULL
			LEFT JOIN vehicle_registrations reg ON reg.vehicle_id = r.vehicle_id AND reg.valid_to IS NULL
			ORDER BY u.fleet_number`, seasonsFor(season))
		if err != nil {
			return fmt.Errorf("read the fleet's tyres: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var f FleetTyreRow
			if err := rows.Scan(&f.UnitID, &f.Label, &f.Registration, &f.OnSeason, &f.StoredRight); err != nil {
				return err
			}
			f.NeedsChange = !slices.Contains(seasonsFor(season), f.OnSeason)
			out = append(out, f)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(out, func(a, b FleetTyreRow) int {
		switch {
		case a.NeedsChange && !b.NeedsChange:
			return -1
		case !a.NeedsChange && b.NeedsChange:
			return 1
		}
		return 0
	})
	return out, nil
}

// OpenFleetTyreJobs opens a job for the tyre change on each of the units, for the
// shop's own customer. Each is its own transaction: a car the availability
// ledger cannot give -- a rental holds it then -- is returned and the others are
// opened, nothing moved or cancelled. For the rental desk, in a shop that runs
// the rental and the workshop.
func OpenFleetTyreJobs(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, unitIDs []string, complaint string) (opened int, refused []string, err error) {
	if !scope.Role.HandsOverRentals() {
		return 0, nil, access.ErrForbidden
	}
	if err := scope.Require(access.Rental, access.Workshop); err != nil {
		return 0, nil, err
	}
	complaint = strings.TrimSpace(complaint)
	if len(unitIDs) < 1 || len(unitIDs) > 100 {
		return 0, nil, errFleetUnits
	}
	if complaint == "" || len(complaint) > 500 {
		return 0, nil, errServiceComplaint
	}
	for _, id := range unitIDs {
		if !looksLikeUUID(id) {
			return opened, refused, ErrNotFound
		}
	}
	for _, id := range unitIDs {
		_, err := startJobForUnit(ctx, pool, scope, id, complaint)
		switch {
		case err == nil:
			opened++
		case errors.Is(err, ErrResourceTaken):
			refused = append(refused, id)
		default:
			return opened, refused, err
		}
	}
	return opened, refused, nil
}

// startJobForUnit is the job for a unit's car with no service block to take
// over: the complaint in the shop's words, the shop's own customer.
func startJobForUnit(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, unitID, complaint string) (string, error) {
	var job string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := lockResourceOfTx(ctx, tx, `SELECT resource_id FROM rental_units WHERE id = $1`, unitID); err != nil {
			return err
		}
		var registration string
		if err := tx.QueryRow(ctx, `
			SELECT coalesce(reg.registration, '') FROM rental_units u JOIN resources r ON r.id = u.resource_id
			LEFT JOIN vehicle_registrations reg ON reg.vehicle_id = r.vehicle_id AND reg.valid_to IS NULL WHERE u.id = $1`, unitID).Scan(&registration); err != nil {
			return err
		}
		if registration == "" {
			return errServiceNoCar
		}
		own, err := ownCustomerTx(ctx, tx, scope)
		if err != nil {
			return err
		}
		job, err = takeInForTx(ctx, tx, scope, registration, complaint, nil, Details{}, own)
		return err
	})
	return job, err
}
