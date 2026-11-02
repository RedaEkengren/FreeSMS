package server

import (
	"encoding/csv"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The rental register (#200): who had which unit when, for whoever runs
// the shop, as a printable page and a CSV. Core routes, not Rental's: the
// duty to show it to the authority outlives the module being switched on.
// Every read asks why, and the operation records it.

func (s *Server) rentalCoreRoutes(m routeMux) {
	m.HandleFunc("GET /rental/register", s.requireSession(s.handleRentalRegister))
	m.HandleFunc("GET /rental/register.csv", s.requireSession(s.handleRentalRegisterCSV))
}

// errBadDate is a date or a moment the register's form sent that did
// not parse.
var errBadDate = errors.New("register: a date did not parse")

// registerForm is what the register page was asked, to fill its form
// again.
type registerForm struct {
	From, To, Unit, Registration, Purpose string
	WhoRegistration, WhoAt                string
}

// readRegister reads the part of the register a request asks for: nothing
// until it gives a purpose, and then the period, the moment, or both.
func (s *Server) readRegister(r *http.Request, scope access.Scope) (registerForm, []workshop.RegisterEntry, *workshop.RegisterEntry, bool, error) {
	f := registerForm{From: r.FormValue("from"), To: r.FormValue("to"), Unit: r.FormValue("unit"),
		Registration: r.FormValue("registration"), Purpose: r.FormValue("purpose"),
		WhoRegistration: r.FormValue("who_registration"), WhoAt: r.FormValue("who_at")}
	if strings.TrimSpace(f.Purpose) == "" {
		return f, nil, nil, false, nil
	}
	loc := s.shopLocation(r)
	var entries []workshop.RegisterEntry
	var who *workshop.RegisterEntry
	if f.From != "" || f.To != "" {
		from, err1 := time.ParseInLocation("2006-01-02", f.From, loc)
		to, err2 := time.ParseInLocation("2006-01-02", f.To, loc)
		if err1 != nil || err2 != nil {
			return f, nil, nil, true, errBadDate
		}
		var err error
		if entries, err = workshop.RentalRegister(r.Context(), s.pool, scope, workshop.RegisterFilter{From: from, To: to.AddDate(0, 0, 1),
			UnitID: f.Unit, Registration: f.Registration}, f.Purpose); err != nil {
			return f, nil, nil, true, err
		}
	}
	if f.WhoRegistration != "" || f.WhoAt != "" {
		at, err := parseWallClock(f.WhoAt, loc, "")
		if err != nil {
			return f, nil, nil, true, errBadDate
		}
		if who, err = workshop.WhoHadRegistration(r.Context(), s.pool, scope, f.WhoRegistration, at, f.Purpose); err != nil {
			return f, nil, nil, true, err
		}
	}
	return f, entries, who, true, nil
}

func (s *Server) handleRentalRegister(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if !session.Scope.Role.RunsTheShop() {
		s.handoverError(w, r, access.ErrForbidden)
		return
	}
	f, entries, who, asked, err := s.readRegister(r, session.Scope)
	if err == errBadDate {
		s.renderError(w, r, http.StatusBadRequest, "That date did not parse", "Use the picker.")
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	d := rentalData{Register: entries, Who: who, RegisterAsked: asked, RegisterForm: f}
	if session.Scope.Uses(access.Rental) {
		// The units to choose from, while Rental is on; off, the register
		// is read by period and registration.
		d.Units, _ = workshop.RentalUnits(r.Context(), s.pool, session.Scope)
	}
	s.render(w, r, http.StatusOK, "rentalregister", pageData{Title: "Rental register", Session: session, Rental: d})
}

func (s *Server) handleRentalRegisterCSV(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if !session.Scope.Role.RunsTheShop() {
		s.handoverError(w, r, access.ErrForbidden)
		return
	}
	_, entries, who, asked, err := s.readRegister(r, session.Scope)
	if err == errBadDate {
		s.renderError(w, r, http.StatusBadRequest, "That date did not parse", "Use the picker.")
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	if !asked {
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", "say why the register is read, in 300 characters or fewer")
		return
	}
	if who != nil {
		entries = append(entries, *who)
	}
	loc := s.shopLocation(r)
	at := func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return t.In(loc).Format("2006-01-02 15:04")
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="rental-register.csv"`)
	out := csv.NewWriter(w)
	_ = out.Write([]string{"booking", "unit", "registration", "renter", "state", "booked_from", "booked_to", "out", "in",
		"driver", "main_driver", "taken_off", "licence_checked_at", "licence_passed", "licence_valid_until", "agreement", "agreement_made"})
	for _, e := range entries {
		starts, ends := e.Starts, e.Ends
		rows := e.Drivers
		if len(rows) == 0 {
			rows = []workshop.RentalDriver{{}}
		}
		for _, dr := range rows {
			checked, passed, until := "", "", ""
			if dr.Check != nil {
				checked, passed = at(&dr.Check.CheckedAt), strconv.FormatBool(dr.Check.Passed)
				if dr.Check.ValidUntil != nil {
					until = dr.Check.ValidUntil.Format("2006-01-02")
				}
			}
			main, off := "", ""
			if dr.Name != "" {
				main, off = strconv.FormatBool(dr.Main), strconv.FormatBool(dr.Removed)
			}
			row := []string{e.Reference, e.Unit, e.Registration, e.Renter, e.State, at(&starts), at(&ends), at(e.Out), at(e.In),
				dr.Name, main, off, checked, passed, until, e.AgreementTitle, at(e.AgreementAt)}
			// A name or a title is somebody's typing: never a formula
			// in the authority's spreadsheet.
			for i := range row {
				row[i] = workshop.SpreadsheetCell(row[i])
			}
			_ = out.Write(row)
		}
	}
	out.Flush()
}

func (s *Server) handleRegisterRetention(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	months, err := strconv.Atoi(strings.TrimSpace(r.FormValue("months")))
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", "keep the register between 1 and 240 months, and say on what basis")
		return
	}
	if err := workshop.SetRentalRegisterRetention(r.Context(), s.pool, session.Scope, months, r.FormValue("basis")); s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/rental/settings", http.StatusSeeOther)
}
