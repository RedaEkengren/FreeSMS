package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/notify"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Getting an invoice or a credit note to the customer: a link by message, or
// by hand. The rules are in the workshop package.

func (s *Server) handleSendDocument(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	channel := r.FormValue("channel")
	refuse := func(msg string) { s.renderError(w, r, http.StatusBadRequest, "Not sent", msg) }

	doc, err := workshop.DocumentByID(r.Context(), s.pool, session.Scope, id)
	if s.handoverError(w, r, err) {
		return
	}
	if (channel == "sms" && s.sms == nil) || (channel == "email" && s.email == nil) {
		refuse("There is no provider for that here; print it or hand it over, and record it.")
		return
	}
	address := r.FormValue("address")
	switch channel {
	case "sms":
		if address, err = notify.Phone(address); err != nil {
			refuse("That cannot be read as a phone number.")
			return
		}
	case "email":
		if address, err = notify.Email(address); err != nil {
			refuse("That cannot be sent to as an email address.")
			return
		}
	default:
		refuse("A text or an email.")
		return
	}
	sig, err := workshop.ShopSignature(r.Context(), s.pool, session.Scope)
	if s.handoverError(w, r, err) {
		return
	}
	p := s.catalogues.For(sig.Locale)
	var subject, body string
	if doc.IsCreditNote() {
		subject = p.T("Credit note %s from %s", doc.Reference(), sig.Name)
		body = p.T("%s: your credit note %s. {link}", sig.Name, doc.Reference())
	} else {
		subject = p.T("Invoice %s from %s", doc.Reference(), sig.Name)
		body = p.T("%s: your invoice %s. {link}", sig.Name, doc.Reference())
	}
	if sig.Phone != "" {
		body += "\n" + p.T("Questions? Ring %s.", sig.Phone)
	}
	_, err = workshop.SendDocument(r.Context(), s.pool, session.Scope, id, workshop.SendInput{
		Channel: channel, Address: address, RecipientName: r.FormValue("recipient"),
		Confirmed: r.FormValue("confirm") == "yes", Subject: subject, Body: body,
		NotBefore: s.sendHours.NotBefore(time.Now(), s.zone()), LinkBase: s.baseURL,
	})
	if errors.Is(err, workshop.ErrInvalid) {
		refuse(trimInvalid(err))
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	go s.sendDue(context.WithoutCancel(r.Context()), time.Now())
	http.Redirect(w, r, "/invoices/"+id, http.StatusSeeOther)
}

func (s *Server) handleHandDocumentOver(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	err := workshop.HandDocumentOver(r.Context(), s.pool, session.Scope, id, workshop.HandInput{
		RecipientName: r.FormValue("recipient"), How: r.FormValue("how"), Note: r.FormValue("note"),
	})
	if errors.Is(err, workshop.ErrInvalid) {
		s.renderError(w, r, http.StatusBadRequest, "Not recorded", trimInvalid(err))
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/invoices/"+id, http.StatusSeeOther)
}

func (s *Server) handleRevokeDocumentLinks(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	if err := workshop.RevokeDocumentLinks(r.Context(), s.pool, session.Scope, id); s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/invoices/"+id, http.StatusSeeOther)
}

// handleCustomerDocument is the customer's copy of one document, on its link.
// It opens that document and nothing around it.
func (s *Server) handleCustomerDocument(w http.ResponseWriter, r *http.Request) {
	doc, err := workshop.OpenDocumentLink(r.Context(), s.pool, s.shop(), r.PathValue("token"))
	switch {
	case err == nil:
	case errors.Is(err, workshop.ErrShareNotUsable), errors.Is(err, workshop.ErrNotFound):
		s.render(w, r, http.StatusNotFound, "status", pageData{
			Locale: s.shopLocale(r), Title: "This link is no longer open", Error: "Ask the workshop for a new one.",
		})
		return
	default:
		s.log.Error("customer document", "error", err)
		s.render(w, r, http.StatusInternalServerError, "status", pageData{
			Locale: s.shopLocale(r), Title: "Something went wrong", Error: "Try again shortly.",
		})
		return
	}
	s.render(w, r, http.StatusOK, "docview", pageData{
		Locale: s.shopLocale(r), Title: doc.Document.Reference(), Document: doc.Document, Balance: doc.Balance,
		SummaryShop: doc.ShopName,
	})
}
