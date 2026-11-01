package server

import (
	"net/http"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"strconv"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Rental's pages (#189 onwards). Every route is registered through the
// rental mux, so a shop that does not run Rental answers each as a path that
// does not exist; the operations check the role and the module again.

// rentalPages are the templates Rental adds.
var rentalPages = []string{"rentalsettings", "rentalunits", "rentalunit"}

// rentalData is what Rental's pages are given.
type rentalData struct {
	Permits    []workshop.RentalPermit
	Profiles   []workshop.RentalProfile
	Checklists []workshop.ChecklistTemplate
	Units      []workshop.RentalUnit
	Unit       workshop.RentalUnit
	// Rentals booked after the end asked for, refused.
	InTheWay []workshop.Block
}

// JoinedHow and LeftHow are the ways a unit joins and leaves the fleet,
// for a form.
func (rentalData) JoinedHow() []string { return workshop.FleetJoinedHow }
func (rentalData) LeftHow() []string   { return workshop.FleetLeftHow }

// FleetLabel is a way of joining or leaving, or a unit's state, as a
// catalogue key.
func (rentalData) FleetLabel(key string) string {
	return map[string]string{
		"bought": "Bought for the fleet", "from_stock": "Moved from sales stock", "leased_in": "Leased in",
		"already_owned": "Already the business's", "other": "Other",
		"sold": "Sold off", "to_stock": "Moved to sales stock", "lease_returned": "Lease returned", "scrapped": "Scrapped",
		workshop.UnitOutOfFleet: "Out of the fleet", workshop.UnitAvailable: "Available", workshop.UnitRented: "Rented out",
		workshop.UnitWorkshop: "At the workshop", workshop.UnitBlocked: "Taken by other use",
	}[key]
}

// ProfileBases are the shipped profiles, for a form.
func (rentalData) ProfileBases() []string { return workshop.RentalProfileBases }

// BaseLabel is a shipped profile's name, as a catalogue key.
func (rentalData) BaseLabel(base string) string {
	return map[string]string{workshop.ProfileCar: "Car", workshop.ProfileTrailer: "Trailer",
		workshop.ProfileMachine: "Machine", workshop.ProfileEquipment: "Equipment"}[base]
}

// LicenceCategories are the categories a profile can accept, for a form.
func (rentalData) LicenceCategories() []string { return workshop.LicenceCategories }

// Unconfirmed is why a rule of a profile is not the law, as a catalogue key.
func (rentalData) Unconfirmed(p workshop.RentalProfile, rule string) string {
	if !p.IsUnconfirmed(rule) {
		return ""
	}
	return workshop.UnconfirmedReason(p.Base, rule)
}

// Minutes is a duration in whole minutes, for a form.
func (rentalData) Minutes(d time.Duration) int { return int(d / time.Minute) }

// HasCategory reports whether a profile accepts category c.
func (rentalData) HasCategory(p workshop.RentalProfile, c string) bool {
	for _, have := range p.Rules.LicenceCategories {
		if have == c {
			return true
		}
	}
	return false
}

func (s *Server) rentalRoutes(m routeMux) {
	m.HandleFunc("GET /rental", s.requireSession(s.handleRentalUnits))
	m.HandleFunc("POST /rental/units", s.requireSession(s.handleRentalUnitAdd))
	m.HandleFunc("GET /rental/units/{id}", s.requireSession(s.handleRentalUnit))
	m.HandleFunc("POST /rental/units/{id}/fleet", s.requireSession(s.handleFleetJoin))
	m.HandleFunc("POST /rental/memberships/{id}/end", s.requireSession(s.handleFleetLeave))
	m.HandleFunc("GET /rental/settings", s.requireSession(s.handleRentalSettings))
	m.HandleFunc("POST /rental/permits", s.requireSession(s.handleRentalPermit))
	m.HandleFunc("POST /rental/permits/{id}/revoke", s.requireSession(s.handleRentalPermitRevoke))
	m.HandleFunc("POST /rental/profiles", s.requireSession(s.handleRentalProfileAdd))
	m.HandleFunc("POST /rental/profiles/{id}", s.requireSession(s.handleRentalProfileSave))
}

// handleRentalSettings shows the permit and the rule profiles: the rental
// desk reads them, whoever runs the shop changes them.
func (s *Server) handleRentalSettings(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	var d rentalData
	var err error
	if d.Permits, err = workshop.RentalPermits(r.Context(), s.pool, session.Scope); s.handoverError(w, r, err) {
		return
	}
	if d.Profiles, err = workshop.RentalProfiles(r.Context(), s.pool, session.Scope); s.handoverError(w, r, err) {
		return
	}
	if session.Scope.Role.RunsTheShop() {
		if d.Checklists, err = workshop.ChecklistTemplates(r.Context(), s.pool, session.Scope); s.handoverError(w, r, err) {
			return
		}
	}
	s.render(w, r, http.StatusOK, "rentalsettings", pageData{Title: "Rental settings", Session: session, Rental: d,
		Today: time.Now().In(s.shopLocation(r)).Format("2006-01-02")})
}

func (s *Server) formDate(r *http.Request, name string) (*time.Time, bool) {
	v := strings.TrimSpace(r.FormValue(name))
	if v == "" {
		return nil, true
	}
	t, err := time.ParseInLocation("2006-01-02", v, s.shopLocation(r))
	if err != nil {
		return nil, false
	}
	return &t, true
}

func (s *Server) handleRentalPermit(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	decided, ok1 := s.formDate(r, "decided_on")
	expires, ok2 := s.formDate(r, "expires_on")
	if !ok1 || !ok2 || decided == nil {
		s.renderError(w, r, http.StatusBadRequest, "That date did not parse", "Use the picker.")
		return
	}
	_, err := workshop.RecordRentalPermit(r.Context(), s.pool, session.Scope, workshop.NewRentalPermit{
		Reference: r.FormValue("reference"), DecidedOn: *decided, ExpiresOn: expires,
		Responsible: r.FormValue("responsible"), Note: r.FormValue("note")})
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/rental/settings", http.StatusSeeOther)
}

func (s *Server) handleRentalPermitRevoke(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	on, ok := s.formDate(r, "revoked_on")
	if !ok || on == nil {
		s.renderError(w, r, http.StatusBadRequest, "That date did not parse", "Use the picker.")
		return
	}
	err := workshop.RevokeRentalPermit(r.Context(), s.pool, session.Scope, r.PathValue("id"), *on, r.FormValue("reason"))
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/rental/settings", http.StatusSeeOther)
}

func (s *Server) handleRentalProfileAdd(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	_, err := workshop.AddRentalProfile(r.Context(), s.pool, session.Scope, r.FormValue("name"), r.FormValue("base"))
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/rental/settings", http.StatusSeeOther)
}

func (s *Server) handleRentalProfileSave(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", "")
		return
	}
	on := func(name string) bool { return r.FormValue(name) != "" }
	minutes, err1 := strconv.Atoi(strings.TrimSpace(r.FormValue("buffer_minutes")))
	age := 0
	var err2 error
	if v := strings.TrimSpace(r.FormValue("minimum_age")); v != "" {
		age, err2 = strconv.Atoi(v)
	}
	var deposit int64
	var err3 error
	if v := strings.TrimSpace(r.FormValue("deposit")); v != "" {
		deposit, err3 = parseMinorUnits(v)
	}
	if err1 != nil || err2 != nil || err3 != nil {
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", "Write the buffer and the age as whole numbers, and the deposit as an amount.")
		return
	}
	_, err := workshop.SaveRentalProfile(r.Context(), s.pool, session.Scope, r.PathValue("id"), workshop.RentalRules{
		PermitRequired: on("permit_required"), RegistrationRequired: on("registration_required"),
		LicenceCheckRequired: on("licence_check_required"), LicenceCategories: r.Form["licence_categories"],
		ReportingRequired: on("reporting_required"), CheckOutTemplateID: r.FormValue("check_out_template"),
		CheckInTemplateID: r.FormValue("check_in_template"), CleaningBuffer: time.Duration(minutes) * time.Minute,
		MileageAllowance: on("mileage_allowance"), DefaultDepositMinor: deposit, MinimumAge: age,
		SubletBanned: on("sublet_banned")})
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/rental/settings", http.StatusSeeOther)
}

