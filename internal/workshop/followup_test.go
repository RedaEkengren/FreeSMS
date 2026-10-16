package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Chasing what is owed and paying back what is owed back: who has it, what was
// promised, a hold that changes no balance, a reminder that is asked after
// again as it goes, and a refund that is not the reversal of a mistake.

func itemOf(t *testing.T, _ any, items []workshop.WorkItem, id string) workshop.WorkItem {
	t.Helper()
	for _, w := range items {
		if w.InvoiceID == id {
			return w
		}
	}
	t.Fatalf("invoice %s is not on the worklist", id)
	return workshop.WorkItem{}
}

func TestSomebodyHasAnInvoiceAndItsHistoryIsKept(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, inv := issued(t, pool)
	tomorrow := time.Now().AddDate(0, 0, 1)
	if err := workshop.TakeFollowUp(ctx, pool, advisor(), inv.ID, workshop.FollowUpSet{OwnerID: userID, Next: &tomorrow, Note: "I will ring on Monday"}); err != nil {
		t.Fatal(err)
	}
	yesterday := time.Now().AddDate(0, 0, -1)
	if err := workshop.LogContact(ctx, pool, advisor(), inv.ID, "phoned", "Voicemail, left a message", &yesterday); err != nil {
		t.Fatal(err)
	}
	f, err := workshop.FollowUpFor(ctx, pool, advisor(), inv.ID)
	if err != nil || f.OwnerID != userID || f.Owner == "" || len(f.Events) != 2 || f.Events[0].Kind != "contact" || f.Events[0].By == "" {
		t.Fatalf("follow-up = %+v, %v", f, err)
	}
	items, _ := workshop.Worklist(ctx, pool, advisor(), time.Now())
	w := itemOf(t, pool, items, inv.ID)
	if w.OwnerID != userID || w.Next == nil || !w.FollowToday || w.LastNote != "Voicemail, left a message" || w.Delivered != "none" {
		t.Errorf("worklist item = %+v", w)
	}
	// Asking about a credit note asks about its invoice.
	if err := workshop.TakeFollowUp(ctx, pool, advisor(), inv.ID, workshop.FollowUpSet{ClearOwner: true, ClearNext: true, Note: "Nobody"}); err != nil {
		t.Fatal(err)
	}
	f, _ = workshop.FollowUpFor(ctx, pool, advisor(), inv.ID)
	if f.OwnerID != "" || f.Next != nil {
		t.Errorf("cleared, it is still %q / %v", f.OwnerID, f.Next)
	}
	if err := workshop.TakeFollowUp(ctx, pool, advisor(), inv.ID, workshop.FollowUpSet{OwnerID: "00000000-0000-0000-0000-000000000000"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("giving it to nobody-in-particular = %v, want ErrInvalid", err)
	}
	addTechnician(t, pool)
	if err := workshop.TakeFollowUp(ctx, pool, advisor(), inv.ID, workshop.FollowUpSet{OwnerID: techUserID}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("giving it to a technician = %v, want ErrInvalid", err)
	}
	for name, err := range map[string]error{
		"take":    workshop.TakeFollowUp(ctx, pool, technician(), inv.ID, workshop.FollowUpSet{Note: "x"}),
		"contact": workshop.LogContact(ctx, pool, technician(), inv.ID, "phoned", "x", nil),
		"dispute": workshop.Dispute(ctx, pool, technician(), inv.ID, "x"),
	} {
		if !errors.Is(err, access.ErrForbidden) {
			t.Errorf("a technician %s = %v, want ErrForbidden", name, err)
		}
	}
	if err := workshop.LogContact(ctx, pool, advisor(), inv.ID, "", "x", nil); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a contact with no way = %v, want ErrInvalid", err)
	}
}

