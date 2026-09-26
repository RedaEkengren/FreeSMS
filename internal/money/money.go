// Package money computes what a job costs.
//
// Every amount is an int64 of minor units -- öre, cents, pence -- and no value
// in this package is ever a float. That is not fastidiousness: 0.1 + 0.2 is not
// 0.3 in binary floating point, and an invoice that disagrees with itself by a
// krona is the kind of thing a customer notices and a bookkeeper cannot
// reconcile.
//
// Quantities are thousandths, because oil is sold by the litre and labour by
// the quarter hour. 4.5 litres is 4500.
package money

import "strings"

// Rounding
//
// Two decisions, both of which change totals, so both are written down rather
// than left to whoever wrote the first function.
//
// **Half away from zero.** 2.5 rounds to 3 and -2.5 to -3. This is what a
// person does by hand and what Swedish commercial practice expects. Go's
// math.Round does the same; banker's rounding (half to even) is better for
// long statistical sums and worse here, because it surprises the person
// checking the arithmetic.
//
// **Per line, then sum.** Each line is rounded to a whole minor unit, and the
// total is the sum of those. The alternative -- keeping full precision and
// rounding once at the bottom -- produces a mathematically nicer number and an
// invoice whose printed lines do not add up to its printed total. The lines
// are what the customer checks.
//
// The difference is real. Three lines of 1.5 hours at 333.33 each:
//
//	per line:  round(49999.5) = 50000, thrice  -> 150000
//	per total: round(149998.5)              -> 149999
//
// One öre, on an invoice where three lines of 500.00 must add to 1500.00. We
// take the first.

// divRound divides and rounds half away from zero, without floating point.
//
// The denominator is always positive here; the numerator may not be.
func divRound(numerator, denominator int64) int64 {
	if denominator == 0 {
		return 0
	}
	if numerator >= 0 {
		return (numerator + denominator/2) / denominator
	}
	return -((-numerator + denominator/2) / denominator)
}

// LineNet is the amount charged for a line before VAT.
//
// quantityMilli is thousandths of a unit; unitPriceMinor is the price of one
// whole unit in minor units.
func LineNet(quantityMilli, unitPriceMinor int64) int64 {
	return divRound(quantityMilli*unitPriceMinor, 1000)
}

// VAT is the tax on a net amount, at a rate in basis points (2500 = 25%).
func VAT(netMinor int64, rateBasisPoints int) int64 {
	return divRound(netMinor*int64(rateBasisPoints), 10000)
}

// Line is one priced line, as the totals care about it.
type Line struct {
	QuantityMilli     int64
	UnitPriceMinor    int64
	VATRateBasis      int
	ChargedToCustomer bool
}

// VATBand is what is owed at one rate.
type VATBand struct {
	RateBasisPoints int
	NetMinor        int64
	VATMinor        int64
}

// Totals is the bottom of an invoice.
type Totals struct {
	NetMinor   int64
	VATMinor   int64
	GrossMinor int64

	// One band per rate in use, ordered by rate. An invoice that mixes rates
	// has to show the split; one that does not still has a band, because the
	// code that renders it should not have two shapes to handle.
	Bands []VATBand
}

// Compute totals a set of lines.
//
// Lines the customer is not paying for -- a supplier warranty, a goodwill
// replacement -- are excluded from the money and still belong on the order.
// They are not zero-rated; they are not charged at all, which is a different
// thing and matters if anyone ever reconciles VAT against turnover.
func Compute(lines []Line) Totals {
	var t Totals
	byRate := map[int]*VATBand{}
	var rates []int

	for _, l := range lines {
		if !l.ChargedToCustomer {
			continue
		}
		net := LineNet(l.QuantityMilli, l.UnitPriceMinor)
		vat := VAT(net, l.VATRateBasis)

		band, seen := byRate[l.VATRateBasis]
		if !seen {
			band = &VATBand{RateBasisPoints: l.VATRateBasis}
			byRate[l.VATRateBasis] = band
			rates = append(rates, l.VATRateBasis)
		}
		band.NetMinor += net
		band.VATMinor += vat

		t.NetMinor += net
		t.VATMinor += vat
	}
	t.GrossMinor = t.NetMinor + t.VATMinor

	// Ascending by rate, so the same invoice always prints in the same order.
	for i := 1; i < len(rates); i++ {
		for j := i; j > 0 && rates[j] < rates[j-1]; j-- {
			rates[j], rates[j-1] = rates[j-1], rates[j]
		}
	}
	for _, r := range rates {
		t.Bands = append(t.Bands, *byRate[r])
	}
	return t
}

