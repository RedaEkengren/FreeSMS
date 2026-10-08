package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/notify"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// sendEvery is how often the sender looks for messages whose time has come.
const sendEvery = 20 * time.Second

// sendLoop sends queued messages until ctx ends. One process per workshop,
// so one sender; it is the only thing that hands a message to a provider.
func (s *Server) sendLoop(ctx context.Context) {
	tick := time.NewTicker(sendEvery)
	defer tick.Stop()
	for {
		s.sendDue(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// sendDue hands every message whose time has come to its provider.
//
// At least once, not exactly once: a process that dies after the provider
// took a message and before the row saying so is written sends it again on
// restart. The window is one request long; the other way round -- writing
// "sent" first -- would lose messages instead, which is worse.
func (s *Server) sendDue(ctx context.Context, now time.Time) {
	// One at a time. The tick and a send straight after queueing could
	// otherwise both pick the same message and hand it over twice.
	s.sendingMu.Lock()
	defer s.sendingMu.Unlock()
	shop := s.shop()
	if shop == "" {
		return
	}
	due, err := workshop.DueMessages(ctx, s.pool, shop, now, 20)
	if err != nil {
		s.log.Error("due messages", "error", err)
		return
	}
	for _, m := range due {
		sender := s.email
		if m.Channel == "sms" {
			sender = s.sms
		}
		if sender == nil {
			_ = workshop.MessageNotSent(ctx, s.pool, shop, m.ID, 1<<10, "no provider is configured for this")
			continue
		}
		sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		id, err := sender.Send(sendCtx, notify.Message{To: m.Recipient, Subject: m.Subject, Text: m.Body})
		cancel()
		if err != nil {
			// The provider's words, which name no key; logged without the
			// recipient, who is a customer.
			s.log.Warn("message not sent", "message", m.ID, "provider", sender.Name(), "error", err)
			if rerr := workshop.MessageNotSent(ctx, s.pool, shop, m.ID, m.Tries, err.Error()); rerr != nil {
				s.log.Error("record message not sent", "error", rerr)
			}
			continue
		}
		if err := workshop.MessageSent(ctx, s.pool, shop, m.ID, sender.Name(), id); err != nil {
			s.log.Error("record message sent", "message", m.ID, "error", err)
		}
	}
}

// handleSendMessage queues a message to the job's customer: the link to the
// job, or to the inspection they are asked to answer, in the shop's
// language, signed with the number to ring.
func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	jobID := r.PathValue("id")
	about, channel := r.FormValue("about"), r.FormValue("channel")
	refuse := func(msg string) { s.renderError(w, r, http.StatusBadRequest, "Not sent", msg) }

	if (channel == "sms" && s.sms == nil) || (channel == "email" && s.email == nil) {
		refuse("There is no provider for that here; send it from your own phone and record it.")
		return
	}
	contact, err := workshop.ContactFor(r.Context(), s.pool, session.Scope, jobID)
	if s.handoverError(w, r, err) {
		return
	}
	var to string
	if channel == "sms" {
		if to, err = notify.Phone(contact.Phone); err != nil {
			refuse("The customer's number cannot be read as a phone number. Correct it on the customer first.")
			return
		}
	} else {
		if to, err = notify.Email(contact.Email); err != nil {
			refuse("The customer has no email address that can be sent to.")
			return
		}
	}

	// The first message to an address is confirmed: one digit out is
	// somebody else, and the link shows what is owed.
	sent, err := workshop.SentBefore(r.Context(), s.pool, session.Scope, jobID, to)
	if s.handoverError(w, r, err) {
		return
	}
	if !sent && r.FormValue("confirm_"+channel) != "yes" {
		refuse(fmt.Sprintf("Tick that %s is the customer's before the first message to it.", to))
		return
	}
	// Once per thing, unless somebody means it.
	if at, err := workshop.AlreadySent(r.Context(), s.pool, session.Scope, jobID, about); s.handoverError(w, r, err) {
		return
	} else if at != nil && r.FormValue("again") != "yes" {
		refuse("That was already sent to this customer; tick \"send it again\" if it should go twice.")
		return
	}

	link, err := s.customerLink(r, jobID, about)
	if errors.Is(err, workshop.ErrNotFound) {
		refuse("There is no finished inspection on this job for the customer to answer.")
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	sig, err := workshop.ShopSignature(r.Context(), s.pool, session.Scope)
	if s.handoverError(w, r, err) {
		return
	}
	job, _, err := workshop.JobByID(r.Context(), s.pool, session.Scope, jobID)
	if s.handoverError(w, r, err) {
		return
	}
	subject, body := s.compose(sig, job, about, link)
	_, err = workshop.QueueMessage(r.Context(), s.pool, session.Scope, workshop.NewMessage{
		JobID: jobID, About: about, Channel: channel, Recipient: to, Subject: subject, Body: body,
		NotBefore: s.sendHours.NotBefore(time.Now(), s.zone()),
	})
	if errors.Is(err, workshop.ErrInvalid) {
		refuse(trimInvalid(err))
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	// Sent now, if it may be, rather than at the next tick.
	go s.sendDue(context.WithoutCancel(r.Context()), time.Now())
	http.Redirect(w, r, "/jobs/"+jobID, http.StatusSeeOther)
}

// customerLink makes the link the message carries: the job's, or the
// inspection's the customer is asked to answer. Making one closes the one
// before, as it always has (#92).
func (s *Server) customerLink(r *http.Request, jobID, about string) (string, error) {
	session := sessionFrom(r.Context())
	if about != "answer" {
		token, err := workshop.CreateJobLink(r.Context(), s.pool, session.Scope, jobID)
		return s.baseURL + "/k/" + token, err
	}
	inspections, err := workshop.InspectionsFor(r.Context(), s.pool, session.Scope, jobID)
	if err != nil {
		return "", err
	}
	for _, in := range inspections {
		if in.Completed() && len(in.Findings()) > 0 {
			token, err := workshop.CreateShare(r.Context(), s.pool, session.Scope, in.ID)
			return s.baseURL + "/i/" + token, err
		}
	}
	return "", workshop.ErrNotFound
}

// compose writes the message in the shop's language: one thing, the link,
// and how to reach a person -- replies to it are not read.
func (s *Server) compose(sig workshop.Signature, job workshop.Job, about, link string) (subject, body string) {
	p := s.catalogues.For(sig.Locale)
	car := job.Registration
	if car == "" {
		car = fmt.Sprintf("#%d", job.Number)
	}
	switch about {
	case "answer":
		body = p.T("%s: we have looked at your car %s and need your answer. %s", sig.Name, car, link)
	case "waiting_part":
		body = p.T("%s: your car %s is waiting for a part. Follow it here: %s", sig.Name, car, link)
	default:
		body = p.T("%s: your car %s is ready to collect. %s", sig.Name, car, link)
	}
	if sig.Phone != "" {
		body += "\n" + p.T("Replies to this are not read. Ring us on %s.", sig.Phone)
	} else {
		body += "\n" + p.T("Replies to this are not read.")
	}
	return p.T("Your car %s", car), body
}

// handleElksDelivered is 46elks reporting what became of a text. The secret
// in the address is the only thing that says it is 46elks, so it is compared
// in constant time; nothing else about the request is trusted.
func (s *Server) handleElksDelivered(w http.ResponseWriter, r *http.Request) {
	secret := r.PathValue("secret")
	if s.smsSecret == "" || subtle.ConstantTimeCompare([]byte(secret), []byte(s.smsSecret)) != 1 {
		http.NotFound(w, r)
		return
	}
	id, status := r.FormValue("id"), strings.ToLower(r.FormValue("status"))
	if id == "" || (status != "delivered" && status != "failed") {
		// "sent" and anything else: nothing new to record.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	err := workshop.MessageDelivery(r.Context(), s.pool, s.shop(), "46elks", id, status == "delivered")
	if err != nil && !errors.Is(err, workshop.ErrNotFound) {
		s.log.Error("record delivery", "error", err)
		http.Error(w, "try again", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
