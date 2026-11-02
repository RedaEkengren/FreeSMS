package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Deals' pages (#175): a deal opened from a lead or a car, its terms
// revised as versions, its known defects answered by each buyer, its
// contract made, signed and completed. What a salesperson may see is
// decided by the operations; a deal never carries what the car cost.

// dealData is what the deal pages show.
type dealData struct {
	Deals     []workshop.Deal
	Deal      workshop.Deal
	Cars      []workshop.StockCar
	Lead      workshop.Lead
	Templates []workshop.DocumentTemplate
	Contract  workshop.DocumentRecord
	// The "as is" phrases the contract says to a consumer.
	Warnings []string
	Form     map[string]string
	Key      string
	All      bool
}

// Word is a buyer kind, tax treatment, state or way of answering, as a
// catalogue key.
func (dealData) Word(key string) string { return workshop.DealLabel(key) }

// Kinds and Treatments are the choices a form offers.
func (dealData) Kinds() []string { return []string{workshop.BuyerConsumer, workshop.BuyerBusiness} }
func (dealData) Treatments() []string {
	return []string{workshop.TaxOrdinary, workshop.TaxMarginScheme}
}

// LineRows are the extra rows a terms form offers: the deal's extras and
// two empty ones.
func (d dealData) LineRows() []workshop.SaleLine {
	return append(append([]workshop.SaleLine{}, d.Deal.Terms.Lines...), workshop.SaleLine{VATRateBasis: 2500},
		workshop.SaleLine{VATRateBasis: 2500})
}

// Qty is a line's quantity as typed.
func (dealData) Qty(milli int64) string {
	if milli == 0 {
		return ""
	}
	return strconv.FormatFloat(float64(milli)/1000, 'f', -1, 64)
}

// Percent is basis points as whole per cent.
func (dealData) Percent(bp int) int { return bp / 100 }

// Kronor is minor units as a form field holds them.
func (dealData) Kronor(minor int64) string { return plainAmount(minor) }

// SignatureParty is the document's own party id for a buyer, to sign.
func (d dealData) SignatureParty(partyID string) string {
	for _, p := range d.Contract.Parties {
		if p.PartyID == partyID {
			return p.ID
		}
	}
	return ""
}

// Signed reports whether the buyer has signed the contract.
func (d dealData) Signed(partyID string) bool {
	for _, p := range d.Contract.Parties {
		if p.PartyID == partyID {
			return p.Signature != nil
		}
	}
	return false
}

func (s *Server) dealRoutes(m routeMux) {
	m.HandleFunc("GET /cars/deals", s.requireSession(s.handleDeals))
	m.HandleFunc("POST /cars/deals", s.requireSession(s.handleDealCreate))
	m.HandleFunc("GET /cars/deals/{id}", s.requireSession(s.handleDeal))
	m.HandleFunc("POST /cars/deals/{id}/terms", s.requireSession(s.handleDealRevise))
	m.HandleFunc("POST /cars/deals/{id}/defects", s.requireSession(s.handleDealDefect))
	m.HandleFunc("POST /cars/deals/{id}/defects/{defect}/answer", s.requireSession(s.handleDealDefectAnswer))
	m.HandleFunc("POST /cars/deals/{id}/contract", s.requireSession(s.handleDealContract))
	m.HandleFunc("POST /cars/deals/{id}/signed", s.requireSession(s.handleDealSigned))
	m.HandleFunc("POST /cars/deals/{id}/complete", s.requireSession(s.handleDealComplete))
	m.HandleFunc("POST /cars/deals/{id}/cancel", s.requireSession(s.handleDealCancel))
}

