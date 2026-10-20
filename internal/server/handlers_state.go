package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

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

	var err error
	if to == workshop.StateClosed {
		// Closing by hand asks why when money is still owed.
		err = workshop.CloseJob(r.Context(), s.pool, session.Scope, id, r.FormValue("reason"))
	} else if to == workshop.StateDeclined {
		// A customer saying no is billed for what was already done, so the
		// lines ticked as performed travel with the decline.
		var performed []int
		for _, v := range r.Form["performed"] {
			n, convErr := strconv.Atoi(v)
			if convErr != nil {
				s.renderError(w, r, http.StatusBadRequest, "Not accepted", "That is not a line.")
				return
			}
			performed = append(performed, n)
		}
		err = workshop.Decline(r.Context(), s.pool, session.Scope, id, performed)
	} else {
		err = workshop.SetState(r.Context(), s.pool, session.Scope, id, to)
	}

	var illegal workshop.ErrIllegalTransition
	switch {
	// Not a 400: the request was well formed and the move is a legal one. It
	// is the route that is wrong, and the message says which one is right.
	case errors.Is(err, workshop.ErrNeedsDocument):
		s.renderError(w, r, http.StatusConflict, "Not invoiced",
			"An order becomes invoiced by issuing an invoice. Use the invoice button.")
		return
	case errors.Is(err, workshop.ErrNotFound):
		s.renderError(w, r, http.StatusNotFound, "Not found", "No such job.")
		return
	case errors.Is(err, workshop.ErrNoCustomer):
		s.renderError(w, r, http.StatusConflict, "Nobody to bill", err.Error())
		return
	case errors.Is(err, workshop.ErrNeedsApproval):
		s.renderError(w, r, http.StatusConflict, "No agreement recorded", err.Error())
		return
	case errors.Is(err, workshop.ErrWouldLoseWork):
		// Worth a page rather than a flash: it is a refusal with a reason and
		// a different action to take, not a slip of the finger.
		s.renderError(w, r, http.StatusConflict, "That would write off work", err.Error())
		return
	case errors.As(err, &illegal):
		s.renderError(w, r, http.StatusConflict, "Not from here", illegal.Error())
		return
	case errors.Is(err, workshop.ErrInvalid):
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", trimInvalid(err))
		return
	case errors.Is(err, access.ErrForbidden):
		s.renderError(w, r, http.StatusForbidden, "Not for your role",
			"That move belongs to the front desk.")
		return
	case err != nil:
		s.fault(w, r, "set state", err)
		return
	}

	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}

// handleMarkPerformed says that the work on a line is done, or that it is not.
func (s *Server) handleMarkPerformed(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	position, convErr := strconv.Atoi(r.PathValue("position"))
	if convErr != nil {
		http.NotFound(w, r)
		return
	}
	err := workshop.MarkPerformed(r.Context(), s.pool, session.Scope, id, position, r.FormValue("performed") != "no")
	switch {
	case errors.Is(err, access.ErrForbidden):
		s.renderError(w, r, http.StatusForbidden, "Not for your role", "")
		return
	case errors.Is(err, workshop.ErrNotFound):
		s.renderError(w, r, http.StatusNotFound, "Not found", "No such line on this job.")
		return
	case errors.Is(err, workshop.ErrInvalid):
		s.renderError(w, r, http.StatusConflict, "Not changed", trimInvalid(err))
		return
	case err != nil:
		s.fault(w, r, "mark performed", err)
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
	// A part from the shelf is priced from the part, by the server, unless a
	// price was typed. An empty field used to be read as zero.
	fromPart := r.FormValue("kind") == "part" && strings.TrimSpace(r.FormValue("part_id")) != "" &&
		strings.TrimSpace(r.FormValue("unit_price")) == ""
	var price int64
	if !fromPart {
		if price, err = parseMinorUnits(r.FormValue("unit_price")); err != nil {
			s.renderError(w, r, http.StatusBadRequest, "That price did not parse", err.Error())
			return
		}
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
		PriceFromPart:  fromPart,
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
		s.fault(w, r, "add line", err)
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}

// handlePriceAnswer makes an item the customer approved into a line. The
// description is the item's, so what the invoice says is what the customer
// said yes to; the front desk types what it costs.
func (s *Server) handlePriceAnswer(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id, itemID := r.PathValue("id"), r.PathValue("itemID")

	quantity, err := parseScaled(r.FormValue("quantity"), 3)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That quantity did not parse",
			"Write it as a number, with a comma or a full stop: 1,5 or 1.5.")
		return
	}
	price, err := parseMinorUnits(r.FormValue("unit_price"))
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That price did not parse", err.Error())
		return
	}
	err = workshop.PriceAnswer(r.Context(), s.pool, session.Scope, id, itemID, workshop.NewLine{
		Kind:           r.FormValue("kind"),
		Description:    r.FormValue("description"),
		QuantityMilli:  quantity,
		UnitPriceMinor: price,
		VATRateBasis:   2500,
	})
	switch {
	case errors.Is(err, access.ErrForbidden):
		s.renderError(w, r, http.StatusForbidden, "Not for your role",
			"Pricing a job is done at the front desk.")
		return
	case errors.Is(err, workshop.ErrNotFound):
		s.renderError(w, r, http.StatusNotFound, "Not found", "No such answer on this job.")
		return
	case errors.Is(err, workshop.ErrAlreadyALine):
		// Somebody else priced it a moment ago. That is the outcome wanted,
		// so the job is shown with their line on it.
		http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
		return
	case errors.Is(err, workshop.ErrInvalid):
		s.renderError(w, r, http.StatusBadRequest, "That line was not accepted", trimInvalid(err))
		return
	case err != nil:
		s.fault(w, r, "price answer", err)
		return
	}
	s.submitted(w, r)
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
	// Twelve digits is a trillion: nothing a form asks for is larger, and
	// multiplied by the scale below it stays far from wrapping an int64.
	if major > 999_999_999_999 {
		return 0, errors.New("that is larger than anything can be here")
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
		s.fault(w, r, "issue invoice", err)
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
		s.fault(w, r, "credit note", err)
		return
	}
	http.Redirect(w, r, "/jobs/"+jobID, http.StatusSeeOther)
}