// handleRentalUnits lists what the shop rents out and what each is doing.
func (s *Server) handleRentalUnits(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	var d rentalData
	var err error
	if d.Units, err = workshop.RentalUnits(r.Context(), s.pool, session.Scope); s.handoverError(w, r, err) {
		return
	}
	if d.Profiles, err = workshop.RentalProfiles(r.Context(), s.pool, session.Scope); s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "rentalunits", pageData{Title: "Rental", Session: session, Rental: d})
}

func (s *Server) handleRentalUnitAdd(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id, err := workshop.AddRentalUnit(r.Context(), s.pool, session.Scope, workshop.NewRentalUnit{
		Registration: r.FormValue("registration"), Name: r.FormValue("name"), ProfileID: r.FormValue("profile"),
		FleetNumber: r.FormValue("fleet_number"), Category: r.FormValue("category")})
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/rental/units/"+id, http.StatusSeeOther)
}

func (s *Server) renderRentalUnit(w http.ResponseWriter, r *http.Request, id string, status int, inTheWay []workshop.Block) {
	session := sessionFrom(r.Context())
	u, err := workshop.RentalUnitByID(r.Context(), s.pool, session.Scope, id)
	if s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, status, "rentalunit", pageData{Title: "Rental unit", Session: session,
		Rental: rentalData{Unit: u, InTheWay: inTheWay}, Today: time.Now().In(s.shopLocation(r)).Format(wallClockLayout)})
}

func (s *Server) handleRentalUnit(w http.ResponseWriter, r *http.Request) {
	s.renderRentalUnit(w, r, r.PathValue("id"), http.StatusOK, nil)
}

func (s *Server) handleFleetJoin(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	at, err := parseWallClock(r.FormValue("at"), s.shopLocation(r), r.FormValue("at_was"))
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That date did not parse", "Use the picker.")
		return
	}
	_, err = workshop.StartFleetMembership(r.Context(), s.pool, session.Scope, r.PathValue("id"), at, r.FormValue("how"), r.FormValue("note"))
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/rental/units/"+r.PathValue("id"), http.StatusSeeOther)
}

func (s *Server) handleFleetLeave(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	unit := r.FormValue("unit")
	at, err := parseWallClock(r.FormValue("at"), s.shopLocation(r), r.FormValue("at_was"))
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That date did not parse", "Use the picker.")
		return
	}
	err = workshop.EndFleetMembership(r.Context(), s.pool, session.Scope, r.PathValue("id"), at, r.FormValue("how"), r.FormValue("note"))
	if f, ok := workshop.AsFutureBookings(err); ok && access.IsUUID(unit) {
		s.renderRentalUnit(w, r, unit, http.StatusConflict, f.Blocks)
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	if !access.IsUUID(unit) {
		unit = ""
	}
	http.Redirect(w, r, "/rental/units/"+unit, http.StatusSeeOther)
}