// termsForm reads the terms a form sends; the buyers are the "buyer"
// values, in order.
func (s *Server) termsForm(r *http.Request) (workshop.SaleTerms, map[string]string, string) {
	form := map[string]string{}
	for _, k := range []string{"buyer_kind", "price", "tax", "warranty", "delivery_on", "delivery_place", "odometer",
		"payment_terms", "note"} {
		form[k] = strings.TrimSpace(r.FormValue(k))
	}
	t := workshop.SaleTerms{Buyers: r.Form["buyer"], BuyerKind: form["buyer_kind"], TaxTreatment: form["tax"],
		Warranty: form["warranty"], DeliveryPlace: form["delivery_place"], PaymentTerms: form["payment_terms"], Note: form["note"]}
	price, err := parseMinorUnits(form["price"])
	if err != nil || form["price"] == "" {
		return t, form, "Say the car's price in kronor."
	}
	t.PriceMinor = price
	if on, ok := s.formDate(r, "delivery_on"); !ok {
		return t, form, "That date did not parse"
	} else {
		t.DeliveryOn = on
	}
	if form["odometer"] != "" {
		km, err := strconv.ParseInt(form["odometer"], 10, 64)
		if err != nil {
			return t, form, "Say what the odometer shows, in whole kilometres."
		}
		t.OdometerKm = &km
	}
	descs, qtys, prices, rates := r.Form["line_desc"], r.Form["line_qty"], r.Form["line_price"], r.Form["line_vat"]
	for i, d := range descs {
		if strings.TrimSpace(d) == "" {
			continue
		}
		get := func(xs []string) string {
			if i < len(xs) {
				return strings.TrimSpace(xs[i])
			}
			return ""
		}
		qty, err1 := parseScaled(firstNonEmpty(get(qtys), "1"), 3)
		unit, err2 := parseMinorUnits(get(prices))
		rate, err3 := strconv.Atoi(firstNonEmpty(get(rates), "25"))
		if err1 != nil || err2 != nil || err3 != nil {
			return t, form, "Each extra needs a quantity, a price in kronor and a VAT rate in per cent."
		}
		t.Lines = append(t.Lines, workshop.SaleLine{Description: d, QuantityMilli: qty, UnitPriceMinor: unit, VATRateBasis: rate * 100})
	}
	return t, form, ""
}

func (s *Server) dealsPage(w http.ResponseWriter, r *http.Request, status int, d dealData, msg string) {
	session := sessionFrom(r.Context())
	var err error
	if d.Deals, err = workshop.Deals(r.Context(), s.pool, session.Scope, d.All); s.handoverError(w, r, err) {
		return
	}
	if d.Cars, err = workshop.CarsInStock(r.Context(), s.pool, session.Scope); s.handoverError(w, r, err) {
		return
	}
	if lead := d.Form["lead"]; lead != "" {
		if d.Lead, err = workshop.LeadByID(r.Context(), s.pool, session.Scope, lead); s.handoverError(w, r, err) {
			return
		}
	}
	if d.Key == "" {
		d.Key = newFormKey()
	}
	s.render(w, r, status, "deal", pageData{Title: "Deals", Session: session, Deals: d, Error: msg})
}

// handleDeals lists the deals, with the form for a new one: from a lead
// or a car when the link says which.
func (s *Server) handleDeals(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.dealsPage(w, r, http.StatusOK, dealData{All: q.Get("all") == "1",
		Form: map[string]string{"lead": q.Get("lead"), "unit": q.Get("unit"), "buyer_kind": workshop.BuyerConsumer}}, "")
}

// handleDealCreate opens a deal. The buyer is the lead's party, or one
// made with the deal from the name and number given.
func (s *Server) handleDealCreate(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", "")
		return
	}
	t, form, msg := s.termsForm(r)
	for _, k := range []string{"unit", "lead", "name", "phone", "email"} {
		form[k] = strings.TrimSpace(r.FormValue(k))
	}
	key := r.FormValue("key")
	// Refused, the form comes back as it was typed: the terms read so far
	// fill it as a deal's terms would.
	back := func(status int, msg string) {
		s.dealsPage(w, r, status, dealData{Form: form, Key: key, Deal: workshop.Deal{Terms: t}}, msg)
	}
	if msg != "" {
		back(http.StatusBadRequest, msg)
		return
	}
	n := workshop.NewDeal{UnitID: form["unit"], LeadID: form["lead"], Terms: t, Key: key}
	if len(t.Buyers) == 0 {
		if form["name"] == "" {
			back(http.StatusBadRequest, "Say who buys: their name, and a phone number or email.")
			return
		}
		n.NewBuyer = &workshop.NewParty{Name: form["name"], Phone: form["phone"], Email: form["email"]}
	}
	d, err := workshop.CreateDeal(r.Context(), s.pool, session.Scope, n)
	if errors.Is(err, workshop.ErrInvalid) {
		back(http.StatusBadRequest, trimInvalid(err))
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/cars/deals/"+d.ID, http.StatusSeeOther)
}

