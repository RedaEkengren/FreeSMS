package server

import "testing"

// Prices are typed in whole currency units and stored as minor units. Going
// through a float to get there introduces exactly the error the integer
// schema exists to avoid: small enough to survive testing, large enough to
// make an invoice disagree with itself by a krona.
func TestParseMinorUnits(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"0", 0},
		{"1295", 129500},
		{"1295.50", 129550},
		{"1295,50", 129550},  // a Swedish counter types a comma
		{"1 295,50", 129550}, // and a thousands space
		{"1295,5", 129550},   // one decimal means tenths, not hundredths
		{"0,01", 1},
		{"1295.00", 129500},
	} {
		got, err := parseMinorUnits(c.in)
		if err != nil {
			t.Errorf("parseMinorUnits(%q) = %v, want %d", c.in, err, c.want)
			continue
		}
		if got != c.want {
			t.Errorf("parseMinorUnits(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseMinorUnitsRefusesNonsense(t *testing.T) {
	for _, in := range []string{"-5", "abc", "12.345", "1295,505", "1.2.3"} {
		if got, err := parseMinorUnits(in); err == nil {
			t.Errorf("parseMinorUnits(%q) = %d, want an error", in, got)
		}
	}
}
