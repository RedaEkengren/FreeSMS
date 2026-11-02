package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Test drives' pages (#173): booked from a lead or for a walk-in, the
// licence checked, the agreement produced and signed, and the keys out and
// back -- on a phone on the forecourt.

// driveData is what the test-drive pages show.
type driveData struct {
	Drive     workshop.TestDrive
	Lead      workshop.Lead
	Cars      []workshop.StockCar
	Templates []workshop.DocumentTemplate
	Agreement workshop.DocumentRecord
	Form      map[string]string
	Key       string
	// The car is reserved for somebody else: book again knowing it.
	Reserved bool
	Now      string
	Today    string
	// The moment the page was made, for what is late.
	At time.Time
	// The shop's day, as a date, for whether a licence is good today: the
	// day StartTestDrive judges it on.
	Day time.Time
}

// shopDay is now's date where the shop is, at midnight UTC, the way the
// database hands back a date.
func shopDay(now time.Time, loc *time.Location) time.Time {
	y, m, d := now.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// Methods are the ways a licence is looked at.
func (driveData) Methods() []string { return workshop.TestDriveLicenceMethods }

// Word is a drive's state or a way of checking, as a catalogue key.
func (driveData) Word(key string) string {
	return map[string]string{
		workshop.DriveBooked: "Booked", workshop.DriveStarted: "Out on the drive", workshop.DriveReturned: "Back",
		workshop.DriveCancelled: "Cancelled",
		"card_inspected":        "The card, looked at", "digital_licence": "A digital licence, looked at",
		"foreign_inspected": "A foreign licence, looked at", "other": "Another way, in the note",
	}[key]
}

// DriverPartyID is the agreement's party id for the driver, to sign.
func (d driveData) DriverPartyID() string {
	for _, p := range d.Agreement.Parties {
		if p.Role == "driver" {
			return p.ID
		}
	}
	return ""
}

func (s *Server) testDriveRoutes(m routeMux) {
	m.HandleFunc("GET /cars/test-drives/new", s.requireSession(s.handleTestDriveForm))
	m.HandleFunc("POST /cars/test-drives", s.requireSession(s.handleTestDriveBook))
	m.HandleFunc("GET /cars/test-drives/{id}", s.requireSession(s.handleTestDrive))
	m.HandleFunc("POST /cars/test-drives/{id}/licence", s.requireSession(s.handleTestDriveLicence))
	m.HandleFunc("POST /cars/test-drives/{id}/agreement", s.requireSession(s.handleTestDriveAgreement))
	m.HandleFunc("POST /cars/test-drives/{id}/signed", s.requireSession(s.handleTestDriveSigned))
	m.HandleFunc("POST /cars/test-drives/{id}/start", s.requireSession(s.handleTestDriveStart))
	m.HandleFunc("POST /cars/test-drives/{id}/return", s.requireSession(s.handleTestDriveReturn))
	m.HandleFunc("POST /cars/test-drives/{id}/cancel", s.requireSession(s.handleTestDriveCancel))
}

func (s *Server) testDriveForm(w http.ResponseWriter, r *http.Request, status int, d driveData, msg string) {
	session := sessionFrom(r.Context())
	var err error
	if d.Cars, err = workshop.CarsInStock(r.Context(), s.pool, session.Scope); s.handoverError(w, r, err) {
		return
	}
	if lead := d.Form["lead"]; lead != "" {
		if d.Lead, err = workshop.LeadByID(r.Context(), s.pool, session.Scope, lead); s.handoverError(w, r, err) {
			return
		}
	}
	if d.Key == "" {
		d.Key = newFormKey()
	}
	d.Now = time.Now().In(s.shopLocation(r)).Format(wallClockLayout)
	s.render(w, r, status, "testdrive", pageData{Title: "Book a test drive", Session: session, Drives: d, Error: msg})
}

// handleTestDriveForm is a drive to book: from a lead, with its car and
// person, or for a walk-in, now.
func (s *Server) handleTestDriveForm(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.testDriveForm(w, r, http.StatusOK, driveData{Form: map[string]string{"lead": q.Get("lead"), "unit": q.Get("unit"), "minutes": "60"}}, "")
}

// handleTestDriveBook books it. A driver with no party yet -- a walk-in --
// becomes one from the name and number given.
func (s *Server) handleTestDriveBook(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	form := map[string]string{}
	for _, k := range []string{"lead", "unit", "driver", "name", "phone", "from", "minutes", "area"} {
		form[k] = strings.TrimSpace(r.FormValue(k))
	}
	key := r.FormValue("key")
	back := func(status int, msg string, reserved bool) {
		s.testDriveForm(w, r, status, driveData{Form: form, Key: key, Reserved: reserved}, msg)
	}
	from, err := time.ParseInLocation(wallClockLayout, form["from"], s.shopLocation(r))
	minutes, merr := strconv.Atoi(form["minutes"])
	if err != nil || merr != nil || minutes < 5 || minutes > 24*60 {
		back(http.StatusBadRequest, "Say when it starts and for how many minutes.", false)
		return
	}
	driver := form["driver"]
	if driver == "" && form["lead"] != "" {
		l, err := workshop.LeadByID(r.Context(), s.pool, session.Scope, form["lead"])
		if s.handoverError(w, r, err) {
			return
		}
		driver = l.PartyID
		if driver == "" {
			form["name"], form["phone"] = firstNonEmpty(form["name"], l.Name), firstNonEmpty(form["phone"], l.Phone)
		}
	}
	var walkIn *workshop.NewParty
	if driver == "" {
		if form["name"] == "" {
			back(http.StatusBadRequest, "Say who drives: their name and phone number.", false)
			return
		}
		// Made with the drive, in its transaction (BookTestDrive).
		walkIn = &workshop.NewParty{Name: form["name"], Phone: form["phone"]}
	}
	d, err := workshop.BookTestDrive(r.Context(), s.pool, session.Scope, workshop.NewTestDrive{StockUnitID: form["unit"],
		DriverPartyID: driver, NewDriver: walkIn, LeadID: form["lead"], From: from, To: from.Add(time.Duration(minutes) * time.Minute), Area: form["area"],
		Key: key, DespiteReservation: r.FormValue("despite") == "1"})
	if errors.Is(err, workshop.ErrDriveReserved) {
		back(http.StatusConflict, trimInvalid(err), true)
		return
	}
	if errors.Is(err, workshop.ErrInvalid) {
		back(http.StatusBadRequest, trimInvalid(err), false)
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/cars/test-drives/"+d.ID, http.StatusSeeOther)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// handleTestDrive is one drive and what comes next for it.
func (s *Server) handleTestDrive(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	var d driveData
	var err error
	if d.Drive, err = workshop.TestDriveByID(r.Context(), s.pool, session.Scope, r.PathValue("id")); s.handoverError(w, r, err) {
		return
	}
	if d.Drive.AgreementID != "" {
		if d.Agreement, err = workshop.DocumentRecordByID(r.Context(), s.pool, session.Scope, d.Drive.AgreementID); s.handoverError(w, r, err) {
			return
		}
	}
	if d.Drive.State == workshop.DriveBooked {
		if d.Templates, err = workshop.TestDriveTemplates(r.Context(), s.pool, session.Scope); s.handoverError(w, r, err) {
			return
		}
	}
	d.Today = time.Now().In(s.shopLocation(r)).Format(time.DateOnly)
	d.At = time.Now()
	d.Day = shopDay(d.At, s.shopLocation(r))
	s.render(w, r, http.StatusOK, "testdrive", pageData{Title: "Test drive", Session: session, Drives: d})
}

func (s *Server) backToDrive(w http.ResponseWriter, r *http.Request, err error) {
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/cars/test-drives/"+r.PathValue("id"), http.StatusSeeOther)
}

func (s *Server) handleTestDriveLicence(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	until, ok := s.formDate(r, "valid_until")
	if !ok {
		s.renderError(w, r, http.StatusBadRequest, "That date did not parse", "Use the picker.")
		return
	}
	s.backToDrive(w, r, workshop.CheckDriverLicence(r.Context(), s.pool, session.Scope, r.PathValue("id"), workshop.LicenceCheckInput{
		IDChecked: r.FormValue("id_checked") == "1", Method: r.FormValue("method"),
		Categories: strings.FieldsFunc(r.FormValue("categories"), func(c rune) bool { return c == ',' || c == ' ' }),
		ValidUntil: until, AgeOK: r.FormValue("age_ok") == "1", Note: r.FormValue("note")}))
}

func (s *Server) handleTestDriveAgreement(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	_, err := workshop.ProduceTestDriveAgreement(r.Context(), s.pool, session.Scope, r.PathValue("id"), r.FormValue("template"),
		r.FormValue("replacing"))
	s.backToDrive(w, r, err)
}

// handleTestDriveSigned records that the driver signed the agreement, on
// paper or in person: no signing service is needed.
func (s *Server) handleTestDriveSigned(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	d, err := workshop.TestDriveByID(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	if s.handoverError(w, r, err) {
		return
	}
	method := workshop.SignedInPerson
	if r.FormValue("method") == workshop.SignedOnPaper {
		method = workshop.SignedOnPaper
	}
	s.backToDrive(w, r, workshop.RecordSignature(r.Context(), s.pool, session.Scope, d.AgreementID, r.FormValue("party"),
		workshop.SignatureEvidence{Method: method, Hash: r.FormValue("hash"), WitnessedBy: session.Scope.UserID}))
}

func (s *Server) handleTestDriveStart(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	km, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("km")), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", "Say what the odometer shows, in whole kilometres.")
		return
	}
	s.backToDrive(w, r, workshop.StartTestDrive(r.Context(), s.pool, session.Scope, r.PathValue("id"), km, r.FormValue("unsigned")))
}

func (s *Server) handleTestDriveReturn(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	km, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("km")), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", "Say what the odometer shows, in whole kilometres.")
		return
	}
	s.backToDrive(w, r, workshop.ReturnTestDrive(r.Context(), s.pool, session.Scope, r.PathValue("id"), km, r.FormValue("seen")))
}

func (s *Server) handleTestDriveCancel(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	s.backToDrive(w, r, workshop.CancelTestDrive(r.Context(), s.pool, session.Scope, r.PathValue("id"), r.FormValue("why")))
}
