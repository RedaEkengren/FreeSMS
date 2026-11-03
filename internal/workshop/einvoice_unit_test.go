package workshop

import "testing"

// The account on an electronic invoice is read out of the payment details
// as the payment code reads it: the number, and not a telephone number
// printed after it.
func TestTheElectronicInvoicesAccountIsTheGiroAlone(t *testing.T) {
	for _, c := range []struct{ scheme, printed, want string }{
		{"bankgiro", "Bankgiro 123-4567", "1234567"},
		{"bankgiro", "Bankgiro 5050-1055 08 123 45", "50501055"},
		{"plusgiro", "PG 17 60 99-0, tel 08-123 45", "1760990"},
		{"bankgiro", "Ring oss", ""},
		{"iban", "SE45 5000 0000 0583 9825 7466", "SE4550000000058398257466"},
	} {
		if got := paymentAccount(c.scheme, c.printed); got != c.want {
			t.Errorf("%s %q: %q, want %q", c.scheme, c.printed, got, c.want)
		}
	}
}