func TestAPromiseToPayLeavesTheBalanceAloneAndIsKeptOrBroken(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, inv := issued(t, pool)
	before, _ := workshop.BalanceFor(ctx, pool, advisor(), inv.ID)
	part := inv.GrossMinor / 2
	if err := workshop.RecordPromise(ctx, pool, advisor(), inv.ID, part, time.Now().AddDate(0, 0, -3), "Pays half on the 1st"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.RecordPromise(ctx, pool, advisor(), inv.ID, inv.GrossMinor*2, time.Now(), "Too much"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("promising more than is owed = %v, want ErrInvalid", err)
	}
	after, _ := workshop.BalanceFor(ctx, pool, advisor(), inv.ID)
	if after.OutstandingMinor() != before.OutstandingMinor() {
		t.Error("a promise changed the balance")
	}
	f, _ := workshop.FollowUpFor(ctx, pool, advisor(), inv.ID)
	if len(f.Promises) != 1 || !f.Promises[0].Broken || f.Promises[0].Kept {
		t.Fatalf("an unpaid promise whose day has gone = %+v", f.Promises)
	}
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{AmountMinor: &part, Method: "bank", PaidOn: time.Now()}); err != nil {
		t.Fatal(err)
	}
	f, _ = workshop.FollowUpFor(ctx, pool, advisor(), inv.ID)
	if !f.Promises[0].Kept || f.Promises[0].Broken {
		t.Errorf("after paying what was promised = %+v", f.Promises)
	}
}

func TestADisputeHoldsTheReminderAndNothingElse(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, inv := issued(t, pool)
	before, _ := workshop.BalanceFor(ctx, pool, advisor(), inv.ID)
	if err := workshop.Dispute(ctx, pool, advisor(), inv.ID, "Says the alignment was never done"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.Dispute(ctx, pool, advisor(), inv.ID, "Again"); err != nil {
		t.Fatalf("disputing twice = %v, want quiet", err)
	}
	in := workshop.ReminderInput{Channel: "email", Address: "kund@example.com", RecipientName: "A Customer", Confirmed: true,
		Subject: "x", Body: "Owed {owed} {link}", LinkBase: "https://x"}
	if _, err := workshop.QueueReminder(ctx, pool, advisor(), inv.ID, in); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a reminder on a disputed invoice = %v, want ErrInvalid", err)
	}
	if after, _ := workshop.BalanceFor(ctx, pool, advisor(), inv.ID); after.OutstandingMinor() != before.OutstandingMinor() {
		t.Error("a dispute changed the balance")
	}
	items, _ := workshop.Worklist(ctx, pool, advisor(), time.Now())
	if !itemOf(t, pool, items, inv.ID).Disputed {
		t.Error("the worklist does not show the hold")
	}
	if err := workshop.ResolveDispute(ctx, pool, advisor(), inv.ID, "It was done; photos shown"); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.QueueReminder(ctx, pool, advisor(), inv.ID, in); err != nil {
		t.Errorf("a reminder once the hold is lifted: %v", err)
	}
	f, _ := workshop.FollowUpFor(ctx, pool, advisor(), inv.ID)
	if f.Disputed != nil || len(f.Events) < 3 {
		t.Errorf("after resolving: disputed %v, %d events", f.Disputed, len(f.Events))
	}
}

