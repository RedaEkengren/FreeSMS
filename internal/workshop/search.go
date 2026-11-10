package workshop

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// One search box across modules (#159). Each module, and Core, finds what
// is its own through a SearchProvider, with the caller's role applied in
// its own query: what comes back is only what the caller may see, and a
// record they may not see adds nothing -- no result, no count, no place in
// the order. Core merges them and ranks them.

// Found is one result, whatever it is.
type Found struct {
	// "vehicle", "job", "customer", "deal" or "rental".
	Kind   string
	Module access.Module
	ID     string
	// What the result is called: a plate, a job's number, a name.
	Heading string
	// A line beneath it: the car's make and model, a job's car.
	Detail string
	// What matched, as a catalogue key.
	MatchedOn string
	// The modules a vehicle is active in, of those the caller may see.
	ActiveIn []access.Module
	rank     int
	// The whole of what was typed is this record's plate, chassis number
	// or job number, not a part of it.
	exact bool
}

// Link is the result's page.
func (f Found) Link() string {
	switch f.Kind {
	case "job":
		return "/jobs/" + f.ID
	case "customer":
		return "/customers/" + f.ID
	case "deal":
		return "/cars/deals/" + f.ID
	case "rental":
		return "/rental/bookings/" + f.ID
	}
	return "/vehicles/" + f.ID
}

// The ranks, best first: an exact registration or chassis number, then a
// reference -- a job's number, a part of a chassis number -- then a name
// or a description.
const (
	rankExact = iota
	rankReference
	rankName
)

// SearchProvider is how a module puts what it finds in the one search.
// Find is asked only while the shop runs Module (Core's always), and checks
// the caller's role itself, in its query.
type SearchProvider struct {
	Module access.Module
	Name   string
	Find   func(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, query string) ([]Found, error)
}

// searchProviders are every provider there is. Dealer and Rental add theirs
// -- a stock unit, a deal, a rental booking by its reference -- with the
// issues that build them.
var searchProviders = []SearchProvider{
	{Name: "vehicles", Find: findVehicles},
	{Name: "customers", Find: findCustomers},
	{Module: access.Workshop, Name: "jobs", Find: findJobs},
	{Module: access.Dealer, Name: "deals", Find: findDeals},
	{Module: access.Rental, Name: "rental bookings", Find: findRentalBookings},
}

// searchLimit is the most characters a search is asked with.
const searchLimit = 100

// ErrSearchInput is a search asked with what no search can be.
var ErrSearchInput = fmt.Errorf("%w: a search is at most %d characters, with no null character", ErrInvalid, searchLimit)

// SearchAll is the one search: every provider of a module the shop runs, and
// Core's, merged and ranked -- an exact registration or chassis number
// first, then a reference, then a name -- and within a rank as each
// provider ordered its own. Every role may search; each provider decides
// what the role finds.
func SearchAll(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, query string) ([]Found, error) {
	query = strings.TrimSpace(query)
	switch {
	case query == "":
		return nil, nil
	case strings.ContainsRune(query, 0), !utf8.ValidString(query), utf8.RuneCountInString(query) > searchLimit:
		return nil, ErrSearchInput
	}
	var out []Found
	for _, p := range searchProviders {
		if p.Module != "" && !scope.Uses(p.Module) {
			continue
		}
		found, err := p.Find(ctx, pool, scope, query)
		if errors.Is(err, access.ErrForbidden) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("search %s: %w", p.Name, err)
		}
		out = append(out, found...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].rank < out[j].rank })
	return out, nil
}

// Straight is the one result a search goes to without a list: the only
// result, or the only one that is exactly what was typed -- a plate, a
// chassis number, a job's number -- among weaker matches. The digits of a
// plate turn up in somebody's telephone number, and the front desk that
// typed the plate wants the car, not a choice. Two exact matches are a
// choice.
func Straight(results []Found) (Found, bool) {
	if len(results) == 1 {
		return results[0], true
	}
	var only Found
	n := 0
	for _, f := range results {
		if f.exact {
			only, n = f, n+1
		}
	}
	return only, n == 1
}

