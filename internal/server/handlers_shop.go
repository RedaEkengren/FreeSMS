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

	s.render(w, r, http.StatusOK, "shop", pageData{
		Title:   "The workshop",
		Session: session,
		Shop:    details,
	})
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