func TestAReminderIsAskedAfterAgainAsItGoes(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, inv := issued(t, pool)
	prev, err := workshop.PrepareReminder(ctx, pool, advisor(), inv.ID, time.Now())
	if err != nil || prev.Outstanding != inv.GrossMinor || !prev.NeverDelivered {
		t.Fatalf("PrepareReminder = %+v, %v", prev, err)
	}
	in := workshop.ReminderInput{Channel: "email", Address: "kund@example.com", RecipientName: "A Customer", Confirmed: true,
		Subject: "Reminder", Body: "Still owed: {owed}. {link}", LinkBase: "https://verkstad.example"}
	bad := in
	bad.Confirmed = false
	if _, err := workshop.QueueReminder(ctx, pool, advisor(), inv.ID, bad); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an unconfirmed reminder = %v, want ErrInvalid", err)
	}
	bad = in
	bad.Body = "Pay up"
	if _, err := workshop.QueueReminder(ctx, pool, advisor(), inv.ID, bad); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a reminder with no amount or link = %v, want ErrInvalid", err)
	}
	msg, err := workshop.QueueReminder(ctx, pool, advisor(), inv.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	due, _ := workshop.DueMessages(ctx, pool, shopID, time.Now().Add(24*time.Hour), 10)
	if len(due) != 1 || due[0].ReminderFor != inv.ID || due[0].About != "reminder" {
		t.Fatalf("due = %+v", due)
	}

	// Half is paid while it waits: it goes, with what is owed then.
	half := inv.GrossMinor / 2
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{AmountMinor: &half, Method: "bank", PaidOn: time.Now()}); err != nil {
		t.Fatal(err)
	}
	owed, hold, err := workshop.StillOwed(ctx, pool, shopID, inv.ID)
	if err != nil || hold || owed != inv.GrossMinor-half {
		t.Errorf("StillOwed after a part payment = %d, %v, %v; want %d", owed, hold, err, inv.GrossMinor-half)
	}
	// Settled just before dispatch: it is cancelled, not sent.
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{Method: "bank", PaidOn: time.Now()}); err != nil {
		t.Fatal(err)
	}
	owed, _, _ = workshop.StillOwed(ctx, pool, shopID, inv.ID)
	if owed != 0 {
		t.Fatalf("settled, still owed %d", owed)
	}
	if err := workshop.MessageCancelled(ctx, pool, shopID, msg, "It was paid before this went; nothing was sent."); err != nil {
		t.Fatal(err)
	}
	if due, _ = workshop.DueMessages(ctx, pool, shopID, time.Now().Add(24*time.Hour), 10); len(due) != 0 {
		t.Errorf("a cancelled reminder is still due: %+v", due)
	}
	if _, err := workshop.QueueReminder(ctx, pool, advisor(), inv.ID, in); !errors.Is(err, workshop.ErrNothingOwed) {
		t.Errorf("a reminder for a settled invoice = %v, want ErrNothingOwed", err)
	}
}

