package money

import (
	"strconv"
	"strings"
	"testing"
)

func TestLineNet(t *testing.T) {
	for _, c := range []struct {
		name          string
		quantityMilli int64
		unitMinor     int64
		want          int64
	}{
		{"one of something", 1000, 129500, 129500},
		{"four and a half litres", 4500, 12900, 58050},
		{"a quarter hour", 250, 89500, 22375},
		{"nothing", 0, 129500, 0},
		{"free", 1000, 0, 0},
		// 1.5 x 333.33 is 499.995, which is half a minor unit. Half away from
		// zero takes it up.
		{"exactly half an öre rounds up", 1500, 33333, 50000},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := LineNet(c.quantityMilli, c.unitMinor); got != c.want {
				t.Errorf("LineNet(%d, %d) = %d, want %d", c.quantityMilli, c.unitMinor, got, c.want)
			}
		})
	}
}

func TestVAT(t *testing.T) {
	for _, c := range []struct {
		net  int64
		rate int
		want int64
	}{
		{100000, 2500, 25000}, // 1000.00 at 25%
		{100000, 1200, 12000}, // 12%, food and some transport
		{100000, 600, 6000},   // 6%
		{100000, 0, 0},        // exempt
		// 3.00 at 25% is 0.75 exactly.
		{300, 2500, 75},
		// 0.01 at 25% is 0.0025, which rounds to nothing.
		{1, 2500, 0},
		// 0.02 at 25% is 0.005: half, so up.
		{2, 2500, 1},
	} {
		if got := VAT(c.net, c.rate); got != c.want {
			t.Errorf("VAT(%d, %d) = %d, want %d", c.net, c.rate, got, c.want)
		}
	}
}

// The case the rounding rule is written down for. Rounding per line and
// rounding the sum give different answers, and only one of them produces an
// invoice whose printed lines add up to its printed total.
func TestPerLineAndPerTotalRoundingDisagree(t *testing.T) {
	lines := make([]Line, 3)
	for i := range lines {
		lines[i] = Line{QuantityMilli: 1500, UnitPriceMinor: 33333, VATRateBasis: 0, ChargedToCustomer: true}
	}
	got := Compute(lines)

	// Per line: round(499.995) = 500.00, three times.
	if got.NetMinor != 150000 {
		t.Errorf("net = %d, want 150000 -- three lines of 500.00", got.NetMinor)
	}

	// What rounding once at the bottom would have given, for the record.
	perTotal := divRound(3*1500*33333, 1000)
	if perTotal != 149999 {
		t.Fatalf("the fixture no longer demonstrates the difference: per-total = %d", perTotal)
	}
	if got.NetMinor == perTotal {
		t.Error("per-line and per-total agree here, so this test no longer guards anything")
	}
}

// Lines somebody else is paying for belong on the order and not in the money.
func TestWarrantyLinesAreNotCharged(t *testing.T) {
	got := Compute([]Line{
		{QuantityMilli: 1000, UnitPriceMinor: 100000, VATRateBasis: 2500, ChargedToCustomer: true},
		{QuantityMilli: 1000, UnitPriceMinor: 0, VATRateBasis: 2500, ChargedToCustomer: false},
	})
	if got.NetMinor != 100000 || got.VATMinor != 25000 || got.GrossMinor != 125000 {
		t.Errorf("totals = %+v, want 1000.00 net and 250.00 VAT", got)
	}
	if len(got.Bands) != 1 {
		t.Errorf("got %d VAT bands, want 1 -- an uncharged line must not create one", len(got.Bands))
	}
}

// An invoice that mixes rates has to show the split.
func TestMixedRatesProduceBandsInOrder(t *testing.T) {
	got := Compute([]Line{
		{QuantityMilli: 1000, UnitPriceMinor: 100000, VATRateBasis: 2500, ChargedToCustomer: true},
		{QuantityMilli: 1000, UnitPriceMinor: 50000, VATRateBasis: 600, ChargedToCustomer: true},
		{QuantityMilli: 1000, UnitPriceMinor: 20000, VATRateBasis: 2500, ChargedToCustomer: true},
	})
	if len(got.Bands) != 2 {
		t.Fatalf("got %d bands, want 2", len(got.Bands))
	}
	if got.Bands[0].RateBasisPoints != 600 || got.Bands[1].RateBasisPoints != 2500 {
		t.Errorf("bands are %d%% then %d%%, want ascending",
			got.Bands[0].RateBasisPoints/100, got.Bands[1].RateBasisPoints/100)
	}
	if got.Bands[1].NetMinor != 120000 {
		t.Errorf("the 25%% band is %d, want 120000 -- both lines at that rate", got.Bands[1].NetMinor)
	}
	if got.GrossMinor != got.NetMinor+got.VATMinor {
		t.Error("gross is not net plus VAT")
	}
}

// Rounding cash does not change what was invoiced.
func TestCashRound(t *testing.T) {
	for _, c := range []struct{ gross, want int64 }{
		{129550, 129600}, // 1295.50 -> 1296
		{129549, 129500}, // 1295.49 -> 1295
		{129500, 129500}, // already whole
		{50, 100},        // 0.50 -> 1.00
		{49, 0},          // 0.49 -> 0
	} {
		if got := CashRound(c.gross); got != c.want {
			t.Errorf("CashRound(%d) = %d, want %d", c.gross, got, c.want)
		}
	}
}

