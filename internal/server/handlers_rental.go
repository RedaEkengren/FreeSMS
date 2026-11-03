package server

import (
	"fmt"
	"net/http"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/money"
	"strconv"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Rental's pages (#189 onwards). Every route is registered through the
// rental mux, so a shop that does not run Rental answers each as a path that
// does not exist; the operations check the role and the module again.

// rentalPages are the templates Rental adds.
var rentalPages = []string{"rentalsettings", "rentalunits", "rentalunit", "rentalprices", "rentalbooking", "rentalhandover", "rentalregister",
	"rentaldamage", "rentalfines", "rentalfigures"}

// rentalData is what Rental's pages are given.
type rentalData struct {
	Permits    []workshop.RentalPermit
	Profiles   []workshop.RentalProfile
	Checklists []workshop.ChecklistTemplate
	Units      []workshop.RentalUnit
	Unit       workshop.RentalUnit
	// Rentals booked after the end asked for, refused.
	InTheWay   []workshop.Block
	Plans      []workshop.RatePlan
	Extras     []workshop.RentalExtra
	Day        []workshop.ActionItem
	Bookings   []workshop.RentalBooking
	Booking    workshop.RentalBooking
	Renters    []workshop.Renter
	Drivers    []workshop.RentalDriver
	Gaps       []workshop.HandoverGap
	Agreements []workshop.DocumentRecord
	Templates  []workshop.DocumentTemplate
	// A booking's handovers, and the protocol on the handover page.
	Handovers workshop.RentalHandoverView
	Protocol  workshop.Protocol
	// The unit's newest period in the fleet: reported, insured (#202).
	FleetUse workshop.FleetUse
	// Handover gaps let go on the booking.
	Overrides []workshop.GapOverride
	// The rental register (#200): what was read, what was asked, and how
	// long it is kept.
	Register      []workshop.RegisterEntry
	Who           *workshop.RegisterEntry
	RegisterAsked bool
	RegisterForm  registerForm
	Retention     *workshop.RegisterRetention
	// A damage case and its steps (#197).
	DamageCase workshop.DamageCaseDetail
	// A booking's deposit (#196).
	Deposit workshop.RentalDeposit
	// A booking's return charges, and how each kind is taxed (#195).
	Charges []workshop.RentalCharge
	// The units' figures for a month, and per category (#203).
	Figures      []workshop.UnitFigures
	FigureTotals []workshop.UnitFigures
	// The month shown, as the month field holds it.
	MonthValue string
	// Charges for vehicles (#198), and an import's preview and its file.
	Fines       []workshop.Fine
	FinePreview []workshop.Fine
	FineFile    string
	// A booking's invoices and credit notes (#199).
	Invoices      []workshop.RentalInvoice
	ChargeClasses []workshop.ChargeClass
	// A booking form's own key: sent twice, one booking.
	Key string
	Now time.Time
}

// LineGross is a frozen line's amount with its VAT, as the renter reads it.
func (rentalData) LineGross(l workshop.RentalBookingLine) int64 {
	return l.Net() + money.VAT(l.Net(), l.VATRateBp)
}

// UnitGross is a frozen line's unit price with its VAT.
func (rentalData) UnitGross(l workshop.RentalBookingLine) int64 {
	return l.UnitPriceMinor + money.VAT(l.UnitPriceMinor, l.VATRateBp)
}

// PricedNet is what a changed line was priced at, net.
func (rentalData) PricedNet(l workshop.RentalBookingLine) int64 {
	if l.PricedMinor == nil {
		return 0
	}
	return *l.PricedMinor
}

// Count is a line's quantity as a plain number, for $.Num.
func (rentalData) Count(milli int64) string {
	if milli%1000 == 0 {
		return strconv.FormatInt(milli/1000, 10)
	}
	s := fmt.Sprintf("%d.%03d", milli/1000, milli%1000)
	return strings.TrimSuffix(strings.TrimRight(s, "0"), ".")
}

// StateLabel is a booking's state, or what happened to it, as a catalogue
// key.
func (rentalData) StateLabel(state string) string {
	return map[string]string{
		workshop.RentalHeld: "Held", workshop.RentalHoldExpired: "Hold run out", workshop.RentalConfirmed: "Confirmed",
		workshop.RentalCheckedOut: "Out with the renter", workshop.RentalReturned: "Returned", workshop.RentalCancelled: "Cancelled",
		workshop.RentalNoShow: "No-show", "requested": "Requested", "moved": "Moved", "extended": "Extended",
	}[state]
}

// ExtraKinds are the kinds of extra, for a form.
func (rentalData) ExtraKinds() []string { return workshop.RentalExtraKinds }

// RentalWord is a rental key -- a kind of extra, a billed unit, what an
// extra is charged per -- as a catalogue key.
func (rentalData) RentalWord(key string) string {
	return map[string]string{
		"extra_driver": "Extra driver", "child_seat": "Child seat", "excess_reduction": "Excess reduction",
		"delivery": "Delivery", "cancellation_fee": "Cancellation fee", "no_show_fee": "No-show fee",
		"young_driver": "Young driver", "other": "Other",
		"hour": "hour", "day": "day", "weekend": "weekend", "week": "week", "month": "month", "rental": "rental",
		"at_start": "Priced by the season it starts in", "split": "Split where the new season starts",
	}[key]
}

// GrossOf is a net amount with VAT at rate.
func (rentalData) GrossOf(net int64, rateBp int) int64 { return net + money.VAT(net, rateBp) }

// Percent is basis points as whole per cent.
func (rentalData) Percent(bp int) int { return bp / 100 }

// Gross is a net unit price with VAT at rate, as a consumer is shown it.
func (rentalData) Gross(net *int64, rateBp int) int64 {
	if net == nil {
		return 0
	}
	return *net + money.VAT(*net, rateBp)
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

// LicenceMethods are the ways a licence is checked, for a form.
func (rentalData) LicenceMethods() []string { return workshop.LicenceMethods }

// LicenceMethod is a way, as a catalogue key.
func (rentalData) LicenceMethod(m string) string {
	return map[string]string{workshop.LicenceCardInspected: "The card, looked at", workshop.LicenceDigitalLicence: "A digital licence, looked at",
		workshop.LicenceForeignInspected: "A foreign licence, looked at", workshop.LicenceOtherMethod: "Another way, in the note"}[m]
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
	m.HandleFunc("GET /rental/damage/{id}", s.requireSession(s.handleRentalDamage))
	m.HandleFunc("POST /rental/damage/{id}/steps", s.requireSession(s.handleRentalDamageStep))
	m.HandleFunc("POST /rental/bookings/{id}/deposit/{what}", s.requireSession(s.handleRentalDeposit))
	m.HandleFunc("GET /rental", s.requireSession(s.handleRentalUnits))
	m.HandleFunc("GET /rental/prices", s.requireSession(s.handleRentalPrices))
	m.HandleFunc("POST /rental/bookings", s.requireSession(s.handleRentalBook))
	m.HandleFunc("GET /rental/bookings/{id}", s.requireSession(s.handleRentalBooking))
	m.HandleFunc("POST /rental/bookings/{id}/{action}", s.requireSession(s.handleRentalBookingAction))
	m.HandleFunc("POST /rental/drivers/{id}/check", s.requireSession(s.handleLicenceCheck))
	m.HandleFunc("POST /rental/drivers/{id}/remove", s.requireSession(s.handleDriverRemove))
	m.HandleFunc("POST /rental/agreements/{id}/accepted", s.requireSession(s.handleAgreementAccepted))
	m.HandleFunc("POST /rental/plans", s.requireSession(s.handleRatePlanAdd))
	m.HandleFunc("POST /rental/plans/{id}/seasons", s.requireSession(s.handleRateSeasonAdd))
	m.HandleFunc("POST /rental/plans/{id}/archive", s.requireSession(s.handleRatePlanArchive))
	m.HandleFunc("POST /rental/extras", s.requireSession(s.handleRentalExtraAdd))
	m.HandleFunc("POST /rental/extras/{id}/archive", s.requireSession(s.handleRentalExtraArchive))
	m.HandleFunc("POST /rental/units", s.requireSession(s.handleRentalUnitAdd))
	m.HandleFunc("GET /rental/units/{id}", s.requireSession(s.handleRentalUnit))
	m.HandleFunc("POST /rental/units/{id}/fleet", s.requireSession(s.handleFleetJoin))
	m.HandleFunc("POST /rental/memberships/{id}/end", s.requireSession(s.handleFleetLeave))
	m.HandleFunc("POST /rental/memberships/{id}/report", s.requireSession(s.handleFleetReport))
	m.HandleFunc("POST /rental/memberships/{id}/insurance", s.requireSession(s.handleFleetInsurance))
	m.HandleFunc("GET /rental/settings", s.requireSession(s.handleRentalSettings))
	m.HandleFunc("POST /rental/permits", s.requireSession(s.handleRentalPermit))
	m.HandleFunc("POST /rental/permits/{id}/revoke", s.requireSession(s.handleRentalPermitRevoke))
	m.HandleFunc("POST /rental/profiles", s.requireSession(s.handleRentalProfileAdd))
	m.HandleFunc("POST /rental/profiles/{id}", s.requireSession(s.handleRentalProfileSave))
	m.HandleFunc("POST /rental/register/retention", s.requireSession(s.handleRegisterRetention))
	s.rentalHandoverRoutes(m)
	s.chargeRoutes(m)
	s.fineRoutes(m)
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
	if d.Retention, err = workshop.RentalRegisterRetention(r.Context(), s.pool, session.Scope); s.handoverError(w, r, err) {
		return
	}
	if d.ChargeClasses, err = workshop.ChargeClasses(r.Context(), s.pool, session.Scope); s.handoverError(w, r, err) {
		return
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
		ReportingRequired: on("reporting_required"), InsuranceRequired: on("insurance_required"), CheckOutTemplateID: r.FormValue("check_out_template"),
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
	if d.Day, err = workshop.RentalDay(r.Context(), s.pool, session.Scope); s.handoverError(w, r, err) {
		return
	}
	now := time.Now()
	if d.Bookings, err = workshop.RentalBookings(r.Context(), s.pool, session.Scope, now, now.AddDate(0, 2, 0)); s.handoverError(w, r, err) {
		return
	}
	d.Now = now
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
	d := rentalData{Unit: u, InTheWay: inTheWay, Key: newFormKey(), Now: time.Now()}
	if d.Renters, err = workshop.Renters(r.Context(), s.pool, session.Scope); s.handoverError(w, r, err) {
		return
	}
	if d.Extras, err = workshop.RentalExtras(r.Context(), s.pool, session.Scope); s.handoverError(w, r, err) {
		return
	}
	if d.Plans, err = workshop.RatePlans(r.Context(), s.pool, session.Scope); s.handoverError(w, r, err) {
		return
	}
	if d.Bookings, err = workshop.RentalBookings(r.Context(), s.pool, session.Scope, time.Now(), time.Now().AddDate(1, 0, 0)); s.handoverError(w, r, err) {
		return
	}
	if len(u.Memberships) > 0 {
		if d.FleetUse, err = workshop.FleetUseOf(r.Context(), s.pool, session.Scope, u.Memberships[0].ID); s.handoverError(w, r, err) {
			return
		}
	}
	s.render(w, r, status, "rentalunit", pageData{Title: "Rental unit", Session: session,
		Rental: d, Today: time.Now().In(s.shopLocation(r)).Format(wallClockLayout)})
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

// handleRentalPrices shows the plans and extras: whoever runs the shop sets
// them, the rental desk reads them.
func (s *Server) handleRentalPrices(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	var d rentalData
	var err error
	if d.Plans, err = workshop.RatePlans(r.Context(), s.pool, session.Scope); s.handoverError(w, r, err) {
		return
	}
	if d.Extras, err = workshop.RentalExtras(r.Context(), s.pool, session.Scope); s.handoverError(w, r, err) {
		return
	}
	if d.Units, err = workshop.RentalUnits(r.Context(), s.pool, session.Scope); s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "rentalprices", pageData{Title: "Rental prices", Session: session, Rental: d,
		Today: time.Now().In(s.shopLocation(r)).Format("2006-01-02")})
}

func (s *Server) rentalPricesDone(w http.ResponseWriter, r *http.Request, err error) {
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/rental/prices", http.StatusSeeOther)
}

func (s *Server) handleRatePlanAdd(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	vat, err := strconv.Atoi(strings.TrimSpace(r.FormValue("vat_percent")))
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", "Write the VAT rate as a whole per cent.")
		return
	}
	_, err = workshop.CreateRatePlan(r.Context(), s.pool, session.Scope, workshop.NewRatePlan{Name: r.FormValue("name"),
		Category: r.FormValue("category"), UnitID: r.FormValue("unit"), VATRateBp: vat * 100, SeasonRule: r.FormValue("season_rule")})
	s.rentalPricesDone(w, r, err)
}

// grossToNet reads a price including VAT at rate as the net it is stored
// as; blank is no price.
func grossToNet(v string, rateBp int) (*int64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	gross, err := parseMinorUnits(v)
	if err != nil {
		return nil, err
	}
	net := money.Scale(gross, 10000, int64(10000+rateBp))
	return &net, nil
}

func (s *Server) handleRateSeasonAdd(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	rate, _ := strconv.Atoi(r.FormValue("vat_bp"))
	from, ok1 := s.formDate(r, "valid_from")
	until, ok2 := s.formDate(r, "valid_until")
	if !ok1 || !ok2 || from == nil {
		s.renderError(w, r, http.StatusBadRequest, "That date did not parse", "Use the picker.")
		return
	}
	season := workshop.RateSeason{ValidFrom: *from, ValidUntil: until}
	var bad error
	for name, into := range map[string]**int64{"hour": &season.Hour, "day": &season.Day, "weekend": &season.Weekend,
		"week": &season.Week, "month": &season.Month} {
		if *into, bad = grossToNet(r.FormValue(name), rate); bad != nil {
			break
		}
	}
	if bad == nil {
		season.ExtraKmMinor, bad = grossToNet(r.FormValue("extra_km"), rate)
	}
	ints := map[string]int{}
	for _, name := range []string{"included_km", "minimum_minutes", "grace_minutes"} {
		if v := strings.TrimSpace(r.FormValue(name)); v != "" && bad == nil {
			ints[name], bad = strconv.Atoi(v)
		}
	}
	if bad != nil {
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", "Write prices as amounts and kilometres and minutes as whole numbers.")
		return
	}
	if v, ok := ints["included_km"]; ok {
		season.IncludedKmPerDay = &v
	}
	season.Minimum = time.Duration(ints["minimum_minutes"]) * time.Minute
	season.Grace = time.Duration(ints["grace_minutes"]) * time.Minute
	_, err := workshop.AddRateSeason(r.Context(), s.pool, session.Scope, r.PathValue("id"), season)
	s.rentalPricesDone(w, r, err)
}

func (s *Server) handleRatePlanArchive(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	s.rentalPricesDone(w, r, workshop.ArchiveRatePlan(r.Context(), s.pool, session.Scope, r.PathValue("id")))
}

func (s *Server) handleRentalExtraAdd(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	vat, err := strconv.Atoi(strings.TrimSpace(r.FormValue("vat_percent")))
	category := "S"
	if r.FormValue("outside_vat") != "" {
		vat, category, err = 0, "O", nil
	}
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", "Write the VAT rate as a whole per cent.")
		return
	}
	price, err := grossToNet(r.FormValue("price"), vat*100)
	if err != nil || price == nil {
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", "Write prices as amounts and kilometres and minutes as whole numbers.")
		return
	}
	_, err = workshop.AddRentalExtra(r.Context(), s.pool, session.Scope, workshop.NewRentalExtra{Kind: r.FormValue("kind"),
		Name: r.FormValue("name"), Per: r.FormValue("per"), PriceMinor: *price, VATRateBp: vat * 100, VATCategory: category})
	s.rentalPricesDone(w, r, err)
}

func (s *Server) handleRentalExtraArchive(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	s.rentalPricesDone(w, r, workshop.ArchiveRentalExtra(r.Context(), s.pool, session.Scope, r.PathValue("id")))
}

// handleRentalBook books a unit from its page: for a renter picked, or one
// made on the spot.
func (s *Server) handleRentalBook(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", "")
		return
	}
	loc := s.shopLocation(r)
	from, err1 := parseWallClock(r.FormValue("from"), loc, "")
	to, err2 := parseWallClock(r.FormValue("to"), loc, "")
	if err1 != nil || err2 != nil {
		s.renderError(w, r, http.StatusBadRequest, "That date did not parse", "Use the picker.")
		return
	}
	n := workshop.NewRentalBooking{UnitID: r.FormValue("unit"), RenterID: r.FormValue("renter"), From: from, To: to,
		PlanID: r.FormValue("plan"), Key: r.FormValue("key")}
	if v := strings.TrimSpace(r.FormValue("hold_until")); v != "" {
		until, err := parseWallClock(v, loc, "")
		if err != nil {
			s.renderError(w, r, http.StatusBadRequest, "That date did not parse", "Use the picker.")
			return
		}
		n.HoldUntil = &until
	}
	for _, e := range r.Form["extra"] {
		n.Extras = append(n.Extras, workshop.ExtraChoice{ExtraID: e, Count: 1})
	}
	if v := strings.TrimSpace(r.FormValue("rent_price")); v != "" {
		gross, err := parseMinorUnits(v)
		if err != nil {
			s.renderError(w, r, http.StatusBadRequest, "Not accepted", "Write prices as amounts and kilometres and minutes as whole numbers.")
			return
		}
		n.Overrides = []workshop.PriceOverride{{Position: 1, UnitGrossMinor: &gross, Reason: r.FormValue("rent_reason")}}
	}
	if n.RenterID == "" {
		// Made with the booking, under its key: the form sent twice makes
		// them once.
		n.NewRenter = &workshop.NewParty{Kind: "private", Name: r.FormValue("renter_name"),
			Phone: r.FormValue("renter_phone"), Email: r.FormValue("renter_email")}
	}
	id, err := workshop.BookRental(r.Context(), s.pool, session.Scope, n)
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/rental/bookings/"+id, http.StatusSeeOther)
}

