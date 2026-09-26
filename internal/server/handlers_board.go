package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// handleHome sends each role to the screen it needs.
//
// A technician's first screen is their work; an advisor's is the counter. One
// address for "where I start" means nobody has to remember a second one, and a
// bookmark keeps working when somebody's role changes.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	if sessionFrom(r.Context()).Scope.Role.SeesCustomerPersonalData() {
		s.handleBoard(w, r)
		return
	}
	s.handleJobs(w, r)
}

func (s *Server) handleBoard(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())

	entries, err := workshop.Board(r.Context(), s.pool, session.Scope)
	if errors.Is(err, access.ErrForbidden) {
		s.renderError(w, r, http.StatusForbidden, "Not for your role",
			"The counter board shows customer details, so it is limited to the front desk and the owner.")
		return
	}
	if err != nil {
		s.log.Error("board", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
		return
	}

	s.render(w, r, http.StatusOK, "board", pageData{
		Title:   "In the shop",
		Session: session,
		Board:   entries,
	})
}

func (s *Server) handleNewJobForm(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if !session.Scope.Role.SeesCustomerPersonalData() {
		s.renderError(w, r, http.StatusForbidden, "Not for your role",
			"Taking a vehicle in is done at the front desk.")
		return
	}
	s.render(w, r, http.StatusOK, "newjob", pageData{
		Title:     "Take in a vehicle",
		Session:   session,
		CanLookUp: s.canLookUp(),
	})
}

func (s *Server) handleNewJob(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())

	form := intakeForm{
		Registration: strings.TrimSpace(r.FormValue("registration")),
		OdometerKm:   strings.TrimSpace(r.FormValue("odometer_km")),
		Complaint:    strings.TrimSpace(r.FormValue("complaint")),
	}

	var odometer *int64
	if form.OdometerKm != "" {
		// Accept what a person types: "18 500", "18500 km", "18.500".
		digits := strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, form.OdometerKm)
		km, err := strconv.ParseInt(digits, 10, 64)
		if err != nil || digits == "" {
			s.render(w, r, http.StatusBadRequest, "newjob", pageData{
				Title: "Take in a vehicle", Session: session, Form: form,
				Error: "The odometer reading should be a number of kilometres.",
			})
			return
		}
		odometer = &km
	}

	// Details only matter for a vehicle being created; TakeInWith ignores them
	// for a car already on file.
	details := workshop.Details{
		Make:   strings.TrimSpace(r.FormValue("make")),
		Model:  strings.TrimSpace(r.FormValue("model")),
		Engine: strings.TrimSpace(r.FormValue("engine")),
		VIN:    strings.TrimSpace(r.FormValue("vin")),
	}
	if v := strings.TrimSpace(r.FormValue("model_year")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 16); err == nil {
			y := int16(n)
			details.ModelYear = &y
		}
	}

	id, err := workshop.TakeInWith(r.Context(), s.pool, session.Scope,
		form.Registration, form.Complaint, odometer, details)
	switch {
	case errors.Is(err, access.ErrForbidden):
		s.renderError(w, r, http.StatusForbidden, "Not for your role",
			"Taking a vehicle in is done at the front desk.")
		return
	case errors.Is(err, workshop.ErrInvalid):
		s.render(w, r, http.StatusBadRequest, "newjob", pageData{
			Title: "Take in a vehicle", Session: session, Form: form,
			Error: strings.TrimPrefix(err.Error(), "workshop: invalid: "),
		})
		return
	case err != nil:
		s.log.Error("take in", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
		return
	}

	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}
