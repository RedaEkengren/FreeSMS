package workshop

import (
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/money"
)

func ln(kind string, net int64, rate int) invoiceLine {
	return invoiceLine{Kind: kind, QuantityMilli: 1000, UnitPriceMinor: net, NetMinor: net,
		VATRateBasis: rate, VATMinor: money.VAT(net, rate)}
}

func total(lines []invoiceLine, kind string) (net int64) {
	for _, l := range lines {
		if l.Kind == kind {
			net += l.NetMinor
		}
	}
	return net
}

// Förbrukningsmaterial is a share of the labour, not of the parts, at the
// labour's VAT rate; the fee is a line of its own.
func TestConsumablesAreAShareOfTheLabourOnly(t *testing.T) {
	order := []invoiceLine{ln("labour", 134250, 2500), ln("part", 160900, 2500), ln("sublet", 50000, 2500)}
	got := surchargeLines(Surcharges{ConsumablesBasis: 500, InvoiceFeeMinor: 4900}, "en", order, true)
	// 5 per cent of 1 342,50 is 67,125: 67,13.
	if c := total(got, "consumables"); c != 6713 {
		t.Errorf("consumables %d, want 6713: 5%% of the labour and nothing on the parts", c)
	}
	if f := total(got, "fee"); f != 4900 {
		t.Errorf("fee %d, want 4900", f)
	}
	for _, l := range got {
		if l.VATRateBasis != 2500 {
			t.Errorf("%s at %d, want the labour's 25 per cent", l.Kind, l.VATRateBasis)
		}
	}
	if got[0].Description != "Consumables, 5.0% of the labour" {
		t.Errorf("description %q", got[0].Description)
	}
	if sv := surchargeLines(Surcharges{ConsumablesBasis: 500}, "sv", order, true); sv[0].Description != "Förbrukningsmaterial, 5,0 % av arbetet" {
		t.Errorf("in Swedish: %q", sv[0].Description)
	}
}

// A ceiling cuts the total down to exactly the ceiling, each labour rate in
// proportion -- no öre left over or invented.
func TestTheCeilingIsExact(t *testing.T) {
	cap := int64(10000)
	order := []invoiceLine{ln("labour", 333333, 2500), ln("labour", 111111, 1200)}
	got := surchargeLines(Surcharges{ConsumablesBasis: 1000, ConsumablesCapMinor: &cap}, "en", order, true)
	if c := total(got, "consumables"); c != cap {
		t.Errorf("consumables %d across the rates, want exactly the ceiling %d", c, cap)
	}
	if len(got) != 2 {
		t.Errorf("%d lines, want one per labour rate", len(got))
	}
	under := surchargeLines(Surcharges{ConsumablesBasis: 500, ConsumablesCapMinor: &cap}, "en",
		[]invoiceLine{ln("labour", 10000, 2500)}, true)
	if c := total(under, "consumables"); c != 500 {
		t.Errorf("under the ceiling: %d, want the plain 5%%", c)
	}
}

// The fee is charged on an original invoice, never on a credit note; and
// nothing set is nothing charged.
func TestTheFeeIsForOriginalsAndNothingIsNothing(t *testing.T) {
	order := []invoiceLine{ln("labour", 100000, 2500)}
	if got := surchargeLines(Surcharges{InvoiceFeeMinor: 4900}, "en", order, false); total(got, "fee") != 0 {
		t.Error("a fee on a credit note")
	}
	if got := surchargeLines(Surcharges{}, "en", order, true); len(got) != 0 {
		t.Errorf("no settings added %d lines", len(got))
	}
	if got := surchargeLines(Surcharges{ConsumablesBasis: 500}, "en", []invoiceLine{ln("part", 100000, 2500)}, true); len(got) != 0 {
		t.Error("consumables on an order with no labour")
	}
}