// handleInvoice renders an issued document.
//
// There is no PDF, and there is deliberately no stored file: the rows are
// frozen, so rendering them is deterministic and a file would be a cache of
// something that cannot change. The browser's own print is how this becomes
// paper, which is why the stylesheet has a print block.
func (s *Server) handleInvoiceDocument(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())

	doc, err := workshop.DocumentByID(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	switch {
	case errors.Is(err, access.ErrForbidden):
		s.renderError(w, r, http.StatusForbidden, "Not for your role",
			"An invoice is the front desk's business.")
		return
	case errors.Is(err, workshop.ErrNotFound):
		s.renderError(w, r, http.StatusNotFound, "Not found", "No such invoice.")
		return
	case err != nil:
		s.fault(w, r, "read invoice", err)
		return
	}

	balance, err := workshop.BalanceFor(r.Context(), s.pool, session.Scope, doc.ID)
	if err != nil {
		s.log.Error("read balance", "error", err)
	}
	deliveries, err := workshop.DeliveriesFor(r.Context(), s.pool, session.Scope, doc.ID)
	if err != nil {
		s.log.Error("read deliveries", "error", err)
	}
	contact, _ := workshop.ContactFor(r.Context(), s.pool, session.Scope, doc.WorkOrderID)
	corrections, err := workshop.CorrectionsFor(r.Context(), s.pool, session.Scope, doc.ID)
	if err != nil {
		s.log.Error("read corrections", "error", err)
	}
	s.render(w, r, http.StatusOK, "invoice", pageData{
		Corrections: corrections,
		Title:       doc.Reference(),
		Session:     session,
		Deliveries:  deliveries,
		Contact:     contact,
		CanText:     s.sms != nil,
		CanEmail:    s.email != nil,
		Document:    doc,
		Balance:     balance,
		Methods:     workshop.PaymentMethods,
		Today:       time.Now().In(s.shopLocation(r)).Format("2006-01-02"),
	})
}

// handleJobParts answers the search beside the shelf on a job page.
//
// A separate route rather than a reload of the whole job, because the job page
// is long and re-rendering it to filter one list is the kind of thing that
// makes a page feel slow -- which is the single most common complaint about
// every system this one is meant to replace.
func (s *Server) handleJobParts(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")

	parts, err := workshop.PartsForJob(r.Context(), s.pool, session.Scope, id, r.URL.Query().Get("q"))
	switch {
	case errors.Is(err, access.ErrForbidden):
		s.renderError(w, r, http.StatusForbidden, "Not for your role", "")
		return
	case errors.Is(err, workshop.ErrNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		s.log.Error("search parts", "error", err)
		http.Error(w, "could not search", http.StatusInternalServerError)
		return
	}

	s.renderPartial(w, r, "job", "shelf", pageData{
		Session:   session,
		Job:       workshop.Job{ID: id},
		Parts:     parts,
		PartQuery: r.URL.Query().Get("q"),
	})
}

// handlePresentQuote puts the lines the customer has not answered to them as
// the next numbered version.
func (s *Server) handlePresentQuote(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	_, err := workshop.PresentQuote(r.Context(), s.pool, session.Scope, id)
	if s.quoteError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}

// handleQuoteAnswer writes down a yes or no the customer gave by telephone or
// at the counter, with the amount they agreed to.
func (s *Server) handleQuoteAnswer(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	answer := workshop.QuoteAnswer{
		Decision: r.FormValue("decision"),
		Via:      r.FormValue("via"),
		Note:     r.FormValue("note"),
	}
	if answer.Decision == "approved" {
		amount, err := parseMinorUnits(r.FormValue("agreed"))
		if err != nil || strings.TrimSpace(r.FormValue("agreed")) == "" {
			s.renderError(w, r, http.StatusBadRequest, "That amount did not parse",
				"Write what the customer agreed to pay, including VAT: 1 562,50.")
			return
		}
		answer.AgreedGrossMinor = amount
	}
	err := workshop.RecordQuoteAnswer(r.Context(), s.pool, session.Scope, id, r.PathValue("quoteID"), answer)
	if s.quoteError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}

// quoteError answers for the errors the quote operations share, and reports
// whether it did.
func (s *Server) quoteError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, access.ErrForbidden):
		s.renderError(w, r, http.StatusForbidden, "Not for your role",
			"Quotes and the customer's answers are the front desk's.")
	case errors.Is(err, workshop.ErrNotFound):
		s.renderError(w, r, http.StatusNotFound, "Not found", "No such job.")
	case errors.Is(err, workshop.ErrQuoteSuperseded):
		s.renderError(w, r, http.StatusConflict, "That quote has been replaced",
			"A newer version of the quote has been put to the customer. Open the job and answer that one.")
	case errors.Is(err, workshop.ErrInvalid):
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", trimInvalid(err))
	default:
		s.fault(w, r, "quote", err)
	}
	return true
}
