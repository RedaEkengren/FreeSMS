package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A rental's deposit on its booking's page (#196): asked, held on the card,
// received, given back. Applied to a claim from the damage case's invoice
// (#199). Nothing is processed: every amount is money that moved, or a
// hold taken, in the shop's own terminal.

// RentalDepositMethods are the ways a deposit is asked to come.
func (rentalData) RentalDepositMethods() []string { return workshop.RentalDepositMethods }

// handleRentalDeposit does what the booking page's deposit forms ask.
func (s *Server) handleRentalDeposit(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	amount := func() (int64, bool) {
		v := strings.TrimSpace(r.FormValue("amount"))
		n, err := parseMinorUnits(v)
		if err != nil || v == "" {
			s.renderError(w, r, http.StatusBadRequest, "Not accepted", "Say the amount in kronor.")
			return 0, false
		}
		return n, true
	}
	var err error
	switch r.PathValue("what") {
	case "require":
		n, ok := amount()
		if !ok {
			return
		}
		err = workshop.RequireRentalDeposit(r.Context(), s.pool, session.Scope, id, "", n, r.FormValue("method"))
	case "hold":
		n, ok := amount()
		if !ok {
			return
		}
		until, perr := parseWallClock(r.FormValue("valid_until"), s.shopLocation(r), "")
		if perr != nil {
			s.renderError(w, r, http.StatusBadRequest, "That date did not parse", "Use the picker.")
			return
		}
		err = workshop.HoldRentalDeposit(r.Context(), s.pool, session.Scope, id, n, r.FormValue("reference"), until)
	case "release":
		err = workshop.ReleaseRentalDepositHold(r.Context(), s.pool, session.Scope, id, r.FormValue("reason"))
	case "receive", "refund":
		n, ok := amount()
		if !ok {
			return
		}
		var d workshop.RentalDeposit
		if d, err = workshop.RentalDepositOf(r.Context(), s.pool, session.Scope, id); s.handoverError(w, r, err) {
			return
		}
		p := workshop.NewPayment{AmountMinor: &n, Method: r.FormValue("method"), PaidOn: time.Now(), Reference: r.FormValue("reference")}
		if r.PathValue("what") == "receive" {
			if d.DepositID == "" {
				s.renderError(w, r, http.StatusBadRequest, "Not accepted", "Ask for the deposit first.")
				return
			}
			if k := strings.TrimSpace(r.FormValue("key")); k != "" {
				p.Key = "rental-deposit:" + k
			}
			_, err = workshop.RecordDepositPayment(r.Context(), s.pool, session.Scope, d.DepositID, p)
		} else {
			// Only a payment into this booking's deposit is given back from here.
			payment := r.FormValue("payment")
			found := false
			for _, q := range d.Balance.Payments {
				found = found || q.ID == payment
			}
			if !found {
				http.NotFound(w, r)
				return
			}
			if k := strings.TrimSpace(r.FormValue("key")); k != "" {
				p.Key = "rental-refund:" + payment + ":" + k
			}
			_, err = workshop.RefundPayment(r.Context(), s.pool, session.Scope, payment, p, r.FormValue("reason"))
		}
	default:
		http.NotFound(w, r)
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/rental/bookings/"+id+"#deposit", http.StatusSeeOther)
}
