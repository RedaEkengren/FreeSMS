package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// handleShop shows the workshop's own particulars: what goes at the top of an
// invoice, and what the law requires to be there.
func (s *Server) handleShop(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())

	details, err := workshop.ShopDetailsFor(r.Context(), s.pool, session.Scope)
	if errors.Is(err, access.ErrForbidden) {
		s.renderError(w, r, http.StatusForbidden, "Not for your role",
			"The workshop's own details belong to whoever runs it.")
		return
	}
	if err != nil {
		s.log.Error("read shop details", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
		return
	}

	charges, err := workshop.SurchargesFor(r.Context(), s.pool, session.Scope)
	if err != nil {
		s.log.Error("read surcharges", "error", err)
	}
	s.render(w, r, http.StatusOK, "shop", pageData{
		Title:     "The workshop",
		Session:   session,
		Shop:      details,
		Charges:   charges,
		Languages: workshop.Languages(),
	})
}

// handleSaveSurcharges sets förbrukningsmaterial and the invoicing fee.
// Typed as a person writes them: "5" or "5,0" per cent, amounts in kronor.
func (s *Server) handleSaveSurcharges(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	pct, err := parseScaled(r.FormValue("consumables_percent"), 2)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That percentage did not parse", "Write it as 5 or 5,5.")
		return
	}
	charges := workshop.Surcharges{ConsumablesBasis: int(pct)}
	if v := strings.TrimSpace(r.FormValue("consumables_cap")); v != "" {
		cap, err := parseMinorUnits(v)
		if err != nil {
			s.renderError(w, r, http.StatusBadRequest, "That ceiling did not parse", err.Error())
			return
		}
		charges.ConsumablesCapMinor = &cap
	}
	if charges.InvoiceFeeMinor, err = parseMinorUnits(r.FormValue("invoice_fee")); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That fee did not parse", err.Error())
		return
	}
	if err := workshop.SaveSurcharges(r.Context(), s.pool, session.Scope, charges); s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/shop", http.StatusSeeOther)
}

func (s *Server) handleSaveShop(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())

	// Blank means the default rather than zero: somebody who clears the field
	// has not said "due on the day of issue".
	terms := 30
	if v := strings.TrimSpace(r.FormValue("payment_terms_days")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			s.renderError(w, r, http.StatusBadRequest, "That did not parse",
				"Payment terms are a number of days.")
			return
		}
		terms = n
	}

	details := workshop.ShopDetails{
		Name:             r.FormValue("name"),
		AddressLine1:     r.FormValue("address_line1"),
		AddressLine2:     r.FormValue("address_line2"),
		PostalCode:       r.FormValue("postal_code"),
		City:             r.FormValue("city"),
		OrgNumber:        r.FormValue("org_number"),
		VATNumber:        r.FormValue("vat_number"),
		Phone:            r.FormValue("phone"),
		Email:            r.FormValue("email"),
		PaymentReference: r.FormValue("payment_reference"),
		PaymentTermsDays: terms,
		FTax:             r.FormValue("f_tax") == "yes",
		Locale:           r.FormValue("locale"),
	}

	err := workshop.SaveDetails(r.Context(), s.pool, session.Scope, details)
	switch {
	case errors.Is(err, access.ErrForbidden):
		s.renderError(w, r, http.StatusForbidden, "Not for your role", "")
		return
	case errors.Is(err, workshop.ErrInvalid):
		s.renderError(w, r, http.StatusBadRequest, "Not saved", trimInvalid(err))
		return
	case err != nil:
		s.log.Error("save shop details", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
		return
	}

	s.submitted(w, r)
	http.Redirect(w, r, "/shop", http.StatusSeeOther)
}
