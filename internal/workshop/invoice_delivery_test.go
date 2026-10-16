package workshop_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Getting an invoice or a credit note to the customer, and knowing whether it
// arrived: each attempt is a row, a link opens one document, and a message a
// provider took is not a message anybody has read.

func issued(t *testing.T, pool *pgxpool.Pool) (job string, inv workshop.Invoice) {
	t.Helper()
	job = readyToInvoice(t, pool)
	inv, err := workshop.Issue(context.Background(), pool, advisor(), job)
	if err != nil {
		t.Fatal(err)
	}
	return job, inv
}

func send(t *testing.T, pool *pgxpool.Pool, invoice, to, who string) string {
	t.Helper()
	_, err := workshop.SendDocument(context.Background(), pool, advisor(), invoice, workshop.SendInput{
		Channel: "email", Address: to, RecipientName: who, Confirmed: true,
		Subject: "Invoice", Body: "Your invoice: {link}", LinkBase: "https://verkstad.example",
	})
	if err != nil {
		t.Fatalf("SendDocument: %v", err)
	}
	var body string
	if err := pool.QueryRow(context.Background(), `
		SELECT body FROM outbound_messages ORDER BY created_at DESC LIMIT 1`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	i := strings.Index(body, "/d/")
	if i < 0 {
		t.Fatalf("the message carries no link: %q", body)
	}
	return body[i+3:]
}

func TestAnInvoiceLinkOpensThatDocumentAndOpeningItIsTheProofOfReceipt(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, inv := issued(t, pool)
	token := send(t, pool, inv.ID, "kund@example.com", "A Customer")

	ds, _ := workshop.DeliveriesFor(ctx, pool, advisor(), inv.ID)
	if len(ds) != 1 || ds[0].Status() != "waiting to be sent" || ds[0].Reached() {
		t.Fatalf("a queued message is %q", ds[0].Status())
	}
	// The provider takes it, then reports it delivered: still no proof the
	// customer has read it, until the link is opened.
	var msg string
	if err := pool.QueryRow(ctx, `SELECT id FROM outbound_messages ORDER BY created_at DESC LIMIT 1`).Scan(&msg); err != nil {
		t.Fatal(err)
	}
	if err := workshop.MessageSent(ctx, pool, shopID, msg, "test", "p1"); err != nil {
		t.Fatal(err)
	}
	ds, _ = workshop.DeliveriesFor(ctx, pool, advisor(), inv.ID)
	if ds[0].Status() != "handed to the provider" || ds[0].Reached() {
		t.Errorf("a sent message is %q (reached %v)", ds[0].Status(), ds[0].Reached())
	}
	if err := workshop.MessageDelivery(ctx, pool, shopID, "test", "p1", true); err != nil {
		t.Fatal(err)
	}

	doc, err := workshop.OpenDocumentLink(ctx, pool, shopID, token)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Document.ID != inv.ID || doc.Document.CustomerName == "" || len(doc.Document.Lines) == 0 || !doc.Document.Seller.Known() {
		t.Errorf("the customer's document is incomplete: %+v", doc.Document)
	}
	if doc.Balance.OutstandingMinor() != inv.GrossMinor {
		t.Errorf("the balance is %d, want %d", doc.Balance.OutstandingMinor(), inv.GrossMinor)
	}
	ds, _ = workshop.DeliveriesFor(ctx, pool, advisor(), inv.ID)
	if ds[0].Status() != "opened by the customer" || !ds[0].Reached() || ds[0].Opens != 1 {
		t.Errorf("after opening: %q, %d opens", ds[0].Status(), ds[0].Opens)
	}
	if _, err := workshop.OpenDocumentLink(ctx, pool, shopID, "not-a-token"); !errors.Is(err, workshop.ErrShareNotUsable) {
		t.Errorf("a wrong link = %v, want ErrShareNotUsable", err)
	}
}

func TestADocumentOnTheLinkDoesNotChangeWhenTheWorkshopOrTheBalanceDoes(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, inv := issued(t, pool)
	token := send(t, pool, inv.ID, "kund@example.com", "A Customer")
	before, err := workshop.OpenDocumentLink(ctx, pool, shopID, token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE shops SET name = 'Helt nytt namn AB'`); err != nil {
		t.Fatal(err)
	}
	half := inv.GrossMinor / 2
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{AmountMinor: &half, Method: "bank", PaidOn: time.Now()}); err != nil {
		t.Fatal(err)
	}
	after, err := workshop.OpenDocumentLink(ctx, pool, shopID, token)
	if err != nil {
		t.Fatal(err)
	}
	if after.Document.Seller.Name != before.Document.Seller.Name || after.Document.GrossMinor != before.Document.GrossMinor ||
		after.Document.CustomerName != before.Document.CustomerName || len(after.Document.Lines) != len(before.Document.Lines) {
		t.Errorf("the issued document moved: %q -> %q", before.Document.Seller.Name, after.Document.Seller.Name)
	}
	if after.Balance.PaidMinor != half || after.Balance.OutstandingMinor() != inv.GrossMinor-half {
		t.Errorf("the balance did not follow the payment: %+v", after.Balance)
	}
}

func TestACreditNoteHasItsOwnLinkAndPointsAtTheInvoice(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, inv := issued(t, pool)
	invToken := send(t, pool, inv.ID, "kund@example.com", "A Customer")
	credit, err := workshop.CreditNote(ctx, pool, advisor(), inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	creditToken := send(t, pool, credit.ID, "kund@example.com", "A Customer")

	got, err := workshop.OpenDocumentLink(ctx, pool, shopID, creditToken)
	if err != nil || got.Document.ID != credit.ID || got.Document.CreditOf != inv.Reference() {
		t.Fatalf("the credit note's link opens %q credit-of %q, %v", got.Document.ID, got.Document.CreditOf, err)
	}
	orig, err := workshop.OpenDocumentLink(ctx, pool, shopID, invToken)
	if err != nil || orig.Document.ID != inv.ID || len(orig.Document.CreditedBy) != 1 || orig.Document.CreditedBy[0] != credit.Reference() {
		t.Fatalf("the invoice's link opens %q credited-by %v, %v", orig.Document.ID, orig.Document.CreditedBy, err)
	}
	// Neither link opens the other, and sending the credit note left the
	// invoice's link alone.
	if orig.Document.ID == got.Document.ID {
		t.Error("two links open the same document")
	}
}

func TestResendingClosesTheEarlierLinkAndRevokingClosesThemAll(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, inv := issued(t, pool)
	first := send(t, pool, inv.ID, "fel@example.com", "A Customer")
	second := send(t, pool, inv.ID, "ratt@example.com", "A Customer")
	if _, err := workshop.OpenDocumentLink(ctx, pool, shopID, first); !errors.Is(err, workshop.ErrShareNotUsable) {
		t.Errorf("the link sent to the wrong address still opens: %v", err)
	}
	if _, err := workshop.OpenDocumentLink(ctx, pool, shopID, second); err != nil {
		t.Fatal(err)
	}
	ds, _ := workshop.DeliveriesFor(ctx, pool, advisor(), inv.ID)
	if len(ds) != 2 || ds[0].LinkLive == ds[1].LinkLive {
		t.Errorf("deliveries: %d, live %v / %v", len(ds), ds[0].LinkLive, ds[1].LinkLive)
	}
	if err := workshop.RevokeDocumentLinks(ctx, pool, advisor(), inv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.OpenDocumentLink(ctx, pool, shopID, second); !errors.Is(err, workshop.ErrShareNotUsable) {
		t.Errorf("a revoked link opens: %v", err)
	}
}

func TestADocumentGoesToWhoeverTheDeskConfirmsNotWhoeverOwnsTheCar(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, inv := issued(t, pool)
	in := workshop.SendInput{Channel: "email", Address: "ekonomi@bolaget.example", RecipientName: "Bolaget AB, ekonomi",
		Subject: "x", Body: "{link}", LinkBase: "https://x"}
	if _, err := workshop.SendDocument(ctx, pool, advisor(), inv.ID, in); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("sending without confirming who it is for = %v, want ErrInvalid", err)
	}
	in.Confirmed = true
	for name, mut := range map[string]func(*workshop.SendInput){
		"no recipient": func(i *workshop.SendInput) { i.RecipientName = " " },
		"no address":   func(i *workshop.SendInput) { i.Address = "" },
		"no link":      func(i *workshop.SendInput) { i.Body = "Pay up" },
		"bad channel":  func(i *workshop.SendInput) { i.Channel = "fax" },
	} {
		bad := in
		mut(&bad)
		if _, err := workshop.SendDocument(ctx, pool, advisor(), inv.ID, bad); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("%s = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := workshop.SendDocument(ctx, pool, advisor(), inv.ID, in); err != nil {
		t.Fatal(err)
	}
	ds, _ := workshop.DeliveriesFor(ctx, pool, advisor(), inv.ID)
	if len(ds) != 1 || ds[0].RecipientName != "Bolaget AB, ekonomi" || ds[0].Address != "ekonomi@bolaget.example" {
		t.Errorf("the delivery records %+v", ds)
	}
	// The invoice itself still says who it was issued to.
	doc, _ := workshop.DocumentByID(ctx, pool, advisor(), inv.ID)
	if doc.CustomerName == "Bolaget AB, ekonomi" {
		t.Error("sending to someone else rewrote who the invoice is for")
	}
	if _, err := workshop.SendDocument(ctx, pool, technician(), inv.ID, in); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician sent an invoice: %v", err)
	}
}

func TestAFailedProviderRetryIsListedUntilSomeoneHandsItOver(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, inv := issued(t, pool)
	since := time.Now().AddDate(0, 0, -7)

	// Issued and nothing tried: on the list.
	list, err := workshop.UndeliveredDocuments(ctx, pool, advisor(), since)
	if err != nil || len(list) != 1 || list[0].Why != "none" {
		t.Fatalf("never tried: %+v, %v", list, err)
	}
	send(t, pool, inv.ID, "kund@example.com", "A Customer")
	var msg string
	if err := pool.QueryRow(ctx, `SELECT id FROM outbound_messages ORDER BY created_at DESC LIMIT 1`).Scan(&msg); err != nil {
		t.Fatal(err)
	}
	// The provider fails it again and again: retry, then failed.
	for tries := 0; tries < 20; tries++ {
		if err := workshop.MessageNotSent(ctx, pool, shopID, msg, tries, "mailbox full"); err != nil {
			t.Fatal(err)
		}
	}
	ds, _ := workshop.DeliveriesFor(ctx, pool, advisor(), inv.ID)
	if ds[0].Status() != "did not go" || !ds[0].Failed() {
		t.Errorf("after the provider gave up: %q (failed %v)", ds[0].Status(), ds[0].Failed())
	}
	list, _ = workshop.UndeliveredDocuments(ctx, pool, advisor(), since)
	if len(list) != 1 || list[0].Why != "failed" {
		t.Fatalf("a failed delivery: %+v", list)
	}
	// Printed at the counter and handed over: it reached them another way.
	if err := workshop.HandDocumentOver(ctx, pool, advisor(), inv.ID, workshop.HandInput{RecipientName: "A Customer", How: "printed"}); err != nil {
		t.Fatal(err)
	}
	if list, _ = workshop.UndeliveredDocuments(ctx, pool, advisor(), since); len(list) != 0 {
		t.Errorf("a handed-over document is still listed: %+v", list)
	}
	if err := workshop.HandDocumentOver(ctx, pool, advisor(), inv.ID, workshop.HandInput{RecipientName: "A Customer", How: "carrier pigeon"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an unknown way = %v, want ErrInvalid", err)
	}
	if err := workshop.HandDocumentOver(ctx, pool, technician(), inv.ID, workshop.HandInput{RecipientName: "A", How: "handed"}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician recorded a handover: %v", err)
	}
}
