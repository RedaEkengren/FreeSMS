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
	s.renderShop(w, r, http.StatusOK, nil, "")
}

// renderShop is the shop's page, with what is still open in a module when
// switching it off was refused for that.
// renderShop shows the shop page; open and reason are a switch-off refused
// over open business, shown with the list and the reason the person typed,
// so ticking "I have seen what is open" does not lose why.
func (s *Server) renderShop(w http.ResponseWriter, r *http.Request, status int, open *workshop.OpenBusiness, reason string) {
	session := sessionFrom(r.Context())

	details, err := workshop.ShopDetailsFor(r.Context(), s.pool, session.Scope)
	if errors.Is(err, access.ErrForbidden) {
		s.renderError(w, r, http.StatusForbidden, "Not for your role",
			"The workshop's own details belong to whoever runs it.")
		return
	}
	if err != nil {
		s.fault(w, r, "read shop details", err)
		return
	}

	// The workshop's own settings, when the shop runs it.
	var charges workshop.Surcharges
	var final workshop.FinalCheckSettings
	var checklists []workshop.Template
	if session.Scope.Uses(access.Workshop) {
		if charges, err = workshop.SurchargesFor(r.Context(), s.pool, session.Scope); err != nil {
			s.log.Error("read surcharges", "error", err)
		}
		if final, err = workshop.FinalCheckSettingsFor(r.Context(), s.pool, session.Scope); err != nil {
			s.log.Error("read final check setting", "error", err)
		}
		if checklists, err = workshop.Templates(r.Context(), s.pool, session.Scope); err != nil {
			s.log.Error("read checklists", "error", err)
		}
	}
	modules, err := workshop.ModulesFor(r.Context(), s.pool, session.Scope)
	if err != nil {
		s.log.Error("read modules", "error", err)
	}
	changes, err := workshop.ModuleChanges(r.Context(), s.pool, session.Scope)
	if err != nil {
		s.log.Error("read module changes", "error", err)
	}
	retention := s.retentionFor(r, session.Scope)
	s.render(w, r, status, "shop", pageData{
		Title:         "The workshop",
		Retention:     retention,
		Session:       session,
		Shop:          details,
		Charges:       charges,
		Languages:     workshop.Languages(),
		FinalSetting:  final,
		Templates:     checklists,
		Modules:       modules,
		ModuleChanges: changes,
		ModuleOpen:    open,
		ModuleReason:  reason,
	})
}

// handleSwitchModule switches one of the shop's modules on or off (#143).
// Switching off with work still open shows the shop page again with that
// work listed, and does nothing until the person ticks that they mean it.
func (s *Server) handleSwitchModule(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	module := access.Module(r.FormValue("module"))
	changed, err := workshop.SwitchModule(r.Context(), s.pool, session.Scope, module,
		r.FormValue("on") == "yes", r.FormValue("reason"), r.FormValue("confirm") == "yes")
	if open, ok := workshop.AsOpenBusiness(err); ok {
		s.renderShop(w, r, http.StatusConflict, open, r.FormValue("reason"))
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	if changed {
		s.modules.forget()
		s.log.Info("module switched", "module", module, "on", r.FormValue("on") == "yes", "by", session.Scope.UserID)
	}
	http.Redirect(w, r, "/shop", http.StatusSeeOther)
}

// handleSaveFinalCheck sets whether ready needs a final check.
func (s *Server) handleSaveFinalCheck(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	err := workshop.SaveFinalCheck(r.Context(), s.pool, session.Scope, workshop.FinalCheckSettings{
		TemplateID: r.FormValue("template_id"),
		ByAnother:  r.FormValue("by_another") == "yes",
	})
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/shop", http.StatusSeeOther)
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
		EndpointID:       r.FormValue("endpoint_id"),
		PaymentScheme:    r.FormValue("payment_scheme"),
		SwishNumber:      r.FormValue("swish_number"),
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
		s.fault(w, r, "save shop details", err)
		return
	}

	s.submitted(w, r)
	http.Redirect(w, r, "/shop", http.StatusSeeOther)
}

// retentionData is the shop page's retention section (#150, #153): how long
// each kind of document and each purpose of an identity value is kept.
type retentionData struct {
	Documents []retentionRow
	Identity  []retentionRow
}

// retentionRow is one setting: Name is what the form posts, Label a
// catalogue key, Value the years or days now in force.
type retentionRow struct {
	Name, Label string
	Value       int
}

// retentionFor reads both settings; a failure is logged and the section is
// left out rather than the whole page.
func (s *Server) retentionFor(r *http.Request, scope access.Scope) *retentionData {
	docs, err := workshop.DocumentRetention(r.Context(), s.pool, scope)
	if err != nil {
		s.log.Error("read document retention", "error", err)
		return nil
	}
	ids, err := workshop.IdentityRetention(r.Context(), s.pool, scope)
	if err != nil {
		s.log.Error("read identity retention", "error", err)
		return nil
	}
	d := &retentionData{}
	for _, k := range workshop.DocumentKinds {
		d.Documents = append(d.Documents, retentionRow{k, workshop.DocumentKindLabel(k), docs[k]})
	}
	for _, p := range workshop.IdentityPurposes {
		if scope.Require(p.Module) == nil {
			d.Identity = append(d.Identity, retentionRow{p.Name, identityData{}.IdentityPurposeName(p.Name), ids[p.Name]})
		}
	}
	return d
}

// handleSaveRetention sets one retention: a document kind in years or an
// identity purpose in days. A row is added; the earlier setting stays.
func (s *Server) handleSaveRetention(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	n, convErr := strconv.Atoi(strings.TrimSpace(r.FormValue("value")))
	var err error
	switch {
	case convErr != nil:
		err = workshop.ErrInvalid
	case r.FormValue("what") == "identity":
		err = workshop.SetIdentityRetention(r.Context(), s.pool, session.Scope, r.FormValue("name"), n)
	default:
		err = workshop.SetDocumentRetention(r.Context(), s.pool, session.Scope, r.FormValue("name"), n)
	}
	if errors.Is(err, workshop.ErrInvalid) {
		s.renderError(w, r, http.StatusBadRequest, "Not a valid retention", "Give a whole number within the range shown.")
		return
	}
	if errors.Is(err, access.ErrForbidden) {
		s.renderError(w, r, http.StatusForbidden, "Not for your role", "How long things are kept belongs to whoever runs the workshop.")
		return
	}
	if err != nil {
		s.fault(w, r, "save retention", err)
		return
	}
	http.Redirect(w, r, "/shop#retention", http.StatusSeeOther)
}
