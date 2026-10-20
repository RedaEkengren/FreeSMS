package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A line put on after the customer said yes is not part of what they agreed
// to. Invoicing it is something somebody decides, not something that happens.
func TestALineNobodyAgreedToIsNotInvoicedUnasked(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := readyToInvoice(t, pool)
	if err := workshop.AddLine(ctx, pool, advisor(), job, workshop.NewLine{
		Kind: "fee", Description: "Extra", QuantityMilli: 1000, UnitPriceMinor: 50000, VATRateBasis: 2500}); err != nil {
		t.Fatal(err)
	}

	if _, err := workshop.Issue(ctx, pool, advisor(), job); !errors.Is(err, workshop.ErrUnagreedLines) {
		t.Fatalf("Issue with an unagreed line = %v, want ErrUnagreedLines", err)
	}
	// Refused whole: no number was used.
	inv, err := workshop.Issue(ctx, pool, advisor(), job, workshop.InvoiceUnagreedLines())
	if err != nil {
		t.Fatalf("Issue when told to include it: %v", err)
	}
	if inv.Reference() != "A-1" {
		t.Errorf("the refusal used a number: first invoice is %s", inv.Reference())
	}
}

// Putting the line to the customer is the other way through.
func TestALineTheCustomerAgreedToLaterIsInvoiced(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := readyToInvoice(t, pool)
	if err := workshop.AddLine(ctx, pool, advisor(), job, workshop.NewLine{
		Kind: "fee", Description: "Extra", QuantityMilli: 1000, UnitPriceMinor: 50000, VATRateBasis: 2500}); err != nil {
		t.Fatal(err)
	}
	agree(t, pool, job)
	if _, err := workshop.Issue(ctx, pool, advisor(), job); err != nil {
		t.Fatalf("Issue after the customer agreed: %v", err)
	}
}

// A payment dated a year before the invoice, or one far above it, is asked
// about; a few days before is ordinary (paid on Friday, written up on Monday).
func TestAPaymentOutOfProportionIsRefusedUntilConfirmed(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{
		AmountMinor: amount(100), Method: "bank", PaidOn: time.Now().AddDate(-1, 0, 0)}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a payment a year before the invoice = %v, want refused", err)
	}
	ten := inv.GrossMinor * 10
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{
		AmountMinor: &ten, Method: "bank", PaidOn: time.Now()}); !errors.Is(err, workshop.ErrOverpayment) {
		t.Errorf("ten times the invoice = %v, want ErrOverpayment", err)
	}
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{
		AmountMinor: &ten, Method: "bank", PaidOn: time.Now(), ConfirmedOverpayment: true}); err != nil {
		t.Errorf("a confirmed overpayment = %v", err)
	}
}
