package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/notify"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Telling a customer about a booking: previewed, confirmed, queued through the
// same sender as everything else.

// BookingPreview is a message composed and shown before it is queued.
type BookingPreview struct {
	Kind, Channel, Address, Recipient, Arrival, Subject, Body string
	// A reminder goes this long before the booking.
	NotBefore time.Time
}

// composeBooking writes the message in the shop's language with the booking's
// time as the shop's clock shows it, the workshop's number and the way to
// reach a person.
func (s *Server) composeBooking(sig workshop.Signature, b workshop.Booking, kind, arrival string) (subject, body string) {
	p := s.catalogues.For(sig.Locale)
	when := b.Starts.In(s.zone()).Format("2006-01-02 15:04")
	switch kind {
	case "reminder":
		subject = p.T("Reminder: your booking at %s", sig.Name)
		body = p.T("%s: a reminder of your booking on %s (%s).", sig.Name, when, b.What)
	case "moved":
		subject = p.T("Your booking at %s has moved", sig.Name)
		body = p.T("%s: your booking has moved to %s (%s).", sig.Name, when, b.What)
	case "cancelled":
		subject = p.T("Your booking at %s is cancelled", sig.Name)
		body = p.T("%s: your booking on %s is cancelled.", sig.Name, when)
	default:
		subject = p.T("Your booking at %s", sig.Name)
		body = p.T("%s: your booking is confirmed for %s (%s).", sig.Name, when, b.What)
	}
	if arrival = strings.TrimSpace(arrival); arrival != "" {
		body += " " + arrival
	}
	if sig.Phone != "" {
		body += "\n" + p.T("Replies to this are not read. Ring us on %s.", sig.Phone)
	} else {
		body += "\n" + p.T("Replies to this are not read.")
	}
	return subject, body
}

func (s *Server) handleBookingMessage(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	refuse := func(msg string) { s.renderError(w, r, http.StatusBadRequest, "Not sent", msg) }

	kind, channel := r.FormValue("kind"), r.FormValue("channel")
	if (channel == "sms" && s.sms == nil) || (channel == "email" && s.email == nil) {
		refuse("There is no provider for that here; ring or give it on paper, and write it down.")
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
	b, events, err := workshop.BookingByID(r.Context(), s.pool, session.Scope, id)
	if s.handoverError(w, r, err) {
		return
	}
	sig, err := workshop.ShopSignature(r.Context(), s.pool, session.Scope)
	if s.handoverError(w, r, err) {
		return
	}
	subject, body := s.composeBooking(sig, b, kind, r.FormValue("arrival"))

	// A reminder goes the day before, inside the hours the shop sends in, and
	// before the booking itself.
	not := s.sendHours.NotBefore(time.Now(), s.zone())
	if kind == "reminder" {
		// The day before at the same hour on the wall, which across the night
		// the clocks change is not twenty-four hours.
		st := b.Starts.In(s.zone())
		day := time.Date(st.Year(), st.Month(), st.Day()-1, st.Hour(), st.Minute(), 0, 0, s.zone())
		if day.Before(time.Now()) {
			day = time.Now()
		}
		not = s.sendHours.NotBefore(day, s.zone())
		if !not.Before(b.Starts) {
			refuse("There is no time left to send a reminder inside the hours messages go.")
			return
		}
	}
	if r.FormValue("send") != "1" {
		msgs, _ := workshop.BookingMessagesFor(r.Context(), s.pool, session.Scope, id)
		rows, _ := workshop.PlannerRows(r.Context(), s.pool, session.Scope)
		s.render(w, r, http.StatusOK, "booking", pageData{
			Title: "Booking", Session: session, Booking: b, BookingEvents: events, BookingMessages: msgs,
			Planner: plannerPage{Rows: rows},
			BookingPreview: &BookingPreview{Kind: kind, Channel: channel, Address: address, Recipient: r.FormValue("recipient"),
				Arrival: r.FormValue("arrival"), Subject: subject, Body: body, NotBefore: not},
			CanText: s.sms != nil, CanEmail: s.email != nil,
		})
		return
	}
	_, err = workshop.QueueBookingMessage(r.Context(), s.pool, session.Scope, id, workshop.BookingMessageInput{
		Kind: kind, Channel: channel, Address: address, Recipient: r.FormValue("recipient"),
		Confirmed: r.FormValue("confirm") == "yes", Subject: subject, Body: body, NotBefore: not, Again: r.FormValue("again") == "yes",
	})
	if errors.Is(err, workshop.ErrInvalid) {
		refuse(trimInvalid(err))
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	go s.sendDue(context.WithoutCancel(r.Context()), time.Now())
	http.Redirect(w, r, "/bookings/"+id, http.StatusSeeOther)
}

func (s *Server) handleBookingContact(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	var err error
	if r.FormValue("by_hand") != "" {
		err = workshop.ConfirmBookingByHand(r.Context(), s.pool, session.Scope, id, r.FormValue("how"))
	} else {
		err = workshop.CorrectBookingContact(r.Context(), s.pool, session.Scope, id, r.FormValue("name"), r.FormValue("phone"))
	}
	if errors.Is(err, workshop.ErrInvalid) {
		s.renderError(w, r, http.StatusBadRequest, "Not saved", trimInvalid(err))
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/bookings/"+id, http.StatusSeeOther)
}
