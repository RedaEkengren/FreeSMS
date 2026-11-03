package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Return charges (#195): confirmed on the booking's page, from what the
// return suggests or by hand; how each kind is taxed on the settings page.

// ChargeKinds are the kinds of return charge a form offers.
func (rentalData) ChargeKinds() []string { return workshop.ChargeKinds }

// ChargeChannels are how a customer is told of a charge.
func (rentalData) ChargeChannels() []string { return workshop.DamageChannels }

// ChargeName is what a kind of charge is called on a page: a catalogue
// key.
func (rentalData) ChargeName(kind string) string {
	return map[string]string{
		workshop.ChargeLate: "Late return", workshop.ChargeExtraKm: "Kilometres over the allowance", workshop.ChargeFuel: "Fuel or charging",
		workshop.ChargeCleaning: "Cleaning", workshop.ChargeDamage: "Damage compensation", workshop.ChargeExcess: "Excess",
		workshop.ChargeCongestionTax: "Congestion tax, re-invoiced", workshop.ChargeParkingTolls: "Parking and tolls",
		workshop.ChargeAdminFee: "Administration fee", workshop.ChargeCancellationFee: "Cancellation fee", workshop.ChargeNoShowFee: "No-show fee",
	}[kind]
}

// handleChargeClass sets how a kind of charge is taxed.
func (s *Server) handleChargeClass(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	c := workshop.ChargeClass{Kind: r.FormValue("kind"), VATCategory: r.FormValue("category"), Confirmed: r.FormValue("confirmed") != ""}
	if v := strings.TrimSpace(r.FormValue("rate")); v != "" {
		bp, err := parseScaled(v, 2)
		if err != nil {
			s.renderError(w, r, http.StatusBadRequest, "Not accepted", "Say the rate in per cent.")
			return
		}
		c.VATRateBp = int(bp)
	}
	if s.handoverError(w, r, workshop.SetChargeClass(r.Context(), s.pool, session.Scope, c)) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/rental/settings#charges", http.StatusSeeOther)
}

// handleRentalCharge confirms a charge on the booking.
func (s *Server) handleRentalCharge(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	n := workshop.NewCharge{Kind: r.FormValue("kind"), Description: r.FormValue("description"), DamageCaseID: r.FormValue("damage_case"),
		Proposed: r.FormValue("proposed") != "", Key: r.FormValue("key")}
	if n.Kind != workshop.ChargeDamage {
		qty, err1 := parseScaled(strings.TrimSpace(r.FormValue("quantity")), 3)
		price, err2 := parseMinorUnits(strings.TrimSpace(r.FormValue("price")))
		if err1 != nil || err2 != nil {
			s.renderError(w, r, http.StatusBadRequest, "Not accepted", "Write the quantity as a number and the price in kronor.")
			return
		}
		n.QuantityMilli, n.UnitPriceMinor = qty, price
	}
	_, err := workshop.ConfirmCharge(r.Context(), s.pool, session.Scope, r.PathValue("id"), n)
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/rental/bookings/"+r.PathValue("id")+"#charges", http.StatusSeeOther)
}

// handleRentalChargeEvent records the customer told of a charge, or the
// charge withdrawn.
func (s *Server) handleRentalChargeEvent(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	var err error
	switch r.PathValue("what") {
	case "inform":
		err = workshop.InformCharge(r.Context(), s.pool, session.Scope, r.PathValue("charge"), r.FormValue("channel"))
	case "withdraw":
		err = workshop.WithdrawCharge(r.Context(), s.pool, session.Scope, r.PathValue("charge"), r.FormValue("reason"))
	default:
		http.NotFound(w, r)
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/rental/bookings/"+r.PathValue("id")+"#charges", http.StatusSeeOther)
}

// Kronor is an amount as a form field holds it: kronor, with öre when
// there are any.
func (rentalData) Kronor(minor int64) string {
	if minor%100 == 0 {
		return strconv.FormatInt(minor/100, 10)
	}
	return fmt.Sprintf("%d,%02d", minor/100, minor%100)
}

// chargeRoutes registers the return charges.
func (s *Server) chargeRoutes(m routeMux) {
	m.HandleFunc("POST /rental/charge-classes", s.requireSession(s.handleChargeClass))
	m.HandleFunc("POST /rental/bookings/{id}/charges", s.requireSession(s.handleRentalCharge))
	m.HandleFunc("POST /rental/bookings/{id}/charges/{charge}/{what}", s.requireSession(s.handleRentalChargeEvent))
}
