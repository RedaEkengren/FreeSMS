package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The planner: a week of bookings, one row per technician, each block where
// it is in the day. The counter's, like the bookings.

// calendarWeek is the Monday of the week asked for, in the shop's zone.
func calendarWeek(r *http.Request, loc *time.Location) time.Time {
	day := time.Now().In(loc)
	if v := r.URL.Query().Get("week"); v != "" {
		if t, err := time.ParseInLocation("2006-01-02", v, loc); err == nil {
			day = t
		}
	}
	y, m, d := day.Date()
	offset := (int(day.Weekday()) + 6) % 7 // Monday first
	return time.Date(y, m, d-offset, 0, 0, 0, 0, loc)
}

func (s *Server) handleCalendar(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	loc := s.zone()
	monday := calendarWeek(r, loc)
	end := time.Date(monday.Year(), monday.Month(), monday.Day()+7, 0, 0, 0, 0, loc)

	bookings, err := workshop.BookingsBetween(r.Context(), s.pool, session.Scope, monday, end)
	if s.handoverError(w, r, err) {
		return
	}
	rows, err := workshop.PlannerRows(r.Context(), s.pool, session.Scope)
	if s.handoverError(w, r, err) {
		return
	}
	capacity, err := workshop.CapacityBetween(r.Context(), s.pool, session.Scope, monday, end)
	if s.handoverError(w, r, err) {
		return
	}
	var found []workshop.Booking
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q != "" {
		if found, err = workshop.SearchBookings(r.Context(), s.pool, session.Scope, q); err != nil {
			s.log.Error("search bookings", "error", err)
		}
	}
	// Saturday and Sunday only when something is booked on them.
	days := 5
	for _, b := range bookings {
		if wd := b.Starts.In(loc).Weekday(); wd == time.Saturday && days < 6 {
			days = 6
		} else if wd == time.Sunday {
			days = 7
		}
	}
	s.render(w, r, http.StatusOK, "calendar", pageData{
		Title:   "Bookings",
		Session: session,
		Planner: plannerPage{
			Monday:    monday,
			Prev:      monday.AddDate(0, 0, -7).Format("2006-01-02"),
			Next:      monday.AddDate(0, 0, 7).Format("2006-01-02"),
			Days:      workshop.BuildWeek(monday, days, rows, bookings, capacity, loc),
			Rows:      rows,
			HourMarks: workshop.PlannerHours(),
			Query:     q,
			Found:     found,
			Searched:  q != "",
			Lifts:     capacity.Lifts,
			Reasons:   workshop.AbsenceReasons,
		},
	})
}

// plannerPage is what the calendar draws.
type plannerPage struct {
	Lifts      int
	Reasons    map[string]string
	Monday     time.Time
	Prev, Next string
	Days       []workshop.PlannerDay
	Rows       []workshop.PlannerRow
	HourMarks  []int
	Query      string
	Found      []workshop.Booking
	Searched   bool
}

// bookingForm reads a booking as the counter types it: a day, a start, a
// length, in the shop's wall clock.
func (s *Server) bookingForm(r *http.Request) (workshop.NewBooking, error) {
	loc := s.zone()
	starts, err := parseWallClock(strings.TrimSpace(r.FormValue("date"))+"T"+strings.TrimSpace(r.FormValue("time")), loc, "")
	if err != nil {
		return workshop.NewBooking{}, err
	}
	minutes, err := strconv.Atoi(strings.TrimSpace(r.FormValue("minutes")))
	if err != nil || minutes <= 0 {
		return workshop.NewBooking{}, errLength
	}
	nb := workshop.NewBooking{
		Starts:        starts,
		Ends:          starts.Add(time.Duration(minutes) * time.Minute),
		TechnicianID:  r.FormValue("technician_id"),
		Registration:  r.FormValue("registration"),
		CustomerName:  r.FormValue("customer_name"),
		CustomerPhone: r.FormValue("customer_phone"),
		What:          r.FormValue("what"),
		PartsNeeded:   r.FormValue("parts_needed") == "yes",
	}
	if v := strings.TrimSpace(r.FormValue("estimate_hours")); v != "" {
		h, err := parseScaled(v, 2)
		if err != nil || h <= 0 {
			return workshop.NewBooking{}, errEstimate
		}
		m := int(h * 60 / 100)
		nb.EstimateMinutes = &m
	}
	return nb, nil
}

