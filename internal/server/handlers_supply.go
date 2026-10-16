package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Parts bought outside FreeSMS: orders, deliveries, returns. The rules are in
// the workshop package; these read the forms.

func (s *Server) today(r *http.Request) time.Time {
	return time.Now().In(s.shopLocation(r))
}

// dayField reads a date input in the shop's zone; empty is nil.
func (s *Server) dayField(r *http.Request, name string) (*time.Time, bool) {
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

func (s *Server) handleSupply(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	desk, err := workshop.SupplyDeskFor(r.Context(), s.pool, session.Scope, s.today(r))
	if s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "supply", pageData{
		Title: "Supply", Session: session, Supply: desk, Today: s.today(r).Format("2006-01-02"),
	})
}

func (s *Server) handleOrder(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	order, err := workshop.OrderByID(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	if s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "order", pageData{
		Title: order.Supplier, Session: session, Order: order, Today: s.today(r).Format("2006-01-02"),
	})
}

func (s *Server) handleRecordOrder(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", "That form could not be read.")
		return
	}
	in := workshop.OrderInput{
		Supplier: r.FormValue("supplier"), Reference: r.FormValue("reference"), Note: r.FormValue("note"),
	}
	var ok bool
	var placed, eta *time.Time
	if placed, ok = s.dayField(r, "placed_on"); !ok {
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", "That is not a date.")
		return
	}
	if placed != nil {
		in.PlacedOn = *placed
	}
	if eta, ok = s.dayField(r, "eta"); !ok {
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", "That is not a date.")
		return
	}
	in.ETA = eta
	cost := func(name string) (*int64, bool) {
		v := strings.TrimSpace(r.FormValue(name))
		if v == "" {
			return nil, true
		}
		c, err := parseMinorUnits(v)
		if err != nil || c < 0 {
			return nil, false
		}
		return &c, true
	}
	// One line for each request ticked, and one more for anything else.
	for key := range r.Form {
		id, found := strings.CutPrefix(key, "use_")
		if !found {
			continue
		}
		qty, err := parseScaled(r.FormValue("qty_"+id), 3)
		if err != nil || qty <= 0 {
			s.renderError(w, r, http.StatusBadRequest, "That quantity did not parse", "Write it as a number: 1,5 or 1.5.")
			return
		}
		c, good := cost("cost_" + id)
		if !good {
			s.renderError(w, r, http.StatusBadRequest, "That cost did not parse", "Write it as an amount: 150 or 150,50.")
			return
		}
		in.Lines = append(in.Lines, workshop.OrderLineInput{RequestID: id, Quantity: float64(qty) / 1000, ExpectedUnitCostMinor: c,
			PartNumber: strings.TrimSpace(r.FormValue("part_" + id))})
	}
	if d := strings.TrimSpace(r.FormValue("extra_desc")); d != "" || strings.TrimSpace(r.FormValue("extra_part")) != "" {
		qty, err := parseScaled(r.FormValue("extra_qty"), 3)
		if err != nil || qty <= 0 {
			s.renderError(w, r, http.StatusBadRequest, "That quantity did not parse", "Write it as a number: 1,5 or 1.5.")
			return
		}
		c, good := cost("extra_cost")
		if !good {
			s.renderError(w, r, http.StatusBadRequest, "That cost did not parse", "Write it as an amount: 150 or 150,50.")
			return
		}
		in.Lines = append(in.Lines, workshop.OrderLineInput{Description: d, PartNumber: r.FormValue("extra_part"),
			Quantity: float64(qty) / 1000, ExpectedUnitCostMinor: c})
	}
	id, err := workshop.RecordOrder(r.Context(), s.pool, session.Scope, in)
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/supply/orders/"+id, http.StatusSeeOther)
}

func (s *Server) handleLineETA(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	eta, ok := s.dayField(r, "eta")
	if !ok || eta == nil {
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", "Say which day it is expected.")
		return
	}
	err := workshop.ChangeETA(r.Context(), s.pool, session.Scope, r.PathValue("line"), *eta, r.FormValue("note"))
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, backTo(r), http.StatusSeeOther)
}

