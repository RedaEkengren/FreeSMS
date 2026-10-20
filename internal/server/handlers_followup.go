package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/notify"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Following up what is owed and paying back what is owed back.

func (s *Server) handleFollowUpPage(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	f, err := workshop.FollowUpFor(r.Context(), s.pool, session.Scope, id)
	if s.handoverError(w, r, err) {
		return
	}
	people, err := workshop.DeskPeople(r.Context(), s.pool, session.Scope)
	// A role that may not see the desk's people is not an error: the list is left out.
	if err != nil && !errors.Is(err, access.ErrForbidden) {
		s.log.Error("read people", "error", err)
	}
	doc, err := workshop.DocumentByID(r.Context(), s.pool, session.Scope, f.InvoiceID)
	if s.handoverError(w, r, err) {
		return
	}
	cust, _ := workshop.ContactFor(r.Context(), s.pool, session.Scope, doc.WorkOrderID)
	preview, err := workshop.PrepareReminder(r.Context(), s.pool, session.Scope, f.InvoiceID, s.today(r))
	if err != nil {
		s.log.Error("prepare reminder", "error", err)
	}
	s.render(w, r, http.StatusOK, "followup", pageData{
		Title: "Follow up", Session: session, FollowUp: f, DeskPeople: people, Contact: cust, Document: doc,
		Reminder: preview, Methods: workshop.PaymentMethods, CanText: s.sms != nil, CanEmail: s.email != nil,
		Today: s.today(r).Format("2006-01-02"),
	})
}

func (s *Server) day(r *http.Request, name string) (*time.Time, bool) {
	return s.dayField(r, name)
}

func (s *Server) handleFollowUp(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	bad := func(msg string) { s.renderError(w, r, http.StatusBadRequest, "Not recorded", msg) }
	var err error
	switch r.FormValue("action") {
	case "take":
		next, ok := s.day(r, "next")
		if !ok {
			bad("That is not a date.")
			return
		}
		in := workshop.FollowUpSet{Next: next, ClearNext: next == nil && r.FormValue("clear_next") != "", Note: r.FormValue("note")}
		switch o := r.FormValue("owner"); o {
		case "":
		case "none":
			in.ClearOwner = true
		case "me":
			in.OwnerID = session.Scope.UserID
		default:
			in.OwnerID = o
		}
		err = workshop.TakeFollowUp(r.Context(), s.pool, session.Scope, id, in)
	case "contact":
		next, ok := s.day(r, "next")
		if !ok {
			bad("That is not a date.")
			return
		}
		err = workshop.LogContact(r.Context(), s.pool, session.Scope, id, r.FormValue("how"), r.FormValue("note"), next)
	case "promise":
		by, ok := s.day(r, "by")
		amount, aerr := parseMinorUnits(strings.TrimSpace(r.FormValue("amount")))
		if !ok || by == nil || aerr != nil {
			bad("Say how much, and by which day.")
			return
		}
		err = workshop.RecordPromise(r.Context(), s.pool, session.Scope, id, amount, *by, r.FormValue("note"))
	case "dispute":
		err = workshop.Dispute(r.Context(), s.pool, session.Scope, id, r.FormValue("note"))
	case "resolve":
		err = workshop.ResolveDispute(r.Context(), s.pool, session.Scope, id, r.FormValue("note"))
	case "refund":
		paid, ok := s.day(r, "paid_on")
		if !ok || paid == nil {
			bad("Say which day it was paid out.")
			return
		}
		var amount int64
		if v := strings.TrimSpace(r.FormValue("amount")); v != "" {
			if amount, err = parseMinorUnits(v); err != nil {
				bad("Write the amount as 150 or 150,50.")
				return
			}
		}
		_, err = workshop.RecordRefund(r.Context(), s.pool, session.Scope, id, workshop.RefundInput{
			AmountMinor: amount, Method: r.FormValue("method"), PaidOn: *paid, Reference: r.FormValue("reference"),
		})
	default:
		bad("Say what to do.")
		return
	}
	if errors.Is(err, workshop.ErrInvalid) || errors.Is(err, workshop.ErrNothingOwed) {
		bad(trimInvalid(err))
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/invoices/"+id+"/followup", http.StatusSeeOther)
}

func (s *Server) handleReminder(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	channel := r.FormValue("channel")
	refuse := func(msg string) { s.renderError(w, r, http.StatusBadRequest, "Not sent", msg) }
	if (channel == "sms" && s.sms == nil) || (channel == "email" && s.email == nil) {
		refuse("There is no provider for that here; ring or write yourself and record it.")
		return
	}
	address := r.FormValue("address")
	var err error
	switch channel {
	case "sms":
		address, err = notify.Phone(address)
	case "email":
		address, err = notify.Email(address)
	default:
		refuse("A text or an email.")
		return
	}
	if err != nil {
		refuse("That cannot be sent to.")
		return
	}
	doc, err := workshop.DocumentByID(r.Context(), s.pool, session.Scope, id)
	if s.handoverError(w, r, err) {
		return
	}
	sig, err := workshop.ShopSignature(r.Context(), s.pool, session.Scope)
	if s.handoverError(w, r, err) {
		return
	}
	p := s.catalogues.For(sig.Locale)
	subject := p.T("Reminder: invoice %s from %s", doc.Reference(), sig.Name)
	body := p.T("%s: a reminder about invoice %s. Still to pay: {owed}. {link}", sig.Name, doc.Reference())
	if sig.Phone != "" {
		body += "\n" + p.T("Questions? Ring %s.", sig.Phone)
	}
	_, err = workshop.QueueReminder(r.Context(), s.pool, session.Scope, id, workshop.ReminderInput{
		Channel: channel, Address: address, RecipientName: r.FormValue("recipient"), Confirmed: r.FormValue("confirm") == "yes",
		Subject: subject, Body: body, NotBefore: s.sendHours.NotBefore(time.Now(), s.zone()), LinkBase: s.baseURL,
	})
	if errors.Is(err, workshop.ErrInvalid) || errors.Is(err, workshop.ErrNothingOwed) {
		refuse(trimInvalid(err))
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/invoices/"+id+"/followup", http.StatusSeeOther)
}
