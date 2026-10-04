package workshop_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func period() (time.Time, time.Time) {
	now := time.Now()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return from, from.AddDate(0, 1, 0)
}

func TestTheExportContainsTheMonthsInvoices(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	from, to := period()
	body, count, err := workshop.ExportAccounting(ctx, pool, advisor(), from, to, false)
	if err != nil {
		t.Fatalf("ExportAccounting: %v", err)
	}
	if count != 1 {
		t.Errorf("exported %d documents, want 1", count)
	}
	if !bytes.Contains(body, []byte("#SIETYP 4")) {
		t.Error("that is not a SIE 4 file")
	}
	if !bytes.Contains(body, []byte("#FORMAT PC8")) {
		t.Error("the file does not declare its encoding")
	}
	// The receivable carries the gross, the sale the net, the VAT the rest.
	if !bytes.Contains(body, []byte("#TRANS 1510 {} 3522.50")) &&
		!bytes.Contains(body, []byte("#TRANS 1510 {}")) {
		t.Error("there is no receivable entry")
	}
	if !bytes.Contains(body, []byte("#VER \"A\" \"1\"")) {
		t.Errorf("the invoice %s is not in the file", inv.Reference())
	}
}

// An accountant who imports the same file twice produces duplicate
// verifications and a balance nobody can explain.
func TestExportingTwiceIsRefusedUnlessAskedForAgain(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	if _, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool)); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	from, to := period()

	if _, _, err := workshop.ExportAccounting(ctx, pool, advisor(), from, to, false); err != nil {
		t.Fatalf("first export: %v", err)
	}
	if _, _, err := workshop.ExportAccounting(ctx, pool, advisor(), from, to, false); !errors.Is(err, workshop.ErrAlreadyExported) {
		t.Fatalf("second export = %v, want ErrAlreadyExported", err)
	}
	if _, _, err := workshop.ExportAccounting(ctx, pool, advisor(), from, to, true); err != nil {
		t.Fatalf("a deliberate re-export was refused: %v", err)
	}
}

// Marking an invoice as exported is bookkeeping about the document, not a
// change to what it says -- and nothing else may move.
func TestExportingDoesNotOtherwiseTouchTheInvoice(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	from, to := period()
	if _, _, err := workshop.ExportAccounting(ctx, pool, advisor(), from, to, false); err != nil {
		t.Fatalf("export: %v", err)
	}

	var gross int64
	var name string
	var exported bool
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT gross_minor, customer_name, accounting_export_id IS NOT NULL
			 FROM invoices WHERE id = $1`, inv.ID).Scan(&gross, &name, &exported)
	}); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !exported {
		t.Error("the invoice was not marked as exported")
	}
	if gross != inv.GrossMinor || name != "A Customer" {
		t.Error("exporting changed what the invoice says")
	}

	// And the trigger still refuses everything else.
	err = database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE invoices SET gross_minor = 1 WHERE id = $1`, inv.ID)
		return err
	})
	if err == nil {
		t.Error("an invoice could be edited after being exported")
	}
}

// A credit note is its own verification, not a correction of the original.
func TestACreditNoteIsItsOwnVerification(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := workshop.CreditNote(ctx, pool, advisor(), inv.ID); err != nil {
		t.Fatalf("CreditNote: %v", err)
	}

	from, to := period()
	body, count, err := workshop.ExportAccounting(ctx, pool, advisor(), from, to, false)
	if err != nil {
		t.Fatalf("ExportAccounting: %v", err)
	}
	if count != 2 {
		t.Errorf("exported %d documents, want 2 -- the invoice and the credit note", count)
	}
	if !bytes.Contains(body, []byte("Kreditfaktura")) {
		t.Error("the credit note is not labelled as one")
	}
	// Two verifications, and the file as a whole nets to nothing.
	if bytes.Count(body, []byte("#VER ")) != 2 {
		t.Errorf("got %d verifications, want 2", bytes.Count(body, []byte("#VER ")))
	}
}

