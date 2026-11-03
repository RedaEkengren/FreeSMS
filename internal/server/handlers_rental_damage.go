package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A rental damage case's page (#197): what was found, every step taken,
// and the next.

// DamageSteps, DamageLiabilities, DamageChannels, DamageResponses and
// DamageOutcomes are what the step form offers.
func (rentalData) DamageSteps() []string       { return workshop.DamageSteps }
func (rentalData) DamageLiabilities() []string { return workshop.DamageLiabilities }
func (rentalData) DamageChannels() []string    { return workshop.DamageChannels }
func (rentalData) DamageResponses() []string   { return workshop.DamageResponses }
func (rentalData) DamageOutcomes() []string    { return workshop.DamageOutcomes }

func (s *Server) handleRentalDamage(w http.ResponseWriter, r *http.Request) {
	s.rentalDamagePage(w, r, http.StatusOK, "")
}

func (s *Server) rentalDamagePage(w http.ResponseWriter, r *http.Request, status int, msg string) {
	session := sessionFrom(r.Context())
	c, err := workshop.DamageCaseByID(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	if s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, status, "rentaldamage", pageData{Title: "Damage case", Session: session, Rental: rentalData{DamageCase: c}, Error: msg})
}

// handleRentalDamageStep records the next step of the case.
func (s *Server) handleRentalDamageStep(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	step := workshop.DamageStep{Step: r.FormValue("step"), Reason: r.FormValue("reason")}
	switch step.Step {
	case "liability_assessed":
		step.Liability = r.FormValue("liability")
	case "amount_settled":
		n, err := parseMinorUnits(strings.TrimSpace(r.FormValue("amount")))
		if err != nil || strings.TrimSpace(r.FormValue("amount")) == "" {
			s.rentalDamagePage(w, r, http.StatusBadRequest, "Say the amount in kronor.")
			return
		}
		step.AmountMinor = &n
	case "customer_informed":
		step.Channel = r.FormValue("channel")
		if v := strings.TrimSpace(r.FormValue("respond_by")); v != "" {
			day, err := time.ParseInLocation("2006-01-02", v, s.shopLocation(r))
			if err != nil {
				s.rentalDamagePage(w, r, http.StatusBadRequest, "Use the picker.")
				return
			}
			step.RespondBy = &day
		}
	case "customer_response":
		step.Response = r.FormValue("response")
	case "outcome":
		step.Outcome = r.FormValue("outcome")
	}
	err := workshop.TakeDamageStep(r.Context(), s.pool, session.Scope, r.PathValue("id"), step)
	if errors.Is(err, workshop.ErrInvalid) {
		s.rentalDamagePage(w, r, http.StatusBadRequest, trimInvalid(err))
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/rental/damage/"+r.PathValue("id"), http.StatusSeeOther)
}
