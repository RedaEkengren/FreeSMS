package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// handleSetState moves an order, and says plainly when it will not move.
func (s *Server) handleSetState(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	to := workshop.State(r.FormValue("state"))

	// No blanket role check here. Which moves belong to which role is decided
	// in SetState, transition by transition: a technician owns "needs parts"
	// and "ready", the counter owns everything that is a conversation with
	// the customer. Refusing the whole endpoint to technicians is what made
	// them walk across the workshop to say the car was finished.

	err := workshop.SetState(r.Context(), s.pool, session.Scope, id, to)

	var illegal workshop.ErrIllegalTransition
	switch {
	case errors.Is(err, workshop.ErrNotFound):
		s.renderError(w, r, http.StatusNotFound, "Not found", "No such job.")
		return
	case errors.Is(err, workshop.ErrNoCustomer):
		s.renderError(w, r, http.StatusConflict, "Nobody to bill", err.Error())
		return
	case errors.Is(err, workshop.ErrWouldLoseWork):
		// Worth a page rather than a flash: it is a refusal with a reason and
		// a different action to take, not a slip of the finger.
		s.renderError(w, r, http.StatusConflict, "That would write off work", err.Error())
		return
	case errors.As(err, &illegal):
		s.renderError(w, r, http.StatusConflict, "Not from here", illegal.Error())
		return
	case errors.Is(err, access.ErrForbidden):
		s.renderError(w, r, http.StatusForbidden, "Not for your role",
			"That move belongs to the front desk.")
		return
	case err != nil:
		s.log.Error("set state", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
		return
	}

	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}

// handleAddLine puts a line on an order from the job page.
func (s *Server) handleAddLine(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if !session.Scope.Role.SeesCustomerPersonalData() {
		s.renderError(w, r, http.StatusForbidden, "Not for your role",
			"Pricing a job is done at the front desk.")
		return
	}
	id := r.PathValue("id")

	// A Swedish mechanic types 1,5 for an hour and a half. Refusing that is
	// refusing the way half the intended users write numbers. Parsed to
	// thousandths as text, because a float has no business anywhere near a
	// figure that ends up on an invoice.
	quantity, err := parseScaled(r.FormValue("quantity"), 3)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That quantity did not parse",
			"Write it as a number, with a comma or a full stop: 1,5 or 1.5.")
		return
	}

	// Prices are typed in whole currency units and stored as minor units.
	// Parsing to a float and multiplying would introduce exactly the rounding
	// error the schema avoids by storing integers, so the text is split
	// instead.
	price, err := parseMinorUnits(r.FormValue("unit_price"))
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That price did not parse", err.Error())
		return
	}

	line := workshop.NewLine{
		Kind:           r.FormValue("kind"),
		Description:    r.FormValue("description"),
		QuantityMilli:  quantity,
		UnitPriceMinor: price,
		VATRateBasis:   2500,
		CostBearer:     r.FormValue("cost_bearer"),
		LabourTimeID:   strings.TrimSpace(r.FormValue("labour_time_id")),
		PartID:         strings.TrimSpace(r.FormValue("part_id")),
	}

	err = workshop.AddLine(r.Context(), s.pool, session.Scope, id, line)
	switch {
	case errors.Is(err, workshop.ErrNotFound):
		s.renderError(w, r, http.StatusNotFound, "Not found", "No such job.")
		return
	case errors.Is(err, workshop.ErrInvalid):
		s.renderError(w, r, http.StatusBadRequest, "That line was not accepted", trimInvalid(err))
		return
	case err != nil:
		s.log.Error("add line", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
		return
	}
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}

// parseMinorUnits turns "1 295,50" into 129550 without going through a float.
//
// A float cannot hold 129550 as a product of 1295.50 and 100 exactly, and the
// error is small enough to survive testing and large enough to make an invoice
// disagree with itself by a krona.
func parseMinorUnits(s string) (int64, error) { return parseScaled(s, 2) }

// parseScaled reads a decimal as an integer of 10^decimals units.
//
// Prices have two places; quantities have three, because labour is sold by the
// quarter hour and oil by the tenth of a litre.
func parseScaled(s string, decimals int) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, " ", "")
	s = strings.Replace(s, ",", ".", 1)

	scale := int64(1)
	for i := 0; i < decimals; i++ {
		scale *= 10
	}

	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	major, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || major < 0 {
		return 0, errors.New("write it as a number, for example 1295 or 1295,50")
	}

	var minor int64
	if hasFrac {
		if len(frac) > decimals {
			return 0, fmt.Errorf("that has more than %d decimals", decimals)
		}
		// Pad so that "5" with two decimals means 50, not 5.
		for len(frac) < decimals {
			frac += "0"
		}
		if frac != "" {
			minor, err = strconv.ParseInt(frac, 10, 64)
			if err != nil {
				return 0, errors.New("write it as a number, for example 1295 or 1295,50")
			}
		}
	}
	return major*scale + minor, nil
}

// handleInvoice issues the invoice for a job.
func (s *Server) handleInvoice(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")

	_, err := workshop.Issue(r.Context(), s.pool, session.Scope, id)

	var illegal workshop.ErrIllegalTransition
	switch {
	case errors.Is(err, access.ErrForbidden):
		s.renderError(w, r, http.StatusForbidden, "Not for your role",
			"Invoicing is done at the front desk.")
		return
	case errors.Is(err, workshop.ErrNotFound):
		s.renderError(w, r, http.StatusNotFound, "Not found", "No such job.")
		return
	case errors.Is(err, workshop.ErrAlreadyInvoiced),
		errors.Is(err, workshop.ErrNothingToInvoice),
		errors.Is(err, workshop.ErrNoCustomer):
		s.renderError(w, r, http.StatusConflict, "Not invoiced", err.Error())
		return
	case errors.As(err, &illegal):
		s.renderError(w, r, http.StatusConflict, "Not from here", illegal.Error())
		return
	case err != nil:
		s.log.Error("issue invoice", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
		return
	}
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}

// handleCreditNote reverses an issued invoice.
func (s *Server) handleCreditNote(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	jobID := r.PathValue("id")

	_, err := workshop.CreditNote(r.Context(), s.pool, session.Scope, r.FormValue("invoice_id"))
	switch {
	case errors.Is(err, access.ErrForbidden):
		s.renderError(w, r, http.StatusForbidden, "Not for your role", "")
		return
	case errors.Is(err, workshop.ErrNotFound):
		s.renderError(w, r, http.StatusNotFound, "Not found", "No such invoice.")
		return
	case errors.Is(err, workshop.ErrInvalid):
		s.renderError(w, r, http.StatusConflict, "Not credited", trimInvalid(err))
		return
	case err != nil:
		s.log.Error("credit note", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
		return
	}
	http.Redirect(w, r, "/jobs/"+jobID, http.StatusSeeOther)
}
