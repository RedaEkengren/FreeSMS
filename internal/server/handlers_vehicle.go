package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/vehicledata"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// handleSearch is the one field at the top of every screen.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	query := strings.TrimSpace(r.URL.Query().Get("q"))

	// One box across every module the shop runs (#159), each result only
	// what the caller's role may see.
	results, err := workshop.SearchAll(r.Context(), s.pool, session.Scope, query)
	if errors.Is(err, workshop.ErrInvalid) {
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", trimInvalid(err))
		return
	}
	if err != nil {
		s.fault(w, r, "search", err)
		return
	}

	// One hit goes straight there, and so does the one exact plate, chassis
	// number or job number among weaker hits. Making somebody choose from a
	// list is a tap for nothing, and at a counter with a telephone in the
	// other hand that tap is the difference.
	if only, ok := workshop.Straight(results); ok {
		http.Redirect(w, r, only.Link(), http.StatusSeeOther)
		return
	}

	s.render(w, r, http.StatusOK, "search", pageData{
		Title:   "Search",
		Session: session,
		Query:   query,
		Found:   results,
	})
}

func (s *Server) handleVehicle(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())

	vehicle, err := workshop.VehicleByID(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	if s.handoverError(w, r, err) {
		return
	}
	var changes []workshop.Change
	if session.Scope.Role.SeesCustomerPersonalData() {
		if changes, err = workshop.VehicleChanges(r.Context(), s.pool, session.Scope, vehicle.ID); err != nil {
			s.log.Error("read vehicle changes", "error", err)
		}
	}
	// Its cycles through the dealer's stock (#160): a car bought twice is
	// two. Not asked for a role that may not read them.
	var units []workshop.StockUnit
	if session.Scope.Role.BuysVehicles() || session.Scope.Role.SellsVehicles() {
		if units, err = workshop.StockUnitsOfVehicle(r.Context(), s.pool, session.Scope, vehicle.ID); err != nil {
			s.log.Error("read stock units", "error", err)
		}
	}
	s.render(w, r, http.StatusOK, "vehicle", pageData{
		Title:          vehicle.Describe(),
		Session:        session,
		Vehicle:        vehicle,
		VehicleChanges: changes,
		StockUnits:     units,
	})
}

// handleVehicleLookup fills in what a registration is, when the shop has a
// service to ask.
//
// It returns a fragment htmx swaps into the intake form. Every failure returns
// an empty fragment with a word about why, and none of them stops the form
// being submitted: somebody is standing at the counter with keys in their
// hand, and a car that cannot be taken in because a third party is down is the
// worst outcome available.
func (s *Server) handleVehicleLookup(w http.ResponseWriter, r *http.Request) {
	s.vehicleLookupFragment(w, r, workshop.LookupIntake, "newjob", "lookup", "/jobs/new/lookup", "lookup")
}

// handlePurchaseLookup is the same for a car being bought (#155): the
// buyer sees its inspection, tax, driving ban and recorded mileage before
// paying, where the provider gives them.
func (s *Server) handlePurchaseLookup(w http.ResponseWriter, r *http.Request) {
	s.vehicleLookupFragment(w, r, workshop.LookupPurchase, "carbuy", "carlookup", "/cars/stock/new/lookup", "carlookup")
}

// handleRentalUnitLookup is the same for a vehicle being added to the
// rental fleet: what the registration is, to check before it is added.
func (s *Server) handleRentalUnitLookup(w http.ResponseWriter, r *http.Request) {
	s.vehicleLookupFragment(w, r, workshop.LookupRentalUnit, "rentalunits", "unitlookup", "/rental/units/lookup", "unitlookup")
}

// vehicleLookupFragment looks a registration up for purpose and renders
// block of page with what it found, for htmx to swap into target.
func (s *Server) vehicleLookupFragment(w http.ResponseWriter, r *http.Request, purpose, page, block, url, target string) {
	session := sessionFrom(r.Context())
	q := r.URL.Query()
	registration := strings.TrimSpace(q.Get("registration"))
	// What the person has typed already is theirs: a lookup fills in only
	// what is empty, and a VIN that differs is a conflict, not a change.
	typed := vehicledata.Vehicle{Make: strings.TrimSpace(q.Get("make")), Model: strings.TrimSpace(q.Get("model")),
		Engine: strings.TrimSpace(q.Get("engine"))}
	if y, err := strconv.Atoi(strings.TrimSpace(q.Get("model_year"))); err == nil && y > 1885 && y < 2100 {
		typed.ModelYear = int16(y)
	}
	if vin := strings.TrimSpace(q.Get("vin")); vin != "" {
		typed.VIN = vehicledata.Of(vin)
	}

	data := pageData{Session: session, Lookup: typed, CanLookUp: s.canLookUp(), LookupRegistration: registration,
		LookupURL: url, LookupTarget: target}
	found, err := workshop.LookUpVehicle(r.Context(), s.pool, session.Scope, s.vehicleLookup, s.lookupProvider,
		registration, purpose, typed.VIN.Value, q.Get("refresh") != "")
	switch {
	case errors.Is(err, vehicledata.ErrNotConfigured):
		// Not an error the person needs to see. The shop has not set this up.
	case err != nil:
		s.handoverError(w, r, err)
		return
	case found.Outcome == workshop.LookupNotFound:
		data.LookupNote = "Nothing is registered under that number. Type what you can."
	case found.Outcome == workshop.LookupFailed:
		data.LookupNote = "The lookup service did not answer. Type what you can; this does not stop you."
	default:
		data.Lookup = fillEmpty(typed, found.Vehicle)
		data.Looked = &found
		data.LookupNote = "Filled in from the registration. Change anything that is wrong."
		if found.VINConflict {
			data.LookupConflict = found.Vehicle.VIN.Value
		}
	}

	s.renderPartial(w, r, page, block, data)
}

// fillEmpty is what was typed, with what a lookup found where nothing was.
// The VIN is never taken over one typed; the status facts are the lookup's.
func fillEmpty(typed, found vehicledata.Vehicle) vehicledata.Vehicle {
	v := found
	if typed.Make != "" {
		v.Make = typed.Make
	}
	if typed.Model != "" {
		v.Model = typed.Model
	}
	if typed.ModelYear != 0 {
		v.ModelYear = typed.ModelYear
	}
	if typed.Engine != "" {
		v.Engine = typed.Engine
	}
	if typed.VIN.Value != "" {
		v.VIN = typed.VIN
	}
	return v
}
