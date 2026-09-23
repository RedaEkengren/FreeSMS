package server

import (
	"net/http"
	"strings"

	"github.com/RedaEkengren/RedaSMS/internal/workshop"
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
