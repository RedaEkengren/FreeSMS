package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// handleHome sends each role to the screen it needs.
//
// A technician's first screen is their work; an advisor's is the counter. One
// address for "where I start" means nobody has to remember a second one, and a
// bookmark keeps working when somebody's role changes.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	if sessionFrom(r.Context()).Scope.Role.SeesCustomerPersonalData() {
		s.handleBoard(w, r)
		return
	}
	s.handleJobs(w, r)
}

func (s *Server) handleBoard(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())

	entries, err := workshop.Board(r.Context(), s.pool, session.Scope)
	if errors.Is(err, access.ErrForbidden) {
		s.renderError(w, r, http.StatusForbidden, "Not for your role",
			"The counter board shows customer details, so it is limited to the front desk and the owner.")
		return
	}
	if err != nil {
		s.fault(w, r, "board", err)
		return
	}

	// An invoiced car still on the board with its invoice overdue says so:
	// the counter is where the customer collecting it is standing.
	if owed, err := workshop.Receivables(r.Context(), s.pool, session.Scope); err != nil {
		s.log.Error("receivables", "error", err)
	} else {
		overdue := map[string]bool{}
		for _, b := range owed {
			if b.Overdue() {
				overdue[b.WorkOrderID] = true
			}
		}
		for i := range entries {
			entries[i].Overdue = overdue[entries[i].ID]
		}
	}

	backlog, err := workshop.Backlog(r.Context(), s.pool, session.Scope)
	if err != nil {
		s.log.Error("backlog", "error", err)
	}

	s.render(w, r, http.StatusOK, "board", pageData{
		Title:   "In the shop",
		Session: session,
		Board:   entries,
		Backlog: backlog,
	})
}

func (s *Server) handleNewJobForm(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if !session.Scope.Role.SeesCustomerPersonalData() {
		s.renderError(w, r, http.StatusForbidden, "Not for your role",
			"Taking a vehicle in is done at the front desk.")
		return
	}
	// Taken in from a booking: what the customer said on the telephone is
	// already typed.
	var form intakeForm
	intake := s.intakeFor(r, session.Scope, nil, "", "", false)
	if id := r.URL.Query().Get("booking"); id != "" {
		if b, _, err := workshop.BookingByID(r.Context(), s.pool, session.Scope, id); err == nil && b.Status == "booked" {
			form = intakeForm{Registration: b.Registration, Complaint: b.What, BookingID: b.ID}
			intake = s.intakeFor(r, session.Scope, &b, "", b.TechnicianID, false)
		}
	}
	s.render(w, r, http.StatusOK, "newjob", pageData{
		Title:     "Take in a vehicle",
		Session:   session,
		CanLookUp: s.canLookUp(),
		Form:      form,
		Intake:    intake,
	})
}

// intakeFor gathers what the desk is asked to confirm for a booked car: the
// customers it might be, found by the number and the name the booking holds,
// and the technicians who can take the job. A failed lookup shortens the
// lists; the car can still be taken in.
func (s *Server) intakeFor(r *http.Request, scope access.Scope, booking *workshop.Booking, customer, technician string, alsoOwner bool) *intakeBooking {
	in := &intakeBooking{Customer: customer, TechnicianID: technician, AlsoOwner: alsoOwner}
	seen := map[string]bool{}
	if booking != nil {
		b := *booking
		in.Booked, in.Booking = true, b
		for _, q := range []struct {
			text   string
			number bool
		}{{b.CustomerPhone, true}, {b.CustomerName, false}} {
			if strings.TrimSpace(q.text) == "" {
				continue
			}
			found, err := workshop.FindCustomers(r.Context(), s.pool, scope, q.text)
			if err != nil {
				s.log.Error("find customers for a booking", "error", err)
				continue
			}
			for _, c := range found {
				if !seen[c.ID] {
					seen[c.ID] = true
					in.Matches = append(in.Matches, intakeMatch{Customer: c, SameNumber: q.number})
				}
			}
		}
	}
	rows, err := workshop.PlannerRows(r.Context(), s.pool, scope)
	if err != nil {
		s.log.Error("technicians for an intake", "error", err)
	}
	in.Technicians = rows
	if technician != "" {
		in.TechnicianGone = true
		for _, t := range rows {
			if t.ID == technician {
				in.TechnicianGone = false
			}
		}
		if in.TechnicianGone {
			in.TechnicianID = ""
		}
	}
	return in
}

