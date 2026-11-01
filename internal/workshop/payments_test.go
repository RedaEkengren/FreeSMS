package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
)

func amount(v int64) *int64 { return &v }

// Half paid is neither paid nor unpaid; the rest settles it; the ledger has
// both rows and who recorded them, and the day the money came apart from the
// day it was typed in.
func TestAPartPaymentLeavesTheRestOwed(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	friday := time.Now().AddDate(0, 0, -3)
	half := inv.GrossMinor / 2
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{
		AmountMinor: &half, Method: "bank", PaidOn: friday, Reference: "OCR 1001"}); err != nil {
		t.Fatalf("RecordPayment: %v", err)
	}
	b, _ := workshop.BalanceFor(ctx, pool, advisor(), inv.ID)
	if b.OutstandingMinor() != inv.GrossMinor-half || b.Settled() {
		t.Errorf("after half: %d outstanding, settled %v", b.OutstandingMinor(), b.Settled())
	}
	if p := b.Payments[0]; p.PaidOn.Format("2006-01-02") != friday.Format("2006-01-02") || p.RecordedBy == "" ||
		p.RecordedAt.Before(time.Now().Add(-time.Minute)) || p.Series != "B" || p.Number != 1 {
		t.Errorf("payment %+v: want Friday's date, today's entry, who entered it, and B-1", p)
	}

	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{Method: "bank", PaidOn: time.Now()}); err != nil {
		t.Fatalf("settle the rest: %v", err)
	}
	if b, _ = workshop.BalanceFor(ctx, pool, advisor(), inv.ID); !b.Settled() {
		t.Errorf("after the rest: %d outstanding", b.OutstandingMinor())
	}
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{Method: "bank", PaidOn: time.Now()}); !errors.Is(err, workshop.ErrNothingOwed) {
		t.Errorf("settling a settled invoice: %v", err)
	}
}

// Cash is rounded to the krona at the counter and the difference is its own
// figure; the invoice's total is untouched.
func TestCashIsRoundedAndTheRoundingKeptApart(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	inv, _ := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	p, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{Method: "cash", PaidOn: time.Now()})
	if err != nil {
		t.Fatalf("RecordPayment: %v", err)
	}
	if p.AmountMinor%100 != 0 || p.AmountMinor+p.RoundingMinor != inv.GrossMinor {
		t.Errorf("cash %d and rounding %d, want whole kronor and the two making the invoice's %d",
			p.AmountMinor, p.RoundingMinor, inv.GrossMinor)
	}
	b, _ := workshop.BalanceFor(ctx, pool, advisor(), inv.ID)
	if !b.Settled() || b.GrossMinor != inv.GrossMinor {
		t.Errorf("settled %v, gross %d: the rounding must settle it without changing the invoice", b.Settled(), b.GrossMinor)
	}
}

// Somebody pays the old figure after a credit note: the balance goes negative
// and says so. The credit note counts against what is owed.
func TestAnOverpaymentAfterACreditNoteShows(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	inv, _ := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	credit, err := workshop.CreditNote(ctx, pool, advisor(), inv.ID)
	if err != nil {
		t.Fatalf("CreditNote: %v", err)
	}
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), credit.ID, workshop.NewPayment{
		AmountMinor: amount(100), Method: "bank", PaidOn: time.Now()}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a payment against the credit note: %v, want it sent to the invoice", err)
	}
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{
		AmountMinor: amount(inv.GrossMinor), Method: "bank", PaidOn: time.Now(), ConfirmedOverpayment: true, OverpaymentReason: "Betalade två gånger"}); err != nil {
		t.Fatalf("RecordPayment: %v", err)
	}
	b, _ := workshop.BalanceFor(ctx, pool, advisor(), credit.ID)
	if b.InvoiceID != inv.ID || b.OutstandingMinor() != -inv.GrossMinor {
		t.Errorf("balance %d on %s: want the whole payment owed back, on the invoice", b.OutstandingMinor(), b.Reference)
	}
	list, _ := workshop.Receivables(ctx, pool, advisor())
	if len(list) != 1 || list[0].OutstandingMinor() >= 0 {
		t.Errorf("receivables %v: money owed back must be listed, not hidden", list)
	}
}

