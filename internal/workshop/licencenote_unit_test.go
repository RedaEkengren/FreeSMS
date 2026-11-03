package workshop

import "testing"

// The rule for a licence check's note, both ways: what is refused because
// it may be an identifier -- a number, a licence number, a date, written in
// figures or with the month's name -- and what a desk writes that is not
// one: a clock time, a small range, a year (review 3 #5, #6).
func TestWhatALicenceNoteMayHold(t *testing.T) {
	for _, c := range []struct {
		note    string
		refused bool
	}{
		// Numbers however split.
		{"Kort 12345678", true},
		{"pnr 850101-1234", true},
		{"85 01 01  1234", true},
		{"8 5 0 1 0 1", true},
		{"nr ١٢٣٤٥٦٧٨", true},
		{"no. B072RRE2I55", true},
		// Dates in figures.
		{"DOB 01.01.1985", true},
		{"född 1/1/85", true},
		{"12.3.1990", true},
		{"1985-01-01", true},
		{"född 12/3", true},
		{"född 12.3", true},
		// Dates with the month's name, Swedish and English.
		{"född 12 mars 1990", true},
		{"född 3 maj", true},
		{"Född den 1:a januari", true},
		{"f. 12 dec. 85", true},
		{"DOB March 12, 1990", true},
		{"born 12th of March", true},
		{"born 3 Oct 1985", true},
		{"Sept 9", true},
		// A day written with a leading zero beside a month's name.
		{"född 05 mars 1990", true},
		{"född 05 mars", true},
		{"01 jan 1985", true},
		{"DOB March 05", true},
		{"DOB 3/12/1990", true},
		{"född 5/3", true},
		// Figures glued to the word before them.
		{"född1/1", true},
		{"f12.3", true},
		{"DOB01.01.1985", true},
		// Not identifiers.
		{"", false},
		{"Tyskt körkort, giltigt i Sverige", false},
		{"Klass C1E på kortet", false},
		{"Förnyat 2031", false},
		{"Förnyat i mars 2031", false},
		{"Kontrollerat kl 14.30", false},
		{"Kontrollerat kl. 9.05", false},
		{"Kontrollerat klockan 10.15", false},
		{"checked at 14:30", false},
		{"3-4 dagar", false},
		{"körkort 2-3 år gammalt", false},
		{"May drive abroad", false},
		{"Kortet var lite slitet", false},
		// A fraction, and a month with its year: not a day and a month.
		{"tank 3/4", false},
		{"tank 1/2", false},
		{"giltigt till 12/2031", false},
		{"giltigt till 12/31", false},
		{"utfärdat 2019-04", false},
		{"tank3/4", false},
	} {
		if got := holdsIdentifier(c.note); got != c.refused {
			t.Errorf("%q: refused %v, want %v", c.note, got, c.refused)
		}
	}
}
