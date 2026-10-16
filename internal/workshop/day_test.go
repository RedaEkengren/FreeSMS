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

// Closing the day: sales apart from cash, every figure traceable, the local
// calendar day with its daylight-saving edges, comparisons with the other side
// that need a reason when they differ, and a sign-off that notices when the day
// has changed under it.

func today() string { return time.Now().In(shopZone()).Format("2006-01-02") }

func shopZone() *time.Location {
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		return time.UTC
	}
	return loc
}

func dayOf(t *testing.T, pool *pgxpool.Pool, day string) workshop.DayFigures {
	t.Helper()
	d, err := workshop.DayFor(context.Background(), pool, advisor(), day)
	if err != nil {
		t.Fatalf("DayFor %s: %v", day, err)
	}
	return d
}

func method(d workshop.DayFigures, m string) workshop.MethodFigures {
	for _, x := range d.Methods {
		if x.Method == m {
			return x
		}
	}
	return workshop.MethodFigures{Method: m}
}

func TestADayKeepsSalesApartFromCashAndLinksEveryFigureToItsSource(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, inv := issued(t, pool)
	half := inv.GrossMinor / 2
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{AmountMinor: &half, Method: "bank", PaidOn: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// All paid in cash, rounded to the krona, and then a part credited and paid back.
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{Method: "cash", PaidOn: time.Now()}); err != nil {
		t.Fatal(err)
	}
	credit, err := workshop.IssueCredit(ctx, pool, advisor(), inv.ID, workshop.CreditRequest{Lines: []workshop.CreditLine{{Position: 1, QuantityMilli: 1000}}, Reason: "One hour too many", Key: "d1"})
	if err != nil {
		t.Fatal(err)
	}
	refund, err := workshop.RecordRefund(ctx, pool, advisor(), inv.ID, workshop.RefundInput{Method: "bank", PaidOn: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	d := dayOf(t, pool, today())
	if d.InvoicedGross != inv.GrossMinor || d.CreditedGross != credit.GrossMinor || len(d.Documents) != 2 {
		t.Errorf("sales: invoiced %d credited %d (%d documents)", d.InvoicedGross, d.CreditedGross, len(d.Documents))
	}
	if d.NetSales() != inv.NetMinor+credit.NetMinor {
		t.Errorf("net sales %d, want %d", d.NetSales(), inv.NetMinor+credit.NetMinor)
	}
	bank, cash := method(d, "bank"), method(d, "cash")
	if bank.Received != half || bank.PaidOut != -refund.AmountMinor || bank.Movement() != half+refund.AmountMinor {
		t.Errorf("bank: received %d paid out %d movement %d", bank.Received, bank.PaidOut, bank.Movement())
	}
	if cash.Received != inv.GrossMinor-half-cash.Rounding || cash.Rounding == 0 && (inv.GrossMinor-half)%100 != 0 {
		t.Errorf("cash: received %d rounding %d", cash.Received, cash.Rounding)
	}
	for _, e := range append(append([]workshop.DayEntry{}, bank.Entries...), cash.Entries...) {
		if e.ID == "" || e.InvoiceID != inv.ID || e.Reference == "" {
			t.Errorf("an entry that cannot be followed to its source: %+v", e)
		}
	}
	for _, x := range d.Documents {
		if x.ID == "" || x.Reference == "" {
			t.Errorf("a document that cannot be followed: %+v", x)
		}
	}
	// Money is not inferred from a record: nothing is compared until somebody does.
	if bank.Compared || cash.Compared {
		t.Error("a payment record was taken as the bank's word")
	}
	if d.OwedToUs != 0 || d.OwedBack != 0 {
		t.Errorf("owed %d / owed back %d after paying and refunding it all", d.OwedToUs, d.OwedBack)
	}
	if dayOf(t, pool, time.Now().AddDate(0, 0, -1).In(shopZone()).Format("2006-01-02")).InvoicedGross != 0 {
		t.Error("yesterday shows today's invoice")
	}
}

func TestADayIsTheShopsLocalDayAcrossTheClockChange(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	add := func(n int, at string) {
		if _, err := pool.Exec(ctx, `
			INSERT INTO invoices (shop_id, series, number, work_order_id, customer_name, net_minor, vat_minor, gross_minor, issued_at)
			VALUES ($1, 'A', $2, $3, 'X', 1000, 250, 1250, $4::timestamptz)`, shopID, n, newJob(t, pool), at); err != nil {
			t.Fatal(err)
		}
	}
	// Sweden springs forward at 02:00 on 29 March 2026.
	add(901, "2026-03-28T22:30:00Z") // 23:30 on the 28th, local
	add(902, "2026-03-28T23:30:00Z") // 00:30 on the 29th, local
	add(903, "2026-03-29T21:30:00Z") // 23:30 on the 29th, local (CEST)
	add(904, "2026-03-29T22:30:00Z") // 00:30 on the 30th, local
	for day, want := range map[string]int{"2026-03-28": 1, "2026-03-29": 2, "2026-03-30": 1} {
		if got := len(dayOf(t, pool, day).Documents); got != want {
			t.Errorf("%s has %d documents, want %d", day, got, want)
		}
	}
	if _, err := workshop.DayFor(ctx, pool, advisor(), "yesterday-ish"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a day that is not a date = %v, want ErrInvalid", err)
	}
}

func TestAComparisonThatDiffersNeedsAReasonAndTheLatestIsTheCurrentOne(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, inv := issued(t, pool)
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{Method: "bank", PaidOn: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := workshop.RecordSettlement(ctx, pool, advisor(), today(), "bank", inv.GrossMinor-500, ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a difference with no reason = %v, want ErrInvalid", err)
	}
	if err := workshop.RecordSettlement(ctx, pool, advisor(), today(), "bank", inv.GrossMinor-500, "Bank fee taken before the statement"); err != nil {
		t.Fatal(err)
	}
	m := method(dayOf(t, pool, today()), "bank")
	if !m.Compared || m.Difference() != -500 || m.Explanation == "" || m.ComparedBy == "" {
		t.Errorf("after a difference: %+v", m)
	}
	if err := workshop.RecordSettlement(ctx, pool, advisor(), today(), "bank", inv.GrossMinor, ""); err != nil {
		t.Fatalf("agreeing needs no reason: %v", err)
	}
	if m = method(dayOf(t, pool, today()), "bank"); m.Difference() != 0 || !m.Compared {
		t.Errorf("after the second comparison: %+v", m)
	}
	for name, err := range map[string]error{
		"bad method": workshop.RecordSettlement(ctx, pool, advisor(), today(), "barter", 0, "x"),
		"bad day":    workshop.RecordSettlement(ctx, pool, advisor(), "never", "bank", 0, "x"),
		"technician": workshop.RecordSettlement(ctx, pool, technician(), today(), "bank", 0, "x"),
	} {
		if name == "technician" && !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s = %v, want ErrForbidden", name, err)
		}
		if name != "technician" && !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("%s = %v, want ErrInvalid", name, err)
		}
	}
}

func TestASignedOffDayNoticesALaterCorrectionAndOnlyTheOwnerSignsOff(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	_, inv := issued(t, pool)
	if _, err := workshop.ReviewDay(ctx, pool, advisor(), today(), ""); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the counter signed a day off: %v", err)
	}
	if _, err := workshop.ReviewDay(ctx, pool, owner(), time.Now().AddDate(0, 0, 9).Format("2006-01-02"), ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("signing off a day that has not happened = %v, want ErrInvalid", err)
	}
	r1, err := workshop.ReviewDay(ctx, pool, owner(), today(), "Looked, all fine")
	if err != nil || r1.Version != 1 {
		t.Fatalf("ReviewDay = %+v, %v", r1, err)
	}
	if again, _ := workshop.ReviewDay(ctx, pool, owner(), today(), "Pressed twice"); again.Version != 1 {
		t.Errorf("signing off an unchanged day again made version %d", again.Version)
	}
	d := dayOf(t, pool, today())
	if d.Review == nil || d.Stale || d.Review.By == "" {
		t.Fatalf("the day after sign-off: %+v", d.Review)
	}
	// A payment entered for this day afterwards, and one for an earlier day that
	// does not touch it.
	yesterday := time.Now().AddDate(0, 0, -1)
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{AmountMinor: amount(1000), Method: "bank", PaidOn: yesterday}); err != nil {
		t.Fatal(err)
	}
	if d = dayOf(t, pool, today()); d.Stale {
		t.Error("a payment dated yesterday made today stale")
	}
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{AmountMinor: amount(2000), Method: "swish", PaidOn: time.Now()}); err != nil {
		t.Fatal(err)
	}
	d = dayOf(t, pool, today())
	if !d.Stale || d.Review.Version != 1 {
		t.Fatalf("a payment added after the sign-off: stale %v, review %+v", d.Stale, d.Review)
	}
	r2, err := workshop.ReviewDay(ctx, pool, owner(), today(), "Looked again")
	if err != nil || r2.Version != 2 {
		t.Fatalf("second review = %+v, %v", r2, err)
	}
	if d = dayOf(t, pool, today()); d.Stale || d.Review.Version != 2 {
		t.Errorf("after the second sign-off: stale %v version %d", d.Stale, d.Review.Version)
	}
	// And yesterday, which the late-recorded payment belongs to, was never signed off.
	if y := dayOf(t, pool, yesterday.In(shopZone()).Format("2006-01-02")); y.Review != nil || method(y, "bank").Received != 1000 {
		t.Errorf("yesterday: review %v, bank %d", y.Review, method(y, "bank").Received)
	}
}

