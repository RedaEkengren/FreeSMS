package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Enquiries: a customer asking what something would cost, before a car is in
// the shop. The front desk's, and the customer's own page for the answer.

func (s *Server) enquiryError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, access.ErrForbidden):
		s.renderError(w, r, http.StatusForbidden, "Not for your role",
			"Enquiries and proposals are the front desk's.")
	case errors.Is(err, workshop.ErrNotFound):
		s.renderError(w, r, http.StatusNotFound, "Not found", "No such enquiry.")
	case errors.Is(err, workshop.ErrQuoteSuperseded):
		s.renderError(w, r, http.StatusConflict, "That proposal has been replaced",
			"A newer version has been put to the customer. Open the enquiry and answer that one.")
	case errors.Is(err, workshop.ErrQuoteExpired):
		s.renderError(w, r, http.StatusConflict, "That proposal has stopped standing",
			"Put a new version to the customer; nothing is answered on an old price.")
	case errors.Is(err, workshop.ErrInvalid):
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", trimInvalid(err))
	default:
		s.log.Error("enquiry", "error", err, "path", r.URL.Path)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
	}
	return true
}

func (s *Server) handleEnquiries(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	rows, err := workshop.Enquiries(r.Context(), s.pool, session.Scope)
	if s.enquiryError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "enquiries", pageData{Title: "Enquiries", Session: session, Enquiries: rows})
}

func (s *Server) handleCreateEnquiry(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id, err := workshop.CreateEnquiry(r.Context(), s.pool, session.Scope, workshop.NewEnquiry{
		Subject: r.FormValue("subject"), Registration: r.FormValue("registration"), Vehicle: r.FormValue("vehicle"),
		CustomerName: r.FormValue("customer_name"), CustomerPhone: r.FormValue("customer_phone")})
	if s.enquiryError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/enquiries/"+id, http.StatusSeeOther)
}

func (s *Server) renderEnquiry(w http.ResponseWriter, r *http.Request, link string) {
	session := sessionFrom(r.Context())
	e, err := workshop.EnquiryByID(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	if s.enquiryError(w, r, err) {
		return
	}
	rows, err := workshop.PlannerRows(r.Context(), s.pool, session.Scope)
	if err != nil {
		s.log.Error("read planner rows", "error", err)
	}
	s.render(w, r, http.StatusOK, "enquiry", pageData{
		Title: "Enquiry", Session: session, Enquiry: e, ShareURL: link, Planner: plannerPage{Rows: rows},
	})
}

func (s *Server) handleEnquiry(w http.ResponseWriter, r *http.Request) { s.renderEnquiry(w, r, "") }

func (s *Server) handleEnquiryLine(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
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
	err = workshop.AddEnquiryLine(r.Context(), s.pool, session.Scope, id, workshop.NewLine{
		Kind: r.FormValue("kind"), Description: r.FormValue("description"),
		QuantityMilli: quantity, UnitPriceMinor: price, VATRateBasis: 2500})
	if s.enquiryError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/enquiries/"+id, http.StatusSeeOther)
}

func (s *Server) handleEnquiryRemoveLine(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	pos, err := strconv.Atoi(r.PathValue("position"))
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "Not found", "No such line.")
		return
	}
	if s.enquiryError(w, r, workshop.RemoveEnquiryLine(r.Context(), s.pool, session.Scope, id, pos)) {
		return
	}
	http.Redirect(w, r, "/enquiries/"+id, http.StatusSeeOther)
}

func (s *Server) handleEnquiryPresent(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	days := 14
	if v := strings.TrimSpace(r.FormValue("valid_days")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			s.renderError(w, r, http.StatusBadRequest, "Not accepted", "Say for how many days the price stands.")
			return
		}
		days = n
	}
	_, err := workshop.PresentEnquiry(r.Context(), s.pool, session.Scope, id, workshop.Terms{
		ValidDays: days, PriceKind: r.FormValue("price_kind"), Limits: r.FormValue("limits")})
	if s.enquiryError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/enquiries/"+id, http.StatusSeeOther)
}

