package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// handleRecordPayment records money arriving against an invoice. An empty
// amount settles whatever is owed -- in cash, rounded to the krona.
func (s *Server) handleRecordPayment(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	p := workshop.NewPayment{Method: r.FormValue("method"), Reference: r.FormValue("reference")}
	if v := strings.TrimSpace(r.FormValue("amount")); v != "" {
		negative := strings.HasPrefix(v, "-")
		n, err := parseMinorUnits(strings.TrimPrefix(v, "-"))
		if err != nil {
			s.renderError(w, r, http.StatusBadRequest, "That amount did not parse", err.Error())
			return
		}
		if negative {
			// Money going back to the customer: an overpayment refunded.
			n = -n
		}
		p.AmountMinor = &n
	}
	day, err := time.ParseInLocation("2006-01-02", r.FormValue("paid_on"), s.shopLocation(r))
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That date did not parse", "Use the picker.")
		return
	}
	p.PaidOn = day
	if _, err := workshop.RecordPayment(r.Context(), s.pool, session.Scope, id, p); s.paymentError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/invoices/"+id, http.StatusSeeOther)
}

// handleReversePayment undoes a payment recorded in error with one that
// reverses it.
func (s *Server) handleReversePayment(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if err := workshop.ReversePayment(r.Context(), s.pool, session.Scope, r.PathValue("id")); s.paymentError(w, r, err) {
		return
	}
	back := r.FormValue("invoice")
	if back == "" || !strings.HasPrefix(back, "/invoices/") {
		back = "/receivables"
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// handleReceivables lists what is owed, overdue first.
func (s *Server) handleReceivables(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	list, err := workshop.Receivables(r.Context(), s.pool, session.Scope)
	if s.paymentError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "receivables", pageData{Title: "Owed", Session: session, Receivables: list})
}

func (s *Server) paymentError(w http.ResponseWriter, r *http.Request, err error) bool {
	if errors.Is(err, workshop.ErrInvalid) {
		s.renderError(w, r, http.StatusBadRequest, "Not recorded", trimInvalid(err))
		return true
	}
	return s.handoverError(w, r, err)
}
