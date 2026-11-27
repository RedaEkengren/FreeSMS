package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// ServiceKinds is what a plan can be for.
func (rentalData) ServiceKinds() []string { return workshop.ServiceKinds }

// handleServicePlan makes or changes a unit's plan for a kind of service.
func (s *Server) handleServicePlan(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	err := workshop.SetServicePlan(r.Context(), s.pool, session.Scope, r.PathValue("id"), workshop.ServicePlan{
		Kind: r.FormValue("kind"), IntervalMonths: formInt(r, "months"), IntervalKm: formInt(r, "km"),
		DurationMinutes: formInt(r, "hours") * 60, InUse: r.FormValue("end") == ""})
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/rental/units/"+r.PathValue("id"), http.StatusSeeOther)
}

// handleServiceDone records that a kind of service was done.
func (s *Server) handleServiceDone(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	day, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(r.FormValue("done_on")), s.shopLocation(r))
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That date did not parse", "Use the picker.")
		return
	}
	var km *int64
	if v := strings.TrimSpace(r.FormValue("km")); v != "" {
		n, err := parseScaled(v, 0)
		if err != nil {
			s.renderError(w, r, http.StatusBadRequest, "That did not parse", "Write the kilometres as a whole number.")
			return
		}
		km = &n
	}
	err = workshop.RecordServiceDone(r.Context(), s.pool, session.Scope, r.PathValue("id"), r.FormValue("kind"), day, km, "")
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/rental/units/"+r.PathValue("id"), http.StatusSeeOther)
}

func (s *Server) handleRentalService(w http.ResponseWriter, r *http.Request) {
	s.renderRentalService(w, r, rentalData{})
}

func (s *Server) renderRentalService(w http.ResponseWriter, r *http.Request, d rentalData) {
	session := sessionFrom(r.Context())
	rows, err := workshop.ServicesDueSoon(r.Context(), s.pool, session.Scope, workshop.ServiceHorizonDays)
	if s.handoverError(w, r, err) {
		return
	}
	d.ServiceRows = rows
	s.render(w, r, http.StatusOK, "rentalservice", pageData{Title: "Fleet services", Session: session, Rental: d})
}

// handleRentalServicePlan sets time aside for the services that fall due and
// have none, and says which could not be given any.
func (s *Server) handleRentalServicePlan(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	n, left, err := workshop.PlanDueServices(r.Context(), s.pool, session.Scope, workshop.ServiceHorizonDays)
	if s.handoverError(w, r, err) {
		return
	}
	s.renderRentalService(w, r, rentalData{ServicePlanned: n, ServiceLeft: left, ServiceRan: true})
}
