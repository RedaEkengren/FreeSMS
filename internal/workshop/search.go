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
	// "vehicle", "job" or "customer".
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
	// Where each is active, of what the caller may see: the workshop's
	// open jobs for whoever works in it.
	active := map[string]bool{}
	if len(ids) > 0 && scope.Uses(access.Workshop) && scope.Role.WorksInTheWorkshop() {
		err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT DISTINCT w.vehicle_id::text FROM work_orders w WHERE w.vehicle_id = ANY($1::uuid[]) AND `+openJob, ids)
			if err != nil {
				return err
			}
			got, err := pgx.CollectRows(rows, pgx.RowTo[string])
			for _, id := range got {
				active[id] = true
			}
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("read where the vehicles are active: %w", err)
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
		if active[r.VehicleID] {
			f.ActiveIn = []access.Module{access.Workshop}
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