func (s *Server) dealPage(w http.ResponseWriter, r *http.Request, status int, msg string) {
	session := sessionFrom(r.Context())
	var d dealData
	var err error
	if d.Deal, err = workshop.DealByID(r.Context(), s.pool, session.Scope, r.PathValue("id")); s.handoverError(w, r, err) {
		return
	}
	if d.Deal.ContractID != "" {
		if d.Contract, err = workshop.DocumentRecordByID(r.Context(), s.pool, session.Scope, d.Deal.ContractID); s.handoverError(w, r, err) {
			return
		}
		d.Warnings = workshop.SalesContractWarnings(d.Contract.Body, d.Deal.Terms.BuyerKind)
	}
	if d.Deal.State != workshop.DealCancelled {
		if d.Templates, err = workshop.SalesContractTemplates(r.Context(), s.pool, session.Scope); s.handoverError(w, r, err) {
			return
		}
	}
	d.Form = map[string]string{}
	s.render(w, r, status, "deal", pageData{Title: "Deal", Session: session, Deals: d, Error: msg})
}

// handleDeal is one deal: its terms, defects, contract and what is left
// before it is complete.
func (s *Server) handleDeal(w http.ResponseWriter, r *http.Request) {
	s.dealPage(w, r, http.StatusOK, "")
}

func (s *Server) backToDeal(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, workshop.ErrInvalid) {
		s.dealPage(w, r, http.StatusBadRequest, trimInvalid(err))
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/cars/deals/"+r.PathValue("id"), http.StatusSeeOther)
}

func (s *Server) handleDealRevise(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if err := r.ParseForm(); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", "")
		return
	}
	t, _, msg := s.termsForm(r)
	if msg != "" {
		s.dealPage(w, r, http.StatusBadRequest, msg)
		return
	}
	version, _ := strconv.Atoi(r.FormValue("version"))
	_, err := workshop.ReviseDeal(r.Context(), s.pool, session.Scope, r.PathValue("id"), version, t)
	s.backToDeal(w, r, err)
}

func (s *Server) handleDealDefect(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	_, err := workshop.AddDefect(r.Context(), s.pool, session.Scope, r.PathValue("id"), r.FormValue("description"), "")
	s.backToDeal(w, r, err)
}

// handleDealDefectAnswer records a buyer's answer to a defect given here,
// in person.
func (s *Server) handleDealDefectAnswer(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	err := workshop.AnswerDefect(r.Context(), s.pool, session.Scope, r.PathValue("defect"), r.FormValue("party"),
		r.FormValue("accepted") == "1", "in_person", "")
	s.backToDeal(w, r, err)
}

func (s *Server) handleDealContract(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	_, _, err := workshop.ProduceSalesContract(r.Context(), s.pool, session.Scope, r.PathValue("id"), r.FormValue("template"),
		r.FormValue("replacing"))
	s.backToDeal(w, r, err)
}

// handleDealSigned records that a buyer signed the contract, on paper or
// here in person: no signing service is needed.
func (s *Server) handleDealSigned(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	d, err := workshop.DealByID(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	if s.handoverError(w, r, err) {
		return
	}
	method := workshop.SignedInPerson
	if r.FormValue("method") == workshop.SignedOnPaper {
		method = workshop.SignedOnPaper
	}
	s.backToDeal(w, r, workshop.RecordSignature(r.Context(), s.pool, session.Scope, d.ContractID, r.FormValue("party"),
		workshop.SignatureEvidence{Method: method, Hash: r.FormValue("hash"), WitnessedBy: session.Scope.UserID}))
}

func (s *Server) handleDealComplete(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	_, err := workshop.CompleteDeal(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	s.backToDeal(w, r, err)
}

func (s *Server) handleDealCancel(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	s.backToDeal(w, r, workshop.CancelDeal(r.Context(), s.pool, session.Scope, r.PathValue("id"), r.FormValue("reason")))
}
