package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/money"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// lineForm is a correction as it was typed, handed back with what saving it
// would do so that saving is a second, deliberate press.
type lineForm struct {
	JobID                    string
	Position, Revision       int
	Description              string
	Quantity, UnitPrice, VAT string
	CostBearer, PartID       string
	Reason                   string
	Withdraw                 bool
	Change                   *workshop.LineChange
	History                  []workshop.LineRevision
}

func percent(basis int) string {
	s := strconv.FormatFloat(float64(basis)/100, 'f', 2, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return strings.Replace(s, ".", ",", 1)
}

// lineError turns the outcomes only line corrections have into pages.
func (s *Server) lineError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, workshop.ErrLineChanged):
		s.renderError(w, r, http.StatusConflict, "Somebody else got there first",
			"This line was changed while you were looking at it. Open it again and decide from what it says now.")
		return true
	}
	return s.handoverError(w, r, err)
}

// handleLineEditPage shows a line with its history and a form to correct it.
func (s *Server) handleLineEditPage(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if !session.Scope.Role.SeesCustomerPersonalData() {
		s.renderError(w, r, http.StatusForbidden, "Not for your role", "Pricing a job is done at the front desk.")
		return
	}
	id := r.PathValue("id")
	position, err := strconv.Atoi(r.PathValue("position"))
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "Not found", "No such line.")
		return
	}
	job, lines, err := workshop.JobByID(r.Context(), s.pool, session.Scope, id)
	if s.handoverError(w, r, err) {
		return
	}
	for _, l := range lines {
		if l.Position != position {
			continue
		}
		history, err := workshop.LineHistory(r.Context(), s.pool, session.Scope, id, position)
		if s.handoverError(w, r, err) {
			return
		}
		s.render(w, r, http.StatusOK, "lineedit", pageData{
			Title: "Correct a line", Session: session, Job: job,
			LineForm: lineForm{
				JobID: id, Position: position, Revision: l.Revision, Description: l.Description,
				Quantity: l.Quantity(), UnitPrice: money.Format(l.UnitPriceMinor), VAT: percent(l.VATRateBasis),
				CostBearer: l.CostBearer, PartID: l.PartID, History: history,
			},
		})
		return
	}
	s.renderError(w, r, http.StatusNotFound, "Not found", "No such line.")
}

func (s *Server) parseLineForm(w http.ResponseWriter, r *http.Request) (lineForm, workshop.LineEdit, bool) {
	position, err := strconv.Atoi(r.PathValue("position"))
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "Not found", "No such line.")
		return lineForm{}, workshop.LineEdit{}, false
	}
	f := lineForm{
		JobID: r.PathValue("id"), Position: position,
		Description: strings.TrimSpace(r.FormValue("description")), Quantity: strings.TrimSpace(r.FormValue("quantity")),
		UnitPrice: strings.TrimSpace(r.FormValue("unit_price")), VAT: strings.TrimSpace(r.FormValue("vat")),
		CostBearer: r.FormValue("cost_bearer"), PartID: strings.TrimSpace(r.FormValue("part_id")),
		Reason: strings.TrimSpace(r.FormValue("reason")),
	}
	rev, err := parseScaled(r.FormValue("revision"), 0)
	if err != nil || rev < 1 {
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", "That line version is not a number.")
		return f, workshop.LineEdit{}, false
	}
	f.Revision = int(rev)
	return f, workshop.LineEdit{}, true
}

// handleLineEdit previews a correction, and saves it once confirmed.
func (s *Server) handleLineEdit(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	f, edit, ok := s.parseLineForm(w, r)
	if !ok {
		return
	}
	quantity, err := parseScaled(f.Quantity, 3)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That quantity did not parse",
			"Write it as a number, with a comma or a full stop: 1,5 or 1.5.")
		return
	}
	price, err := parseMinorUnits(f.UnitPrice)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That price did not parse", err.Error())
		return
	}
	vat, err := parseScaled(f.VAT, 2)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That tax rate did not parse",
			"Write it as a percentage, with a comma or a full stop: 25 or 12,5.")
		return
	}
	edit = workshop.LineEdit{
		Description: f.Description, QuantityMilli: quantity, UnitPriceMinor: price,
		VATRateBasis: int(vat), CostBearer: f.CostBearer, PartID: f.PartID, Reason: f.Reason,
	}
	if r.FormValue("confirm") == "" {
		c, err := workshop.PreviewLineEdit(r.Context(), s.pool, session.Scope, f.JobID, f.Position, f.Revision, edit)
		if s.lineError(w, r, err) {
			return
		}
		f.Change = &c
		s.render(w, r, http.StatusOK, "lineedit", pageData{Title: "Check the correction", Session: session, LineForm: f})
		return
	}
	_, err = workshop.EditLine(r.Context(), s.pool, session.Scope, f.JobID, f.Position, f.Revision, edit)
	if s.lineError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/jobs/"+f.JobID, http.StatusSeeOther)
}

// handleLineWithdraw previews taking a line back, and does it once confirmed.
func (s *Server) handleLineWithdraw(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	f, _, ok := s.parseLineForm(w, r)
	if !ok {
		return
	}
	f.Withdraw = true
	if r.FormValue("confirm") == "" {
		c, err := workshop.PreviewWithdrawal(r.Context(), s.pool, session.Scope, f.JobID, f.Position, f.Revision, f.Reason)
		if s.lineError(w, r, err) {
			return
		}
		f.Change = &c
		s.render(w, r, http.StatusOK, "lineedit", pageData{Title: "Check the withdrawal", Session: session, LineForm: f})
		return
	}
	_, err := workshop.WithdrawLine(r.Context(), s.pool, session.Scope, f.JobID, f.Position, f.Revision, f.Reason)
	if s.lineError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/jobs/"+f.JobID, http.StatusSeeOther)
}
