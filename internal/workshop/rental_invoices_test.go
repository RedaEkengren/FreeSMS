package workshop_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Rental invoicing (#199).

// A rental is invoiced once it is over, from what was frozen on it and the
// charges the customer was told of: each once while its invoice stands,
// lines at different treatments adding up through internal/money, a
// damage outside the scope of VAT carried as O onto the document and its
// credit note, and the credited rent invoiced again.
func TestARentalIsInvoicedOnceFromWhatWasFrozenAndConfirmed(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	c := newDamageOn(t, pool, "GHI 56J", "V3")
	dc, _ := workshop.DamageCaseByID(ctx, pool, rentalDesk(), c)
	booking := dc.BookingID
	b, err := workshop.RentalBookingByID(ctx, pool, rentalDesk(), booking)
	if err != nil {
		t.Fatal(err)
	}
	toll, err := workshop.ConfirmCharge(ctx, pool, rentalDesk(), booking, workshop.NewCharge{Kind: workshop.ChargeCongestionTax,
		Description: "Trängselskatt", QuantityMilli: 3000, UnitPriceMinor: 3_333})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.IssueRentalInvoice(ctx, pool, rentalDesk(), booking, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("invoiced a charge the customer was not told of: %v", err)
	}
	if err := workshop.InformCharge(ctx, pool, rentalDesk(), toll, "email"); err != nil {
		t.Fatal(err)
	}
	amount := int64(150_000)
	steps(t, pool, c, workshop.DamageStep{Step: "amount_settled", AmountMinor: &amount},
		workshop.DamageStep{Step: "customer_informed", Channel: "email"})
	damage, err := workshop.ConfirmCharge(ctx, pool, rentalDesk(), booking, workshop.NewCharge{Kind: workshop.ChargeDamage, DamageCaseID: c})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.InformCharge(ctx, pool, rentalDesk(), damage, "letter"); err != nil {
		t.Fatal(err)
	}
	inv, err := workshop.IssueRentalInvoice(ctx, pool, rentalDesk(), booking, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.IssueRentalInvoice(ctx, pool, rentalDesk(), booking, ""); !errors.Is(err, workshop.ErrNothingToInvoice) {
		t.Errorf("invoiced twice: %v", err)
	}
	doc, err := workshop.DocumentByID(ctx, pool, everything(advisor()), inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	var net, vat int64
	outside := 0
	for _, l := range doc.Lines {
		net, vat = net+l.NetMinor, vat+l.VATMinor
		if l.VATCategory == "O" {
			outside++
			if l.VATMinor != 0 || l.NetMinor != amount || l.Kind != workshop.LineRentalCharge {
				t.Errorf("the damage line: %+v", l)
			}
		}
	}
	tollGross := int64(9_999) + 2_500 // 3 x 33,33 net, 25 % VAT rounded on the line
	switch {
	case doc.Source != workshop.SourceRental || len(doc.Lines) != len(b.Lines)+2 || outside != 1:
		t.Errorf("the invoice: source %q, %d lines (booking %d), %d outside the scope", doc.Source, len(doc.Lines), len(b.Lines), outside)
	case net+vat != doc.GrossMinor || doc.GrossMinor != b.Totals.GrossMinor+tollGross+amount:
		t.Errorf("the invoice comes to %d (lines %d), want %d", doc.GrossMinor, net+vat, b.Totals.GrossMinor+tollGross+amount)
	}
	credit, err := workshop.CreditNote(ctx, pool, everything(advisor()), inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	cdoc, err := workshop.DocumentByID(ctx, pool, everything(advisor()), credit.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range cdoc.Lines {
		if l.Kind == workshop.LineRentalCharge && l.VATRateBasis == 0 && l.VATCategory != "O" {
			t.Errorf("the damage credited without its category: %+v", l)
		}
	}
	again, err := workshop.IssueRentalInvoice(ctx, pool, rentalDesk(), booking, "")
	if err != nil || again.GrossMinor != inv.GrossMinor {
		t.Errorf("invoiced again after the credit note: %+v, %v; want %d", again, err, inv.GrossMinor)
	}
	from, to := time.Now().AddDate(0, 0, -1), time.Now().AddDate(0, 0, 1)
	if _, _, err := workshop.ExportAccounting(ctx, pool, everything(owner()), from, to, false); !errors.Is(err, workshop.ErrRentalPostings) {
		t.Errorf("a rental exported before the accountant has said how: %v", err)
	}
}

// Two requests at once invoice a rental once.
func TestTwoRequestsAtOnceInvoiceARentalOnce(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	c := newDamageOn(t, pool, "GHI 56J", "V3")
	dc, _ := workshop.DamageCaseByID(ctx, pool, rentalDesk(), c)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := workshop.IssueRentalInvoice(ctx, pool, rentalDesk(), dc.BookingID, "")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	ok, nothing := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, workshop.ErrNothingToInvoice):
			nothing++
		default:
			t.Errorf("an invoice request: %v", err)
		}
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM invoices WHERE rental_id = $1`, dc.BookingID).Scan(&count); err != nil || ok != 1 || nothing != 1 || count != 1 {
		t.Errorf("%d issued, %d found nothing, %d invoices (%v); want one", ok, nothing, count, err)
	}
}

// A rental not yet over is not invoiced; a cancelled one is invoiced its
// fee and no rent, to a company paying for the renter.
func TestACancelledRentalIsInvoicedItsFeeAlone(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, out, _ := checkedOut(t, pool)
	if _, err := workshop.IssueRentalInvoice(ctx, pool, rentalDesk(), out, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a rental invoiced while out: %v", err)
	}
	_, booking := handoverReadyWith(t, pool, "GHI 56J", "V3")
	if err := workshop.CancelRentalBooking(ctx, pool, rentalDesk(), booking, "Kunden avbokade", false); err != nil {
		t.Fatal(err)
	}
	fee, err := workshop.ConfirmCharge(ctx, pool, rentalDesk(), booking, workshop.NewCharge{Kind: workshop.ChargeCancellationFee,
		Description: "Avbokningsavgift", QuantityMilli: 1000, UnitPriceMinor: 20_000})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.InformCharge(ctx, pool, rentalDesk(), fee, "sms"); err != nil {
		t.Fatal(err)
	}
	firm, err := workshop.CreateParty(ctx, pool, rentalDesk(), workshop.NewParty{Kind: "company", Name: "Åkeri AB",
		OrgNumber: "556677-8899", City: "Solna", Role: workshop.PartyRenter})
	if err != nil {
		t.Fatal(err)
	}
	inv, err := workshop.IssueRentalInvoice(ctx, pool, rentalDesk(), booking, firm)
	if err != nil {
		t.Fatal(err)
	}
	if inv.GrossMinor != 25_000 || inv.CustomerName != "Åkeri AB" {
		t.Errorf("the cancellation invoice: %d to %q; want 250 kr to Åkeri AB", inv.GrossMinor, inv.CustomerName)
	}
}

// A rental paid at the counter as it is invoiced is a cash invoice (#156):
// the invoice and its payment in one transaction, the document saying it
// was paid and how. A payment that does not add up takes no number and
// leaves the rental to invoice.
func TestARentalPaidAtTheReturnIsACashInvoice(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	dc, err := workshop.DamageCaseByID(ctx, pool, rentalDesk(), newDamageOn(t, pool, "GHI 56J", "V3"))
	if err != nil {
		t.Fatal(err)
	}
	booking := dc.BookingID
	b, err := workshop.RentalBookingByID(ctx, pool, rentalDesk(), booking)
	if err != nil {
		t.Fatal(err)
	}
	short := workshop.CashSale{Payments: []workshop.CashPayment{{Method: "card", AmountMinor: b.Totals.GrossMinor - 100}}}
	if _, err := workshop.IssueRentalInvoiceAndPay(ctx, pool, rentalDesk(), booking, "", short); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("paid short: %v", err)
	}
	if invs, err := workshop.RentalInvoices(ctx, pool, rentalDesk(), booking); err != nil || len(invs) != 0 {
		t.Errorf("a payment that did not add up left %d invoices, %v", len(invs), err)
	}
	if _, err := workshop.IssueRentalInvoiceAndPay(ctx, pool, technician(), booking, "", short); err == nil {
		t.Error("a technician invoiced a rental")
	}
	sale := workshop.CashSale{Payments: []workshop.CashPayment{{Method: "card", AmountMinor: b.Totals.GrossMinor, Reference: "T-77"}}}
	inv, err := workshop.IssueRentalInvoiceAndPay(ctx, pool, rentalDesk(), booking, "", sale)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := workshop.DocumentByID(ctx, pool, everything(advisor()), inv.ID)
	switch {
	case err != nil:
		t.Fatal(err)
	case doc.Source != workshop.SourceRental || doc.PaidOnIssue == nil || len(doc.PaidOnIssue.With) != 1 ||
		doc.PaidOnIssue.With[0].Method != "card" || doc.PaidOnIssue.With[0].AmountMinor != b.Totals.GrossMinor:
		t.Errorf("the cash invoice: source %q, paid %+v", doc.Source, doc.PaidOnIssue)
	}
	if _, err := workshop.IssueRentalInvoiceAndPay(ctx, pool, rentalDesk(), booking, "", sale); !errors.Is(err, workshop.ErrNothingToInvoice) {
		t.Errorf("invoiced and paid twice: %v", err)
	}
}
