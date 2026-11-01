package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The payment flows a shop takes money through in person, whether each has
// been checked against the cash register rules, and what has been taken in
// person this financial year against the shop's threshold (#156). The
// owner's. FreeSMS is not a cash register and never says a shop needs none.
func (s *Server) handlePaymentFlows(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	flows, err := workshop.PaymentFlows(r.Context(), s.pool, session.Scope)
	if s.handoverError(w, r, err) {
		return
	}
	turnover, err := workshop.InPersonTurnoverFor(r.Context(), s.pool, session.Scope)
	if s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "paymentflows", pageData{Title: "Money taken in person", Session: session,
		PaymentFlows: flows, Turnover: turnover, Today: time.Now().In(s.shopLocation(r)).Format("2006-01-02")})
}

func (s *Server) handlePaymentFlowAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := sessionFrom(r.Context())
		var err error
		switch action {
		case "add":
			_, err = workshop.AddPaymentFlow(r.Context(), s.pool, session.Scope, r.FormValue("name"), r.FormValue("method"))
		case "check", "withdraw":
			var on time.Time
			if action == "check" {
				if on, err = time.ParseInLocation("2006-01-02", r.FormValue("checked_on"), s.shopLocation(r)); err != nil {
					s.renderError(w, r, http.StatusBadRequest, "That date did not parse", "Use the picker.")
					return
				}
			}
			err = workshop.CheckPaymentFlow(r.Context(), s.pool, session.Scope, r.PathValue("id"), action == "check",
				r.FormValue("adviser"), on, r.FormValue("note"))
		case "threshold":
			var n int64
			if n, err = parseMinorUnits(strings.TrimSpace(r.FormValue("threshold"))); err != nil {
				s.renderError(w, r, http.StatusBadRequest, "That amount did not parse", err.Error())
				return
			}
			month, _ := strconv.Atoi(r.FormValue("year_starts"))
			err = workshop.SetCashRegisterThreshold(r.Context(), s.pool, session.Scope, n, month)
		}
		if s.paymentError(w, r, err) {
			return
		}
		s.submitted(w, r)
		http.Redirect(w, r, "/payment-flows", http.StatusSeeOther)
	}
}
