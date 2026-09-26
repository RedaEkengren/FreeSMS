package server

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

func (s *Server) handleStock(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())

	parts, err := workshop.Parts(r.Context(), s.pool, session.Scope)
	if s.handoverError(w, r, err) {
		return
	}
	// What is running out goes to the top. The parts desk opens this screen to
	// find out what to order, and a catalogue in number order answers a
	// question nobody asked. Sorted here rather than in the query because
	// "short" is Available() against the minimum, which is Go's answer and
	// should have one definition.
	sort.SliceStable(parts, func(i, j int) bool {
		return partUrgency(parts[i]) < partUrgency(parts[j])
	})
	bands, err := workshop.PriceBands(r.Context(), s.pool, session.Scope)
	if err != nil {
		s.log.Error("price bands", "error", err)
	}

	// What a month of write-offs cost, which is the number the ledger exists
	// for. Only the front desk sees it; it is a figure about the business
	// rather than about the shelf.
	var writeOffs []workshop.WriteOffCost
	if session.Scope.Role.SeesCustomerPersonalData() {
		loc := s.shopLocation(r)
		now := time.Now().In(loc)
		from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
		writeOffs, err = workshop.WriteOffs(r.Context(), s.pool, session.Scope, from, from.AddDate(0, 1, 0))
		if err != nil {
			s.log.Error("write-offs", "error", err)
		}
	}

	s.render(w, r, http.StatusOK, "stock", pageData{
		Title:      "Stock",
		Session:    session,
		Parts:      parts,
		PriceBands: bands,
		WriteOffs:  writeOffs,
		Reasons:    workshop.WriteOffReasons(),
	})
}

// handleStockMove records one entry in the ledger.
func (s *Server) handleStockMove(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())

	quantity, err := parseScaled(r.FormValue("quantity"), 3)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That quantity did not parse",
			"Write it as a number: 4 or 4,5.")
		return
	}
	qty := float64(quantity) / 1000
	if qty <= 0 {
		s.renderError(w, r, http.StatusBadRequest, "That quantity did not parse",
			"Say how many; the direction comes from what you are doing.")
		return
	}

	kind := r.FormValue("kind")
	m := workshop.Movement{
		Kind: kind,
		Note: r.FormValue("note"),
	}
	// The form asks how many; the sign belongs to the kind of movement, not to
	// whoever is typing at a counter.
	switch kind {
	case "received":
		m.Quantity = qty
	case "consumed", "returned", "written_off":
		m.Quantity = -qty
		m.Reason = r.FormValue("reason")
	case "counted":
		// A stocktake says what is there, so the movement is the difference.
		m.Quantity = qty
	default:
		s.renderError(w, r, http.StatusBadRequest, "Unknown movement", "")
		return
	}

	if err := workshop.Move(r.Context(), s.pool, session.Scope, m,
		r.FormValue("part_id"), "", ""); s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/stock", http.StatusSeeOther)
}

func (s *Server) handleSavePriceBand(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())

	var upTo *int64
	if v := strings.TrimSpace(r.FormValue("up_to")); v != "" {
		parsed, err := parseMinorUnits(v)
		if err != nil {
			s.renderError(w, r, http.StatusBadRequest, "That ceiling did not parse", err.Error())
			return
		}
		upTo = &parsed
	}
	markup, err := parseScaled(r.FormValue("markup"), 2)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That markup did not parse", "A percentage: 45 or 45,5.")
		return
	}
	if err := workshop.SavePriceBand(r.Context(), s.pool, session.Scope, upTo, int(markup)); s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/stock", http.StatusSeeOther)
}

// partUrgency ranks a part for the stock screen: below nothing first, because
// that is a discrepancy somebody has to explain; then at or below the minimum,
// which is an order to place; then everything else, in number order, which the
// query already produced and a stable sort preserves.
func partUrgency(p workshop.Part) int {
	switch {
	case p.Negative():
		return 0
	case p.Short():
		return 1
	default:
		return 2
	}
}
