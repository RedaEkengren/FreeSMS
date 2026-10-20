package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Work sent to a supplier.

func (s *Server) handleOutsourcedPage(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	job, lines, err := workshop.JobByID(r.Context(), s.pool, session.Scope, id)
	if s.handoverError(w, r, err) {
		return
	}
	items, err := workshop.OutsourcedFor(r.Context(), s.pool, session.Scope, id)
	if s.handoverError(w, r, err) {
		return
	}
	data := pageData{Title: "Work at suppliers", Session: session, Job: job, Lines: lines, Outsourced: items,
		Today: s.today(r).Format("2006-01-02"), Outcomes: outcomeOptions()}
	if session.Scope.Role.SeesParts() {
		people, err := workshop.DeskPeople(r.Context(), s.pool, session.Scope)
		// A role that may not see the desk's people is not an error: the list is left out.
		if err != nil && !errors.Is(err, access.ErrForbidden) {
			s.log.Error("read people", "error", err)
		}
		data.DeskPeople = people
	}
	s.render(w, r, http.StatusOK, "outsourced", data)
}

func outcomeOptions() []workshop.Option {
	var out []workshop.Option
	for _, o := range workshop.OutsourcedOutcomes {
		out = append(out, workshop.Option{Value: o, Label: workshop.OutsourcedOutcomeLabel(o)})
	}
	return out
}

func (s *Server) handleOutsourcedList(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	items, err := workshop.OutsourcedOpen(r.Context(), s.pool, session.Scope)
	if s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "outsourcedlist", pageData{Title: "Work at suppliers", Session: session, Outsourced: items})
}

func (s *Server) handleOutsourced(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	bad := func(msg string) { s.renderError(w, r, http.StatusBadRequest, "Not recorded", msg) }
	money := func(name string) (*int64, bool) {
		v := strings.TrimSpace(r.FormValue(name))
		if v == "" {
			return nil, true
		}
		a, err := parseMinorUnits(v)
		if err != nil {
			return nil, false
		}
		return &a, true
	}
	var err error
	switch r.FormValue("action") {
	case "request":
		back, ok := s.dayField(r, "expected_back")
		estimate, ok2 := money("estimate")
		if !ok || !ok2 {
			bad("Write the date as a date and the estimate as 150 or 150,50.")
			return
		}
		_, err = workshop.RequestOutsourced(r.Context(), s.pool, session.Scope, workshop.OutsourcedInput{
			JobID: id, LineID: r.FormValue("line"), Supplier: r.FormValue("supplier"), Contact: r.FormValue("contact"),
			Scope: r.FormValue("scope"), ExternalRef: r.FormValue("external_ref"), Item: r.FormValue("item"),
			ItemKind: r.FormValue("item_kind"), ExpectedBack: back, EstimateMinor: estimate, Optional: r.FormValue("optional") != "",
		})
	case "record":
		back, ok := s.dayField(r, "expected_back")
		amount, ok2 := money("amount")
		if !ok || !ok2 {
			bad("Write the date as a date and the amount as 150 or 150,50.")
			return
		}
		err = workshop.RecordOutsourced(r.Context(), s.pool, session.Scope, r.FormValue("item"), workshop.OutsourcedEventInput{
			Kind: r.FormValue("kind"), Note: r.FormValue("note"), Outcome: r.FormValue("outcome"), ExpectedBack: back,
			AmountMinor: amount, InvoiceRef: r.FormValue("invoice_ref"), Assignee: r.FormValue("assignee"),
		})
	default:
		bad("Say what to do.")
		return
	}
	if errors.Is(err, workshop.ErrInvalid) {
		bad(trimInvalid(err))
		return
	}
	if errors.Is(err, workshop.ErrFinished) {
		bad("That job is finished; nothing more can be sent out on it.")
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/jobs/"+id+"/outsourced", http.StatusSeeOther)
}