func (s *Server) handleRentalBooking(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	d := rentalData{Now: time.Now()}
	var err error
	if d.Booking, err = workshop.RentalBookingByID(r.Context(), s.pool, session.Scope, r.PathValue("id")); s.handoverError(w, r, err) {
		return
	}
	if d.Drivers, err = workshop.RentalDrivers(r.Context(), s.pool, session.Scope, d.Booking.ID); s.handoverError(w, r, err) {
		return
	}
	if d.Booking.Open() {
		if d.Gaps, err = workshop.RentalBookingGaps(r.Context(), s.pool, session.Scope, d.Booking.ID); s.handoverError(w, r, err) {
			return
		}
		if d.Templates, err = workshop.RentalAgreementTemplates(r.Context(), s.pool, session.Scope); s.handoverError(w, r, err) {
			return
		}
	}
	if d.Agreements, err = workshop.RentalAgreements(r.Context(), s.pool, session.Scope, d.Booking.ID); s.handoverError(w, r, err) {
		return
	}
	if d.Handovers, err = workshop.RentalHandovers(r.Context(), s.pool, session.Scope, d.Booking.ID); s.handoverError(w, r, err) {
		return
	}
	if d.Overrides, err = workshop.RentalGapOverrides(r.Context(), s.pool, session.Scope, d.Booking.ID); s.handoverError(w, r, err) {
		return
	}
	if d.Deposit, err = workshop.RentalDepositOf(r.Context(), s.pool, session.Scope, d.Booking.ID); s.handoverError(w, r, err) {
		return
	}
	if d.Charges, err = workshop.RentalCharges(r.Context(), s.pool, session.Scope, d.Booking.ID); s.handoverError(w, r, err) {
		return
	}
	// Whom a driver or an invoice can be: the renters there are.
	if d.Renters, err = workshop.Renters(r.Context(), s.pool, session.Scope); s.handoverError(w, r, err) {
		return
	}
	if d.Invoices, err = workshop.RentalInvoices(r.Context(), s.pool, session.Scope, d.Booking.ID); s.handoverError(w, r, err) {
		return
	}
	d.Key = newFormKey()
	s.render(w, r, http.StatusOK, "rentalbooking", pageData{Title: "Rental booking", Session: session, Rental: d, Methods: workshop.PaymentMethods,
		Today: time.Now().In(s.shopLocation(r)).Format("2006-01-02")})
}

