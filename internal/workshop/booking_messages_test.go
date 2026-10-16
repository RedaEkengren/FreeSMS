package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Telling a customer about a booking: through the same queue, with no job made
// to hold it, never for a booking that is no longer what the message says, and
// with a way by hand when nothing can be sent.

func soon(hours int) time.Time {
	return time.Now().Add(time.Duration(hours) * time.Hour).Truncate(time.Minute)
}

func aBooking(t *testing.T, pool *pgxpool.Pool, in time.Duration) string {
	t.Helper()
	starts := time.Now().Add(in).Truncate(time.Minute)
	id, err := workshop.CreateBooking(context.Background(), pool, advisor(), workshop.NewBooking{
		Starts: starts, Ends: starts.Add(time.Hour), What: "Service", CustomerName: "Greta Gren", CustomerPhone: "070-123 45 67", Registration: "ABC 12D"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func tell(t *testing.T, pool *pgxpool.Pool, booking, kind string) string {
	t.Helper()
	id, err := workshop.QueueBookingMessage(context.Background(), pool, advisor(), booking, workshop.BookingMessageInput{
		Kind: kind, Channel: "sms", Address: "+46701234567", Recipient: "Greta Gren", Confirmed: true,
		Subject: "Your booking", Body: "Booked.", NotBefore: time.Now()})
	if err != nil {
		t.Fatalf("QueueBookingMessage(%s): %v", kind, err)
	}
	return id
}

func lastEvent(t *testing.T, pool *pgxpool.Pool, msg string) string {
	t.Helper()
	var e string
	if err := pool.QueryRow(context.Background(), `SELECT event FROM message_events WHERE message_id = $1 ORDER BY at DESC, id DESC LIMIT 1`, msg).Scan(&e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestABookingMessageNeedsNoJobAndGoesThroughTheSameQueue(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	b := aBooking(t, pool, 48*time.Hour)
	msg := tell(t, pool, b, "confirm")
	var job *string
	if err := pool.QueryRow(ctx, `SELECT work_order_id::text FROM outbound_messages WHERE id = $1`, msg).Scan(&job); err != nil || job != nil {
		t.Fatalf("a booking message hangs on job %v (%v)", job, err)
	}
	due, _ := workshop.DueMessages(ctx, pool, shopID, time.Now().Add(time.Minute), 10)
	if len(due) != 1 || due[0].BookingID != b || due[0].JobID != "" || due[0].About != "booking_confirm" {
		t.Fatalf("due = %+v", due)
	}
	if err := workshop.MessageSent(ctx, pool, shopID, msg, "test", "p1"); err != nil {
		t.Fatalf("recording a booking message sent: %v", err)
	}
	// Taken by the provider is not delivered, and it is no job's contact.
	if n := countRows(t, pool, `SELECT count(*) FROM customer_contacts`); n != 0 {
		t.Errorf("a booking message wrote %d contacts onto no job", n)
	}
	ms, _ := workshop.BookingMessagesFor(ctx, pool, advisor(), b)
	if len(ms) != 1 || ms[0].State != "sent" {
		t.Errorf("messages = %+v", ms)
	}
	if err := workshop.MessageDelivery(ctx, pool, shopID, "test", "p1", true); err != nil {
		t.Fatal(err)
	}
	if ms, _ = workshop.BookingMessagesFor(ctx, pool, advisor(), b); ms[0].State != "delivered" {
		t.Errorf("after the provider reported it, %q", ms[0].State)
	}

	// The same again for the same time is the counter's decision.
	_, err := workshop.QueueBookingMessage(ctx, pool, advisor(), b, workshop.BookingMessageInput{Kind: "confirm", Channel: "sms",
		Address: "+46701234567", Recipient: "Greta Gren", Confirmed: true, Body: "x"})
	if !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("confirming twice = %v, want ErrInvalid", err)
	}
	if _, err := workshop.QueueBookingMessage(ctx, pool, advisor(), b, workshop.BookingMessageInput{Kind: "confirm", Channel: "sms",
		Address: "+46701234567", Recipient: "Greta Gren", Confirmed: true, Body: "x", Again: true}); err != nil {
		t.Errorf("confirming again on purpose: %v", err)
	}
	for name, in := range map[string]workshop.BookingMessageInput{
		"unconfirmed":                          {Kind: "confirm", Channel: "sms", Address: "+46701234567", Recipient: "G", Body: "x"},
		"no address":                           {Kind: "confirm", Channel: "sms", Recipient: "G", Confirmed: true, Body: "x"},
		"bad kind":                             {Kind: "nag", Channel: "sms", Address: "+46701234567", Recipient: "G", Confirmed: true, Body: "x"},
		"cancelled message for a live booking": {Kind: "cancelled", Channel: "sms", Address: "+46701234567", Recipient: "G", Confirmed: true, Body: "x"},
	} {
		if _, err := workshop.QueueBookingMessage(ctx, pool, advisor(), b, in); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("%s = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := workshop.QueueBookingMessage(ctx, pool, technician(), b, workshop.BookingMessageInput{Kind: "confirm"}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician told a customer: %v", err)
	}
}

func TestMovingABookingJustBeforeDispatchStopsTheStaleMessages(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	b := aBooking(t, pool, 72*time.Hour)
	confirm := tell(t, pool, b, "confirm")
	remind := workshop.BookingMessageInput{Kind: "reminder", Channel: "sms", Address: "+46701234567", Recipient: "Greta Gren", Confirmed: true,
		Body: "Tomorrow.", NotBefore: time.Now().Add(24 * time.Hour)}
	reminder, err := workshop.QueueBookingMessage(ctx, pool, advisor(), b, remind)
	if err != nil {
		t.Fatal(err)
	}
	bk, _, _ := workshop.BookingByID(ctx, pool, advisor(), b)
	// Moved to another day: both stop before they go.
	if err := workshop.MoveBooking(ctx, pool, advisor(), b, bk.Starts.Add(48*time.Hour), bk.Ends.Add(48*time.Hour), ""); err != nil {
		t.Fatal(err)
	}
	if lastEvent(t, pool, confirm) != "cancelled" || lastEvent(t, pool, reminder) != "cancelled" {
		t.Errorf("after a move: confirmation %s, reminder %s", lastEvent(t, pool, confirm), lastEvent(t, pool, reminder))
	}
	if due, _ := workshop.DueMessages(ctx, pool, shopID, time.Now().Add(72*time.Hour), 10); len(due) != 0 {
		t.Errorf("stopped messages are still due: %+v", due)
	}
	// The message about the move is for the new time and goes.
	moved := tell(t, pool, b, "moved")
	if lastEvent(t, pool, moved) != "queued" {
		t.Error("the message about the move was stopped by the move")
	}
	// And the sender's own recheck: a message written for the old time is not
	// wanted now, one for the new time is.
	ok, _, _ := workshop.BookingMessageStillWanted(ctx, pool, shopID, b, "booking_reminder", bk.Starts)
	if ok {
		t.Error("a reminder for the old time is still wanted")
	}
	nb, _, _ := workshop.BookingByID(ctx, pool, advisor(), b)
	if ok, why, _ := workshop.BookingMessageStillWanted(ctx, pool, shopID, b, "booking_moved", nb.Starts); !ok {
		t.Errorf("the message about the new time is not wanted: %s", why)
	}
}

func TestACancelledBookingStopsItsRemindersAndMayBeToldSo(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	b := aBooking(t, pool, 72*time.Hour)
	reminder, _ := workshop.QueueBookingMessage(ctx, pool, advisor(), b, workshop.BookingMessageInput{Kind: "reminder", Channel: "sms",
		Address: "+46701234567", Recipient: "Greta Gren", Confirmed: true, Body: "Tomorrow.", NotBefore: time.Now().Add(24 * time.Hour)})
	if err := workshop.CancelBooking(ctx, pool, advisor(), b, "Customer rang"); err != nil {
		t.Fatal(err)
	}
	if lastEvent(t, pool, reminder) != "cancelled" {
		t.Errorf("the reminder is %s after the booking was cancelled", lastEvent(t, pool, reminder))
	}
	if _, err := workshop.QueueBookingMessage(ctx, pool, advisor(), b, workshop.BookingMessageInput{Kind: "confirm", Channel: "sms",
		Address: "+46701234567", Recipient: "G", Confirmed: true, Body: "x"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("confirming a cancelled booking = %v, want ErrInvalid", err)
	}
	told := tell(t, pool, b, "cancelled")
	if ok, _, _ := workshop.BookingMessageStillWanted(ctx, pool, shopID, b, "booking_cancelled", time.Time{}); !ok {
		t.Error("the cancellation message is not wanted for a cancelled booking")
	}
	if lastEvent(t, pool, told) != "queued" {
		t.Error("the cancellation message was stopped")
	}
}

func TestACarThatHasAlreadyComeIsNotRemindedToCome(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	b := aBooking(t, pool, 3*time.Hour)
	reminder, _ := workshop.QueueBookingMessage(ctx, pool, advisor(), b, workshop.BookingMessageInput{Kind: "reminder", Channel: "sms",
		Address: "+46701234567", Recipient: "Greta Gren", Confirmed: true, Body: "Today.", NotBefore: time.Now().Add(time.Hour)})
	km := int64(100)
	if _, _, err := workshop.TakeInBooking(ctx, pool, advisor(), workshop.BookingIntake{BookingID: b, Registration: "ABC 12D", Complaint: "Service", OdometerKm: &km}); err != nil {
		t.Fatal(err)
	}
	if lastEvent(t, pool, reminder) != "cancelled" {
		t.Errorf("the reminder is %s after the car arrived", lastEvent(t, pool, reminder))
	}
	if _, err := workshop.QueueBookingMessage(ctx, pool, advisor(), b, workshop.BookingMessageInput{Kind: "reminder", Channel: "sms",
		Address: "+46701234567", Recipient: "G", Confirmed: true, Body: "x", NotBefore: time.Now()}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a reminder for a car that came = %v, want ErrInvalid", err)
	}
}

func TestAnOutageIsRetriedAndAGivenUpMessageIsInTheCountersQueueUntilTheyAct(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	b := aBooking(t, pool, 48*time.Hour)
	msg := tell(t, pool, b, "confirm")
	// The provider is down: tried again, then it takes it.
	if err := workshop.MessageNotSent(ctx, pool, shopID, msg, 0, "provider down"); err != nil {
		t.Fatal(err)
	}
	if lastEvent(t, pool, msg) != "retry" {
		t.Fatalf("after an outage: %s", lastEvent(t, pool, msg))
	}
	if list, _ := workshop.BookingsNeedingAttention(ctx, pool, advisor()); len(list) != 0 {
		t.Errorf("a message being retried is listed as failed: %+v", list)
	}
	if err := workshop.MessageSent(ctx, pool, shopID, msg, "test", "p9"); err != nil {
		t.Fatal(err)
	}
	// Another that is given up on.
	b2 := aBooking(t, pool, 50*time.Hour)
	m2 := tell(t, pool, b2, "confirm")
	for tries := 0; tries < 20; tries++ {
		_ = workshop.MessageNotSent(ctx, pool, shopID, m2, tries, "no route")
	}
	list, err := workshop.BookingsNeedingAttention(ctx, pool, advisor())
	if err != nil || len(list) != 1 || list[0].BookingID != b2 {
		t.Fatalf("needing attention = %+v, %v", list, err)
	}
	items, _ := workshop.WaitingFor(ctx, pool, advisor())
	var queued bool
	for _, it := range items {
		if it.Kind == "booking_not_told" && it.BookingID == b2 {
			queued = true
		}
	}
	if !queued {
		t.Error("the counter's queue does not have the booking whose message failed")
	}
	// The queue is the counter's, not the technician's.
	addTechnician(t, pool)
	titems, _ := workshop.WaitingFor(ctx, pool, technician())
	for _, it := range titems {
		if it.Kind == "booking_not_told" {
			t.Error("a technician's queue has a booking's contact in it")
		}
	}
	// Told by phone instead: it leaves the queue.
	if err := workshop.ConfirmBookingByHand(ctx, pool, advisor(), b2, "Rang and she confirmed"); err != nil {
		t.Fatal(err)
	}
	if list, _ = workshop.BookingsNeedingAttention(ctx, pool, advisor()); len(list) != 0 {
		t.Errorf("a booking told by hand is still listed: %+v", list)
	}
	if err := workshop.ConfirmBookingByHand(ctx, pool, advisor(), b2, " "); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a hand confirmation with no words = %v, want ErrInvalid", err)
	}
}

func TestACorrectedContactStopsWhatWasGoingToTheWrongNumber(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	b := aBooking(t, pool, 48*time.Hour)
	msg := tell(t, pool, b, "confirm")
	if err := workshop.CorrectBookingContact(ctx, pool, advisor(), b, "Greta Gren", "070-765 43 21"); err != nil {
		t.Fatal(err)
	}
	if lastEvent(t, pool, msg) != "cancelled" {
		t.Errorf("a message to the old number is %s after the correction", lastEvent(t, pool, msg))
	}
	bk, events, _ := workshop.BookingByID(ctx, pool, advisor(), b)
	if bk.CustomerPhone != "070-765 43 21" || events[len(events)-1].Event != "contact_corrected" {
		t.Errorf("booking = %+v, last event %q", bk, events[len(events)-1].Event)
	}
	if err := workshop.CorrectBookingContact(ctx, pool, advisor(), b, "Greta", "12"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a number that is not one = %v, want ErrInvalid", err)
	}
	if err := workshop.CorrectBookingContact(ctx, pool, technician(), b, "x", "070-111 22 33"); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician corrected a contact: %v", err)
	}
}
