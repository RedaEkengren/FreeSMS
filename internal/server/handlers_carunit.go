package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// One car in stock (#167, #164, #162): its status and the moves the role
// may make, and for whoever buys cars its purchase, its costs and the checks
// before money leaves. Each part is read through the operation that checks
// the role; a part the role may not read is simply not there.

// carUnitData is the page for one stock unit.
type carUnitData struct {
	Unit    workshop.StockUnit
	Vehicle workshop.Vehicle
	Moves   []workshop.StockStatus
	// For whoever may see what a car cost; nil for any other role.
	Purchase *workshop.Purchase
	Costs    *workshop.StockCosts
	// For whoever buys cars.
	Checks      []workshop.StockCheck
	Outstanding []string
	Overridden  bool
	ShowChecks  bool
	Form        map[string]string
}

// CheckKinds are the kinds of check a form offers.
func (carUnitData) CheckKinds() []string { return workshop.CheckKinds }

// CheckKindLabel is a kind of check in words, a catalogue key.
func (carUnitData) CheckKindLabel(k string) string { return workshop.CheckKindLabel(k) }

// CostKinds are the kinds of cost a form offers.
func (carUnitData) CostKinds() []string { return workshop.CostKinds }

// CostKindLabel is a kind of cost in words, a catalogue key.
func (carUnitData) CostKindLabel(k string) string { return workshop.CostKindLabel(k) }

// SellerTypeLabel is a kind of seller in words, a catalogue key.
func (carUnitData) SellerTypeLabel(t string) string { return workshop.SellerTypeLabel(t) }

// Today is today's date as a form's date field holds it.
func (d carUnitData) Today() string { return time.Now().Format(time.DateOnly) }

// handleCarUnit is one car in stock.
func (s *Server) handleCarUnit(w http.ResponseWriter, r *http.Request) {
	s.carUnitPage(w, r, http.StatusOK, nil, "")
}

func (s *Server) carUnitPage(w http.ResponseWriter, r *http.Request, status int, form map[string]string, msg string) {
	session := sessionFrom(r.Context())
	scope := session.Scope
	unit, err := workshop.StockUnitByID(r.Context(), s.pool, scope, r.PathValue("id"))
	if s.handoverError(w, r, err) {
		return
	}
	d := &carUnitData{Unit: unit, Form: form, Moves: workshop.StockMovesFor(scope.Role, unit.Status)}
	if d.Vehicle, err = workshop.VehicleByID(r.Context(), s.pool, scope, unit.VehicleID); err != nil {
		s.log.Error("read vehicle", "error", err)
	}
	if scope.Role.SeesPurchasePrice() {
		if p, err := workshop.PurchaseOfUnit(r.Context(), s.pool, scope, unit.ID); err == nil {
			d.Purchase = &p
		} else if !errors.Is(err, workshop.ErrNotFound) {
			s.log.Error("read purchase", "error", err)
		}
		if c, err := workshop.CostsOfUnit(r.Context(), s.pool, scope, unit.ID); err == nil {
			d.Costs = &c
		} else {
			s.log.Error("read costs", "error", err)
		}
	}
	if scope.Role.BuysVehicles() {
		d.ShowChecks = true
		if d.Checks, err = workshop.StockChecks(r.Context(), s.pool, scope, unit.ID); err != nil {
			s.log.Error("read checks", "error", err)
		}
		if d.Outstanding, d.Overridden, err = workshop.ChecksOutstanding(r.Context(), s.pool, scope, unit.ID); err != nil {
			s.log.Error("read outstanding checks", "error", err)
		}
	}
	s.render(w, r, status, "carunit", pageData{Title: "Car in stock", Session: session, CarUnit: d, Error: msg})
}

// carUnitRefused answers a refused form on the unit's page, or as the
// operation's error says.
func (s *Server) carUnitRefused(w http.ResponseWriter, r *http.Request, err error, form map[string]string) bool {
	if errors.Is(err, workshop.ErrInvalid) {
		s.carUnitPage(w, r, http.StatusBadRequest, form, trimInvalid(err))
		return true
	}
	return s.handoverError(w, r, err)
}