func TestTheDayListsWhatShouldBePutRightBeforeSigningOff(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	// A clock left running.
	running := working(t, pool)
	// Done and never invoiced.
	done := inProgress(t, pool, "Replace brake discs")
	move(t, pool, done, workshop.StateReady)
	// A part taken out and never priced.
	stockShelf(t, pool, 3)
	taken := working(t, pool)
	_ = running
	if _, err := pool.Exec(ctx, `
		INSERT INTO stock_movements (shop_id, part_id, kind, quantity, work_order_id) VALUES ($1, $2, 'consumed', -1, $3)`, shopID, stockedPart, taken); err != nil {
		t.Fatal(err)
	}
	// Invoiced and owed back, and an invoice nobody has tried to deliver.
	_, inv := mixedInvoice(t, pool)
	if _, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{Method: "bank", PaidOn: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.IssueCredit(ctx, pool, advisor(), inv.ID, workshop.CreditRequest{Lines: []workshop.CreditLine{{Position: 3}}, Reason: "x", Key: "e"}); err != nil {
		t.Fatal(err)
	}
	d := dayOf(t, pool, today())
	e := d.Exceptions
	if len(e.RunningClocks) != 1 || len(e.ReadyNotInvoiced) != 1 || len(e.UnpricedTakeOuts) != 1 || e.RefundTasks != 1 || len(e.Undelivered) == 0 {
		t.Errorf("exceptions: clocks %v ready %v takeouts %v refunds %d undelivered %d",
			e.RunningClocks, e.ReadyNotInvoiced, e.UnpricedTakeOuts, e.RefundTasks, len(e.Undelivered))
	}
	if e.Count() < 5 {
		t.Errorf("count = %d", e.Count())
	}
	r, err := workshop.ReviewDay(ctx, pool, owner(), today(), "Signed with open items")
	if err != nil || r.Open != e.Count() {
		t.Errorf("the sign-off remembers %d open items (%v), want %d", r.Open, err, e.Count())
	}
}

func TestTheDayShowsWhereTheDocumentsWentAndWhichHandoverWasARepeat(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	issued(t, pool)
	if d := dayOf(t, pool, today()); d.Exports.DocsNot != 1 || d.Exports.DocsExported != 0 {
		t.Fatalf("before any handover: %+v", d.Exports)
	}
	from := time.Now().AddDate(0, 0, -1).In(shopZone())
	to := time.Now().AddDate(0, 0, 1).In(shopZone())
	if _, _, err := workshop.ExportAccounting(ctx, pool, advisor(), from, to, false); err != nil {
		t.Fatal(err)
	}
	if d := dayOf(t, pool, today()); d.Exports.DocsExported != 1 || d.Exports.DocsNot != 0 {
		t.Errorf("after the handover: %+v", d.Exports)
	}
	if _, _, err := workshop.ExportAccounting(ctx, pool, advisor(), from, to, true); err != nil {
		t.Fatal(err)
	}
	ex, _ := workshop.AccountingExports(ctx, pool, advisor())
	if len(ex) != 2 {
		t.Fatalf("%d handovers", len(ex))
	}
	// Newest first: the second covers the same days again.
	if ex[0].Number != 2 || !ex[0].Repeat || ex[1].Number != 1 || ex[1].Repeat {
		t.Errorf("handovers: %+v / %+v", ex[0], ex[1])
	}
}
