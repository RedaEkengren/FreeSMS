package workshop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/vehicledata"
)

// Vehicle lookup (#155): a registration looked up through whatever
// provider the shop has an agreement with, for taking a car in, buying one
// or registering a rental unit. Typing is the first-class path; a lookup
// fills in what is empty, never what a person typed, and never blocks the
// form it serves. Every lookup is a row -- who, which registration, why,
// which provider, when, what it said -- kept for a year, and the answer is
// read from it again for a day before the provider is asked again.

// The purposes a registration is looked up for.
const (
	LookupIntake     = "intake"
	LookupPurchase   = "purchase"
	LookupRentalUnit = "rental_unit"
)

// The outcomes of a lookup.
const (
	LookupFound    = "found"
	LookupNotFound = "not_found"
	LookupFailed   = "failed"
)

// lookupFresh is how long an answer is read again rather than asked for:
// a day, the age a person at the counter is shown.
const lookupFresh = 24 * time.Hour

// vehicleLookupRetention is how long the record of a lookup is kept: who
// looked which registration up, and why. A year is a season of a car's
// visits and purchases; after it the question "who looked this up" is no
// longer the shop's to answer.
const vehicleLookupRetention = 365 * 24 * time.Hour

// mayLookUp checks roles and modules for a lookup for purpose: the front
// desk taking a car into the workshop, the buyer buying one, whoever keeps
// the rental fleet registering a unit.
func mayLookUp(roles access.Roles, scope access.Scope, purpose string) error {
	switch purpose {
	case LookupIntake:
		if !roles.SeesCustomerPersonalData() {
			return access.ErrForbidden
		}
		return scope.Require(access.Workshop)
	case LookupPurchase:
		if !roles.BuysVehicles() {
			return access.ErrForbidden
		}
		return scope.Require(access.Dealer)
	case LookupRentalUnit:
		if !roles.KeepsResources() {
			return access.ErrForbidden
		}
		return scope.Require(access.Rental)
	}
	return fmt.Errorf("%w: that is not a reason to look a registration up", ErrInvalid)
}

// VehicleLookup is what came of looking a registration up.
type VehicleLookup struct {
	Outcome  string
	Vehicle  vehicledata.Vehicle
	Provider string
	// When the provider said it: now, or earlier for an answer read again.
	At     time.Time
	Cached bool
	// The VIN typed differs from the one the provider gave: shown as a
	// conflict, never applied.
	VINConflict bool
}

// Age is how old the answer is, for a page to say.
func (l VehicleLookup) Age(now time.Time) time.Duration { return now.Sub(l.At) }

// lookupFacts is a vehicle as the row keeps it: the facts there are, and a
// fact the provider did not give is not a key.
func lookupFacts(v vehicledata.Vehicle) map[string]any {
	f := map[string]any{}
	put := func(k string, v any, ok bool) {
		if ok {
			f[k] = v
		}
	}
	put("make", v.Make, v.Make != "")
	put("model", v.Model, v.Model != "")
	put("model_year", v.ModelYear, v.ModelYear != 0)
	put("engine", v.Engine, v.Engine != "")
	put("vin", v.VIN.Value, v.VIN.Provided)
	put("inspection", v.Inspection.Value, v.Inspection.Provided)
	put("tax", v.Tax.Value, v.Tax.Provided)
	put("driving_ban", v.DrivingBan.Value, v.DrivingBan.Provided)
	put("odometer_km", v.OdometerKm.Value, v.OdometerKm.Provided)
	return f
}