func (s *Server) carUnitDone(w http.ResponseWriter, r *http.Request) {
	s.submitted(w, r)
	http.Redirect(w, r, "/cars/stock/"+r.PathValue("id"), http.StatusSeeOther)
}

// handleMoveCarUnit moves the car to another status.
func (s *Server) handleMoveCarUnit(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	err := workshop.MoveStockUnit(r.Context(), s.pool, session.Scope, r.PathValue("id"),
		workshop.StockStatus(r.FormValue("to")), r.FormValue("reason"))
	if s.carUnitRefused(w, r, err, map[string]string{"reason": r.FormValue("reason")}) {
		return
	}
	s.carUnitDone(w, r)
}

// handleCarUnitCheck records a check made by hand.
func (s *Server) handleCarUnitCheck(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	err := workshop.RecordStockCheck(r.Context(), s.pool, session.Scope, r.PathValue("id"), workshop.NewStockCheck{
		Kind: r.FormValue("kind"), Result: r.FormValue("result"), Note: r.FormValue("check_note")})
	if s.carUnitRefused(w, r, err, map[string]string{"check_note": r.FormValue("check_note")}) {
		return
	}
	s.carUnitDone(w, r)
}

// handleCarUnitOverride lets money leave with checks outstanding.
func (s *Server) handleCarUnitOverride(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	err := workshop.OverrideStockChecks(r.Context(), s.pool, session.Scope, r.PathValue("id"), r.FormValue("override_reason"))
	if s.carUnitRefused(w, r, err, map[string]string{"override_reason": r.FormValue("override_reason")}) {
		return
	}
	s.carUnitDone(w, r)
}

// handleCarUnitCost records a cost spent on the car.
func (s *Server) handleCarUnitCost(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	form := map[string]string{}
	for _, k := range []string{"cost_kind", "net", "rate", "incurred_on", "ref", "cost_note"} {
		form[k] = strings.TrimSpace(r.FormValue(k))
	}
	net, err := parseMinorSigned(form["net"])
	if err != nil || form["net"] == "" {
		s.carUnitPage(w, r, http.StatusBadRequest, form, "Say what it cost before VAT, as a number.")
		return
	}
	rate, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(form["rate"]), "%"))
	if err != nil {
		s.carUnitPage(w, r, http.StatusBadRequest, form, "The VAT rate is a whole number of per cent.")
		return
	}
	day, err := time.ParseInLocation(time.DateOnly, form["incurred_on"], s.shopLocation(r))
	if err != nil {
		s.carUnitPage(w, r, http.StatusBadRequest, form, "Say the day it was spent.")
		return
	}
	_, err = workshop.RecordStockCost(r.Context(), s.pool, session.Scope, r.PathValue("id"), workshop.NewStockCost{
		Kind: form["cost_kind"], NetMinor: net, VATRateBasis: rate * 100, IncurredOn: day, ExternalRef: form["ref"], Note: form["cost_note"]})
	if s.carUnitRefused(w, r, err, form) {
		return
	}
	s.carUnitDone(w, r)
}

// handleCarUnitReverseCost reverses a cost entered by mistake.
func (s *Server) handleCarUnitReverseCost(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	_, err := workshop.ReverseStockCost(r.Context(), s.pool, session.Scope, r.PathValue("cost"), r.FormValue("reverse_reason"))
	if s.carUnitRefused(w, r, err, map[string]string{"reverse_reason": r.FormValue("reverse_reason")}) {
		return
	}
	s.carUnitDone(w, r)
}

// carUnitRoutes registers the pages of one car in stock.
func (s *Server) carUnitRoutes(m routeMux) {
	m.HandleFunc("GET /cars/stock/{id}", s.requireSession(s.handleCarUnit))
	m.HandleFunc("POST /cars/stock/{id}/move", s.requireSession(s.handleMoveCarUnit))
	m.HandleFunc("POST /cars/stock/{id}/checks", s.requireSession(s.handleCarUnitCheck))
	m.HandleFunc("POST /cars/stock/{id}/checks/override", s.requireSession(s.handleCarUnitOverride))
	m.HandleFunc("POST /cars/stock/{id}/costs", s.requireSession(s.handleCarUnitCost))
	m.HandleFunc("POST /cars/stock/{id}/costs/{cost}/reverse", s.requireSession(s.handleCarUnitReverseCost))
}
