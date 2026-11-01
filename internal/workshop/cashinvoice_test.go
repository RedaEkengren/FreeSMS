package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Cash invoices (#156).

// Issued and paid in one step: the invoice and its payments in one
// transaction, the document saying it was paid and how, cash rounded to the
// krona with the difference kept apart, a split between card and cash. A
// payment that does not add up writes nothing -- no invoice, no number
// taken. Corrected by a credit note and a refund, never an edit.
func TestACashInvoiceIsIssuedAndPaidInOneStep(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	desk := advisor()

	job := readyToInvoice(t, pool)
	for name, sale := range map[string]workshop.CashSale{
		"by bank":        {Payments: []workshop.CashPayment{{Method: "bank"}}},
		"too much":       {Payments: []workshop.CashPayment{{Method: "card", AmountMinor: 99_999_999}, {Method: "cash"}}},
		"the rest twice": {Payments: []workshop.CashPayment{{Method: "card"}, {Method: "cash"}}},
		"nothing":        {},
	} {
		if _, err := workshop.IssueAndPay(ctx, pool, desk, job, sale); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("%s: %v, want refused", name, err)
		}
	}
	if n := countRows(t, pool, `SELECT count(*) FROM invoices`); n != 0 {
		t.Fatalf("%d invoices written by refused cash sales", n)
	}

	inv, err := workshop.IssueAndPay(ctx, pool, desk, job, workshop.CashSale{Payments: []workshop.CashPayment{
		{Method: "card", AmountMinor: 100000, Reference: "Kvitto 4711"}, {Method: "cash"}}})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Number != 1 {
		t.Errorf("the cash invoice is number %d; the refusals took numbers", inv.Number)
	}
	doc, err := workshop.DocumentByID(ctx, pool, desk, inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := doc.PaidOnIssue
	if p == nil || len(p.With) != 2 || p.With[0].Method != "card" || p.With[0].AmountMinor != 100000 || p.With[0].Reference != "Kvitto 4711" ||
		p.With[1].Method != "cash" || p.With[1].AmountMinor%100 != 0 || p.With[0].AmountMinor+p.With[1].AmountMinor+p.With[1].RoundingMinor != inv.GrossMinor {
		t.Errorf("the document's payment: %+v", p)
	}
	if b, _ := workshop.BalanceFor(ctx, pool, desk, inv.ID); !b.Settled() {
		t.Errorf("a cash invoice owes %d", b.OutstandingMinor())
	}
	plain, err := workshop.Issue(ctx, pool, desk, readyToInvoice(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := workshop.DocumentByID(ctx, pool, desk, plain.ID); d.PaidOnIssue != nil {
		t.Error("an ordinary invoice says it was paid on issue")
	}

	// Corrected by a credit note and the money given back as a refund.
	if _, err := workshop.CreditNote(ctx, pool, desk, inv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.RefundPayment(ctx, pool, desk, p.With[0].PaymentID, workshop.NewPayment{
		AmountMinor: minor(100000), Method: "card", PaidOn: time.Now()}, "Krediterad"); err != nil {
		t.Errorf("the card part refunded: %v", err)
	}
	if d, _ := workshop.DocumentByID(ctx, pool, desk, inv.ID); d.PaidOnIssue == nil || len(d.PaidOnIssue.With) != 2 {
		t.Error("the cash invoice's payment changed with its correction")
	}
	// Frozen; and an invoice issued before is not made a cash invoice now.
	for _, stmt := range []string{`UPDATE cash_invoices SET paid_with = '[]'`, `DELETE FROM cash_invoices`} {
		err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt)
			return err
		})
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "42501" {
			t.Errorf("%q as the application: %v, want refused for want of the privilege", stmt, err)
		}
	}
	if _, err := workshop.RecordPayment(ctx, pool, desk, plain.ID, workshop.NewPayment{Method: "cash", PaidOn: time.Now()}); err != nil {
		t.Fatal(err)
	}
	b, _ := workshop.BalanceFor(ctx, pool, desk, plain.ID)
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO cash_invoices (invoice_id, shop_id, paid_with) VALUES ($1, $2, jsonb_build_array(jsonb_build_object('payment_id', $3::text)))`,
			plain.ID, shopID, b.Payments[0].ID)
		return err
	}); err == nil {
		t.Error("the database made an invoice paid last week a cash invoice")
	}
}

// A manual invoice is paid on issue whatever modules run; a payment flow
// in use and not checked is on the owner's list until the adviser's check
// is recorded; the year's figure in person counts cash, card and Swish
// against the shop's threshold. All of it the owner's.
func TestPaymentFlowsAreCheckedAndTheYearIsCounted(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	currentPool = pool
	if _, err := workshop.AddPaymentFlow(ctx, pool, advisor(), "Kort i disken", "card"); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the front desk noted a flow: %v", err)
	}
	flow, err := workshop.AddPaymentFlow(ctx, pool, owner(), "Kort i disken för hyrtillägg", "card")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.AddPaymentFlow(ctx, pool, owner(), "kort i disken för hyrtillägg ", "card"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("the same flow twice: %v", err)
	}
	if n := len(waiting(t, owner(), "flow_unchecked")); n != 0 {
		t.Errorf("a flow nobody used is on the owner's list (%d)", n)
	}
	rental := advisor()
	rental.Modules = []access.Module{access.Rental}
	if _, err := workshop.IssueManualAndPay(ctx, pool, rental, workshop.ManualInvoice{CustomerID: fixtureCustomer,
		Lines: []workshop.SourcedLine{{Description: "Extra tankning", QuantityMilli: 1000, UnitPriceMinor: 50000, VATRateBasis: 2500}}},
		workshop.CashSale{FlowID: flow, Payments: []workshop.CashPayment{{Method: "swish", Reference: "SW-77"}}}); err != nil {
		t.Fatalf("a rental extra paid by Swish at the return: %v", err)
	}
	if items := waiting(t, owner(), "flow_unchecked"); len(items) != 1 || items[0].Text != "Kort i disken för hyrtillägg" {
		t.Errorf("the flow in use, not checked: %+v", items)
	}
	if err := workshop.CheckPaymentFlow(ctx, pool, owner(), flow, true, "", time.Now(), ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a check by nobody: %v", err)
	}
	if err := workshop.CheckPaymentFlow(ctx, pool, owner(), flow, true, "Revisor Rut", time.Now(), "Kontantfaktura räcker enligt henne"); err != nil {
		t.Fatal(err)
	}
	if n := len(waiting(t, owner(), "flow_unchecked")); n != 0 {
		t.Errorf("a checked flow is on the list (%d)", n)
	}
	if err := workshop.CheckPaymentFlow(ctx, pool, owner(), flow, false, "", time.Time{}, "Ny regel 2027"); err != nil {
		t.Fatal(err)
	}
	flows, _ := workshop.PaymentFlows(ctx, pool, owner())
	if len(flows) != 1 || flows[0].Checked || flows[0].Used != 1 {
		t.Errorf("the flow after the check was withdrawn: %+v", flows)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM payment_flow_checks`); n != 2 {
		t.Errorf("%d checks recorded, want both as rows", n)
	}

	if _, err := workshop.InPersonTurnoverFor(ctx, pool, advisor()); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the front desk read the in-person figure: %v", err)
	}
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), mustIssue(t), workshop.NewPayment{AmountMinor: minor(1000), Method: "bank", PaidOn: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := workshop.SetCashRegisterThreshold(ctx, pool, owner(), 62500, 1); err != nil {
		t.Fatal(err)
	}
	got, err := workshop.InPersonTurnoverFor(ctx, pool, owner())
	if err != nil || got.TakenMinor != 62500 || !got.Over() || got.Since.Month() != time.January {
		t.Errorf("the year in person: %+v, %v; want the Swish 625 kr and not the bank payment", got, err)
	}
}

func mustIssue(t *testing.T) string {
	t.Helper()
	inv, err := workshop.Issue(context.Background(), currentPool, advisor(), readyToInvoice(t, currentPool))
	if err != nil {
		t.Fatal(err)
	}
	return inv.ID
}