// A mistake is corrected by a row that reverses it, once; the first row
// stays; and the database refuses a quiet edit or delete of either.
func TestAMistakeIsReversedNotDeleted(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	inv, _ := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	wrong, _ := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{
		AmountMinor: amount(50000), Method: "card", PaidOn: time.Now()})
	if err := workshop.ReversePayment(ctx, pool, advisor(), wrong.ID, "Registrerad av misstag"); err != nil {
		t.Fatalf("ReversePayment: %v", err)
	}
	if err := workshop.ReversePayment(ctx, pool, advisor(), wrong.ID, "Registrerad av misstag"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("reversing twice: %v", err)
	}
	b, _ := workshop.BalanceFor(ctx, pool, advisor(), inv.ID)
	if b.OutstandingMinor() != inv.GrossMinor || len(b.Payments) != 2 || !b.Payments[0].Reversed {
		t.Errorf("after the reversal: %d outstanding, %d rows; want the full amount owed and both rows kept",
			b.OutstandingMinor(), len(b.Payments))
	}
	for _, q := range []string{`UPDATE invoice_payments SET amount_minor = 1`, `DELETE FROM invoice_payments`} {
		err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q)
			return err
		})
		if err == nil {
			t.Errorf("%q was allowed on recorded payments", q)
		}
	}
}

// Overdue is worked out from the terms on the document, not from today's
// setting: shortening the terms does not make last month's invoices late.
func TestOverdueUsesTheTermsOnTheDocument(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	details, _ := workshop.ShopDetailsFor(ctx, pool, owner())
	details.PaymentTermsDays = 30
	if err := workshop.SaveDetails(ctx, pool, owner(), details); err != nil {
		t.Fatalf("SaveDetails: %v", err)
	}
	inv, _ := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	details.PaymentTermsDays = 0
	workshop.SaveDetails(ctx, pool, owner(), details)

	b, _ := workshop.BalanceFor(ctx, pool, advisor(), inv.ID)
	if b.Overdue() || b.Due.Sub(b.Today) < 29*24*time.Hour {
		t.Errorf("due %s, overdue %v: want 30 days from issue, as printed", b.Due, b.Overdue())
	}
}

// Money is the front desk's.
func TestOnlyTheFrontDeskRecordsMoney(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	inv, _ := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	for _, s := range []access.Scope{technician(), partsDeskScope()} {
		if _, err := workshop.RecordPayment(ctx, pool, s, inv.ID, workshop.NewPayment{Method: "cash", PaidOn: time.Now()}); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("RecordPayment as %s: %v", s.Role, err)
		}
		if _, err := workshop.BalanceFor(ctx, pool, s, inv.ID); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("BalanceFor as %s: %v", s.Role, err)
		}
		if _, err := workshop.Receivables(ctx, pool, s); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("Receivables as %s: %v", s.Role, err)
		}
	}
}

// A payment reaches the accounts as its own voucher in series B, dated the
// day the money came: into the till, the öre to rounding, out of the
// receivable.
func TestAPaymentIsBookedAgainstTheReceivable(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	inv, _ := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	p, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{Method: "cash", PaidOn: time.Now()})
	if err != nil {
		t.Fatalf("RecordPayment: %v", err)
	}
	from, to := period()
	body, count, err := workshop.ExportAccounting(ctx, pool, advisor(), from, to, false)
	if err != nil {
		t.Fatalf("ExportAccounting: %v", err)
	}
	if count != 2 {
		t.Errorf("exported %d, want the invoice and the payment", count)
	}
	posted := postings(t, body)
	// Over both vouchers the receivable clears: charged, then paid.
	if posted[1510] != 0 {
		t.Errorf("the receivable is left at %d after the invoice and its payment", posted[1510])
	}
	if posted[1910] != p.AmountMinor || posted[3740] != p.RoundingMinor {
		t.Errorf("till %d and rounding %d, want %d and %d", posted[1910], posted[3740], p.AmountMinor, p.RoundingMinor)
	}
	if !containsLine(body, `#VER "B" "1" `+p.PaidOn.Format("20060102")+` "Betalning A-1 A Customer"`) {
		t.Errorf("no voucher B 1 dated today:\n%s", body)
	}
}