func (s *Server) handleNewJob(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())

	form := intakeForm{
		Registration: strings.TrimSpace(r.FormValue("registration")),
		BookingID:    strings.TrimSpace(r.FormValue("booking_id")),
		OdometerKm:   strings.TrimSpace(r.FormValue("odometer_km")),
		Complaint:    strings.TrimSpace(r.FormValue("complaint")),
	}

	choice, chosen := strings.CutPrefix(r.FormValue("customer"), "existing:")
	if chosen {
		choice = "existing"
	} else {
		choice = map[string]string{"new": "new"}[r.FormValue("customer")]
	}
	alsoOwner := r.FormValue("also_owner") != ""
	technician := strings.TrimSpace(r.FormValue("technician_id"))
	newName, newPhone := strings.TrimSpace(r.FormValue("new_name")), strings.TrimSpace(r.FormValue("new_phone"))
	promiseText := strings.TrimSpace(r.FormValue("promised_at"))
	// A name typed under "a new customer" is that choice, ticked or not: the
	// alternative is dropping what somebody just typed without a word.
	if choice == "" && form.BookingID == "" && newName != "" {
		choice = "new"
	}
	// The form as it came, with the booking's choices, for sending it back.
	again := func(status int, message string) {
		var booking *workshop.Booking
		if form.BookingID != "" {
			if b, _, err := workshop.BookingByID(r.Context(), s.pool, session.Scope, form.BookingID); err == nil {
				booking = &b
			}
		}
		intake := s.intakeFor(r, session.Scope, booking, r.FormValue("customer"), technician, alsoOwner)
		intake.NewName, intake.NewPhone, intake.Promise = newName, newPhone, promiseText
		s.render(w, r, status, "newjob", pageData{
			Title: "Take in a vehicle", Session: session, Form: form, Intake: intake,
			CanLookUp: s.canLookUp(), Error: message,
		})
	}

	// The promise is only what the desk typed. The booking's slot, the hours
	// sold and a guess are all different things.
	var promised *time.Time
	if promiseText != "" {
		t, err := parseWallClock(promiseText, s.shopLocation(r), "")
		if err != nil {
			again(http.StatusBadRequest, err.Error())
			return
		}
		promised = &t
	}

	var odometer *int64
	if form.OdometerKm != "" {
		// Accept what a person types: "18 500", "18500 km", "18.500".
		digits := strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, form.OdometerKm)
		km, err := strconv.ParseInt(digits, 10, 64)
		// A minus sign is refused rather than dropped with the other
		// punctuation, and so is a reading the database would not hold.
		if err != nil || digits == "" || strings.Contains(form.OdometerKm, "-") || km >= 10_000_000 {
			again(http.StatusBadRequest, "The odometer reading should be a number of kilometres.")
			return
		}
		odometer = &km
	}

	// Details only matter for a vehicle being created; TakeInWith ignores them
	// for a car already on file.
	details := workshop.Details{
		Make:   strings.TrimSpace(r.FormValue("make")),
		Model:  strings.TrimSpace(r.FormValue("model")),
		Engine: strings.TrimSpace(r.FormValue("engine")),
		VIN:    strings.TrimSpace(r.FormValue("vin")),
	}
	if v := strings.TrimSpace(r.FormValue("model_year")); v != "" {
		n, err := strconv.ParseInt(v, 10, 16)
		if err != nil || n < minModelYear || n > maxModelYear {
			again(http.StatusBadRequest, "That is not a model year.")
			return
		}
		y := int16(n)
		details.ModelYear = &y
	}

	var id string
	var err error
	if form.BookingID != "" {
		// The job and the end of the booking in one go. A second submission
		// of the same booking is answered with the job the first one made.
		id, _, err = workshop.TakeInBooking(r.Context(), s.pool, session.Scope, workshop.BookingIntake{
			BookingID: form.BookingID, Registration: form.Registration, Complaint: form.Complaint,
			OdometerKm: odometer, Details: details,
			Customer: choice, CustomerID: strings.TrimPrefix(r.FormValue("customer"), "existing:"),
			AlsoOwner: alsoOwner, TechnicianID: technician, PromisedAt: promised,
		})
	} else {
		id, err = workshop.TakeInWalkIn(r.Context(), s.pool, session.Scope, workshop.WalkIn{
			Registration: form.Registration, Complaint: form.Complaint, OdometerKm: odometer, Details: details,
			Customer: choice, CustomerID: strings.TrimPrefix(r.FormValue("customer"), "existing:"),
			NewName: newName, NewPhone: newPhone, AlsoOwner: alsoOwner,
			TechnicianID: technician, PromisedAt: promised,
		})
	}
	switch {
	case errors.Is(err, workshop.ErrBookingEnded), errors.Is(err, workshop.ErrNotFound) && form.BookingID != "":
		// Not a job in the booking's name. The form comes back as it was
		// typed, without the booking: sent again, it is a walk-in, and that
		// is a decision the person at the counter makes knowing it.
		msg := "That booking is not there any more. Send the form again to take the car in without it."
		if errors.Is(err, workshop.ErrBookingEnded) {
			msg = "That booking was cancelled or marked as not coming while the form was open. Send the form again to take the car in without it."
		}
		form.BookingID = ""
		walkIn := s.intakeFor(r, session.Scope, nil, r.FormValue("customer"), technician, alsoOwner)
		walkIn.NewName, walkIn.NewPhone, walkIn.Promise = newName, newPhone, promiseText
		s.render(w, r, http.StatusConflict, "newjob", pageData{
			Title: "Take in a vehicle", Session: session, CanLookUp: s.canLookUp(), Form: form, Intake: walkIn, Error: msg,
		})
		return
	case errors.Is(err, access.ErrForbidden):
		s.renderError(w, r, http.StatusForbidden, "Not for your role",
			"Taking a vehicle in is done at the front desk.")
		return
	case errors.Is(err, workshop.ErrInvalid):
		again(http.StatusBadRequest, strings.TrimPrefix(err.Error(), "workshop: invalid: "))
		return
	case err != nil:
		s.fault(w, r, "take in", err)
		return
	}

	s.submitted(w, r)
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}

// handleIntakeCustomers lists customers on file for the intake form's search,
// as choices to be ticked. Nothing is chosen for the desk.
func (s *Server) handleIntakeCustomers(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if !session.Scope.Role.SeesCustomerPersonalData() {
		s.renderError(w, r, http.StatusForbidden, "Not for your role",
			"Taking a vehicle in is done at the front desk.")
		return
	}
	found, err := workshop.FindCustomers(r.Context(), s.pool, session.Scope, r.URL.Query().Get("q"))
	if err != nil {
		s.log.Error("find customers for an intake", "error", err)
	}
	s.renderPartial(w, r, "newjob", "found", pageData{Session: session, Customers: found})
}
