package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
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
	started, err := parseWallClock(r.FormValue("started_at"), loc, r.FormValue("started_was"))
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That start time will not do", err.Error())
		return
	}
	ended, err := parseWallClock(r.FormValue("ended_at"), loc, r.FormValue("ended_was"))
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That end time will not do", err.Error())
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
func (s *Server) shopLocation(*http.Request) *time.Location { return s.zone() }

// wallClockLayout is what a datetime-local control sends and is given.
const wallClockLayout = "2006-01-02T15:04"

// parseWallClock reads a time a person typed, as a wall clock in the shop's
// zone -- never the browser's: a datetime-local control has no zone at all,
// and the page writes the shop's wall clock into it.
//
// was is the instant the control was filled with, as RFC 3339. A field sent
// back unchanged keeps exactly that instant. Without it the second 02:30 on
// the night the clocks go back could not be saved without moving an hour,
// because the wall clock alone does not say which 02:30 it was.
//
// Otherwise, at the two hours a year a wall clock is not one instant:
//   - a time that does not exist -- 02:30 the night the clocks go forward --
//     is refused, rather than quietly becoming 03:30 or 01:30;
//   - a time that exists twice -- 02:30 the night they go back -- is the
//     earlier of the two.
func parseWallClock(value string, loc *time.Location, was string) (time.Time, error) {
	if prev, err := time.Parse(time.RFC3339, was); err == nil && prev.In(loc).Format(wallClockLayout) == value {
		return prev, nil
	}
	t, err := time.ParseInLocation(wallClockLayout, value, loc)
	if err != nil {
		return time.Time{}, errors.New("use the picker: a date and a time")
	}
	if t.In(loc).Format(wallClockLayout) != value {
		return time.Time{}, fmt.Errorf("%s does not exist here: the clocks went forward that night", strings.Replace(value, "T", " ", 1))
	}
	// The same wall clock an hour either side means it happened twice.
	for _, other := range []time.Time{t.Add(-time.Hour), t.Add(time.Hour)} {
		if other.In(loc).Format(wallClockLayout) == value && other.Before(t) {
			t = other
		}
	}
	return t, nil
}