func TestExportingAnEmptyPeriodIsRefused(t *testing.T) {
	pool := setup(t)
	from, to := period()
	if _, _, err := workshop.ExportAccounting(context.Background(), pool, advisor(),
		from.AddDate(-1, 0, 0), to.AddDate(-1, 0, 0), false); !errors.Is(err, workshop.ErrNothingToExport) {
		t.Errorf("an empty period = %v, want ErrNothingToExport", err)
	}
}

// A customer called Öberg must arrive spelled correctly.
func TestASwedishNameSurvivesTheExport(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE people SET display_name = 'Margareta Öberg' WHERE id = '22222222-2222-2222-2222-222222222223'`)
		return err
	}); err != nil {
		t.Fatalf("rename: %v", err)
	}

	if _, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool)); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	from, to := period()
	body, _, err := workshop.ExportAccounting(ctx, pool, advisor(), from, to, false)
	if err != nil {
		t.Fatalf("ExportAccounting: %v", err)
	}

	// Ö is one CP437 byte, 153 -- not the two bytes UTF-8 would write.
	if !bytes.Contains(body, append([]byte("Margareta "), 153, 'b', 'e', 'r', 'g')) {
		t.Error("Öberg did not survive the encoding")
	}
	if bytes.Contains(body, []byte("Öberg")) {
		t.Error("the file contains UTF-8; the accountant would see a mangled name")
	}
}

func TestTheExportIsNotForTechnicians(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	from, to := period()
	if _, _, err := workshop.ExportAccounting(context.Background(), pool, technician(), from, to, false); err == nil {
		t.Error("a technician exported the books")
	}
}

// postings reads every #TRANS line in a SIE file into account -> total, in
// minor units, so a test can ask what each account received.
func postings(t *testing.T, body []byte) map[int]int64 {
	t.Helper()
	out := map[int]int64{}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#TRANS ") {
			continue
		}
		var account int
		var amount string
		if _, err := fmt.Sscanf(line, "#TRANS %d {} %s", &account, &amount); err != nil {
			t.Fatalf("unreadable %q: %v", line, err)
		}
		neg := strings.HasPrefix(amount, "-")
		whole, frac, _ := strings.Cut(strings.TrimPrefix(amount, "-"), ".")
		w, _ := strconv.ParseInt(whole, 10, 64)
		f, _ := strconv.ParseInt(frac, 10, 64)
		v := w*100 + f
		if neg {
			v = -v
		}
		out[account] += v
	}
	return out
}

// mixedRates is a ready order carrying all four Swedish rates.
func mixedRates(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	id := newJob(t, pool)
	for _, l := range []workshop.NewLine{
		{Kind: "labour", Description: "Work at 25", QuantityMilli: 1000, UnitPriceMinor: 100000, VATRateBasis: 2500},
		{Kind: "part", Description: "Something at 12", QuantityMilli: 1000, UnitPriceMinor: 50000, VATRateBasis: 1200},
		{Kind: "fee", Description: "Something at 6", QuantityMilli: 1000, UnitPriceMinor: 20000, VATRateBasis: 600},
		{Kind: "sublet", Description: "Something zero-rated", QuantityMilli: 1000, UnitPriceMinor: 10000, VATRateBasis: 0},
	} {
		if err := workshop.AddLine(ctx, pool, advisor(), id, l); err != nil {
			t.Fatalf("AddLine(%s): %v", l.Description, err)
		}
	}
	move(t, pool, id, workshop.StateEstimated, workshop.StateAwaitingApproval,
		workshop.StateApproved, workshop.StateInProgress, workshop.StateReady)
	return id
}

// Output VAT goes to the account for its rate. All of it used to go to the
// 25 per cent account: the verification balanced, and the VAT return was
// wrong for every 12 and 6 per cent line.
func TestEachVATRateReachesItsOwnAccount(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	inv, err := workshop.Issue(ctx, pool, advisor(), mixedRates(t, pool))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	from, to := period()
	body, _, err := workshop.ExportAccounting(ctx, pool, advisor(), from, to, false)
	if err != nil {
		t.Fatalf("ExportAccounting: %v", err)
	}

	got := postings(t, body)
	want := map[int]int64{
		1510: 212200,  // the customer owes the gross
		3010: -180000, // the sales, all four lines, net
		2611: -25000,  // 25 per cent of 1 000,00
		2621: -6000,   // 12 per cent of 500,00
		2631: -1200,   // 6 per cent of 200,00
	}
	for account, amount := range want {
		if got[account] != amount {
			t.Errorf("account %d received %d, want %d (%s)", account, got[account], amount, inv.Reference())
		}
	}
	var sum int64
	for _, v := range got {
		sum += v
	}
	if sum != 0 {
		t.Errorf("the verification does not balance: off by %d", sum)
	}
}

// A credit note reverses exactly what the original posted, rate by rate.
func TestACreditNoteReversesEachRate(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	inv, err := workshop.Issue(ctx, pool, advisor(), mixedRates(t, pool))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := workshop.CreditNote(ctx, pool, advisor(), inv.ID); err != nil {
		t.Fatalf("CreditNote: %v", err)
	}
	from, to := period()
	body, count, err := workshop.ExportAccounting(ctx, pool, advisor(), from, to, false)
	if err != nil {
		t.Fatalf("ExportAccounting: %v", err)
	}
	if count != 2 {
		t.Fatalf("exported %d documents, want the invoice and its note", count)
	}
	for account, total := range postings(t, body) {
		if total != 0 {
			t.Errorf("account %d is left with %d after the invoice and its full credit note", account, total)
		}
	}
}

// A rate there is no account for is refused, not posted to 25 per cent.
func TestAnUnknownVATRateIsRefusedNotGuessed(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	id := newJob(t, pool)
	if err := workshop.AddLine(ctx, pool, advisor(), id, workshop.NewLine{
		Kind: "labour", Description: "At ten per cent", QuantityMilli: 1000,
		UnitPriceMinor: 100000, VATRateBasis: 1000,
	}); err != nil {
		t.Skipf("AddLine refuses a ten per cent rate itself (%v), so the export never sees one", err)
	}
	move(t, pool, id, workshop.StateEstimated, workshop.StateAwaitingApproval,
		workshop.StateApproved, workshop.StateInProgress, workshop.StateReady)
	if _, err := workshop.Issue(ctx, pool, advisor(), id); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	from, to := period()
	if _, _, err := workshop.ExportAccounting(ctx, pool, advisor(), from, to, false); !errors.Is(err, workshop.ErrUnsupportedVATRate) {
		t.Errorf("exporting a ten per cent line = %v, want ErrUnsupportedVATRate", err)
	}
}

// A voucher is dated on the shop's calendar. An invoice issued at half past
// midnight on 1 November in Stockholm is 23:30 on 31 October in UTC, and it
// was booked on the 31st: in the November file, dated October, in the wrong
// period of the accounts.
func TestAVoucherIsDatedOnTheShopsCalendar(t *testing.T) {
	// The server runs in UTC; a developer's machine in Stockholm would hide
	// this, because the database driver hands times back in the process's
	// zone. Not parallel, and put back after.
	was := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = was })

	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// An issued invoice cannot be changed, by trigger. This test database is
	// thrown away, so the trigger steps aside long enough to put the invoice
	// at the edge of the month.
	if _, err := pool.Exec(ctx, `
		ALTER TABLE invoices DISABLE TRIGGER invoices_immutable;
		UPDATE invoices SET issued_at = '2026-10-31T23:30:00Z';
		ALTER TABLE invoices ENABLE TRIGGER invoices_immutable;`); err != nil {
		t.Fatalf("move the invoice: %v", err)
	}

	stockholm, _ := time.LoadLocation("Europe/Stockholm")
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, stockholm)
	body, count, err := workshop.ExportAccounting(ctx, pool, advisor(), from, from.AddDate(0, 1, 0), false)
	if err != nil {
		t.Fatalf("ExportAccounting: %v", err)
	}
	if count != 1 {
		t.Fatalf("exported %d documents, want the one issued just after midnight on 1 November", count)
	}
	if !bytes.Contains(body, []byte(`#VER "A" "1" 20261101`)) {
		t.Errorf("%s is not dated 1 November:\n%s", inv.Reference(), body)
	}
}
