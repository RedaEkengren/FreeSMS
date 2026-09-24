package server

import (
	"net/http"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// handleTime is the hours screen: mine, and — for the front desk — everything
// that needs a second person to look at it.
func (s *Server) handleTime(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())

	// A fortnight, because that is the span somebody is asked about: last week
	// and this one.
	mine, err := workshop.MyTime(r.Context(), s.pool, session.Scope, time.Now().AddDate(0, 0, -14))
	if err != nil {
		s.log.Error("my time", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
		return
	}

	var flagged []workshop.TimeEntry
	if session.Scope.Role.SeesCustomerPersonalData() {
		flagged, err = workshop.FlaggedTime(r.Context(), s.pool, session.Scope)
		if err != nil {
			s.log.Error("flagged time", "error", err)
		}
	}

	s.render(w, r, http.StatusOK, "time", pageData{
		Title:       "Hours",
		Session:     session,
		TimeMine:    mine,
		TimeFlagged: flagged,
	})
}

// handleCorrectTime adjusts an entry, keeping what it said before.
func (s *Server) handleCorrectTime(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())

	// Times come from the browser as local wall clock. The shop's timezone is
	// what a person meant when they typed 07:30, so that is what it is read
	// in -- reading it as UTC would move every correction by an hour or two
	// depending on the season.
	loc := s.shopLocation(r)
	const layout = "2006-01-02T15:04"

	started, err := time.ParseInLocation(layout, r.FormValue("started_at"), loc)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That start time did not parse", "Use the picker.")
		return
	}
	ended, err := time.ParseInLocation(layout, r.FormValue("ended_at"), loc)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That end time did not parse", "Use the picker.")
		return
	}

	err = workshop.CorrectTime(r.Context(), s.pool, session.Scope,
		r.FormValue("entry_id"), started, ended, r.FormValue("note"))
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/time", http.StatusSeeOther)
}

// shopLocation returns the shop's timezone, falling back to UTC.
//
// Stored times are UTC; this is only for reading what a person typed and for
// showing it back. A shop whose timezone is misconfigured gets UTC rather
// than an error, because a wrong offset is better than a page that will not
// load.
func (s *Server) shopLocation(r *http.Request) *time.Location {
	name, err := workshop.ShopTimezone(r.Context(), s.pool, s.shop())
	if err != nil {
		s.log.Warn("read shop timezone", "error", err)
		return time.UTC
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		s.log.Warn("unknown timezone", "timezone", name, "error", err)
		return time.UTC
	}
	return loc
}
