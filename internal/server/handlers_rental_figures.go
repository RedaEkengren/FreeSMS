package server

import (
	"fmt"
	"net/http"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A rental unit's figures by month (#203), for whoever runs the shop.

// WholeHours is a duration in whole hours, for a page.
func (rentalData) WholeHours(d time.Duration) int64 { return int64(d / time.Hour) }

// Percent1 is tenths of a per cent as a decimal, for $.Num: 153 is 15.3.
func (rentalData) Percent1(tenths int64) string { return fmt.Sprintf("%d.%d", tenths/10, tenths%10) }

func (s *Server) handleRentalFigures(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	loc := s.shopLocation(r)
	month := r.URL.Query().Get("month")
	from, err := time.ParseInLocation("2006-01", month, loc)
	if err != nil {
		now := time.Now().In(loc)
		from = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	}
	to := from.AddDate(0, 1, 0)
	figs, err := workshop.RentalFigures(r.Context(), s.pool, session.Scope, from, to)
	if s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "rentalfigures", pageData{Title: "Rental figures", Session: session,
		Rental: rentalData{Figures: figs, FigureTotals: workshop.CategoryTotals(figs), MonthValue: from.Format("2006-01"), Now: time.Now()}})
}
