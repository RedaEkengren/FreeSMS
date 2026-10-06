package workshop_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
)

// sieReading is what jsiSIE -- internal/sie/testdata/siecheck -- understood.
type sieReading struct {
	Errors   []string          `json:"errors"`
	Company  string            `json:"company"`
	Accounts map[string]string `json:"accounts"`
	Vouchers []struct {
		Series, Number, Date, Text string
		Rows                       []struct{ Account, Amount string }
	} `json:"vouchers"`
}

// readSIE hands a file to a reader this project did not write.
//
// FREESMS_SIE_READER is the command, for example
// "dotnet internal/sie/testdata/siecheck/bin/siecheck.dll". Skipped when it is
// not set, except where FREESMS_SIE_READER_REQUIRED says it must run: CI sets
// both, so the check cannot quietly stop running there.
func readSIE(t *testing.T, body []byte) sieReading {
	t.Helper()
	reader := os.Getenv("FREESMS_SIE_READER")
	if reader == "" {
		if os.Getenv("FREESMS_SIE_READER_REQUIRED") != "" {
			t.Fatal("FREESMS_SIE_READER is not set, and this run requires the independent SIE reader")
		}
		t.Skip("FREESMS_SIE_READER not set")
	}
	path := filepath.Join(t.TempDir(), "export.se")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	parts := strings.Fields(reader)
	out, err := exec.Command(parts[0], append(parts[1:], path)...).Output()
	if err != nil {
		t.Fatalf("the SIE reader failed: %v\n%s", err, out)
	}
	var r sieReading
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("the SIE reader said %s: %v", out, err)
	}
	return r
}

