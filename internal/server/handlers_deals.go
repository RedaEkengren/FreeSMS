package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

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
	// A form sent back refused: every field as it was typed, the extras
	// as rows of what was typed in each.
	Typed      bool
	TypedLines []lineRow
	// A revision refused as made over another's: what was typed, shown
	// apart from the form, which holds the terms as they now stand.
	Unsaved      map[string]string
	UnsavedLines []lineRow
	// The sale's delivery, once made (#179).
	Delivery *workshop.Delivery
	// What a delivery refused was missing, as catalogue keys.
	Missing []string
	// The sale's complaints, newest first (#181).
	Complaints []workshop.Complaint
}

// ComplaintChannels, ComplaintStatuses and ComplaintRemedies are what a
// complaint form offers.
func (dealData) ComplaintChannels() []string { return workshop.ComplaintChannels }
func (dealData) ComplaintSteps() []string    { return workshop.ComplaintStatuses[1:] }
func (dealData) ComplaintRemedies() []string { return workshop.ComplaintRemedies }

// DeliveryKm is the odometer read at delivery, 0 when none was.
func (d dealData) DeliveryKm() int64 {
	if d.Delivery == nil || d.Delivery.OdometerKm == nil {
		return 0
	}
	return *d.Delivery.OdometerKm
}

// Now is this moment as a datetime field holds it.
func (dealData) Now() string { return time.Now().Format("2006-01-02T15:04") }

// termsFields are the terms form's fields in its order, with their labels:
// the one list the form's carry-over is read from.
var termsFields = [][2]string{{"buyer_kind", "The buyer is"}, {"price", "Price of the car, VAT included, in kronor"},
	{"tax", "How the sale is taxed"}, {"warranty", "Warranty"}, {"delivery_on", "Delivered on"}, {"delivery_place", "Where"},
	{"odometer", "Odometer, km"}, {"payment_terms", "Payment"}, {"note", "Note"}}

// unsavedField is a field typed and not saved: its label, its value, and
// whether the value is one of the form's choices, shown in words.
type unsavedField struct {
	Label, Value string
	Choice       bool
}

// UnsavedFields are the terms fields typed and not saved, in the form's
// order; empty ones left out. A choice -- the buyer kind, the tax
// treatment -- is its words, as the form shows it.
func (d dealData) UnsavedFields() []unsavedField {
	var out []unsavedField
	for _, f := range termsFields {
		v := d.Unsaved[f[0]]
		if v == "" {
			continue
		}
		u := unsavedField{Label: f[1], Value: v}
		if f[0] == "buyer_kind" || f[0] == "tax" {
			if w := workshop.DealLabel(v); w != "" {
				u.Value, u.Choice = w, true
			}
		}
		out = append(out, u)
	}
	return out
}

// lineRow is an extra as a form shows it.
type lineRow struct{ Desc, Qty, Price, VAT string }

// Field is a terms field as the form shows it: as it was typed when a
// form comes back refused or the deal is new, the deal's own otherwise.
func (d dealData) Field(name string) string {
	if d.Typed || d.Deal.ID == "" {
		return d.Form[name]
	}
	t := d.Deal.Terms
	switch name {
	case "buyer_kind":
		return t.BuyerKind
	case "price":
		return plainAmount(t.PriceMinor)
	case "tax":
		return t.TaxTreatment
	case "warranty":
		return t.Warranty
	case "delivery_on":
		if t.DeliveryOn != nil {
			return t.DeliveryOn.Format("2006-01-02")
		}
	case "delivery_place":
		return t.DeliveryPlace
	case "odometer":
		if t.OdometerKm != nil {
			return strconv.FormatInt(*t.OdometerKm, 10)
		}
	case "payment_terms":
		return t.PaymentTerms
	case "note":
		return t.Note
	}
	return ""
}

// Word is a buyer kind, tax treatment, state or way of answering, as a
// catalogue key.
func (dealData) Word(key string) string { return workshop.DealLabel(key) }

