package workshop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Issued with the shop's surcharges as lines of its own; warranty labour
// attracts nothing; a later change of setting leaves the invoice as it was;
// the credit note reverses every line and charges no fee of its own; and the
// export puts each on its own account.
func TestSurchargesAreChargedFrozenCreditedAndBooked(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	cap := int64(50000)
	if err := workshop.SaveSurcharges(ctx, pool, owner(), workshop.Surcharges{
		ConsumablesBasis: 500, ConsumablesCapMinor: &cap, InvoiceFeeMinor: 4900}); err != nil {
		t.Fatalf("SaveSurcharges: %v", err)
	}

	job := newJob(t, pool)
	for _, l := range []workshop.NewLine{
		{Kind: "labour", Description: "Kamrem", QuantityMilli: 1500, UnitPriceMinor: 89500, VATRateBasis: 2500, CostBearer: "customer"},
		{Kind: "labour", Description: "Garantiarbete", QuantityMilli: 2000, UnitPriceMinor: 0, VATRateBasis: 2500, CostBearer: "supplier"},
		{Kind: "part", Description: "Remsats", QuantityMilli: 1000, UnitPriceMinor: 160900, VATRateBasis: 2500, CostBearer: "customer"},
	} {
		if err := workshop.AddLine(ctx, pool, advisor(), job, l); err != nil {
			t.Fatalf("AddLine: %v", err)
		}
	}
	preview, err := workshop.PreviewSurcharges(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatalf("PreviewSurcharges: %v", err)
	}
	move(t, pool, job, workshop.StateEstimated, workshop.StateAwaitingApproval,
		workshop.StateApproved, workshop.StateInProgress, workshop.StateReady)
	inv, err := workshop.Issue(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	byKind := func(d workshop.Document) map[string]int64 {
		out := map[string]int64{}
		for _, l := range d.Lines {
			out[l.Kind] += l.NetMinor
		}
		return out
	}
	doc, _ := workshop.DocumentByID(ctx, pool, advisor(), inv.ID)
	got := byKind(doc)
	// 5 per cent of 1 342,50 paid labour: 67,13. The warranty hours are not
	// the customer's and attract nothing.
	if got["consumables"] != 6713 || got["fee"] != 4900 {
		t.Errorf("issued with consumables %d and fee %d, want 6713 and 4900", got["consumables"], got["fee"])
	}
	if len(preview) != 2 || preview[0].NetMinor != 6713 || preview[1].NetMinor != 4900 {
		t.Errorf("the job page's preview %+v does not match what was issued", preview)
	}
	// 1 342,50 + 1 609,00 + 67,13 + 49,00 = 3 067,63 net.
	if doc.NetMinor != 306763 {
		t.Errorf("net %d, want 306763", doc.NetMinor)
	}

	// This year's settings do not reach last year's invoice.
	workshop.SaveSurcharges(ctx, pool, owner(), workshop.Surcharges{ConsumablesBasis: 1000, InvoiceFeeMinor: 9900})
	again, _ := workshop.DocumentByID(ctx, pool, advisor(), inv.ID)
	if a := byKind(again); a["consumables"] != 6713 || a["fee"] != 4900 || again.NetMinor != doc.NetMinor {
		t.Errorf("a change of setting reached an issued invoice: %v", a)
	}

	credit, err := workshop.CreditNote(ctx, pool, advisor(), inv.ID)
	if err != nil {
		t.Fatalf("CreditNote: %v", err)
	}
	cdoc, _ := workshop.DocumentByID(ctx, pool, advisor(), credit.ID)
	if c := byKind(cdoc); c["fee"] != -4900 || c["consumables"] != -6713 || cdoc.NetMinor != -doc.NetMinor {
		t.Errorf("the credit note %v, net %d: want every line reversed and no fee of its own", c, cdoc.NetMinor)
	}

	from, to := period()
	body, _, err := workshop.ExportAccounting(ctx, pool, advisor(), from, to, false)
	if err != nil {
		t.Fatalf("ExportAccounting: %v", err)
	}
	posted := postings(t, body)
	if posted[3590] != 0 || posted[3540] != 0 || posted[3010] != 0 {
		t.Errorf("invoice and credit together should net to nothing on each account: %v", posted)
	}
	// On their own accounts in the invoice's own voucher.
	for _, want := range []string{"#TRANS 3590 {} -67.13", "#TRANS 3540 {} -49.00", "#TRANS 3010 {} -2951.50"} {
		if !containsLine(body, want) {
			t.Errorf("the export has no %q", want)
		}
	}
}

// The settings are the owner's to change; the front desk reads them.
func TestOnlyTheOwnerSetsTheSurcharges(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	for _, s := range []access.Scope{advisor(), technician(), partsDeskScope()} {
		if err := workshop.SaveSurcharges(ctx, pool, s, workshop.Surcharges{ConsumablesBasis: 500}); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("SaveSurcharges as %s: %v", s.Role, err)
		}
	}
	if err := workshop.SaveSurcharges(ctx, pool, owner(), workshop.Surcharges{ConsumablesBasis: 3000}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("30 per cent of the labour: %v, want refused", err)
	}
}

func containsLine(body []byte, line string) bool {
	for _, l := range splitLines(string(body)) {
		if l == line {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, trimCR(s[start:i]))
			start = i + 1
		}
	}
	return append(out, trimCR(s[start:]))
}

func trimCR(s string) string {
	if len(s) > 0 && s[len(s)-1] == '\r' {
		return s[:len(s)-1]
	}
	return s
}
