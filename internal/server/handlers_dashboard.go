package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// handleDashboard shows the month, or whatever period was asked for.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	loc := s.shopLocation(r)

	// Default to the calendar month, in the shop's timezone. A month that runs
	// from midnight UTC puts two hours of the first day in the wrong month for
	// a Swedish workshop, which is exactly the kind of small wrongness that
	// makes somebody stop trusting a figure.
	now := time.Now().In(loc)
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	to := from.AddDate(0, 1, 0)

	const layout = "2006-01-02"
	if v := r.URL.Query().Get("from"); v != "" {
		if parsed, err := time.ParseInLocation(layout, v, loc); err == nil {
			from = parsed
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if parsed, err := time.ParseInLocation(layout, v, loc); err == nil {
			// Inclusive of the day typed: somebody asking for the 31st means
			// the whole of it.
			to = parsed.AddDate(0, 0, 1)
		}
	}

	summary, err := workshop.Summary(r.Context(), s.pool, session.Scope, from, to)
	if errors.Is(err, access.ErrForbidden) {
		s.renderError(w, r, http.StatusForbidden, "Not for your role",
			"The figures are for the owner and the front desk.")
		return
	}
	if err != nil {
		s.log.Error("dashboard", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
		return
	}

	s.render(w, r, http.StatusOK, "dashboard", pageData{
		Title:     "Figures",
		Session:   session,
		Dashboard: summary,
	})
}
