package server

import (
	"net/http"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Closing the day: the figures, what to put right, the comparison with the
// bank and the terminal, and the sign-off.

func (s *Server) dayParam(r *http.Request) string {
	if d := strings.TrimSpace(r.FormValue("d")); d != "" {
		return d
	}
	return s.today(r).Format("2006-01-02")
}

func (s *Server) handleDay(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	day := s.dayParam(r)
	figures, err := workshop.DayFor(r.Context(), s.pool, session.Scope, day)
	if s.paymentError(w, r, err) {
		return
	}
	exports, err := workshop.AccountingExports(r.Context(), s.pool, session.Scope)
	if err != nil {
		s.log.Error("read exports", "error", err)
	}
	prev := figures.Day.AddDate(0, 0, -1).Format("2006-01-02")
	next := figures.Day.AddDate(0, 0, 1).Format("2006-01-02")
	s.render(w, r, http.StatusOK, "day", pageData{
		Title: "The day", Session: session, DayFigures: figures, Exports: exports,
		DayPrev: prev, DayNext: next, DayValue: figures.Day.Format("2006-01-02"),
		Methods: workshop.PaymentMethods, Today: s.today(r).Format("2006-01-02"),
	})
}

func (s *Server) handleDaySettlement(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	day := s.dayParam(r)
	external, err := parseMinorSigned(r.FormValue("external"))
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Not recorded", "Write the amount as 150 or 150,50.")
		return
	}
	err = workshop.RecordSettlement(r.Context(), s.pool, session.Scope, day, r.FormValue("method"), external, r.FormValue("explanation"))
	if s.paymentError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/day?d="+day, http.StatusSeeOther)
}

func (s *Server) handleDayReview(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	day := s.dayParam(r)
	_, err := workshop.ReviewDay(r.Context(), s.pool, session.Scope, day, r.FormValue("note"))
	if s.paymentError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/day?d="+day, http.StatusSeeOther)
}

// parseMinorSigned reads an amount that may be negative: a bank statement's
// total for a day of refunds is.
func parseMinorSigned(v string) (int64, error) {
	v = strings.TrimSpace(v)
	neg := strings.HasPrefix(v, "-")
	n, err := parseMinorUnits(strings.TrimPrefix(v, "-"))
	if err != nil {
		return 0, err
	}
	if neg {
		n = -n
	}
	return n, nil
}