func (s *Server) handleReceiveDelivery(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	qty, err := parseScaled(r.FormValue("quantity"), 3)
	if err != nil || qty <= 0 {
		s.renderError(w, r, http.StatusBadRequest, "That quantity did not parse", "Write it as a number: 1,5 or 1.5.")
		return
	}
	in := workshop.DeliveryInput{
		LineID: r.PathValue("line"), Quantity: float64(qty) / 1000, DeliveryRef: r.FormValue("delivery_ref"),
		Route: r.FormValue("route"), Substitute: r.FormValue("substitute"),
	}
	if v := strings.TrimSpace(r.FormValue("unit_cost")); v != "" {
		c, err := parseMinorUnits(v)
		if err != nil || c < 0 {
			s.renderError(w, r, http.StatusBadRequest, "That cost did not parse", "Write it as an amount: 150 or 150,50.")
			return
		}
		in.UnitCostMinor = &c
	}
	_, _, err = workshop.ReceiveDelivery(r.Context(), s.pool, session.Scope, in)
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, backTo(r), http.StatusSeeOther)
}

// back sends the person to where they were, but only within the supply pages.
func backTo(r *http.Request) string {
	b := r.FormValue("back")
	if strings.HasPrefix(b, "/supply") && !strings.HasPrefix(b, "//") {
		return b
	}
	return "/supply"
}

func (s *Server) handleSubstitute(accept bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := sessionFrom(r.Context())
		var err error
		if accept {
			err = workshop.AcceptSubstitute(r.Context(), s.pool, session.Scope, r.PathValue("receipt"))
		} else {
			_, err = workshop.RejectSubstitute(r.Context(), s.pool, session.Scope, r.PathValue("receipt"))
		}
		if s.handoverError(w, r, err) {
			return
		}
		http.Redirect(w, r, backTo(r), http.StatusSeeOther)
	}
}

func (s *Server) handleCancelOrder(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	var err error
	if r.FormValue("confirmed") != "" {
		err = workshop.ConfirmSupplierCancellation(r.Context(), s.pool, session.Scope, id)
	} else {
		err = workshop.CancelOrder(r.Context(), s.pool, session.Scope, id, r.FormValue("note"))
	}
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/supply/orders/"+id, http.StatusSeeOther)
}

func (s *Server) handlePlanReturn(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	qty, err := parseScaled(r.FormValue("quantity"), 3)
	if err != nil || qty <= 0 {
		s.renderError(w, r, http.StatusBadRequest, "That quantity did not parse", "Write it as a number: 1,5 or 1.5.")
		return
	}
	in := workshop.ReturnInput{
		ReceiptID: r.PathValue("receipt"), Quantity: float64(qty) / 1000, Reason: r.FormValue("reason"),
		Note: r.FormValue("note"), NeedReplacement: r.FormValue("replacement") != "",
	}
	if v := strings.TrimSpace(r.FormValue("expected_credit")); v != "" {
		c, err := parseMinorUnits(v)
		if err != nil || c < 0 {
			s.renderError(w, r, http.StatusBadRequest, "That amount did not parse", "Write it as an amount: 150 or 150,50.")
			return
		}
		in.ExpectedCreditMinor = &c
	}
	_, err = workshop.PlanReturn(r.Context(), s.pool, session.Scope, in)
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, backTo(r), http.StatusSeeOther)
}

func (s *Server) handleReturned(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	err := workshop.MarkReturned(r.Context(), s.pool, session.Scope, r.PathValue("ret"))
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, backTo(r), http.StatusSeeOther)
}

func (s *Server) handleCredit(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	c, err := parseMinorUnits(strings.TrimSpace(r.FormValue("credited")))
	if err != nil || c < 0 {
		s.renderError(w, r, http.StatusBadRequest, "That amount did not parse", "Write it as an amount: 150 or 150,50.")
		return
	}
	err = workshop.ConfirmCredit(r.Context(), s.pool, session.Scope, r.PathValue("ret"), c, r.FormValue("credit_ref"))
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, backTo(r), http.StatusSeeOther)
}
