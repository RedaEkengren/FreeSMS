package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/RedaEkengren/RedaSMS/internal/workshop"
)

// handleLabour is the shop's own time library.
func (s *Server) handleLabour(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if !session.Scope.Role.SeesCustomerPersonalData() {
		s.renderError(w, r, http.StatusForbidden, "Not for your role",
			"The time library is kept at the front desk.")
		return
	}

	times, err := workshop.LabourTimes(r.Context(), s.pool, session.Scope)
	if err != nil {
		s.log.Error("labour times", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
		return
	}
	rate, err := workshop.LabourRate(r.Context(), s.pool, session.Scope)
	if err != nil {
		s.log.Error("labour rate", "error", err)
	}

	s.render(w, r, http.StatusOK, "labour", pageData{
		Title:       "Time library",
		Session:     session,
		LabourTimes: times,
		LabourRate:  rate,
	})
}

func (s *Server) handleSaveLabour(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())

	l := workshop.LabourTime{
		Operation: r.FormValue("operation"),
		Make:      strings.TrimSpace(r.FormValue("make")),
		Model:     strings.TrimSpace(r.FormValue("model")),
		Engine:    strings.TrimSpace(r.FormValue("engine")),
		Note:      strings.TrimSpace(r.FormValue("note")),
	}

	// Times are typed in hours, because that is how a workshop says them, and
	// stored in minutes, because that is how they add up without a fraction.
	hours, err := parseScaled(r.FormValue("hours"), 2)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That time did not parse",
			"Write it in hours: 1,5 or 1.5.")
		return
	}
	l.Minutes = int(hours * 60 / 100)

	if v := strings.TrimSpace(r.FormValue("year_from")); v != "" {
		n, err := strconv.ParseInt(v, 10, 16)
		if err != nil {
			s.renderError(w, r, http.StatusBadRequest, "That year did not parse", "Four digits.")
			return
		}
		y := int16(n)
		l.YearFrom = &y
	}
	if v := strings.TrimSpace(r.FormValue("year_to")); v != "" {
		n, err := strconv.ParseInt(v, 10, 16)
		if err != nil {
			s.renderError(w, r, http.StatusBadRequest, "That year did not parse", "Four digits.")
			return
		}
		y := int16(n)
		l.YearTo = &y
	}

	if err := workshop.SaveLabourTime(r.Context(), s.pool, session.Scope, l); s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/labour", http.StatusSeeOther)
}

func (s *Server) handleSetLabourRate(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())

	rate, err := parseMinorUnits(r.FormValue("rate"))
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That rate did not parse", err.Error())
		return
	}
	if err := workshop.SetLabourRate(r.Context(), s.pool, session.Scope, rate); s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/labour", http.StatusSeeOther)
}
