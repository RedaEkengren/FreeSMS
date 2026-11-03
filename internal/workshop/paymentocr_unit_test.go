package workshop

import (
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/payref"
)

// Two invoices with the same number in two series -- a rebill numbered in
// its original's series -- are issued with two references, so neither
// breaks the one-invoice-per-reference index; the default series' are
// what they always were.
func TestTheSameNumberInTwoSeriesIsTwoReferences(t *testing.T) {
	a, err := paymentOCR(DefaultSeries, 57)
	if want, _ := payref.OCR(57); err != nil || a != want {
		t.Errorf("A-57: %q, %v; want %q as before", a, err, want)
	}
	b, err := paymentOCR("B", 57)
	if err != nil || b == a {
		t.Errorf("B-57: %q, %v; A-57 is %q", b, err, a)
	}
}

// A series no reference can be made for -- a lower-case letter, a hyphen,
// a name too long to fit in 25 digits -- is issued without one rather than
// not at all: a correction must not be refused because its original's
// series has no reference.
func TestASeriesWithNoReferenceIssuesWithoutOne(t *testing.T) {
	for _, series := range []string{"b", "K-2", "Å", "ABCDEFGHIJKL"} {
		ocr, err := paymentOCR(series, 57)
		if err != nil || ocr != "" {
			t.Errorf("%s-57: %q, %v; want no reference and no error", series, ocr, err)
		}
	}
}
