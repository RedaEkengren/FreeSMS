package workshop

import (
	"testing"
	"time"
)

// What a return suggests charging, from the two ends of a rental: the
// kilometres over the allowance at the agreed price, the eighths short,
// and the minutes past the agreed end -- none when nothing was agreed or
// read, and nothing for a return under a minute late.
func TestWhatAReturnSuggestsCharging(t *testing.T) {
	ends := time.Date(2031, 5, 12, 9, 0, 0, 0, time.UTC)
	km, fuel := func(n int64) *int64 { return &n }, func(n int) *int { return &n }
	allowance, price := 200, int64(300)
	out := &RentalHandover{OdometerKm: km(1000), FuelEighths: fuel(8)}
	in := &RentalHandover{OdometerKm: km(1250), FuelEighths: fuel(6), At: ends.Add(90 * time.Minute)}
	got := proposedCharges(out, in, ends, &allowance, &price)
	if len(got) != 3 || got[0].Quantity != 50 || *got[0].UnitPriceMinor != 300 || got[1].Quantity != 2 || got[2].Kind != ChargeLate || got[2].Quantity != 90 {
		t.Errorf("a late return, short of fuel, over the allowance: %+v", got)
	}
	in = &RentalHandover{OdometerKm: km(1250), At: ends.Add(30 * time.Second)}
	if got := proposedCharges(out, in, ends, nil, nil); len(got) != 0 {
		t.Errorf("unlimited kilometres, no fuel read, half a minute late: %+v", got)
	}
	if got := proposedCharges(nil, in, ends, &allowance, &price); got != nil {
		t.Errorf("no check-out: %+v", got)
	}
}
