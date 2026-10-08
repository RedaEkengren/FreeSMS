package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// rotaPage is the week of everybody's hours.
type rotaPage struct {
	Monday     time.Time
	Prev, Next string
	Days       []time.Time
	People     []workshop.StaffRow
	Reasons    map[string]string
	// The rota form's days: weeks, then Monday to Sunday.
	FormWeeks []int
	Weekdays  []time.Time
}

// handleRota is who is in when. Everybody may read it; whoever plans staff
// also gets the reasons, the warnings and the forms -- decided where the data
// is read, not by hiding the forms.
func (s *Server) handleRota(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	loc := s.zone()
	monday := calendarWeek(r, loc)
	var people []workshop.StaffRow
	var err error
	database.Reading(r.Context(), s.pool, session.Scope, func(ctx context.Context) {
		people, err = workshop.StaffWeek(ctx, s.pool, session.Scope, monday, 7)
	})
	if s.handoverError(w, r, err) {
		return
	}
	var days []time.Time
	for i := 0; i < 7; i++ {
		days = append(days, monday.AddDate(0, 0, i))
	}
	s.render(w, r, http.StatusOK, "rota", pageData{Title: "Rota", Session: session, Rota: rotaPage{
		Monday: monday, Prev: monday.AddDate(0, 0, -7).Format("2006-01-02"), Next: monday.AddDate(0, 0, 7).Format("2006-01-02"),
		Days: days, People: people, Reasons: workshop.AbsenceReasons, FormWeeks: []int{0, 1}, Weekdays: days,
	}})
}

// handleRotaChange sets a rota, changes a day, or records somebody away or
// back. The permission is checked in each operation.
func (s *Server) handleRotaChange(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	loc := s.zone()
	user := r.FormValue("user_id")
	day, derr := time.ParseInLocation("2006-01-02", strings.TrimSpace(r.FormValue("date")), loc)
	if derr != nil {
		s.renderError(w, r, http.StatusBadRequest, "Not saved", "Give the day.")
		return
	}
	var err error
	switch r.FormValue("action") {
	case "rota":
		weeks, cerr := strconv.Atoi(r.FormValue("weeks"))
		if cerr != nil {
			s.renderError(w, r, http.StatusBadRequest, "Not saved", "A rota repeats every one to four weeks.")
			return
		}
		var hours []workshop.RotaHours
		for wk := 0; wk < weeks && wk < 4; wk++ {
			for wd := 1; wd <= 7; wd++ {
				from := strings.TrimSpace(r.FormValue(fmt.Sprintf("h%d%d_from", wk, wd)))
				to := strings.TrimSpace(r.FormValue(fmt.Sprintf("h%d%d_to", wk, wd)))
				if from == "" && to == "" {
					continue
				}
				hours = append(hours, workshop.RotaHours{Week: wk, Weekday: wd, Starts: from, Ends: to})
			}
		}
		err = workshop.SetRota(r.Context(), s.pool, session.Scope, user, day, weeks, hours)
	case "shift":
		err = workshop.ChangeShift(r.Context(), s.pool, session.Scope, user, day,
			strings.TrimSpace(r.FormValue("starts")), strings.TrimSpace(r.FormValue("ends")))
	case "rota_day":
		err = workshop.ChangeShift(r.Context(), s.pool, session.Scope, user, day, "", "")
	case "away":
		err = workshop.SetAbsence(r.Context(), s.pool, session.Scope, user, day, r.FormValue("reason"))
	case "back":
		err = workshop.ClearAbsence(r.Context(), s.pool, session.Scope, user, day)
	default:
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", "A rota, a day, away or back.")
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	monday := day.AddDate(0, 0, -(int(day.Weekday())+6)%7)
	http.Redirect(w, r, "/rota?week="+monday.Format("2006-01-02"), http.StatusSeeOther)
}

// handleSchedule is the person's own coming two weeks and what they clocked.
// Looking at it is what deals with "your schedule has changed".
func (s *Server) handleSchedule(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	monday := calendarWeek(r, s.zone())
	mine, err := workshop.ScheduleFor(r.Context(), s.pool, session.Scope, monday, 14)
	if s.handoverError(w, r, err) {
		return
	}
	// A screen refreshing itself is not the person reading it.
	if !background(r) {
		if err := workshop.MarkScheduleSeen(r.Context(), s.pool, session.Scope); err != nil {
			s.log.Error("mark schedule seen", "error", err)
		}
	}
	s.render(w, r, http.StatusOK, "schedule", pageData{Title: "My schedule", Session: session, Schedule: mine})
}

// handleStaffPlanner gives or takes the planning of people.
func (s *Server) handleStaffPlanner(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if s.staffError(w, r, workshop.SetPlanner(r.Context(), s.pool, session.Scope, r.PathValue("id"), r.FormValue("on") == "yes")) {
		return
	}
	http.Redirect(w, r, "/staff", http.StatusSeeOther)
}