// An accounting package reads our SIE file as we meant it.
//
// The file is written by this project and was tested by this project, which
// proves it says what we think and not that anybody else reads it that way --
// the same gap that let every barcode label be unscannable while its own tests
// passed. Here jsiSIE, a parser written to read what Swedish accounting
// software exports, reads a real export: an invoice with three VAT rates,
// fractional quantities that round, a customer named in Swedish with quotes
// and a backslash and a character the code page does not have, and its
// credit note.
func TestAnIndependentReaderReadsTheExportAsMeant(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE people SET display_name = $1 WHERE id = '22222222-2222-2222-2222-222222222223'`,
			"Åsa\tÖberg \"Bilen\" \\ Däck €")
		return err
	}); err != nil {
		t.Fatalf("name the customer: %v", err)
	}

	job := newJob(t, pool)
	for _, l := range []workshop.NewLine{
		// 1,5 h at 895,00: 1 342,50.
		{Kind: "labour", Description: "Byte av bromsskivor", QuantityMilli: 1500, UnitPriceMinor: 89500, VATRateBasis: 2500},
		// 0,7 l at 129,90 is 90,93: rounds.
		{Kind: "part", Description: "Motorolja 5W-30", QuantityMilli: 700, UnitPriceMinor: 12990, VATRateBasis: 2500},
		// 3 at 166,67 at 12 per cent.
		{Kind: "part", Description: "Kaffe till kunden", QuantityMilli: 3000, UnitPriceMinor: 16667, VATRateBasis: 1200},
		// 200,00 at 6 per cent.
		{Kind: "part", Description: "Tidning", QuantityMilli: 1000, UnitPriceMinor: 20000, VATRateBasis: 600},
	} {
		l.CostBearer = "customer"
		if err := workshop.AddLine(ctx, pool, advisor(), job, l); err != nil {
			t.Fatalf("AddLine: %v", err)
		}
	}
	move(t, pool, job, workshop.StateEstimated, workshop.StateAwaitingApproval,
		workshop.StateApproved, workshop.StateInProgress, workshop.StateReady)
	inv, err := workshop.Issue(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := workshop.CreditNote(ctx, pool, advisor(), inv.ID); err != nil {
		t.Fatalf("CreditNote: %v", err)
	}
	// And money that came in for it anyway, in cash, before the credit:
	// a voucher in series B, rounded at the counter.
	paid, err := workshop.RecordPayment(ctx, pool, advisor(), inv.ID, workshop.NewPayment{
		AmountMinor: func() *int64 { v := int64(256300); return &v }(), Method: "cash", PaidOn: time.Now()})
	if err != nil {
		t.Fatalf("RecordPayment: %v", err)
	}

	stockholm, _ := time.LoadLocation("Europe/Stockholm")
	today := time.Now().In(stockholm)
	from := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, stockholm)
	body, count, err := workshop.ExportAccounting(ctx, pool, advisor(), from, from.AddDate(0, 1, 0), false)
	if err != nil || count != 3 {
		t.Fatalf("ExportAccounting: %d documents, %v", count, err)
	}

	r := readSIE(t, body)
	if len(r.Errors) > 0 {
		t.Errorf("the reader reported problems with the file:\n  %s", strings.Join(r.Errors, "\n  "))
	}
	if r.Company != "Verkstaden" {
		t.Errorf("company read as %q", r.Company)
	}
	if len(r.Vouchers) != 3 {
		t.Fatalf("read %d vouchers, want the invoice, its credit note and the payment", len(r.Vouchers))
	}
	// The payment, read by jsiSIE as its own series, into the till and out
	// of the receivable.
	pv := r.Vouchers[2]
	payRows := map[string]string{}
	for _, row := range pv.Rows {
		payRows[row.Account] = row.Amount
	}
	if pv.Series != "B" || payRows["1910"] != "2563.00" || payRows["1510"] != "-2563.00" {
		t.Errorf("payment voucher read as %s %s %v, want B into 1910 and out of 1510", pv.Series, pv.Number, payRows)
	}
	_ = paid
	r.Vouchers = r.Vouchers[:2]

	// Net: 1 342,50 + 90,93 + 500,01 + 200,00 = 2 133,44.
	// VAT:   25% of 1 433,43 = 358,36; 12% of 500,01 = 60,00; 6% of 200,00 = 12,00.
	want := map[string]string{
		"1510": "2563.80", "3010": "-2133.44",
		"2611": "-358.36", "2621": "-60.00", "2631": "-12.00",
	}
	// What jsiSIE reads back: the tab a space, the backslash a slash, the €
	// a question mark because CP437 has no place for it -- and no quotation
	// marks. The file writes them as SIE 4B 5.7 says, \"Bilen\"; jsiSIE does
	// not implement that rule and drops them. internal/sie checks the exact
	// bytes; this checks that nothing else is lost on the way.
	name := "Åsa Öberg Bilen / Däck ?"
	for i, v := range r.Vouchers {
		sign := ""
		if i == 1 {
			sign = "-" // the credit note reverses every row
		}
		if v.Date != today.Format("2006-01-02") {
			t.Errorf("voucher %s %s dated %s, want %s", v.Series, v.Number, v.Date, today.Format("2006-01-02"))
		}
		if !strings.HasSuffix(v.Text, name) {
			t.Errorf("voucher text read as %q, want it to end %q", v.Text, name)
		}
		got := map[string]string{}
		for _, row := range v.Rows {
			got[row.Account] = row.Amount
		}
		for account, amount := range want {
			expect := amount
			if sign == "-" {
				expect = strings.TrimPrefix("-"+amount, "--")
			}
			if got[account] != expect {
				t.Errorf("voucher %d: account %s read as %q, want %s", i+1, account, got[account], expect)
			}
		}
		if len(v.Rows) != len(want) {
			t.Errorf("voucher %d has %d rows, want %d", i+1, len(v.Rows), len(want))
		}
	}
	for _, account := range []string{"1510", "2611", "2621", "2631", "3010"} {
		if r.Accounts[account] == "" {
			t.Errorf("account %s has no name in the file", account)
		}
	}
}
