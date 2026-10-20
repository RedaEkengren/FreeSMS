package workshop_test

import (
	"fmt"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A page prints a quantity the way Go prints a float, so a quantity worked out
// by subtraction must come out as the decimals a person would write.
func TestAQuantityWorkedOutIsWrittenWithoutFloatNoise(t *testing.T) {
	cases := []struct {
		name string
		got  float64
		want string
	}{
		{"available", workshop.Part{OnHand: 0.3, Reserved: 0.1}.Available(), "0.2"},
		{"available, short", workshop.Part{OnHand: -14, Reserved: 0.505}.Available(), "-14.505"},
		{"left to bring", workshop.PartRequest{Quantity: 0.3, Received: 0.1}.Rest(), "0.2"},
		{"left to take out", workshop.JobPart{Priced: 0.3, TakenOut: 0.1}.Remaining(), "0.2"},
		{"left on an order", workshop.SupplyLine{Ordered: 0.3, Received: 0.1}.Rest(), "0.2"},
	}
	for _, c := range cases {
		if got := fmt.Sprint(c.got); got != c.want {
			t.Errorf("%s = %s, want %s", c.name, got, c.want)
		}
	}
}