type formError string

func (e formError) Error() string { return string(e) }

const (
	errLength   formError = "Say how long, in minutes."
	errEstimate formError = "Write the estimate in hours: 1,5 or 1.5."
)

func (s *Server) handleCreateBooking(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	nb, err := s.bookingForm(r)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Not booked", err.Error())
		return
	}
	id, err := workshop.CreateBooking(r.Context(), s.pool, session.Scope, nb)
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/bookings/"+id, http.StatusSeeOther)
}

func (s *Server) handleBooking(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	b, events, err := workshop.BookingByID(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	if s.handoverError(w, r, err) {
		return
	}
	rows, err := workshop.PlannerRows(r.Context(), s.pool, session.Scope)
	if err != nil {
		s.log.Error("read planner rows", "error", err)
	}
	s.render(w, r, http.StatusOK, "booking", pageData{
		Title:   "Booking",
		Session: session,
		Booking: b, BookingEvents: events,
		Planner: plannerPage{Rows: rows},
	})
}

func (s *Server) handleBookingAction(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	var err error
	switch r.FormValue("action") {
	case "move":
		nb, ferr := s.bookingForm(r)
		if ferr != nil {
			s.renderError(w, r, http.StatusBadRequest, "Not moved", ferr.Error())
			return
		}
		err = workshop.MoveBooking(r.Context(), s.pool, session.Scope, id, nb.Starts, nb.Ends, nb.TechnicianID)
	case "cancel":
		err = workshop.CancelBooking(r.Context(), s.pool, session.Scope, id, r.FormValue("reason"))
	case "no_show":
		err = workshop.BookingNoShow(r.Context(), s.pool, session.Scope, id)
	default:
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", "Move, cancel or did not come.")
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/bookings/"+id, http.StatusSeeOther)
}

// handleCapacity records who is away, which days the shop is shut and how
// many lifts it has. The role checks are in the operations.
func (s *Server) handleCapacity(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	loc := s.zone()
	day, derr := time.ParseInLocation("2006-01-02", strings.TrimSpace(r.FormValue("date")), loc)
	needsDay := func() bool {
		if derr != nil {
			s.renderError(w, r, http.StatusBadRequest, "Not saved", "Give the day.")
			return false
		}
		return true
	}
	var err error
	switch r.FormValue("action") {
	case "away":
		if !needsDay() {
			return
		}
		err = workshop.SetAbsence(r.Context(), s.pool, session.Scope, r.FormValue("user_id"), day, r.FormValue("reason"))
	case "back":
		if !needsDay() {
			return
		}
		err = workshop.ClearAbsence(r.Context(), s.pool, session.Scope, r.FormValue("user_id"), day)
	case "shut":
		if !needsDay() {
			return
		}
		err = workshop.CloseDay(r.Context(), s.pool, session.Scope, day, r.FormValue("reason"))
	case "open":
		if !needsDay() {
			return
		}
		err = workshop.OpenDay(r.Context(), s.pool, session.Scope, day)
	case "lifts":
		n, cerr := strconv.Atoi(strings.TrimSpace(r.FormValue("lifts")))
		if cerr != nil {
			s.renderError(w, r, http.StatusBadRequest, "Not saved", "Lifts is a whole number; zero is not limited.")
			return
		}
		err = workshop.SetLifts(r.Context(), s.pool, session.Scope, n)
	default:
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", "Away, back, shut, open or lifts.")
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	week := ""
	if derr == nil {
		week = "?week=" + day.Format("2006-01-02")
	}
	http.Redirect(w, r, "/calendar"+week, http.StatusSeeOther)
}
