package server

import (
	"net/http"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A fleet vehicle's paperwork (#202): reported into and out of rental
// use, and insured for it, recorded from the unit's page.

// fleetRegister is where a report is made; manual until a register
// intermediary is chosen.
var fleetRegister workshop.FleetRegister = workshop.ManualFleetRegister{}

// RegisterSays is what to do to report the unit, as a catalogue key.
func (d rentalData) RegisterSays() string {
	dir := workshop.ReportedIn
	if d.FleetUse.Reported() {
		dir = workshop.ReportedOut
	}
	say, _ := fleetRegister.Instructions(dir)
	return say
}

// RegisterLink is where to report it.
func (d rentalData) RegisterLink() string {
	_, link := fleetRegister.Instructions(workshop.ReportedIn)
	return link
}

func backToUnit(r *http.Request) string {
	if u := r.FormValue("unit"); access.IsUUID(u) {
		return "/rental/units/" + u
	}
	return "/rental"
}

func (s *Server) handleFleetReport(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	on, err := time.ParseInLocation("2006-01-02", r.FormValue("on"), s.shopLocation(r))
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That date did not parse", "Use the picker.")
		return
	}
	_, err = workshop.RecordFleetReport(r.Context(), s.pool, session.Scope, r.PathValue("id"), r.FormValue("direction"), on, r.FormValue("reference"))
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, backToUnit(r), http.StatusSeeOther)
}

func (s *Server) handleFleetInsurance(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	loc := s.shopLocation(r)
	from, err := time.ParseInLocation("2006-01-02", r.FormValue("valid_from"), loc)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That date did not parse", "Use the picker.")
		return
	}
	n := workshop.FleetInsurance{Insurer: r.FormValue("insurer"), PolicyRef: r.FormValue("policy_ref"), ValidFrom: from, CoversRental: r.FormValue("covers")}
	if v := r.FormValue("valid_to"); v != "" {
		to, err := time.ParseInLocation("2006-01-02", v, loc)
		if err != nil {
			s.renderError(w, r, http.StatusBadRequest, "That date did not parse", "Use the picker.")
			return
		}
		n.ValidTo = &to
	}
	_, err = workshop.RecordFleetInsurance(r.Context(), s.pool, session.Scope, r.PathValue("id"), n)
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, backToUnit(r), http.StatusSeeOther)
}