func vehicleFromFacts(raw []byte) (vehicledata.Vehicle, error) {
	var f struct {
		Make       string  `json:"make"`
		Model      string  `json:"model"`
		ModelYear  int16   `json:"model_year"`
		Engine     string  `json:"engine"`
		VIN        *string `json:"vin"`
		Inspection *string `json:"inspection"`
		Tax        *string `json:"tax"`
		DrivingBan *bool   `json:"driving_ban"`
		OdometerKm *int64  `json:"odometer_km"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return vehicledata.Vehicle{}, err
	}
	v := vehicledata.Vehicle{Make: f.Make, Model: f.Model, ModelYear: f.ModelYear, Engine: f.Engine}
	if f.VIN != nil {
		v.VIN = vehicledata.Of(*f.VIN)
	}
	if f.Inspection != nil {
		v.Inspection = vehicledata.Of(*f.Inspection)
	}
	if f.Tax != nil {
		v.Tax = vehicledata.Of(*f.Tax)
	}
	if f.DrivingBan != nil {
		v.DrivingBan = vehicledata.Of(*f.DrivingBan)
	}
	if f.OdometerKm != nil {
		v.OdometerKm = vehicledata.Of(*f.OdometerKm)
	}
	return v, nil
}

// LookUpVehicle looks registration up for purpose through l, the provider
// called provider, and records that it did. A fresh enough answer for the
// same registration, in the registration period it is in now, is read
// again unless refresh is asked for. The provider is asked outside any
// transaction, and what it answers is never an error to the caller: found,
// not found and failed are all outcomes, and the form goes on. typedVIN is
// what the person typed, for the conflict.
func LookUpVehicle(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, l vehicledata.Lookup, provider, registration, purpose, typedVIN string, refresh bool) (VehicleLookup, error) {
	if err := mayLookUp(scope.Role, scope, purpose); err != nil {
		return VehicleLookup{}, err
	}
	normalised, ok := lookupable(registration)
	if !ok {
		return VehicleLookup{Outcome: LookupNotFound}, nil
	}
	if l == nil || strings.TrimSpace(provider) == "" {
		return VehicleLookup{}, vehicledata.ErrNotConfigured
	}
	out := VehicleLookup{Provider: provider}
	if !refresh {
		var facts []byte
		err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
			err := tx.QueryRow(ctx, `
				SELECT facts, looked_up_at FROM vehicle_lookups
				WHERE normalised = $1 AND provider = $2 AND outcome = 'found' AND looked_up_at > clock_timestamp() - $3::interval
				  AND looked_up_at >= coalesce((SELECT max(valid_from) FROM vehicle_registrations WHERE normalised = $1), '-infinity')
				ORDER BY looked_up_at DESC LIMIT 1`, normalised, provider, fmt.Sprintf("%d seconds", int(lookupFresh.Seconds()))).Scan(&facts, &out.At)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("read the last lookup: %w", err)
			}
			if out.Vehicle, err = vehicleFromFacts(facts); err != nil {
				return err
			}
			out.Outcome, out.Cached = LookupFound, true
			// Read again is still a lookup someone made, for a reason.
			return recordLookupTx(ctx, tx, scope.ShopID, scope.UserID, registration, normalised, provider, purpose, "cached", nil)
		})
		if err != nil {
			return VehicleLookup{}, err
		}
		if out.Cached {
			out.VINConflict = vinConflict(typedVIN, out.Vehicle)
			return out, nil
		}
	}
	out, err := askProvider(ctx, l, provider, registration)
	if err != nil {
		return VehicleLookup{}, err
	}
	var facts map[string]any
	if out.Outcome == LookupFound {
		facts = lookupFacts(out.Vehicle)
	}
	if err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return recordLookupTx(ctx, tx, scope.ShopID, scope.UserID, registration, normalised, provider, purpose, out.Outcome, facts)
	}); err != nil {
		return VehicleLookup{}, err
	}
	out.VINConflict = vinConflict(typedVIN, out.Vehicle)
	return out, nil
}

// askProvider asks, and turns what came back into an outcome. Only "not
// configured" is an error: there is then nobody to have asked.
func askProvider(ctx context.Context, l vehicledata.Lookup, provider, registration string) (VehicleLookup, error) {
	out := VehicleLookup{Provider: provider, At: time.Now()}
	v, err := l.Lookup(ctx, registration)
	switch {
	case errors.Is(err, vehicledata.ErrNotConfigured):
		return out, err
	case errors.Is(err, vehicledata.ErrNotFound):
		out.Outcome = LookupNotFound
	case err != nil:
		out.Outcome = LookupFailed
	default:
		out.Outcome, out.Vehicle = LookupFound, v
	}
	return out, nil
}

// lookupable is a registration as a lookup row holds it, normalised, and
// whether it can be one: as typed, at most twenty characters once trimmed,
// as 0084 holds it, and normalised, one to twenty letters and digits. One
// that cannot be a row is never asked about, so no lookup is made that is
// not recorded.
func lookupable(registration string) (string, bool) {
	n := NormaliseRegistration(registration)
	return n, n != "" && len(n) <= 20 && utf8.RuneCountInString(strings.TrimSpace(registration)) <= 20
}

func recordLookupTx(ctx context.Context, tx pgx.Tx, shopID, by, registration, normalised, provider, purpose, outcome string, facts map[string]any) error {
	var raw any
	if facts != nil {
		b, err := json.Marshal(facts)
		if err != nil {
			return err
		}
		raw = b
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO vehicle_lookups (shop_id, registration, normalised, provider, purpose, outcome, facts, looked_up_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, '')::uuid)`,
		shopID, strings.TrimSpace(registration), normalised, provider, purpose, outcome, raw, by)
	if err != nil {
		return fmt.Errorf("record the lookup: %w", err)
	}
	return nil
}