func (s *Server) handleRentalBookingAction(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id, loc := r.PathValue("id"), s.shopLocation(r)
	var err error
	switch r.PathValue("action") {
	case "check-out":
		s.handleRentalHandoverStart(w, r, workshop.HandoverOut)
		return
	case "check-in":
		s.handleRentalHandoverStart(w, r, workshop.HandoverIn)
		return
	case "let-go":
		err = workshop.OverrideHandoverGap(r.Context(), s.pool, session.Scope, id, r.FormValue("code"), r.FormValue("reason"))
	case "confirm":
		err = workshop.ConfirmRentalBooking(r.Context(), s.pool, session.Scope, id)
	case "cancel", "no-show":
		err = workshop.CancelRentalBooking(r.Context(), s.pool, session.Scope, id, r.FormValue("reason"), r.PathValue("action") == "no-show")
	case "move":
		from, err1 := parseWallClock(r.FormValue("from"), loc, r.FormValue("from_was"))
		to, err2 := parseWallClock(r.FormValue("to"), loc, r.FormValue("to_was"))
		if err1 != nil || err2 != nil {
			s.renderError(w, r, http.StatusBadRequest, "That date did not parse", "Use the picker.")
			return
		}
		err = workshop.MoveRentalBooking(r.Context(), s.pool, session.Scope, id, from, to, r.FormValue("reason"))
	case "extend":
		to, perr := parseWallClock(r.FormValue("to"), loc, r.FormValue("to_was"))
		if perr != nil {
			s.renderError(w, r, http.StatusBadRequest, "That date did not parse", "Use the picker.")
			return
		}
		err = workshop.ExtendRentalBooking(r.Context(), s.pool, session.Scope, id, to, r.FormValue("reason"))
	case "drivers":
		party := r.FormValue("party")
		if party == "" {
			party, err = workshop.CreateParty(r.Context(), s.pool, session.Scope, workshop.NewParty{Kind: "private",
				Name: r.FormValue("name"), Phone: r.FormValue("phone"), Role: workshop.PartyRenter})
		}
		if err == nil {
			_, err = workshop.AddRentalDriver(r.Context(), s.pool, session.Scope, id, party, r.FormValue("main") != "")
		}
	case "agreement":
		var doc string
		if doc, err = workshop.ProduceRentalAgreement(r.Context(), s.pool, session.Scope, id, r.FormValue("template")); err == nil {
			s.submitted(w, r)
			http.Redirect(w, r, "/contracts/"+doc, http.StatusSeeOther)
			return
		}
	default:
		http.NotFound(w, r)
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/rental/bookings/"+id, http.StatusSeeOther)
}

// backToBooking is where a driver's or an agreement's form returns: the
// booking it came from, named by the form.
func backToBooking(r *http.Request) string {
	if b := r.FormValue("booking"); access.IsUUID(b) {
		return "/rental/bookings/" + b
	}
	return "/rental"
}

func (s *Server) handleLicenceCheck(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", "")
		return
	}
	l := workshop.LicenceShown{Shown: r.FormValue("shown") != "", AgeOK: r.FormValue("age_ok") != "", Categories: r.Form["categories"],
		Method: r.FormValue("method"), Note: r.FormValue("note")}
	if until, ok := s.formDate(r, "valid_until"); !ok {
		s.renderError(w, r, http.StatusBadRequest, "That date did not parse", "Use the picker.")
		return
	} else if until != nil {
		l.ValidUntil = *until
	}
	_, err := workshop.RecordLicenceCheck(r.Context(), s.pool, session.Scope, r.PathValue("id"), l)
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, backToBooking(r), http.StatusSeeOther)
}

func (s *Server) handleDriverRemove(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	err := workshop.RemoveRentalDriver(r.Context(), s.pool, session.Scope, r.PathValue("id"), r.FormValue("reason"))
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, backToBooking(r), http.StatusSeeOther)
}

// handleAgreementAccepted records that a party accepted the agreement as
// it was shown -- in person, or on paper with who saw it signed. A signing
// service, when there is one, is #214's.
func (s *Server) handleAgreementAccepted(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	method := workshop.SignedInPerson
	if r.FormValue("method") == workshop.SignedOnPaper {
		method = workshop.SignedOnPaper
	}
	err := workshop.RecordSignature(r.Context(), s.pool, session.Scope, r.PathValue("id"), r.FormValue("party"),
		workshop.SignatureEvidence{Method: method, Hash: r.FormValue("hash"), WitnessedBy: r.FormValue("witnessed_by")})
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, backToBooking(r), http.StatusSeeOther)
}