// findVehicles is Search's: every plate a vehicle has carried, its chassis
// number, and its owner's name for a role that may see names.
func findVehicles(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, query string) ([]Found, error) {
	results, err := Search(ctx, pool, scope, query)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, r := range results {
		ids = append(ids, r.VehicleID)
	}
	// Where each is active, of what the caller may see: an open job for
	// whoever works in the workshop, a car in stock for whoever sells or buys
	// cars, a place in the rental fleet for whoever hands out rentals -- each
	// only in a module the shop runs. One statement for all of them; a module
	// the caller may not see is false in the query, not filtered after.
	type where struct{ workshop, dealer, rental bool }
	active := map[string]where{}
	if len(ids) > 0 {
		inWorkshopRole := scope.Uses(access.Workshop) && scope.Role.WorksInTheWorkshop()
		inDealerRole := scope.Uses(access.Dealer) && (scope.Role.SellsVehicles() || scope.Role.BuysVehicles())
		inRentalRole := scope.Uses(access.Rental) && scope.Role.HandsOverRentals()
		if inWorkshopRole || inDealerRole || inRentalRole {
			err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
				rows, err := tx.Query(ctx, `
					SELECT v.id::text,
					  $2 AND EXISTS (SELECT 1 FROM work_orders w WHERE w.vehicle_id = v.id AND `+openJob+`),
					  $3 AND EXISTS (SELECT 1 FROM stock_units u WHERE u.vehicle_id = v.id AND u.closed_at IS NULL),
					  $4 AND EXISTS (SELECT 1 FROM fleet_memberships f WHERE f.vehicle_id = v.id AND f.left_at IS NULL)
					FROM unnest($1::uuid[]) AS v(id)`, ids, inWorkshopRole, inDealerRole, inRentalRole)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var id string
					var w where
					if err := rows.Scan(&id, &w.workshop, &w.dealer, &w.rental); err != nil {
						return err
					}
					active[id] = w
				}
				return rows.Err()
			})
			if err != nil {
				return nil, fmt.Errorf("read where the vehicles are active: %w", err)
			}
		}
	}
	vin := vinLike(NormaliseRegistration(query))
	out := make([]Found, 0, len(results))
	for _, r := range results {
		f := Found{Kind: "vehicle", ID: r.VehicleID, Heading: r.Describe(), MatchedOn: r.MatchedOn, rank: rankName,
			Detail: strings.TrimSpace(r.Make + " " + r.Model)}
		if r.ModelYear != nil {
			f.Detail += " " + strconv.Itoa(int(*r.ModelYear))
		}
		if r.OwnerName != "" {
			f.Detail += " · " + r.OwnerName
		}
		switch r.MatchedOn {
		case "registration", "a plate it used to carry":
			f.rank, f.exact = rankExact, true
		case "chassis number":
			f.rank = rankReference
			if vinLike(NormaliseRegistration(r.VIN)) == vin {
				f.rank, f.exact = rankExact, true
			}
		}
		if w := active[r.VehicleID]; w.workshop {
			f.ActiveIn = append(f.ActiveIn, access.Workshop)
		}
		if w := active[r.VehicleID]; w.dealer {
			f.ActiveIn = append(f.ActiveIn, access.Dealer)
		}
		if w := active[r.VehicleID]; w.rental {
			f.ActiveIn = append(f.ActiveIn, access.Rental)
		}
		out = append(out, f)
	}
	return out, nil
}

// findCustomers is a customer by name, telephone or organisation number,
// for a role that may see who customers are; nothing for any other, asked
// of nobody. An erased person is not found by the name they had, and a
// customer merged into another is found as the one it was merged into.
func findCustomers(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, query string) ([]Found, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, nil
	}
	customers, err := FindCustomers(ctx, pool, scope, query)
	if err != nil {
		return nil, err
	}
	out := make([]Found, 0, len(customers))
	for _, c := range customers {
		out = append(out, Found{Kind: "customer", ID: c.ID, Heading: c.Name, MatchedOn: "customer", rank: rankName})
	}
	return out, nil
}

