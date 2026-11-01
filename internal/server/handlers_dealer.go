package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Car sales' pages (M7). Each reads through the operation that checks the
// role and the module; the page decides nothing about who may see what.

// dealerData is what a car-sales page shows beyond the common page: one
// field on pageData, so the pages of each module keep to their own.
type dealerData struct {
	Calculation workshop.Calculation
	Result      workshop.CalculationResult
	Vehicle     workshop.Vehicle
	// What was typed, given back when it was refused.
	Form map[string]string
}

// SellerTypes are the kinds of seller a form offers.
func (dealerData) SellerTypes() []string { return workshop.SellerTypes }

// SellerTypeLabel is a kind of seller in words, a catalogue key.
func (dealerData) SellerTypeLabel(t string) string { return workshop.SellerTypeLabel(t) }

// CostKinds are the kinds of cost a form offers.
func (dealerData) CostKinds() []string { return workshop.CostKinds }

// CostKindLabel is a kind of cost in words, a catalogue key.
func (dealerData) CostKindLabel(k string) string { return workshop.CostKindLabel(k) }

// Field is what the form holds for name: what was typed, or else what the
// calculation says.
func (d dealerData) Field(name string) string {
	if v, ok := d.Form[name]; ok {
		return v
	}
	in := d.Calculation.Input
	switch name {
	case "sale":
		if in.ExpectedSaleGrossMinor != nil {
			return plainAmount(*in.ExpectedSaleGrossMinor)
		}
	case "days":
		if in.ExpectedStockDays != 0 {
			return strconv.Itoa(in.ExpectedStockDays)
		}
	case "per_day":
		if in.HoldingPerDayMinor != 0 {
			return plainAmount(in.HoldingPerDayMinor)
		}
	case "margin":
		if in.TargetMarginMinor != nil {
			return plainAmount(*in.TargetMarginMinor)
		}
		if in.TargetMarginBasis != nil {
			// Basis points are a percentage with two decimals.
			return plainAmount(int64(*in.TargetMarginBasis)) + " %"
		}
	case "seller_type":
		return in.SellerType
	case "note":
		return in.Note
	default:
		if kind, ok := strings.CutPrefix(name, "cost_"); ok {
			var sum int64
			found := false
			for _, c := range in.Costs {
				if c.Kind == kind {
					sum, found = sum+c.NetMinor, true
				}
			}
			if found {
				return plainAmount(sum)
			}
		}
	}
	return ""
}

// plainAmount is an amount as a form field holds it: digits, and a decimal
// comma when there are öre.
func plainAmount(minor int64) string {
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	s := strconv.FormatInt(minor/100, 10)
	if ore := minor % 100; ore != 0 {
		s += "," + strconv.FormatInt(100+ore, 10)[1:]
	}
	return sign + s
}

// calculationForm reads a calculation as typed: amounts in kronor, the
// margin as an amount or, ending in %, a share of the sale price.
func calculationForm(r *http.Request) (workshop.CalculationInput, map[string]string, error) {
	form := map[string]string{}
	for _, k := range []string{"sale", "days", "per_day", "margin", "seller_type", "note", "registration", "vin"} {
		form[k] = r.FormValue(k)
	}
	// A sentence the page shows, translated (Sentence); named bad so the
	// catalogue test finds it.
	bad := func(msg string) error { return errors.New(msg) }
	in := workshop.CalculationInput{SellerType: r.FormValue("seller_type"), Note: r.FormValue("note")}
	if v := strings.TrimSpace(form["sale"]); v != "" {
		n, err := parseMinorUnits(v)
		if err != nil {
			return in, form, bad("The sale price expected is not a number.")
		}
		in.ExpectedSaleGrossMinor = &n
	}
	for _, k := range workshop.CostKinds {
		v := r.FormValue("cost_" + k)
		form["cost_"+k] = v
		if strings.TrimSpace(v) == "" {
			continue
		}
		n, err := parseMinorUnits(v)
		if err != nil {
			return in, form, bad("A cost is not a number.")
		}
		in.Costs = append(in.Costs, workshop.CalculationCost{Kind: k, NetMinor: n})
	}
	if v := strings.TrimSpace(form["days"]); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return in, form, bad("The days in stock are a whole number.")
		}
		in.ExpectedStockDays = n
	}
	if v := strings.TrimSpace(form["per_day"]); v != "" {
		n, err := parseMinorUnits(v)
		if err != nil {
			return in, form, bad("The cost of a day in stock is not a number.")
		}
		in.HoldingPerDayMinor = n
	}
	if v := strings.TrimSpace(form["margin"]); v != "" {
		if share, ok := strings.CutSuffix(v, "%"); ok {
			n, err := parseMinorSigned(share)
			if err != nil {
				return in, form, bad("The margin wanted is not a number.")
			}
			basis := int(n)
			in.TargetMarginBasis = &basis
		} else {
			n, err := parseMinorSigned(v)
			if err != nil {
				return in, form, bad("The margin wanted is not a number.")
			}
			in.TargetMarginMinor = &n
		}
	}
	return in, form, nil
}