// Money given back on an invoice that is not a refund of one payment --
// an overpayment returned -- is at most what is owed back: never more than
// the customer paid over, and nothing at all on an invoice that owes the
// shop. The database holds it too.
func TestMoneyGivenBackOnAnInvoiceIsAtMostWhatIsOwedBack(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{
		AmountMinor: amount(-1000), Method: "bank", PaidOn: time.Now()}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("money given back on an invoice nobody has paid: %v", err)
	}
	if err := rawPayment(t, pool, "invoice_id, amount_minor", 970, inv.ID, -1000); err == nil {
		t.Error("the database let money be given back on an invoice that owes the shop")
	}
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{
		AmountMinor: amount(inv.GrossMinor + 5000), Method: "bank", PaidOn: time.Now(), ConfirmedOverpayment: true, OverpaymentReason: "Betalade för mycket"}); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{
		AmountMinor: amount(-5001), Method: "bank", PaidOn: time.Now()}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("more given back than was paid over: %v", err)
	}
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{
		AmountMinor: amount(-5000), Method: "bank", PaidOn: time.Now()}); err != nil {
		t.Errorf("the overpayment given back: %v", err)
	}
	if b, _ := workshop.BalanceFor(ctx, pool, advisor(), inv.ID); !b.Settled() {
		t.Errorf("after the overpayment went back: %d outstanding", b.OutstandingMinor())
	}
}

// Whatever the path, what is given back on an invoice is at most what the
// customer paid beyond it: an overpayment returned and then the payment
// that overpaid refunded as well is one too many; a receipt reversed after
// its overpayment went back would leave money given back on an invoice
// that owes the shop; and a refund of an invoice paid exactly waits for a
// credit note. The database holds it too.
func TestEverythingGivenBackOnAnInvoiceIsWithinWhatWasPaidOver(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	desk := advisor()
	inv, err := workshop.Issue(ctx, pool, desk, readyToInvoice(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	first, err := workshop.RecordPayment(ctx, pool, desk, inv.ID, workshop.NewPayment{Method: "bank", PaidOn: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	// Paid exactly: nothing is owed back, so a refund waits for a credit.
	if _, err := workshop.RefundPayment(ctx, pool, desk, first.ID, workshop.NewPayment{
		AmountMinor: minor(1000), Method: "bank", PaidOn: time.Now()}, "Kunden klagade"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a refund on an invoice paid exactly: %v, want refused", err)
	}
	second, err := workshop.RecordPayment(ctx, pool, desk, inv.ID, workshop.NewPayment{
		AmountMinor: amount(inv.GrossMinor), Method: "bank", PaidOn: time.Now(), ConfirmedOverpayment: true, OverpaymentReason: "Betalade två gånger"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.RecordRefund(ctx, pool, desk, inv.ID, workshop.RefundInput{Method: "bank", PaidOn: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.RefundPayment(ctx, pool, desk, second.ID, workshop.NewPayment{
		AmountMinor: minor(inv.GrossMinor), Method: "bank", PaidOn: time.Now()}, "Dubbelbetalning"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("the overpayment given back a second time, as a refund of the payment: %v", err)
	}
	if err := rawPayment(t, pool, "invoice_id, amount_minor, refunds_id, reason", 980, inv.ID, -1000, second.ID, "x"); err == nil {
		t.Error("the database let a refund take more back than was paid over")
	}
	if err := workshop.ReversePayment(ctx, pool, desk, second.ID, "Fel"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a receipt reversed after its overpayment went back: %v", err)
	}
	if b, _ := workshop.BalanceFor(ctx, pool, desk, inv.ID); !b.Settled() {
		t.Errorf("after the refusals: %d outstanding, want settled", b.OutstandingMinor())
	}
	// Credited, the money is owed back, and a refund of a payment gives it.
	if _, err := workshop.CreditNote(ctx, pool, desk, inv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.RefundPayment(ctx, pool, desk, second.ID, workshop.NewPayment{
		AmountMinor: minor(inv.GrossMinor), Method: "bank", PaidOn: time.Now()}, "Krediterad"); err != nil {
		t.Errorf("a refund once the invoice is credited: %v", err)
	}
	if b, _ := workshop.BalanceFor(ctx, pool, desk, inv.ID); !b.Settled() {
		t.Errorf("credited and refunded: %d outstanding, want settled", b.OutstandingMinor())
	}
}
