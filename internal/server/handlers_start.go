package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/notify"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Getting ready for the first customer: a checklist read from the workshop's
// own settings, an invoice to look at, a way to try a message, and short help
// for each kind of person who will use it.

func (s *Server) readinessEnv() workshop.ReadinessEnv {
	return workshop.ReadinessEnv{Email: s.email != nil, SMS: s.sms != nil, Lookup: s.canLookUp()}
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	set, err := workshop.ReadinessFor(r.Context(), s.pool, session.Scope, s.readinessEnv())
	if s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "start", pageData{Title: "Getting started", Session: session, Readiness: set,
		Groups: workshop.ReadinessGroups, CanEmail: s.email != nil, CanText: s.sms != nil, TestResult: r.URL.Query().Get("test")})
}

func (s *Server) handleStartStep(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	step := r.PathValue("step")
	err := workshop.ReadinessStatus(r.Context(), s.pool, session.Scope, step, r.FormValue("state"), r.FormValue("note"))
	if errors.Is(err, workshop.ErrInvalid) {
		s.renderError(w, r, http.StatusBadRequest, "Not recorded", trimInvalid(err))
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/start#"+step, http.StatusSeeOther)
}

func (s *Server) handleStartInvoice(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	doc, lacks, err := workshop.InvoiceExample(r.Context(), s.pool, session.Scope)
	if s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "invoiceexample", pageData{Title: "Look at the invoice", Session: session, Document: doc, Lacks: lacks})
}

// handleStartTestMessage sends one message to an address the owner types, to
// see that the provider works. It is not a customer's message: it is recorded
// nowhere, carries no link, and the address is neither stored nor logged.
func (s *Server) handleStartTestMessage(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if !session.Scope.Role.RunsTheShop() {
		s.renderError(w, r, http.StatusForbidden, "Not for your role", "")
		return
	}
	channel := r.FormValue("channel")
	sender := s.email
	if channel == "sms" {
		sender = s.sms
	}
	if sender == nil {
		s.renderError(w, r, http.StatusBadRequest, "Not sent", "There is no provider for that here. Send messages from your own phone or email and write down that you did.")
		return
	}
	var to string
	var err error
	if channel == "sms" {
		to, err = notify.Phone(r.FormValue("address"))
	} else {
		to, err = notify.Email(r.FormValue("address"))
	}
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Not sent", "That cannot be sent to.")
		return
	}
	sig, err := workshop.ShopSignature(r.Context(), s.pool, session.Scope)
	if s.handoverError(w, r, err) {
		return
	}
	p := s.catalogues.For(sig.Locale)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 20*time.Second)
	defer cancel()
	_, err = sender.Send(ctx, notify.Message{To: to, Subject: p.T("A test from %s", sig.Name),
		Text: p.T("%s: this is a test message from FreeSMS. If you can read it, messages to customers will reach them.", sig.Name)})
	result := "sent"
	if err != nil {
		s.log.Warn("test message not sent", "provider", sender.Name(), "error", err)
		result = "failed"
	}
	http.Redirect(w, r, "/start?test="+result, http.StatusSeeOther)
}

func (s *Server) handleHelp(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	role := r.PathValue("role")
	switch role {
	case "reception", "workshop":
	case "owner":
		if !session.Scope.Role.RunsTheShop() {
			s.renderError(w, r, http.StatusForbidden, "Not for your role", "")
			return
		}
	case "":
		// The page for whoever is asking.
		role = map[string]string{"technician": "workshop", "owner": "owner", "admin": "owner"}[string(session.Scope.Role)]
		if role == "" {
			role = "reception"
		}
	default:
		s.renderError(w, r, http.StatusNotFound, "Not found", "There is no such help.")
		return
	}
	s.render(w, r, http.StatusOK, "help", pageData{Title: "Help", Session: session, HelpRole: role})
}