// handleCalculationForm is a new calculation: the car, by its plate or its
// chassis number, and what the buyer expects.
func (s *Server) handleCalculationForm(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if !session.Scope.Role.SeesPurchasePrice() {
		s.renderError(w, r, http.StatusForbidden, "Not for your role", "What a car may be bought for is the buyer's.")
		return
	}
	s.render(w, r, http.StatusOK, "carcalc", pageData{Title: "Before the bid", Session: session,
		Dealer: &dealerData{Form: map[string]string{"registration": r.URL.Query().Get("registration")}}})
}

// handleSaveCalculation saves a calculation: a new one for the car typed,
// or the next version of the one it was edited from.
func (s *Server) handleSaveCalculation(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	in, form, err := calculationForm(r)
	data := &dealerData{Form: form}
	calculation := r.FormValue("calculation")
	// Refused, an edit comes back as the version it was made from, so it
	// stays an edit of that calculation, with what was typed in the form.
	refuse := func(msg string) {
		if calculation != "" && data.Calculation.ID == "" {
			data.Calculation, _ = workshop.CalculationByID(r.Context(), s.pool, session.Scope, r.FormValue("from"))
			data.Result = data.Calculation.Result()
		}
		s.render(w, r, http.StatusBadRequest, "carcalc", pageData{Title: "Before the bid", Session: session, Dealer: data, Error: msg})
	}
	if err != nil {
		refuse(err.Error())
		return
	}
	version, _ := strconv.Atoi(r.FormValue("version"))
	vehicleID := r.FormValue("vehicle")
	if calculation == "" {
		vehicleID, err = workshop.CarForPurchase(r.Context(), s.pool, session.Scope, workshop.CarToBuy{
			Registration: form["registration"], VIN: form["vin"]})
		if errors.Is(err, workshop.ErrInvalid) {
			refuse(trimInvalid(err))
			return
		}
		if s.handoverError(w, r, err) {
			return
		}
	}
	saved, err := workshop.SaveCalculation(r.Context(), s.pool, session.Scope, vehicleID, calculation, version, in)
	if errors.Is(err, workshop.ErrInvalid) {
		refuse(trimInvalid(err))
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/cars/calculations/"+saved.ID, http.StatusSeeOther)
}

// handleCalculation is one version of a calculation, what it comes to, and
// the form to edit it into the next.
func (s *Server) handleCalculation(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	c, err := workshop.CalculationByID(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	if s.handoverError(w, r, err) {
		return
	}
	v, err := workshop.VehicleByID(r.Context(), s.pool, session.Scope, c.VehicleID)
	if err != nil {
		s.log.Error("read vehicle", "error", err)
	}
	s.render(w, r, http.StatusOK, "carcalc", pageData{Title: "Before the bid", Session: session,
		Dealer: &dealerData{Calculation: c, Result: c.Result(), Vehicle: v}})
}

// handleAcceptCalculation records the bid made on a version.
func (s *Server) handleAcceptCalculation(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	bid, err := parseMinorUnits(r.FormValue("bid"))
	if err != nil || strings.TrimSpace(r.FormValue("bid")) == "" {
		s.renderError(w, r, http.StatusBadRequest, "Not recorded", "Say what was bid, as a number.")
		return
	}
	id := r.PathValue("id")
	_, err = workshop.AcceptCalculation(r.Context(), s.pool, session.Scope, id, bid)
	if errors.Is(err, workshop.ErrInvalid) {
		s.renderError(w, r, http.StatusBadRequest, "Not recorded", trimInvalid(err))
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/cars/calculations/"+id, http.StatusSeeOther)
}
