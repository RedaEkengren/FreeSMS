package workshop

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// What is owed on an invoice is written once, in owedSQL, and read from
// there: a second copy of the sum is a rule that can be changed in one
// place and not the other, and the payment code, the receivables list and
// a technician's "paid" then disagree about the same invoice.
func TestWhatIsOwedIsWrittenOnce(t *testing.T) {
	// An invoice's total with its credit notes added and its payments
	// taken off, however the aliases are named.
	copied := regexp.MustCompile(`(?s)gross_minor\)?\s+FROM invoices \w+ WHERE \w+\.credit_of_id = \w+\.id\), 0\)\s*-\s*coalesce\(\(SELECT sum\(\w*\.?amount_minor \+ \w*\.?rounding_minor\)`)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(copied.FindAll(src, -1)); n > 0 {
			t.Errorf("%s works out what is owed %d times itself; use owedSQL", name, n)
		}
	}
}
