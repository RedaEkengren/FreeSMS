package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/vehicledata"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// handleSearch is the one field at the top of every screen.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	query := strings.TrimSpace(r.URL.Query().Get("q"))

	results, err := workshop.Search(r.Context(), s.pool, session.Scope, query)
	if err != nil {
		s.log.Error("search", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
		return
	}

	// One hit goes straight there. Making somebody choose from a list of one
	// is a tap for nothing, and at a counter with a telephone in the other
	// hand that tap is the difference.
	if len(results) == 1 {
		http.Redirect(w, r, "/vehicles/"+results[0].VehicleID, http.StatusSeeOther)
		return
	}

	s.render(w, r, http.StatusOK, "search", pageData{
		Title:   "Search",
		Session: session,
		Query:   query,
		Results: results,
	})
}

func (s *Server) handleVehicle(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())

	vehicle, err := workshop.VehicleByID(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	if s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "vehicle", pageData{
		Title:   vehicle.Describe(),
		Session: session,
		Vehicle: vehicle,
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
	session := sessionFrom(r.Context())
	registration := strings.TrimSpace(r.URL.Query().Get("registration"))

	data := pageData{Session: session}

	switch vehicle, err := s.vehicleLookup.Lookup(r.Context(), registration); {
	case errors.Is(err, vehicledata.ErrNotConfigured):
		// Not an error the person needs to see. The shop has not set this up.
		data.LookupNote = ""
	case errors.Is(err, vehicledata.ErrNotFound):
		data.LookupNote = "Nothing is registered under that number. Type what you can."
	case err != nil:
		s.log.Warn("vehicle lookup", "error", err)
		data.LookupNote = "The lookup service did not answer. Type what you can; this does not stop you."
	default:
		data.Lookup = vehicle
		data.LookupNote = "Filled in from the registration. Change anything that is wrong."
	}

	s.renderPartial(w, r, "newjob", "lookup", data)
}