// Kinds and Treatments are the choices a form offers.
func (dealData) Kinds() []string { return []string{workshop.BuyerConsumer, workshop.BuyerBusiness} }
func (dealData) Treatments() []string {
	return []string{workshop.TaxOrdinary, workshop.TaxMarginScheme}
}

// LineRows are the extra rows a terms form offers: what was typed, or the
// deal's extras, and two empty ones.
func (d dealData) LineRows() []lineRow {
	rows := append([]lineRow{}, d.TypedLines...)
	if !d.Typed {
		for _, l := range d.Deal.Terms.Lines {
			rows = append(rows, lineRow{Desc: l.Description, Qty: d.Qty(l.QuantityMilli), Price: plainAmount(l.UnitPriceMinor),
				VAT: strconv.Itoa(l.VATRateBasis / 100)})
		}
	}
	return append(rows, lineRow{VAT: "25"}, lineRow{VAT: "25"})
}

// tradeInRow is a trade-in as the terms form holds it, every field as typed.
type tradeInRow struct {
	Reg, VIN, Make, Model, Km, Credit, Loan, Lender, SellerType string
}

// tradeInFields are a trade-in row's fields, by the name the form sends
// them as (ti_<field>, one per row) and keeps them as when refused
// (ti_<n>_<field>).
var tradeInFields = []string{"reg", "vin", "make", "model", "km", "credit", "loan", "lender", "seller_type"}

func (r *tradeInRow) set(field, v string) {
	switch field {
	case "reg":
		r.Reg = v
	case "vin":
		r.VIN = v
	case "make":
		r.Make = v
	case "model":
		r.Model = v
	case "km":
		r.Km = v
	case "credit":
		r.Credit = v
	case "loan":
		r.Loan = v
	case "lender":
		r.Lender = v
	case "seller_type":
		r.SellerType = v
	}
}

// TradeInRows are the trade-in rows a terms form offers: what was typed,
// or the deal's trade-ins, and one empty row.
func (d dealData) TradeInRows() []tradeInRow {
	var rows []tradeInRow
	if d.Typed {
		for n := 0; ; n++ {
			if _, ok := d.Form[fmt.Sprintf("ti_%d_reg", n)]; !ok {
				break
			}
			var row tradeInRow
			for _, f := range tradeInFields {
				row.set(f, d.Form[fmt.Sprintf("ti_%d_%s", n, f)])
			}
			rows = append(rows, row)
		}
	} else {
		for _, t := range d.Deal.Terms.TradeIns {
			row := tradeInRow{Reg: t.Registration, VIN: t.VIN, Make: t.Make, Model: t.Model, Credit: plainAmount(t.CreditMinor),
				Lender: t.Lender, SellerType: t.SellerType}
			if t.OdometerKm != nil {
				row.Km = strconv.FormatInt(*t.OdometerKm, 10)
			}
			if t.LoanMinor > 0 {
				row.Loan = plainAmount(t.LoanMinor)
			}
			rows = append(rows, row)
		}
	}
	return append(rows, tradeInRow{SellerType: workshop.SellerPrivate})
}

// SellerTypes are the kinds of seller a trade-in row offers.
func (dealData) SellerTypes() []string { return workshop.SellerTypes }

// SellerTypeLabel is a kind of seller in words, a catalogue key.
func (dealData) SellerTypeLabel(t string) string { return workshop.SellerTypeLabel(t) }

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
	m.HandleFunc("POST /cars/deals/{id}/invoice", s.requireSession(s.handleDealInvoice))
	m.HandleFunc("POST /cars/deals/{id}/deliver", s.requireSession(s.handleDealDeliver))
	m.HandleFunc("POST /cars/deals/{id}/complaints", s.requireSession(s.handleOpenComplaint))
	m.HandleFunc("POST /cars/deals/{id}/complaints/{complaint}", s.requireSession(s.handleMoveComplaint))
}