// declinedPositions reads the lines ticked "not this one".
func declinedPositions(r *http.Request) ([]int, bool) {
	var out []int
	for _, v := range r.Form["declined"] {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

func (s *Server) handleEnquiryAnswer(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	_ = r.ParseForm()
	declined, ok := declinedPositions(r)
	if !ok {
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", "Those lines are not on the proposal.")
		return
	}
	a := workshop.EnquiryAnswerInput{Decision: r.FormValue("decision"), Via: r.FormValue("via"),
		Note: r.FormValue("note"), DeclinedPositions: declined}
	if a.Decision == "approved" {
		amount, err := parseMinorUnits(r.FormValue("agreed"))
		if err != nil || strings.TrimSpace(r.FormValue("agreed")) == "" {
			s.renderError(w, r, http.StatusBadRequest, "That amount did not parse",
				"Write what the customer agreed to pay, including VAT: 1 562,50.")
			return
		}
		a.AgreedGrossMinor = amount
	}
	if s.enquiryError(w, r, workshop.RecordEnquiryAnswer(r.Context(), s.pool, session.Scope, id, r.PathValue("quoteID"), a)) {
		return
	}
	http.Redirect(w, r, "/enquiries/"+id, http.StatusSeeOther)
}

// handleEnquiryLink makes the customer's link and shows it once.
func (s *Server) handleEnquiryLink(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	token, err := workshop.CreateEnquiryLink(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	if s.enquiryError(w, r, err) {
		return
	}
	s.renderEnquiry(w, r, s.baseURL+"/o/"+token)
}

func (s *Server) handleEnquiryBook(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	nb, err := s.bookingForm(r)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Not booked", err.Error())
		return
	}
	id, err := workshop.BookEnquiry(r.Context(), s.pool, session.Scope, r.PathValue("id"), nb)
	if s.enquiryError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/bookings/"+id, http.StatusSeeOther)
}

// handleProposalPrint is a version of the proposal laid out to print and
// hand over. Every figure on it is the frozen version's.
func (s *Server) handleProposalPrint(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	e, err := workshop.EnquiryByID(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	if s.enquiryError(w, r, err) {
		return
	}
	version, err := strconv.Atoi(r.PathValue("version"))
	var q *workshop.EnquiryQuote
	for i := range e.Versions {
		if err == nil && e.Versions[i].Version == version {
			q = &e.Versions[i]
		}
	}
	if q == nil {
		s.renderError(w, r, http.StatusNotFound, "Not found", "No such version.")
		return
	}
	s.render(w, r, http.StatusOK, "proposal", pageData{
		Title: "Proposal", Session: session, Enquiry: e, Version: q,
	})
}

// handleOffer is the customer's page for a proposal. No account; the link is
// the key.
func (s *Server) handleOffer(w http.ResponseWriter, r *http.Request) {
	o, err := workshop.OfferFor(r.Context(), s.pool, s.shop(), r.PathValue("token"))
	s.renderOffer(w, r, o, err, r.URL.Query().Get("changed") != "")
}

func (s *Server) renderOffer(w http.ResponseWriter, r *http.Request, o workshop.Offer, err error, changed bool) {
	switch {
	case errors.Is(err, workshop.ErrShareNotUsable):
		s.render(w, r, http.StatusNotFound, "offer", pageData{Locale: s.shopLocale(r),
			Title: "This link is no longer open", Error: "Ask the workshop for a new one."})
	case err != nil:
		s.log.Error("offer", "error", err)
		s.render(w, r, http.StatusInternalServerError, "offer", pageData{Locale: s.shopLocale(r),
			Title: "Something went wrong", Error: "Try again shortly."})
	default:
		s.render(w, r, http.StatusOK, "offer", pageData{Locale: s.shopLocale(r), Title: "Your proposal",
			Offer: o, QuoteChanged: changed})
	}
}

// handleOfferAnswer is the customer's own answer: to the version they were
// shown, with the lines they kept.
func (s *Server) handleOfferAnswer(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	_ = r.ParseForm()
	// The form lists the lines they want; the rest are the ones turned down.
	var declined []int
	if r.FormValue("decision") == "approved" {
		wanted := map[string]bool{}
		for _, v := range r.Form["keep"] {
			wanted[v] = true
		}
		o, err := workshop.OfferFor(r.Context(), s.pool, s.shop(), token)
		if err != nil {
			s.renderOffer(w, r, o, err, false)
			return
		}
		if o.Quote != nil && o.Quote.ID == r.PathValue("quoteID") {
			for _, l := range o.Quote.Lines {
				if !wanted[strconv.Itoa(l.Position)] {
					declined = append(declined, l.Position)
				}
			}
		}
	}
	err := workshop.OfferAnswer(r.Context(), s.pool, s.shop(), token, r.PathValue("quoteID"), r.FormValue("decision"), declined)
	switch {
	case err == nil:
		http.Redirect(w, r, "/o/"+token, http.StatusSeeOther)
	case errors.Is(err, workshop.ErrQuoteSuperseded), errors.Is(err, workshop.ErrQuoteExpired):
		http.Redirect(w, r, "/o/"+token+"?changed=1", http.StatusSeeOther)
	case errors.Is(err, workshop.ErrInvalid):
		s.render(w, r, http.StatusBadRequest, "offer", pageData{Locale: s.shopLocale(r),
			Title: "That did not go through", Error: "Choose at least one line to say yes to, or say no."})
	default:
		s.renderOffer(w, r, workshop.Offer{}, err, false)
	}
}