func vinConflict(typed string, v vehicledata.Vehicle) bool {
	typed = NormaliseRegistration(typed)
	return typed != "" && v.VIN.Provided && v.VIN.Value != "" && NormaliseRegistration(v.VIN.Value) != typed
}

// QueueVehicleLookups puts a list of registrations through the outbox
// (#154) rather than asking the provider for each at once: a stock import
// sends many, and the worker sends them at its own pace, retrying with
// back-off when the provider says it is rate limiting. One a day for each
// registration. Returns how many were queued.
func QueueVehicleLookups(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, purpose string, registrations []string) (int, error) {
	if err := mayLookUp(scope.Role, scope, purpose); err != nil {
		return 0, err
	}
	if purpose == LookupIntake {
		return 0, fmt.Errorf("%w: a car taken in is looked up at the counter, not in a list", ErrInvalid)
	}
	module := access.Dealer
	if purpose == LookupRentalUnit {
		module = access.Rental
	}
	day := time.Now().UTC().Format("2006-01-02")
	queued := 0
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		seen := map[string]bool{}
		for _, r := range registrations {
			n, ok := lookupable(r)
			if !ok || seen[n] {
				continue
			}
			seen[n] = true
			if _, err := enqueueTx(ctx, tx, scope, NewOutboxItem{Kind: "vehicle_lookup", Module: module,
				Key:     "lookup:" + purpose + ":" + n + ":" + day,
				Payload: map[string]any{"registration": strings.TrimSpace(r), "purpose": purpose, "by": scope.UserID}}); err != nil {
				return err
			}
			queued++
		}
		return nil
	})
	return queued, err
}

// VehicleLookupAdapter sends the outbox's vehicle lookups through the
// shop's provider and records each as LookUpVehicle does, on behalf of
// whoever queued it. A provider that is rate limiting, or down, is a
// failure the outbox retries; not found is an answer.
type VehicleLookupAdapter struct {
	Pool     *pgxpool.Pool
	Lookup   vehicledata.Lookup
	Provider string
}

// Send looks one registration up.
func (a VehicleLookupAdapter) Send(ctx context.Context, it OutboxItem) error {
	if a.Lookup == nil || a.Provider == "" {
		return ErrManualTask
	}
	registration, _ := it.Payload["registration"].(string)
	purpose, _ := it.Payload["purpose"].(string)
	by, _ := it.Payload["by"].(string)
	out, err := askProvider(ctx, a.Lookup, a.Provider, registration)
	if errors.Is(err, vehicledata.ErrNotConfigured) {
		return ErrManualTask
	}
	var facts map[string]any
	if out.Outcome == LookupFound {
		facts = lookupFacts(out.Vehicle)
	}
	// Every lookup made is a row, the ones that failed as well: each
	// attempt asked the provider, for a reason, on somebody's behalf.
	if err := database.InShop(ctx, a.Pool, it.ShopID, func(ctx context.Context, tx pgx.Tx) error {
		return recordLookupTx(ctx, tx, it.ShopID, by, registration, NormaliseRegistration(registration), a.Provider, purpose, out.Outcome, facts)
	}); err != nil {
		return err
	}
	if out.Outcome == LookupFailed {
		return fmt.Errorf("the vehicle lookup failed for %s", NormaliseRegistration(registration))
	}
	return nil
}

// VehicleLookupEntry is one lookup as its log shows it.
type VehicleLookupEntry struct {
	Registration string
	Provider     string
	Purpose      string
	Outcome      string
	By           string
	At           time.Time
}

// VehicleLookupLog lists the lookups of a registration, newest first: who
// looked it up, why, and what came of it. Whoever runs the shop.
func VehicleLookupLog(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, registration string) ([]VehicleLookupEntry, error) {
	if !scope.Role.RunsTheShop() {
		return nil, access.ErrForbidden
	}
	var out []VehicleLookupEntry
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT l.registration, l.provider, l.purpose, l.outcome, coalesce(p.display_name, ''), l.looked_up_at
			FROM vehicle_lookups l
			LEFT JOIN users u ON u.id = l.looked_up_by LEFT JOIN people p ON p.id = u.person_id
			WHERE l.normalised = $1 ORDER BY l.looked_up_at DESC`, NormaliseRegistration(registration))
		if err != nil {
			return fmt.Errorf("read the lookups: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var e VehicleLookupEntry
			if err := rows.Scan(&e.Registration, &e.Provider, &e.Purpose, &e.Outcome, &e.By, &e.At); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}
