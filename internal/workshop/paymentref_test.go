package workshop_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/payref"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A payment reference with a check digit on every invoice (#234), frozen at
// issue with the shop's Swish number: the shop changing its number later
// changes no invoice already issued, a credit note has no reference, and
// what is owed on the document falls with each payment.
func TestEveryInvoiceIsIssuedWithAPaymentReference(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	currentPool = pool
	shop, err := workshop.ShopDetailsFor(ctx, pool, owner())
	if err != nil {
		t.Fatal(err)
	}
	shop.SwishNumber = "123-456 78 90"
	if err := workshop.SaveDetails(ctx, pool, owner(), shop); err != nil {
		t.Fatal(err)
	}
	inv := mustIssue(t)
	shop.SwishNumber = "1239999999"
	if err := workshop.SaveDetails(ctx, pool, owner(), shop); err != nil {
		t.Fatal(err)
	}
	shop.SwishNumber = "070-123"
	if err := workshop.SaveDetails(ctx, pool, owner(), shop); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a Swish number of seven digits: %v", err)
	}

	d, err := workshop.DocumentByID(ctx, pool, advisor(), inv)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := payref.OCR(d.Number)
	if d.PaymentOCR != want || d.Seller.SwishNumber != "1234567890" || d.OwedMinor != d.GrossMinor {
		t.Errorf("the issued invoice: reference %q (want %q), Swish %q, owed %d of %d", d.PaymentOCR, want, d.Seller.SwishNumber, d.OwedMinor, d.GrossMinor)
	}
	if _, err := pool.Exec(ctx, `UPDATE invoices SET payment_ocr = '18' WHERE id = $1`, inv); err == nil {
		t.Error("an issued invoice's reference was changed")
	}
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv, workshop.NewPayment{AmountMinor: new(int64(10000)), Method: "bank", PaidOn: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if d, _ := workshop.DocumentByID(ctx, pool, advisor(), inv); d.OwedMinor != d.GrossMinor-10000 {
		t.Errorf("owed after a part payment: %d, want %d", d.OwedMinor, d.GrossMinor-10000)
	}
	note, err := workshop.CreditNote(ctx, pool, advisor(), inv)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := workshop.DocumentByID(ctx, pool, advisor(), note.ID); c.PaymentOCR != "" {
		t.Errorf("a credit note has a payment reference: %q", c.PaymentOCR)
	}
}

// A payment is recorded against the invoice its reference is for; a
// reference with a wrong check digit is refused before anything is looked
// up, a well-formed one nobody has is refused as such, and a technician
// looks nothing up.
func TestAPaymentFindsItsInvoiceByItsReference(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	currentPool = pool
	inv := mustIssue(t)
	d, _ := workshop.DocumentByID(ctx, pool, advisor(), inv)
	spaced := d.PaymentOCR[:2] + " " + d.PaymentOCR[2:]
	if id, ref, err := workshop.InvoiceByPaymentReference(ctx, pool, advisor(), spaced); err != nil || id != inv || ref != d.PaymentOCR {
		t.Errorf("by its reference typed with a space: %q %q %v", id, ref, err)
	}
	wrong := d.PaymentOCR[:len(d.PaymentOCR)-1] + string('0'+(d.PaymentOCR[len(d.PaymentOCR)-1]-'0'+1)%10)
	if _, _, err := workshop.InvoiceByPaymentReference(ctx, pool, advisor(), wrong); !errors.Is(err, workshop.ErrInvalid) || !strings.Contains(err.Error(), "check digit") {
		t.Errorf("a wrong check digit: %v", err)
	}
	// Padded with a 0 by a bank, it is still found.
	if id, _, err := workshop.InvoiceByPaymentReference(ctx, pool, advisor(), "0"+d.PaymentOCR); err != nil || id != inv {
		t.Errorf("by its reference with a leading 0 added: %q %v", id, err)
	}
	// Not copied whole: the length digit fits no form of it.
	cut := "12" + string(payref.CheckDigit("12"))
	if _, _, err := workshop.InvoiceByPaymentReference(ctx, pool, advisor(), cut); !errors.Is(err, workshop.ErrInvalid) || !strings.Contains(err.Error(), "length") {
		t.Errorf("a wrong length digit: %v", err)
	}
	nobody, _ := payref.OCR(99999)
	if _, _, err := workshop.InvoiceByPaymentReference(ctx, pool, advisor(), nobody); !errors.Is(err, workshop.ErrInvalid) || !strings.Contains(err.Error(), "no invoice") {
		t.Errorf("a reference nobody has: %v", err)
	}
	if _, _, err := workshop.InvoiceByPaymentReference(ctx, pool, technician(), d.PaymentOCR); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician looked a reference up: %v", err)
	}
}
