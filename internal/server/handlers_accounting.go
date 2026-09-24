package server

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// handleAccounting is where a period is handed to the accountant.
func (s *Server) handleAccounting(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())

	exports, err := workshop.AccountingExports(r.Context(), s.pool, session.Scope)
	if s.handoverError(w, r, err) {
		return
	}

	loc := s.shopLocation(r)
	now := time.Now().In(loc)
	// Last month by default: this month is not finished, and handing over a
	// month that is still running is how a period gets exported twice.
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc).AddDate(0, -1, 0)

	s.render(w, r, http.StatusOK, "accounting", pageData{
		Title:      "Accounting",
		Session:    session,
		Exports:    exports,
		PeriodFrom: from,
		PeriodTo:   from.AddDate(0, 1, -1),
	})
}

func (s *Server) handleAccountingExport(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	loc := s.shopLocation(r)
	const layout = "2006-01-02"

	from, err := time.ParseInLocation(layout, r.FormValue("from"), loc)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That date did not parse", "Use the picker.")
		return
	}
	to, err := time.ParseInLocation(layout, r.FormValue("to"), loc)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That date did not parse", "Use the picker.")
		return
	}
	// Inclusive of the day typed.
	to = to.AddDate(0, 0, 1)

	body, count, err := workshop.ExportAccounting(r.Context(), s.pool, session.Scope,
		from, to, r.FormValue("again") == "yes")
	switch {
	case errors.Is(err, workshop.ErrAlreadyExported):
		s.renderError(w, r, http.StatusConflict, "Already handed over",
			"Documents in that period have gone to the accountant once. Importing the same "+
				"file twice makes duplicate verifications. Tick the box to send it again anyway.")
		return
	case errors.Is(err, workshop.ErrNothingToExport):
		s.renderError(w, r, http.StatusConflict, "Nothing in that period", "No invoices were issued.")
		return
	case err != nil:
		if s.handoverError(w, r, err) {
			return
		}
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="bokforing-%s.se"`, from.Format("2006-01")))
	w.Header().Set("Cache-Control", "no-store")
	s.log.Info("accounting export", "documents", count, "from", from, "to", to)
	_, _ = w.Write(body)
}