// termsForm reads the terms a form sends; the buyers are the "buyer"
// values, in order.
func (s *Server) termsForm(r *http.Request) (workshop.SaleTerms, map[string]string, []lineRow, string) {
	t, form, msg := s.readTerms(r)
	var rows []lineRow
	descs, qtys, prices, rates := r.Form["line_desc"], r.Form["line_qty"], r.Form["line_price"], r.Form["line_vat"]
	at := func(xs []string, i int) string {
		if i < len(xs) {
			return strings.TrimSpace(xs[i])
		}
		return ""
	}
	for i, d := range descs {
		if strings.TrimSpace(d) != "" {
			rows = append(rows, lineRow{Desc: strings.TrimSpace(d), Qty: at(qtys, i), Price: at(prices, i), VAT: at(rates, i)})
		}
	}
	return t, form, rows, msg
}

// readTerms reads the terms a form sends; the buyers are the "buyer"
// values, in order.
func (s *Server) readTerms(r *http.Request) (workshop.SaleTerms, map[string]string, string) {
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
	// Trade-ins (#165): a row with a plate or a chassis number is one, kept
	// in the form as typed so a refusal gives it back.
	n := 0
	var msg string
	for i := range r.Form["ti_reg"] {
		get := func(field string) string {
			if xs := r.Form["ti_"+field]; i < len(xs) {
				return strings.TrimSpace(xs[i])
			}
			return ""
		}
		if get("reg") == "" && get("vin") == "" {
			continue
		}
		for _, f := range tradeInFields {
			form[fmt.Sprintf("ti_%d_%s", n, f)] = get(f)
		}
		n++
		ti := workshop.TradeIn{Registration: get("reg"), VIN: get("vin"), Make: get("make"), Model: get("model"),
			Lender: get("lender"), SellerType: get("seller_type")}
		credit, err := parseMinorUnits(get("credit"))
		if err != nil || get("credit") == "" {
			msg = "Say what each trade-in is credited at, in kronor."
			continue
		}
		ti.CreditMinor = credit
		if v := get("loan"); v != "" {
			loan, err := parseMinorUnits(v)
			if err != nil {
				msg = "A loan on a trade-in is an amount in kronor."
				continue
			}
			ti.LoanMinor = loan
		}
		if v := get("km"); v != "" {
			km, err := strconv.ParseInt(strings.ReplaceAll(v, " ", ""), 10, 64)
			if err != nil {
				msg = "Say what the odometer shows, in whole kilometres."
				continue
			}
			ti.OdometerKm = &km
		}
		t.TradeIns = append(t.TradeIns, ti)
	}
	return t, form, msg
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
	t, form, typedLines, msg := s.termsForm(r)
	for _, k := range []string{"unit", "lead", "name", "phone", "email"} {
		form[k] = strings.TrimSpace(r.FormValue(k))
	}
	key := r.FormValue("key")
	// Refused, the form comes back as it was typed: the terms read so far
	// fill it as a deal's terms would.
	back := func(status int, msg string) {
		s.dealsPage(w, r, status, dealData{Form: form, Key: key, Typed: true, TypedLines: typedLines}, msg)
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
	s.dealPageWith(w, r, status, msg, dealData{})
}

// dealPageTyped is the deal's page with the terms form as it was typed:
// a revision refused for one of its fields.
func (s *Server) dealPageTyped(w http.ResponseWriter, r *http.Request, status int, msg string, typed *dealData) {
	s.dealPageWith(w, r, status, msg, dealData{Typed: true, TypedLines: typed.TypedLines, Form: typed.Form})
}

// dealPageUnsaved is the deal's page with the terms form as the terms now
// stand, and what was typed shown apart: a revision refused as made over
// another's.
func (s *Server) dealPageUnsaved(w http.ResponseWriter, r *http.Request, status int, msg string, form map[string]string, lines []lineRow) {
	s.dealPageWith(w, r, status, msg, dealData{Unsaved: form, UnsavedLines: lines})
}

// dealPageWith is the deal's page, with what the form comes back with
// already in with.
func (s *Server) dealPageWith(w http.ResponseWriter, r *http.Request, status int, msg string, with dealData) {
	session := sessionFrom(r.Context())
	d := with
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
		// Read only: the shipped contract is made when a contract is first
		// made from it, as reading a page writes nothing.
		if d.Templates, err = workshop.DocumentTemplates(r.Context(), s.pool, session.Scope, workshop.DocSalesContract); s.handoverError(w, r, err) {
			return
		}
	}
	if d.Form == nil {
		d.Form = map[string]string{}
	}
	if del, err := workshop.DeliveryOfSale(r.Context(), s.pool, session.Scope, d.Deal.ID); err == nil {
		d.Delivery = &del
		if d.Complaints, err = workshop.ComplaintsOfSale(r.Context(), s.pool, session.Scope, d.Deal.ID); err != nil {
			s.log.Error("read complaints", "error", err)
		}
	} else if !errors.Is(err, workshop.ErrNotFound) {
		s.log.Error("read the delivery", "error", err)
	}
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
	t, form, typedLines, msg := s.termsForm(r)
	typed := &dealData{Form: form, Typed: true, TypedLines: typedLines}
	if msg != "" {
		s.dealPageTyped(w, r, http.StatusBadRequest, msg, typed)
		return
	}
	version, _ := strconv.Atoi(r.FormValue("version"))
	_, err := workshop.ReviseDeal(r.Context(), s.pool, session.Scope, r.PathValue("id"), version, t)
	if errors.Is(err, workshop.ErrDealMoved) {
		// The form shows the terms as they now stand, never what was typed
		// over them; what was typed is shown apart, to carry over by hand.
		s.dealPageUnsaved(w, r, http.StatusBadRequest, trimInvalid(err), form, typedLines)
		return
	}
	if errors.Is(err, workshop.ErrInvalid) {
		s.dealPageTyped(w, r, http.StatusBadRequest, trimInvalid(err), typed)
		return
	}
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

// handleDealInvoice invoices a completed deal; the deal's page then says
// which invoice it is.
func (s *Server) handleDealInvoice(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	_, err := workshop.IssueDealInvoice(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	s.backToDeal(w, r, err)
}

// handleDealDeliver hands the sold car over (#179).
func (s *Server) handleDealDeliver(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	at, err := time.ParseInLocation("2006-01-02T15:04", strings.TrimSpace(r.FormValue("at")), s.shopLocation(r))
	if err != nil {
		s.dealPage(w, r, http.StatusBadRequest, "Say when it was handed over.")
		return
	}
	n := workshop.NewDelivery{At: at, AtHome: r.FormValue("at_home") != "", Override: r.FormValue("override")}
	if v := strings.ReplaceAll(strings.TrimSpace(r.FormValue("odometer")), " ", ""); v != "" {
		km, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			s.dealPage(w, r, http.StatusBadRequest, "Say what the odometer shows, in whole kilometres.")
			return
		}
		n.OdometerKm = &km
	}
	_, err = workshop.DeliverCar(r.Context(), s.pool, session.Scope, r.PathValue("id"), n)
	var refused *workshop.DeliveryRefused
	if errors.As(err, &refused) {
		s.dealPageWith(w, r, http.StatusBadRequest, "Not ready to deliver yet.", dealData{Missing: refused.Missing})
		return
	}
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

// handleOpenComplaint records a buyer's complaint about the car (#181).
func (s *Server) handleOpenComplaint(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	n := workshop.NewComplaint{ReceivedAt: time.Now(), Channel: r.FormValue("channel"), Description: r.FormValue("description"),
		Key: r.FormValue("key")}
	if v := strings.TrimSpace(r.FormValue("noticed_on")); v != "" {
		day, err := time.ParseInLocation(time.DateOnly, v, s.shopLocation(r))
		if err != nil {
			s.dealPage(w, r, http.StatusBadRequest, "That date did not parse")
			return
		}
		n.NoticedOn = &day
	}
	_, err := workshop.OpenComplaint(r.Context(), s.pool, session.Scope, r.PathValue("id"), n)
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

// handleMoveComplaint records the next step of a complaint.
func (s *Server) handleMoveComplaint(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	remedy := r.FormValue("remedy")
	if !strings.HasPrefix(r.FormValue("status"), "remedy_") {
		remedy = ""
	}
	err := workshop.MoveComplaint(r.Context(), s.pool, session.Scope, r.PathValue("complaint"), r.FormValue("status"), remedy, r.FormValue("note"))
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