// findJobs is a job by its number, "#123" or "123", for whoever works in
// the workshop: a technician finds the job and the car, never the
// customer.
func findJobs(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, query string) ([]Found, error) {
	if !scope.Role.WorksInTheWorkshop() {
		return nil, nil
	}
	number, err := strconv.ParseInt(strings.TrimPrefix(query, "#"), 10, 64)
	if err != nil || number <= 0 {
		return nil, nil
	}
	var out []Found
	err = inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		f := Found{Kind: "job", Module: access.Workshop, MatchedOn: "job number", rank: rankReference, exact: true}
		var plate, make_, model string
		err := tx.QueryRow(ctx, `
			SELECT w.id::text, w.number, coalesce(r.registration, ''), coalesce(v.make, ''), coalesce(v.model, '')
			FROM work_orders w JOIN vehicles v ON v.id = w.vehicle_id
			LEFT JOIN vehicle_registrations r ON r.vehicle_id = v.id AND r.valid_to IS NULL
			WHERE w.number = $1`, number).Scan(&f.ID, &number, &plate, &make_, &model)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		f.Heading = "#" + strconv.FormatInt(number, 10)
		f.Detail = strings.TrimSpace(plate + " " + make_ + " " + model)
		out = append(out, f)
		return nil
	})
	return out, err
}

// findDeals is a car sale by its number, "#123" or "123", for whoever sells
// cars. It shows the car, never the buyer: the buyer is on the deal.
func findDeals(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, query string) ([]Found, error) {
	if !scope.Role.SellsVehicles() {
		return nil, nil
	}
	number, err := strconv.ParseInt(strings.TrimPrefix(query, "#"), 10, 64)
	if err != nil || number <= 0 {
		return nil, nil
	}
	var out []Found
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		f := Found{Kind: "deal", Module: access.Dealer, MatchedOn: "deal number", rank: rankReference, exact: true}
		var plate, make_, model string
		err := tx.QueryRow(ctx, `
			SELECT d.id::text, d.number, coalesce(r.registration, ''), coalesce(v.make, ''), coalesce(v.model, '')
			FROM vehicle_sales d JOIN stock_units u ON u.id = d.stock_unit_id JOIN vehicles v ON v.id = u.vehicle_id
			LEFT JOIN vehicle_registrations r ON r.vehicle_id = v.id AND r.valid_to IS NULL
			WHERE d.number = $1`, number).Scan(&f.ID, &number, &plate, &make_, &model)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		f.Heading = "#" + strconv.FormatInt(number, 10)
		f.Detail = strings.TrimSpace(plate + " " + make_ + " " + model)
		out = append(out, f)
		return nil
	})
	return out, err
}

// findRentalBookings is a rental booking by its reference, "R-12" or "r12",
// for whoever hands out rentals. It shows the unit and the period, never
// the renter.
func findRentalBookings(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, query string) ([]Found, error) {
	if !scope.Role.HandsOverRentals() {
		return nil, nil
	}
	q := strings.ToUpper(strings.TrimSpace(query))
	if !strings.HasPrefix(q, "R") {
		return nil, nil
	}
	number, err := strconv.ParseInt(strings.TrimLeft(strings.TrimPrefix(q, "R"), "-# "), 10, 32)
	if err != nil || number <= 0 {
		return nil, nil
	}
	var out []Found
	err = inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		f := Found{Kind: "rental", Module: access.Rental, MatchedOn: "rental booking number", rank: rankReference, exact: true}
		var fleet, category string
		err := tx.QueryRow(ctx, `
			SELECT b.id::text, u.fleet_number, u.category
			FROM rental_bookings b JOIN rental_units u ON u.id = b.unit_id
			WHERE b.number = $1`, number).Scan(&f.ID, &fleet, &category)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		f.Heading = "R-" + strconv.FormatInt(number, 10)
		f.Detail = fleet + " · " + category
		out = append(out, f)
		return nil
	})
	return out, err
}