// CashRound rounds a gross amount to the nearest whole currency unit, for cash
// taken over a counter.
//
// Sweden has no coins below one krona, so a cash sale is rounded at the point
// of payment. **This does not change what was invoiced.** The invoice says
// 1295.50; the drawer takes 1296 and the difference is a rounding line in the
// books, not a correction to the document. Confusing the two is how an
// invoice and a payment stop matching for a reason nobody can find later.
func CashRound(grossMinor int64) int64 {
	return divRound(grossMinor, 100) * 100
}

// Format renders minor units as a plain decimal string.
//
// Locale-aware formatting -- a comma in Swedish, a space between thousands --
// belongs with the i18n work. This at least never lies about the amount.
func Format(minor int64) string {
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	return sign + itoa(minor/100) + "." + pad2(minor%100)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func pad2(n int64) string {
	if n < 10 {
		return "0" + itoa(n)
	}
	return itoa(n)
}

// Display renders an amount for somebody to read, in a language and a
// currency.
//
// Separate from Format, which stays the canonical machine form: it is what
// goes into a form field that will be posted back and parsed, and the SIE
// export has its own representation that must not follow a locale. Grouped
// digits in a hidden input would come back as a parse error, so the two have
// to be different functions and the call site has to choose.
//
// The grouping character is a non-breaking space in the languages that group
// with a space. An ordinary space lets a browser break "1 234 567,89" across
// two lines, and half a total on each line is worse than no grouping at all.
type Display struct {
	// Group separates thousands, Point is the decimal mark.
	Group, Point string

	// Currency as the shop writes it, and whether it goes before the number.
	// Swedish writes "1 295,50 kr"; English writes "SEK 1,295.50".
	Currency string
	Prefix   bool
}

// DisplayFor returns how a language writes money in a currency.
//
// Currency is a property of the shop and language is a property of the reader,
// so they are two arguments. A Swedish workshop invoicing in euro writes
// "1 295,50 €", and an English-speaking reader of that same shop's screens
// sees the same currency, not a converted one.
func DisplayFor(locale, currency string) Display {
	// Empty means the shop is not resolved yet, which happens only before
	// setup, when no amount is on screen anyway. Rendering a bare number is
	// honest; guessing a currency is not.
	currency = strings.TrimSpace(currency)
	switch locale {
	case "sv":
		symbol := currency
		if currency == "SEK" {
			symbol = "kr"
		}
		return Display{Group: " ", Point: ",", Currency: symbol}
	default:
		return Display{Group: ",", Point: ".", Currency: currency, Prefix: true}
	}
}

// Amount renders minor units.
func (d Display) Amount(minor int64) string {
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	// The sign goes in front of the whole thing, currency included. A credit
	// note reading "SEK -1 295,50" invites the eye to read the minus as part
	// of the number and the currency as part of the label above it.
	number := sign + group(itoa(minor/100), d.Group) + d.Point + pad2(minor%100)
	if d.Currency == "" {
		return number
	}
	if d.Prefix {
		return d.Currency + " " + number
	}
	return number + " " + d.Currency
}

// group inserts sep every three digits from the right.
func group(digits, sep string) string {
	if len(digits) <= 3 || sep == "" {
		return digits
	}
	var b strings.Builder
	lead := len(digits) % 3
	if lead == 0 {
		lead = 3
	}
	b.WriteString(digits[:lead])
	for i := lead; i < len(digits); i += 3 {
		b.WriteString(sep)
		b.WriteString(digits[i : i+3])
	}
	return b.String()
}