func TestMoneyOwedBackIsARefundTaskAndPayingItBackIsNotAReversal(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, inv := mixedInvoice(t, pool)
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{Method: "bank", PaidOn: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.RecordRefund(ctx, pool, advisor(), inv.ID, workshop.RefundInput{Method: "bank", PaidOn: time.Now()}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a refund when nothing is owed back = %v, want ErrInvalid", err)
	}
	note, err := workshop.IssueCredit(ctx, pool, advisor(), inv.ID, workshop.CreditRequest{Lines: []workshop.CreditLine{{Position: 3}}, Reason: "Not done", Key: "r"})
	if err != nil {
		t.Fatal(err)
	}
	back := -note.GrossMinor
	items, _ := workshop.Worklist(ctx, pool, advisor(), time.Now())
	w := itemOf(t, pool, items, inv.ID)
	if !w.Refund() || w.Outstanding != -back || items[0].InvoiceID != inv.ID {
		t.Errorf("the refund task = %+v (first on the list: %v)", w, items[0].InvoiceID == inv.ID)
	}
	if _, err := workshop.RecordRefund(ctx, pool, advisor(), inv.ID, workshop.RefundInput{AmountMinor: back + 100, Method: "bank", PaidOn: time.Now()}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("refunding more than is owed back = %v, want ErrInvalid", err)
	}
	if _, err := workshop.RecordRefund(ctx, pool, advisor(), inv.ID, workshop.RefundInput{Method: "teleport", PaidOn: time.Now()}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an unknown way = %v, want ErrInvalid", err)
	}
	// A part first, then the rest.
	pay, err := workshop.RecordRefund(ctx, pool, advisor(), inv.ID, workshop.RefundInput{AmountMinor: 1000, Method: "bank", PaidOn: time.Now(), Reference: "Swish 123"})
	if err != nil || pay.AmountMinor != -1000 {
		t.Fatalf("a part refund = %+v, %v", pay, err)
	}
	if b, _ := workshop.BalanceFor(ctx, pool, advisor(), inv.ID); b.OutstandingMinor() != -(back - 1000) {
		t.Errorf("after a part refund %d is owed back, want %d", -b.OutstandingMinor(), back-1000)
	}
	if _, err := workshop.RecordRefund(ctx, pool, advisor(), inv.ID, workshop.RefundInput{Method: "bank", PaidOn: time.Now()}); err != nil {
		t.Fatal(err)
	}
	b, _ := workshop.BalanceFor(ctx, pool, advisor(), inv.ID)
	if b.OutstandingMinor() != 0 {
		t.Errorf("after refunding it all %d is owed", b.OutstandingMinor())
	}
	if n := countRows(t, pool, `SELECT count(*) FROM invoice_payments WHERE invoice_id = $1 AND amount_minor < 0 AND reverses_id IS NULL`, inv.ID); n != 2 {
		t.Errorf("%d payouts in the ledger, want 2 (separate from reversals)", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM receivable_events WHERE invoice_id = $1 AND kind = 'refund'`, inv.ID); n != 2 {
		t.Errorf("%d refund events, want 2", n)
	}
	// Reversing the payout, as a mistake, reopens what is owed back.
	payouts, _ := workshop.BalanceFor(ctx, pool, advisor(), inv.ID)
	var last string
	for _, p := range payouts.Payments {
		if p.AmountMinor < 0 {
			last = p.ID
		}
	}
	if err := workshop.ReversePayment(ctx, pool, advisor(), last); err != nil {
		t.Fatal(err)
	}
	if b, _ = workshop.BalanceFor(ctx, pool, advisor(), inv.ID); b.OutstandingMinor() >= 0 {
		t.Errorf("after reversing a payout %d, want it owed back again", b.OutstandingMinor())
	}
}

func TestAnOverpaymentIsOwedBack(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, inv := issued(t, pool)
	over := inv.GrossMinor + 5000
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{AmountMinor: &over, Method: "bank", PaidOn: time.Now()}); err != nil {
		t.Fatal(err)
	}
	items, _ := workshop.Worklist(ctx, pool, advisor(), time.Now())
	if w := itemOf(t, pool, items, inv.ID); !w.Refund() || w.Outstanding != -5000 {
		t.Errorf("an overpayment = %+v", w)
	}
	if _, err := workshop.RecordRefund(ctx, pool, advisor(), inv.ID, workshop.RefundInput{Method: "bank", PaidOn: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if b, _ := workshop.BalanceFor(ctx, pool, advisor(), inv.ID); b.OutstandingMinor() != 0 {
		t.Errorf("after refunding the overpayment %d is owed", b.OutstandingMinor())
	}
}

func TestSeveralInvoicesForOnePayerAreSeenTogether(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, first := issued(t, pool)
	_, second := issued(t, pool)
	items, _ := workshop.Worklist(ctx, pool, advisor(), time.Now())
	if itemOf(t, pool, items, first.ID).OtherOpen != 1 || itemOf(t, pool, items, second.ID).OtherOpen != 1 {
		t.Error("two open invoices for one customer do not point at each other")
	}
	// One was sent and nobody knows it arrived; the other never went.
	if _, err := workshop.SendDocument(ctx, pool, advisor(), first.ID, workshop.SendInput{Channel: "email", Address: "k@example.com",
		RecipientName: "A Customer", Confirmed: true, Subject: "x", Body: "{link}", LinkBase: "https://x"}); err != nil {
		t.Fatal(err)
	}
	items, _ = workshop.Worklist(ctx, pool, advisor(), time.Now())
	if itemOf(t, pool, items, first.ID).Delivered != "tried" || itemOf(t, pool, items, second.ID).Delivered != "none" {
		t.Errorf("delivery: %q / %q", itemOf(t, pool, items, first.ID).Delivered, itemOf(t, pool, items, second.ID).Delivered)
	}
	if _, err := workshop.Worklist(ctx, pool, technician(), time.Now()); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician read the worklist: %v", err)
	}
}