func TestFormat(t *testing.T) {
	for _, c := range []struct {
		minor int64
		want  string
	}{
		{0, "0.00"},
		{5, "0.05"},
		{50, "0.50"},
		{129550, "1295.50"},
		{-129550, "-1295.50"},
		{100, "1.00"},
	} {
		if got := Format(c.minor); got != c.want {
			t.Errorf("Format(%d) = %q, want %q", c.minor, got, c.want)
		}
	}
}

// A credit note has to reverse an invoice exactly, including the rounding that
// was applied to it. Computing the negative of each line is what makes that
// true; recomputing from a negative quantity would round the other way.
func TestNegationReversesExactly(t *testing.T) {
	lines := []Line{
		{QuantityMilli: 1500, UnitPriceMinor: 33333, VATRateBasis: 2500, ChargedToCustomer: true},
		{QuantityMilli: 4500, UnitPriceMinor: 12900, VATRateBasis: 2500, ChargedToCustomer: true},
	}
	original := Compute(lines)

	credit := make([]Line, len(lines))
	for i, l := range lines {
		l.QuantityMilli = -l.QuantityMilli
		credit[i] = l
	}
	reversed := Compute(credit)

	if original.GrossMinor+reversed.GrossMinor != 0 {
		t.Errorf("invoice %d and credit note %d do not cancel; %d left over",
			original.GrossMinor, reversed.GrossMinor, original.GrossMinor+reversed.GrossMinor)
	}
}

func TestDivRoundGoesAwayFromZero(t *testing.T) {
	for _, c := range []struct{ n, d, want int64 }{
		{5, 2, 3},
		{-5, 2, -3},
		{3, 2, 2},
		{-3, 2, -2},
		{1, 2, 1},
		{-1, 2, -1},
		{0, 2, 0},
		{7, 0, 0}, // a zero denominator must not panic
	} {
		if got := divRound(c.n, c.d); got != c.want {
			t.Errorf("divRound(%d, %d) = %d, want %d", c.n, c.d, got, c.want)
		}
	}
}

func TestSwedishWritesMoneyTheSwedishWay(t *testing.T) {
	d := DisplayFor("sv", "SEK")
	for _, c := range []struct {
		minor int64
		want  string
	}{
		{0, "0,00 kr"},
		{50, "0,50 kr"},
		{129550, "1 295,50 kr"},
		{123456789, "1 234 567,89 kr"},
		{100000000, "1 000 000,00 kr"},
		// The sign goes in front of the whole thing. A credit note reading
		// "1 295,50 -kr" is nobody's idea of an amount.
		{-129550, "-1 295,50 kr"},
	} {
		if got := d.Amount(c.minor); got != c.want {
			t.Errorf("Amount(%d) = %q, want %q", c.minor, got, c.want)
		}
	}
}

func TestEnglishWritesMoneyTheOtherWay(t *testing.T) {
	d := DisplayFor("en", "SEK")
	if got, want := d.Amount(123456789), "SEK 1,234,567.89"; got != want {
		t.Errorf("Amount = %q, want %q", got, want)
	}
}

// A shop is not required to invoice in the currency of the language it writes
// in, and nothing here converts anything.
func TestTheCurrencyIsTheShopsAndNotTheLanguages(t *testing.T) {
	if got, want := DisplayFor("sv", "EUR").Amount(129550), "1 295,50 EUR"; got != want {
		t.Errorf("Amount = %q, want %q", got, want)
	}
}

// Before setup there is no shop and so no currency. A bare number is honest;
// inventing kronor is not.
func TestAnUnknownCurrencyIsNotGuessed(t *testing.T) {
	if got, want := DisplayFor("sv", "").Amount(129550), "1 295,50"; got != want {
		t.Errorf("Amount = %q, want %q", got, want)
	}
}

// The grouping character must not let a browser break a total across two
// lines. Half a figure on each line is worse than no grouping at all.
func TestThousandsAreSeparatedByANonBreakingSpace(t *testing.T) {
	got := DisplayFor("sv", "SEK").Amount(123456789)
	if strings.Contains(got, " ") {
		t.Errorf("Amount = %q contains an ordinary space, which can wrap", got)
	}
}

// Format is the machine form and must stay parseable: it goes into form fields
// that are posted back, and into places that have their own representation.
func TestFormatStaysAPlainDecimal(t *testing.T) {
	for _, minor := range []int64{0, 50, 129550, 123456789, -129550} {
		got := Format(minor)
		for _, bad := range []string{" ", ",", "kr", "SEK"} {
			if strings.Contains(got, bad) {
				t.Errorf("Format(%d) = %q contains %q; a form value would not parse back", minor, got, bad)
			}
		}
		if _, err := strconv.ParseFloat(got, 64); err != nil {
			t.Errorf("Format(%d) = %q does not parse: %v", minor, got, err)
		}
	}
}
